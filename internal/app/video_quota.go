package app

// /admin/api/video-quota — per-account video quota view, read from each account's own
// Gemini chat history (Gemini exposes no quota API; this is the ground truth we have).
//
// For every enabled account: list recent conversations (rpc MaZiqc), read each one
// touched in the last few hours (rpc hNvQHb), and classify every video turn:
//   video          a generated video (counts against the 5-hour budget)
//   quota_refused  "I can create more videos as soon as your limit resets"
//   policy_refused "I can't generate that video"
//   pending        still generating
// Quota refusals sometimes carry Google's own reset time (a timestamp inside the
// refusal turn, measured: exactly first-usage + 5 h); otherwise it is estimated as
// first video in the window + 5 h. Read-only: no generation, no quota used.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const quotaWindow = 5 * time.Hour

type quotaEvent struct {
	At      int64  `json:"at"`
	Kind    string `json:"kind"`
	CID     string `json:"cid"`
	Title   string `json:"title,omitempty"`
	ResetAt int64  `json:"reset_at,omitempty"` // only on quota_refused, when Google reports it
}

type accountQuota struct {
	ID            int64        `json:"id"`
	Label         string       `json:"label"`
	Status        string       `json:"status"` // available | exhausted | error
	VideosInWin   int          `json:"videos_last_5h"`
	FirstInWin    int64        `json:"window_started_at,omitempty"`
	LastVideoAt   int64        `json:"last_video_at,omitempty"`
	LastRefusalAt int64        `json:"last_refusal_at,omitempty"`
	ResetAt       int64        `json:"reset_at,omitempty"`
	ResetSource   string       `json:"reset_source,omitempty"` // google | estimated
	Error         string       `json:"error,omitempty"`
	Events        []quotaEvent `json:"events,omitempty"`
}

func handleAdminVideoQuota(w http.ResponseWriter, r *http.Request) {
	lookback := 8 * time.Hour
	if h := atoiDefault(r.URL.Query().Get("hours"), 0); h > 0 && h <= 48 {
		lookback = time.Duration(h) * time.Hour
	}
	now := time.Now()
	var out []accountQuota
	for _, a := range accountList() {
		if a.Status != "enabled" {
			continue
		}
		out = append(out, videoQuotaFor(a, now, lookback))
	}
	resp := map[string]any{
		"now":            now.Unix(),
		"window_seconds": int(quotaWindow.Seconds()),
		"accounts":       out,
		"note":           "Gemini app limits are compute-based: refresh every 5 h, plus an unpublished weekly cap; chats share the budget. Measured ~8 videos per account per window.",
	}
	// 统计全靠会话历史：临时对话不进列表、自动删会话会把它删掉，这两种开着时
	// 本服务生成的视频在历史里看不到，计数会偏少。明说，不让人把 0 当成真没用。
	if temporaryChatEligible() || rtCfg().AutoDeleteConversation {
		resp["history_incomplete"] = "temporary_chat or auto_delete_conversation is on, so videos made through this proxy are not in the history and are not counted"
	}
	writeJSON(w, 200, resp)
}

// viaSlot 让统计的每一次上游请求都先过 acquireSlot：跟正式请求同一套出口规则
// （配了代理池就不直连，除非开了 fallback_direct），也记进同一份单 IP 预算。
// 优先用账号绑的出口；拿不到 slot 就报错，不排队等。
func viaSlot(a CookieAccount, fn func(proxyURL string) error) error {
	p, ok, err := acquireSlot(a.ProxyID)
	if !ok {
		return fmt.Errorf("no outbound slot: %w", err)
	}
	defer releaseSlot(p.ID)
	return fn(p.URL)
}

