package app

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestXSRFProactiveBackgroundRefresh(t *testing.T) {
	const ck = "SID=proactive_test; SAPISID=sap_test; __Secure-1PSID=psid_test"
	key := xsrfKey(ck, 0)

	origFetcher := xsrfFetcher
	defer func() {
		xsrfFetcher = origFetcher
		xsrfMu.Lock()
		delete(xsrfCache, key)
		xsrfMu.Unlock()
	}()

	var calls int32
	done := make(chan struct{}, 4)
	xsrfFetcher = func(cookie string, authuser int, proxyURL string) (xsrfEntry, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(20 * time.Millisecond)
		done <- struct{}{}
		return xsrfEntry{
			token:   "new-snlm0e-token",
			pushID:  "new-push",
			pctx:    "new-pctx",
			fetched: time.Now(),
		}, nil
	}

	// 1) 放入一条 16 分钟前的缓存（超过 xsrfRefreshAhead=15m，但仍在 xsrfTTL=20m 内）
	xsrfMu.Lock()
	xsrfCache[key] = xsrfEntry{
		token:    "old-snlm0e-token",
		fetched:  time.Now().Add(-16 * time.Minute),
		cookie:   ck,
		authuser: 0,
	}
	xsrfMu.Unlock()

	// 连续两次 getXSRF 都应立刻返回旧 token（不阻塞），且只触发 1 次后台刷新
	tok1, err := getXSRF(ck, 0, "")
	if err != nil || tok1 != "old-snlm0e-token" {
		t.Fatalf("getXSRF #1 = (%q, %v), want old token immediately", tok1, err)
	}
	tok2, err := getXSRF(ck, 0, "")
	if err != nil || tok2 != "old-snlm0e-token" {
		t.Fatalf("getXSRF #2 = (%q, %v), want old token immediately", tok2, err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("background XSRF refresh did not run")
	}
	// 等锁内回写完成
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		tok, _ := getXSRF(ck, 0, "")
		if tok == "new-snlm0e-token" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if tok, _ := getXSRF(ck, 0, ""); tok != "new-snlm0e-token" {
		t.Fatalf("cache should hold refreshed token, got %q", tok)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("dedup failed: expected 1 background refresh, got %d", got)
	}
}

func TestRefreshExpiringXSRFEvictsDisabledAccount(t *testing.T) {
	const ckEnabled = "SID=en_xsrf; SAPISID=sap_en; __Secure-1PSID=psid_en"
	const ckDisabled = "SID=dis_xsrf; SAPISID=sap_dis; __Secure-1PSID=psid_dis"
	idEn, err := accountInsert("xsrf-en", ckEnabled, "")
	if err != nil {
		t.Fatalf("insert enabled: %v", err)
	}
	idDis, err := accountInsert("xsrf-dis", ckDisabled, "")
	if err != nil {
		t.Fatalf("insert disabled: %v", err)
	}
	_ = accountSetStatus(idDis, "disabled")

	kEn := xsrfKey(ckEnabled, 0)
	kDis := xsrfKey(ckDisabled, 0)
	origFetcher := xsrfFetcher
	defer func() {
		xsrfFetcher = origFetcher
		_ = accountDelete(idEn)
		_ = accountDelete(idDis)
		xsrfMu.Lock()
		delete(xsrfCache, kEn)
		delete(xsrfCache, kDis)
		xsrfMu.Unlock()
	}()

	xsrfFetcher = func(cookie string, authuser int, proxyURL string) (xsrfEntry, error) {
		return xsrfEntry{token: "refreshed", fetched: time.Now()}, nil
	}

	xsrfMu.Lock()
	xsrfCache[kEn] = xsrfEntry{token: "old-en", fetched: time.Now().Add(-16 * time.Minute), cookie: ckEnabled}
	xsrfCache[kDis] = xsrfEntry{token: "old-dis", fetched: time.Now().Add(-16 * time.Minute), cookie: ckDisabled}
	xsrfMu.Unlock()

	refreshExpiringXSRF()

	xsrfMu.Lock()
	_, disExists := xsrfCache[kDis]
	xsrfMu.Unlock()
	if disExists {
		t.Fatal("disabled account entry should be evicted from xsrfCache")
	}
}
