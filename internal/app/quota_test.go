package app

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseQuotaPayload_Valid(t *testing.T) {
	// Sample verified qpEbW payload from live upstream
	payload := []any{
		[]any{},
		float64(2),
		float64(17),
		[]any{float64(1795365365), float64(842252000)},
		float64(48384),
		float64(39959),
	}

	q, err := parseQuotaPayload(6, payload)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}

	if q.AccountID != 6 {
		t.Errorf("expected AccountID 6, got %d", q.AccountID)
	}
	if q.Budget != 48384 {
		t.Errorf("expected Budget 48384, got %d", q.Budget)
	}
	if q.Remaining != 39959 {
		t.Errorf("expected Remaining 39959, got %d", q.Remaining)
	}
	if q.Exhausted {
		t.Errorf("expected Exhausted to be false, got true")
	}
	expectedRatio := float64(39959) / float64(48384)
	if q.Ratio < expectedRatio-0.001 || q.Ratio > expectedRatio+0.001 {
		t.Errorf("expected Ratio ~%.4f, got %.4f", expectedRatio, q.Ratio)
	}
	if q.ResetAt.Unix() != 1795365365 {
		t.Errorf("expected ResetAt unix 1795365365, got %d", q.ResetAt.Unix())
	}
}

func TestParseQuotaPayload_JspbOmittedRemaining(t *testing.T) {
	// Case 1: len == 5 (omitted entirely in jspb)
	payloadLen5 := []any{
		[]any{},
		float64(2),
		float64(17),
		[]any{float64(1795365365)},
		float64(48384),
	}
	q1, err := parseQuotaPayload(1, payloadLen5)
	if err != nil {
		t.Fatalf("unexpected parse error on len 5: %v", err)
	}
	if q1.Remaining != 0 {
		t.Errorf("expected Remaining 0, got %d", q1.Remaining)
	}
	if !q1.Exhausted {
		t.Errorf("expected Exhausted true when remaining is omitted")
	}
	if q1.Ratio != 0.0 {
		t.Errorf("expected Ratio 0.0, got %f", q1.Ratio)
	}
	if q1.ResetAt.Unix() != 1795365365 {
		t.Errorf("expected ResetAt unix 1795365365, got %d", q1.ResetAt.Unix())
	}

	// Case 2: len == 6 with null
	payloadNullRemaining := []any{
		[]any{},
		float64(2),
		float64(17),
		[]any{float64(1795365365)},
		float64(48384),
		nil,
	}
	q2, err := parseQuotaPayload(2, payloadNullRemaining)
	if err != nil {
		t.Fatalf("unexpected parse error on null remaining: %v", err)
	}
	if q2.Remaining != 0 || !q2.Exhausted {
		t.Errorf("expected Remaining 0 and Exhausted true on nil remaining, got %d %v", q2.Remaining, q2.Exhausted)
	}
}

func TestParseQuotaPayload_Malformed(t *testing.T) {
	// Too short (< 5)
	payloadShort := []any{[]any{}, float64(2), float64(17)}
	_, err := parseQuotaPayload(1, payloadShort)
	if err == nil {
		t.Fatal("expected error on payload with len < 5, got nil")
	}
}

func TestParseQuotaPayload_BudgetZero(t *testing.T) {
	payloadZeroBudget := []any{
		[]any{},
		float64(2),
		float64(17),
		[]any{float64(1795365365)},
		float64(0),
		float64(0),
	}
	q, err := parseQuotaPayload(1, payloadZeroBudget)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if !q.Exhausted || q.Ratio != 0.0 {
		t.Errorf("expected budget 0 to yield Exhausted=true and Ratio=0.0")
	}
}