func videoQuotaFor(a CookieAccount, now time.Time, lookback time.Duration) accountQuota {
	q := accountQuota{ID: a.ID, Label: accountDisplayName(&a)}
	sapisid := extractSAPISID(a.Cookie)
	var xsrf string
	err := viaSlot(a, func(proxyURL string) (err error) {
		xsrf, err = getXSRF(a.Cookie, proxyURL)
		return err
	})
	if err != nil {
		// 只有确实是登录态问题才说 "not signed in"；断网、出口被拦、代理池
		// 无可用出口照原样报，免得让人去换一个其实没问题的号。
		q.Status, q.Error = "error", err.Error()
		if xsrfErrBlamesCookie(err) {
			q.Error = "not signed in: " + err.Error()
		}
		return q
	}
	var convs []convSummary
	err = viaSlot(a, func(proxyURL string) (err error) {
		convs, err = listConversations(a.Cookie, sapisid, xsrf, proxyURL, 40)
		return err
	})
	if err != nil {
		q.Status, q.Error = "error", err.Error()
		return q
	}
	since := now.Add(-lookback).Unix()
	for _, c := range convs {
		if c.Updated < since {
			continue
		}
		evs, ok := turnCacheGet(c.CID, c.Updated)
		if !ok {
			var body string
			err := viaSlot(a, func(proxyURL string) (err error) {
				body, err = pollHistoryRaw(c.CID, a.Cookie, sapisid, xsrf, proxyURL)
				return err
			})
			if err != nil {
				continue
			}
			evs = classifyVideoTurns(body)
			// Don't cache in-flight generations; they change once the video lands.
			cacheable := true
			for _, e := range evs {
				if e.Kind == "pending" {
					cacheable = false
				}
			}
			if cacheable {
				turnCachePut(c.CID, c.Updated, evs)
			}
			time.Sleep(150 * time.Millisecond) // stay gentle on the per-IP budget
		}
		for _, ev := range evs {
			if ev.At >= since {
				ev.CID, ev.Title = c.CID, c.Title
				q.Events = append(q.Events, ev)
			}
		}
	}
	sort.Slice(q.Events, func(i, j int) bool { return q.Events[i].At < q.Events[j].At })
	summarizeQuota(&q, now)
	return q
}

// summarizeQuota derives status / counts / reset time from the classified events.
func summarizeQuota(q *accountQuota, now time.Time) {
	winStart := now.Add(-quotaWindow).Unix()
	var lastRefusal *quotaEvent
	for i := range q.Events {
		ev := &q.Events[i]
		switch ev.Kind {
		case "video":
			q.LastVideoAt = ev.At
			if ev.At >= winStart {
				q.VideosInWin++
				if q.FirstInWin == 0 {
					q.FirstInWin = ev.At
				}
			}
		case "quota_refused":
			lastRefusal = ev
			q.LastRefusalAt = ev.At
		}
	}
	q.Status = "available"
	if lastRefusal != nil && lastRefusal.At > q.LastVideoAt {
		reset, src := lastRefusal.ResetAt, "google"
		if reset == 0 {
			// Estimate: first video in the 5 h before the refusal + 5 h.
			src = "estimated"
			for _, ev := range q.Events {
				if ev.Kind == "video" && ev.At >= lastRefusal.At-int64(quotaWindow.Seconds()) {
					reset = ev.At + int64(quotaWindow.Seconds())
					break
				}
			}
			if reset == 0 {
				reset = lastRefusal.At + int64(quotaWindow.Seconds())
			}
		}
		if reset > now.Unix() {
			q.Status, q.ResetAt, q.ResetSource = "exhausted", reset, src
		}
	}
	if q.Status == "available" && q.FirstInWin > 0 {
		q.ResetAt, q.ResetSource = q.FirstInWin+int64(quotaWindow.Seconds()), "estimated"
	}
}

// Per-conversation classification cache, keyed by cid + last-updated time: a repeat
// check only re-reads conversations that changed, so it costs ~1 request per account.
var (
	turnCacheMu sync.Mutex
	turnCache   = map[string][]quotaEvent{}
)

func turnCacheGet(cid string, updated int64) ([]quotaEvent, bool) {
	turnCacheMu.Lock()
	defer turnCacheMu.Unlock()
	v, ok := turnCache[fmt.Sprintf("%s@%d", cid, updated)]
	return v, ok
}

func turnCachePut(cid string, updated int64, evs []quotaEvent) {
	turnCacheMu.Lock()
	defer turnCacheMu.Unlock()
	if len(turnCache) > 5000 {
		turnCache = map[string][]quotaEvent{}
	}
	turnCache[fmt.Sprintf("%s@%d", cid, updated)] = evs
}

type convSummary struct {
	CID, Title string
	Updated    int64
}

