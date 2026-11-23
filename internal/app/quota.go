package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// probeFallbackCooldown is the safe, bounded cooldown applied when qpEbW cannot be reached
// or returns an unexpected payload, preventing a total 5-hour lockout.
const probeFallbackCooldown = 5 * time.Minute

// quotaNow is a mockable hook for time.Now to ensure deterministic unit testing.
var quotaNow = time.Now

// AccountQuota represents the upstream ground-truth quota for a Gemini cookie account.
type AccountQuota struct {
	AccountID int64     `json:"account_id"`
	Budget    int64     `json:"budget"`
	Remaining int64     `json:"remaining"`
	Ratio     float64   `json:"ratio"`
	ResetAt   time.Time `json:"reset_at"`
	Exhausted bool      `json:"exhausted"`
	FetchedAt time.Time `json:"fetched_at"`
	Error     string    `json:"error,omitempty"`
}

type quotaCacheEntry struct {
	quota   AccountQuota
	expires time.Time
}

var (
	quotaCacheMu sync.RWMutex
	quotaCache   = map[int64]quotaCacheEntry{}
)

// getCachedAccountQuota returns a copy of the cached quota if still valid.
func getCachedAccountQuota(accountID int64) (AccountQuota, bool) {
	quotaCacheMu.RLock()
	defer quotaCacheMu.RUnlock()
	entry, ok := quotaCache[accountID]
	if !ok || quotaNow().After(entry.expires) {
		return AccountQuota{}, false
	}
	return entry.quota, true
}

// putCachedAccountQuota caches an account quota entry with dynamic TTL.
func putCachedAccountQuota(accountID int64, q AccountQuota) {
	now := quotaNow()
	var ttl time.Duration
	if q.Error != "" {
		// Negative cache: avoid pounding a failing RPC
		ttl = 30 * time.Second
	} else if q.Exhausted && !q.ResetAt.IsZero() {
		// If exhausted and reset is coming up soon, expire near ResetAt + 15s
		until := q.ResetAt.Sub(now) + 15*time.Second
		if until > 0 && until < 3*time.Minute {
			ttl = until
		} else {
			ttl = 3 * time.Minute
		}
	} else {
		// Normal healthy quota: 3 minutes cache
		ttl = 3 * time.Minute
	}

	quotaCacheMu.Lock()
	quotaCache[accountID] = quotaCacheEntry{
		quota:   q,
		expires: now.Add(ttl),
	}
	quotaCacheMu.Unlock()
}

// quotaCall represents an in-flight singleflight call for an account quota probe.
type quotaCall struct {
	wg  sync.WaitGroup
	val AccountQuota
	err error
}

type quotaSingleflight struct {
	mu sync.Mutex
	m  map[int64]*quotaCall
}

var quotaSF quotaSingleflight

// Do executes and deduplicates concurrent quota fetches for the same accountID.
func (g *quotaSingleflight) Do(key int64, fn func() (AccountQuota, error)) (AccountQuota, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[int64]*quotaCall)
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := new(quotaCall)
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()

	return c.val, c.err
}

// getOrFetchAccountQuota retrieves quota from cache or initiates a singleflight upstream probe.
func getOrFetchAccountQuota(ctx context.Context, accountID int64) (AccountQuota, error) {
	if q, ok := getCachedAccountQuota(accountID); ok {
		return q, nil
	}

	return quotaSF.Do(accountID, func() (AccountQuota, error) {
		// Recheck cache inside singleflight barrier
		if q, ok := getCachedAccountQuota(accountID); ok {
			return q, nil
		}

		q, err := fetchAccountQuotaUpstream(ctx, accountID)
		if err != nil {
			errQuota := AccountQuota{
				AccountID: accountID,
				FetchedAt: quotaNow(),
				Error:     err.Error(),
			}
			putCachedAccountQuota(accountID, errQuota)
			return errQuota, err
		}

		putCachedAccountQuota(accountID, q)
		return q, nil
	})
}

