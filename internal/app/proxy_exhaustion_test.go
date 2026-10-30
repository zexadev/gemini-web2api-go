package app

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func withTestProxySlot(t *testing.T, proxy Proxy, configure func(*RuntimeConfig)) {
	t.Helper()
	proxyMu.Lock()
	oldProxies := proxyCache
	proxyCache = []Proxy{proxy}
	proxyMu.Unlock()
	rtMu.Lock()
	oldConfig := rtVal
	rtVal = RuntimeConfig{
		PerIPConcurrent: 5, PerIPRPM: 30, PerIPRPH: 80,
		ProxyCooldownMin: 120, FallbackDirect: false,
	}
	if configure != nil {
		configure(&rtVal)
	}
	rtMu.Unlock()
	t.Cleanup(func() {
		proxyMu.Lock()
		proxyCache = oldProxies
		proxyMu.Unlock()
		rtMu.Lock()
		rtVal = oldConfig
		rtMu.Unlock()
		slotsMu.Lock()
		delete(slots, proxy.ID)
		slotsMu.Unlock()
	})
}

func TestProxyCircuitBreakerDoesNotReportRPH(t *testing.T) {
	const id int64 = 99900901
	withTestProxySlot(t, Proxy{
		ID: id, URL: "http://127.0.0.1:7897", Enabled: true,
		FailCount: proxyFailThreshold, LastUsed: time.Now().Unix(),
	}, nil)
	_, ok, err := acquireSlot(id)
	if ok || err == nil || !strings.Contains(err.Error(), "proxy_unavailable") {
		t.Fatalf("want circuit-breaker error, got ok=%v err=%v", ok, err)
	}
	var rle *RateLimitError
	if errors.As(err, &rle) {
		t.Fatalf("circuit breaker wrongly classified as a rate limit: %v", err)
	}
}

func TestProxyConcurrencyLimitReportsConcurrent(t *testing.T) {
	const id int64 = 99900902
	withTestProxySlot(t, Proxy{ID: id, URL: "http://127.0.0.1:7897", Enabled: true},
		func(c *RuntimeConfig) { c.PerIPConcurrent = 1 })
	s := getSlot(id)
	s.mu.Lock()
	s.inflight = 1
	s.mu.Unlock()
	_, ok, err := acquireSlot(id)
	var rle *RateLimitError
	if ok || !errors.As(err, &rle) || rle.Reason != "concurrent" {
		t.Fatalf("want true concurrent rate-limit, got ok=%v err=%v", ok, err)
	}
}

func TestProxyCircuitBreakerReturnsHTTP502Not429(t *testing.T) {
	const id int64 = 99900903
	withTestProxySlot(t, Proxy{
		ID: id, URL: "http://127.0.0.1:7897", Enabled: true,
		FailCount: proxyFailThreshold, LastUsed: time.Now().Unix(),
	}, nil)
	request := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gemini-3.6-flash","messages":[{"role":"user","content":"hello"}]}`))
	response := httptest.NewRecorder()
	handleChatCompletions(response, request)
	if response.Code != 502 || !strings.Contains(response.Body.String(), "proxy_unavailable") {
		t.Fatalf("want 502 proxy_unavailable, got HTTP %d %s", response.Code, response.Body.String())
	}
}