// listConversations calls MaZiqc (the sidebar list): [[cid, title, null×3, [sec,ns], …], …].
func listConversations(cookie, sapisid, xsrf, proxyURL string, n int) ([]convSummary, error) {
	freq, _ := json.Marshal([]any{[]any{[]any{"MaZiqc", fmt.Sprintf(`[%d,null,[0,null,1]]`, n), nil, "generic"}}})
	form := url.Values{"f.req": {string(freq)}, "at": {xsrf}}
	ep := fmt.Sprintf("https://gemini.google.com/_/BardChatUi/data/batchexecute?rpcids=MaZiqc&bl=%s&hl=en&_reqid=%d&rt=c",
		currentBL(proxyURL), time.Now().UnixNano()%1000000)
	h := buildGeminiHeaders(cookie, sapisid, "")
	delete(h, "x-goog-ext-525001261-jspb")
	st, _, body, err := uploadPost(ep, h, []byte(form.Encode()), proxyURL)
	if err != nil {
		return nil, err
	}
	if st != 200 {
		return nil, fmt.Errorf("MaZiqc HTTP %d", st)
	}
	var out []convSummary
	for _, p := range wrbPayloads(string(body)) {
		if len(p) < 3 {
			continue
		}
		list, _ := p[2].([]any)
		for _, it := range list {
			c, _ := it.([]any)
			if len(c) < 6 {
				continue
			}
			cs := convSummary{}
			cs.CID, _ = c[0].(string)
			cs.Title, _ = c[1].(string)
			if ts, ok := c[5].([]any); ok && len(ts) > 0 {
				if f, ok := ts[0].(float64); ok {
					cs.Updated = int64(f)
				}
			}
			if cs.CID != "" {
				out = append(out, cs)
			}
		}
	}
	return out, nil
}

// wrbPayloads decodes every "wrb.fr" payload (the inner JSON) in a batchexecute body.
func wrbPayloads(body string) [][]any {
	var out [][]any
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[[") {
			continue
		}
		var arr []any
		if json.Unmarshal([]byte(line), &arr) != nil {
			continue
		}
		for _, it := range arr {
			row, ok := it.([]any)
			if !ok || len(row) < 3 || row[0] != "wrb.fr" {
				continue
			}
			s, _ := row[2].(string)
			var p []any
			if json.Unmarshal([]byte(s), &p) == nil {
				out = append(out, p)
			}
		}
	}
	return out
}

var unixTSRe = regexp.MustCompile(`\[(1[7-9]\d{8}),\d+\]`)

// classifyVideoTurns returns one event per video-tool turn in an hNvQHb body.
// Turn layout: t[0]=[cid,rid], t[3]=candidates, t[4]=[sec,ns] (turn time).
func classifyVideoTurns(body string) []quotaEvent {
	var out []quotaEvent
	for _, p := range wrbPayloads(body) {
		if len(p) == 0 {
			continue
		}
		turns, _ := p[0].([]any)
		for _, t := range turns {
			tl, ok := t.([]any)
			if !ok || len(tl) < 5 {
				continue
			}
			raw, _ := json.Marshal(t)
			s := string(raw)
			if !strings.Contains(s, "video_generation") {
				continue
			}
			ev := quotaEvent{}
			if ts, ok := tl[4].([]any); ok && len(ts) > 0 {
				if f, ok := ts[0].(float64); ok {
					ev.At = int64(f)
				}
			}
			one, _ := json.Marshal([]any{[]any{t}})
			env, _ := json.Marshal([]any{[]any{"wrb.fr", "hNvQHb", string(one)}})
			switch {
			case strings.Contains(s, "out of video generation quota") || strings.Contains(s, "as soon as your limit resets"):
				ev.Kind = "quota_refused"
				for _, m := range unixTSRe.FindAllStringSubmatch(s, -1) {
					var v int64
					fmt.Sscan(m[1], &v)
					if v > ev.At+60 && v > ev.ResetAt {
						ev.ResetAt = v
					}
				}
			case len(pickResponseDataURLs(collectDownloadURLs(string(env)))) > 0:
				ev.Kind = "video"
			case strings.Contains(s, "can't generate that video"):
				ev.Kind = "policy_refused"
			default:
				ev.Kind = "pending"
			}
			out = append(out, ev)
		}
	}
	return out
}