// fetchAccountQuotaUpstream executes the qpEbW batchexecute RPC against Gemini Web.
func fetchAccountQuotaUpstream(ctx context.Context, accountID int64) (AccountQuota, error) {
	a := accountByID(accountID)
	if a == nil {
		return AccountQuota{}, fmt.Errorf("account #%d not found", accountID)
	}
	if a.Cookie == "" {
		return AccountQuota{}, errors.New("account has no cookie")
	}

	sapisid := extractSAPISID(a.Cookie)
	if sapisid == "" {
		return AccountQuota{}, errors.New("account cookie missing SAPISID")
	}

	var parsedQuotas []AccountQuota
	err := viaSlot(*a, func(proxyURL string) error {
		xsrf, err := getXSRF(a.Cookie, a.AuthUser, proxyURL)
		if err != nil {
			return fmt.Errorf("getXSRF failed: %w", err)
		}

		freq, _ := json.Marshal([]any{[]any{[]any{"qpEbW", "[]", nil, "generic"}}})
		form := url.Values{"f.req": {string(freq)}, "at": {xsrf}}
		ep := fmt.Sprintf("%s/_/BardChatUi/data/batchexecute?rpcids=qpEbW&bl=%s&hl=en&_reqid=%d&rt=c",
			geminiBase(a.AuthUser), currentBL(proxyURL), time.Now().UnixNano()%1000000)

		h := buildGeminiHeaders(a.Cookie, sapisid, "")
		setAuthUserHeaders(h, a.AuthUser)
		delete(h, "x-goog-ext-525001261-jspb")

		// Tight timeout for the probe (4 seconds)
		type probeResp struct {
			status int
			body   []byte
			err    error
		}
		ch := make(chan probeResp, 1)

		go func() {
			st, _, b, e := uploadPost(ep, h, []byte(form.Encode()), proxyURL)
			ch <- probeResp{status: st, body: b, err: e}
		}()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(4 * time.Second):
			return errors.New("qpEbW upstream timeout after 4s")
		case r := <-ch:
			if r.err != nil {
				return fmt.Errorf("uploadPost failed: %w", r.err)
			}
			if r.status != 200 {
				return fmt.Errorf("qpEbW HTTP status %d", r.status)
			}

			payloads := wrbPayloads(string(r.body))
			if len(payloads) == 0 {
				logf("[quota] WARN: qpEbW returned 0 wrb payloads for account #%d. Raw body: %.200s", accountID, string(r.body))
				return errors.New("qpEbW returned no wrb.fr payloads")
			}

			for _, p := range payloads {
				q, parseErr := parseQuotaPayload(accountID, p)
				if parseErr == nil {
					parsedQuotas = append(parsedQuotas, q)
					break
				}
			}
			if len(parsedQuotas) == 0 {
				return errors.New("failed to parse valid quota from qpEbW payload")
			}
			return nil
		}
	})

	if err != nil {
		return AccountQuota{}, err
	}
	if len(parsedQuotas) == 0 {
		return AccountQuota{}, errors.New("no quota extracted")
	}
	return parsedQuotas[0], nil
}

// parseQuotaPayload parses the inner array from wrb.fr payload of qpEbW.
// Expected Protobuf jspb array shape: [[], 2, 17, [sec, (nsec)], budget, (remaining)]
// - In batchexecute responses, Google often wraps the inner array in outer envelopes:
//   e.g. [[[[], 2, 17, [sec, nsec], budget, remaining]], ""]
// - payload[3] may be [sec], [sec, nsec], or null.
// - payload[4] is total budget units.
// - payload[5] is remaining units. Protobuf jspb omits zero-values, so if remaining is 0,
//   payload[5] is frequently null or omitted entirely (len == 5).
func parseQuotaPayload(accountID int64, payload []any) (AccountQuota, error) {
	// Unwrap any outer envelopes (e.g. [[[...]], ""])
	for len(payload) > 0 && len(payload) < 5 {
		if inner, ok := payload[0].([]any); ok {
			payload = inner
		} else {
			break
		}
	}

	if len(payload) < 5 {
		raw, _ := json.Marshal(payload)
		logf("[quota] WARN: qpEbW payload too short (<5) for account #%d: %s", accountID, string(raw))
		return AccountQuota{}, fmt.Errorf("qpEbW payload too short (len=%d)", len(payload))
	}

	q := AccountQuota{
		AccountID: accountID,
		FetchedAt: quotaNow(),
	}

	// 1. Reset timestamp at index 3: [sec, (nsec)]
	if tsArr, ok := payload[3].([]any); ok && len(tsArr) > 0 {
		sec := anyToInt64(tsArr[0])
		var nsec int64
		if len(tsArr) > 1 {
			nsec = anyToInt64(tsArr[1])
		}
		if sec > 0 {
			q.ResetAt = time.Unix(sec, nsec)
		}
	}

	// 2. Budget at index 4
	q.Budget = anyToInt64(payload[4])

	// 3. Remaining at index 5 (optional/omitted when 0)
	if len(payload) >= 6 && payload[5] != nil {
		q.Remaining = anyToInt64(payload[5])
	} else {
		// Omitted or null in jspb indicates 0 remaining
		q.Remaining = 0
	}

	// 4. Calculate ratio and exhaustion
	if q.Budget <= 0 {
		q.Ratio = 0.0
		q.Exhausted = true
	} else {
		q.Ratio = float64(q.Remaining) / float64(q.Budget)
		q.Exhausted = (q.Remaining <= 0 || q.Ratio <= 0.001)
	}

	return q, nil
}

// anyToInt64 safely extracts an int64 from any JSON-unmarshaled value.
func anyToInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	default:
		return 0
	}
}

// Transient shed circuit breaker:
// Tracks consecutive transient downgrades for {accountID, model} when quota is intact.
// If consecutive sheds reach 3 within 2 minutes, escalate to a 10-minute cooldown.
type shedRecord struct {
	count     int
	firstShed time.Time
	lastShed  time.Time
}

var (
	shedMu      sync.Mutex
	shedRecords = map[downgradeKey]shedRecord{}
)

func recordTransientShed(accountID int64, model string) int {
	now := quotaNow()
	shedMu.Lock()
	defer shedMu.Unlock()
	key := downgradeKey{accountID: accountID, model: model}
	rec := shedRecords[key]
	if now.Sub(rec.lastShed) > 2*time.Minute {
		rec = shedRecord{count: 1, firstShed: now, lastShed: now}
	} else {
		rec.count++
		rec.lastShed = now
	}
	shedRecords[key] = rec
	return rec.count
}

func clearTransientShed(accountID int64, model string) {
	shedMu.Lock()
	defer shedMu.Unlock()
	if model == "" {
		for k := range shedRecords {
			if k.accountID == accountID {
				delete(shedRecords, k)
			}
		}
	} else {
		delete(shedRecords, downgradeKey{accountID: accountID, model: model})
	}
}