func TestQuotaCache_TTL_And_NegativeCache(t *testing.T) {
	mockTime := time.Date(2026, 11, 22, 20, 0, 0, 0, time.UTC)
	oldNow := quotaNow
	quotaNow = func() time.Time { return mockTime }
	defer func() { quotaNow = oldNow }()

	quotaCacheMu.Lock()
	quotaCache = map[int64]quotaCacheEntry{}
	quotaCacheMu.Unlock()

	// 1. Normal healthy quota (TTL 3 minutes)
	q1 := AccountQuota{
		AccountID: 10,
		Budget:    1000,
		Remaining: 800,
		Ratio:     0.8,
		FetchedAt: mockTime,
	}
	putCachedAccountQuota(10, q1)

	// Within TTL
	mockTime = mockTime.Add(2 * time.Minute)
	cached, ok := getCachedAccountQuota(10)
	if !ok || cached.Remaining != 800 {
		t.Fatalf("expected valid cache hit at +2m, got ok=%v", ok)
	}

	// Past TTL (+3m1s)
	mockTime = mockTime.Add(1*time.Minute + 1*time.Second)
	_, ok = getCachedAccountQuota(10)
	if ok {
		t.Fatal("expected cache miss at +3m1s")
	}

	// 2. Negative cache on error (TTL 30 seconds)
	qErr := AccountQuota{
		AccountID: 20,
		Error:     "upstream 502",
		FetchedAt: mockTime,
	}
	putCachedAccountQuota(20, qErr)

	mockTime = mockTime.Add(20 * time.Second)
	cachedErr, ok := getCachedAccountQuota(20)
	if !ok || cachedErr.Error == "" {
		t.Fatalf("expected valid negative cache hit at +20s, got ok=%v", ok)
	}

	mockTime = mockTime.Add(15 * time.Second) // total +35s
	_, ok = getCachedAccountQuota(20)
	if ok {
		t.Fatal("expected negative cache expiry after 30s")
	}

	// 3. Near-term reset (e.g. resets in 45s, should expire at 45s+15s = 60s)
	resetTime := mockTime.Add(45 * time.Second)
	qNearReset := AccountQuota{
		AccountID: 30,
		Budget:    1000,
		Remaining: 0,
		Exhausted: true,
		ResetAt:   resetTime,
		FetchedAt: mockTime,
	}
	putCachedAccountQuota(30, qNearReset)

	mockTime = mockTime.Add(50 * time.Second) // 50s < 60s
	_, ok = getCachedAccountQuota(30)
	if !ok {
		t.Fatal("expected cache hit at 50s (before reset+15s)")
	}

	mockTime = mockTime.Add(15 * time.Second) // total 65s > 60s
	_, ok = getCachedAccountQuota(30)
	if ok {
		t.Fatal("expected cache miss at 65s (after reset+15s)")
	}
}

func TestQuotaSingleflight_Deduplication(t *testing.T) {
	var sf quotaSingleflight
	var callCount int64

	var wg sync.WaitGroup
	const goroutines = 10

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q, err := sf.Do(99, func() (AccountQuota, error) {
				atomic.AddInt64(&callCount, 1)
				time.Sleep(50 * time.Millisecond) // simulate upstream RPC latency
				return AccountQuota{AccountID: 99, Budget: 50000, Remaining: 40000}, nil
			})
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if q.Remaining != 40000 {
				t.Errorf("unexpected remaining: %d", q.Remaining)
			}
		}()
	}

	wg.Wait()

	if count := atomic.LoadInt64(&callCount); count != 1 {
		t.Fatalf("expected exactly 1 upstream probe call across %d concurrent callers, got %d", goroutines, count)
	}
}

func TestTransientShedCircuitBreaker(t *testing.T) {
	mockTime := time.Date(2026, 11, 22, 20, 0, 0, 0, time.UTC)
	oldNow := quotaNow
	quotaNow = func() time.Time { return mockTime }
	defer func() { quotaNow = oldNow }()

	clearTransientShed(100, "")

	// 1st shed
	c1 := recordTransientShed(100, "4 Neon")
	if c1 != 1 {
		t.Fatalf("expected count 1, got %d", c1)
	}

	// 2nd shed 30s later
	mockTime = mockTime.Add(30 * time.Second)
	c2 := recordTransientShed(100, "4 Neon")
	if c2 != 2 {
		t.Fatalf("expected count 2, got %d", c2)
	}

	// 3rd shed 30s later (within 2-minute window)
	mockTime = mockTime.Add(30 * time.Second)
	c3 := recordTransientShed(100, "4 Neon")
	if c3 != 3 {
		t.Fatalf("expected count 3 (circuit breaker threshold), got %d", c3)
	}

	// Clear on success
	clearTransientShed(100, "4 Neon")
	cAfterClear := recordTransientShed(100, "4 Neon")
	if cAfterClear != 1 {
		t.Fatalf("expected count to reset to 1 after clear, got %d", cAfterClear)
	}

	// Window expiry resets counter
	mockTime = mockTime.Add(3 * time.Minute) // > 2 minutes
	cAfterExpiry := recordTransientShed(100, "4 Neon")
	if cAfterExpiry != 1 {
		t.Fatalf("expected count to reset to 1 after 3 minutes, got %d", cAfterExpiry)
	}
}
