package app

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// withTestProxyPool 替换进程内代理池和限流配置，测试结束恢复。
func withTestProxyPool(t *testing.T, proxies []Proxy, configure func(*RuntimeConfig)) {
	t.Helper()
	proxyMu.Lock()
	oldProxies := proxyCache
	proxyCache = proxies
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
		for _, p := range proxies {
			delete(slots, p.ID)
		}
		slotsMu.Unlock()
		bindingMu.Lock()
		exclusiveOwnerByProxy = map[int64]int64{}
		exclusiveProxyByAccount = map[int64]int64{}
		bindingMu.Unlock()
	})
}

func addProxyTestAccount(t *testing.T, label, sapisid, psid string) *CookieAccount {
	t.Helper()
	id, err := accountAdd(label, "SAPISID="+sapisid+"; __Secure-1PSID="+psid, "")
	if err != nil {
		t.Fatalf("accountAdd: %v", err)
	}
	t.Cleanup(func() { _ = accountDelete(id) })
	a := accountByID(id)
	if a == nil {
		t.Fatalf("account %d disappeared", id)
	}
	return a
}

func TestExclusiveProxyIsHiddenFromOthers(t *testing.T) {
	a := addProxyTestAccount(t, "exclusive-owner", "sap-a", "psid-a")
	b := addProxyTestAccount(t, "shared-user", "sap-b", "psid-b")
	p1 := Proxy{ID: 990001, Name: "owned", URL: "http://127.0.0.1:19001", Enabled: true}
	p2 := Proxy{ID: 990002, Name: "shared", URL: "http://127.0.0.1:19002", Enabled: true}
	withTestProxyPool(t, []Proxy{p1, p2}, nil)

	if err := accountBindProxy(a.ID, p1.ID, true); err != nil {
		t.Fatalf("bind exclusive: %v", err)
	}
	t.Cleanup(func() { _ = accountUnbindProxy(a.ID) })

	// 匿名只能看到共享代理。
	for i := 0; i < 4; i++ {
		p, ok, reason := pickProxyFor(0, 0, 0)
		if !ok {
			t.Fatalf("anonymous pick failed: %s", reason)
		}
		if p.ID != p2.ID {
			t.Fatalf("anonymous got proxy #%d, want shared #%d", p.ID, p2.ID)
		}
		slotRelease(p.ID)
	}

	// 其他账号也不能用别人独占的出口。
	p, ok, reason := pickProxyFor(0, b.ID, 0)
	if !ok {
		t.Fatalf("other account pick failed: %s", reason)
	}
	if p.ID != p2.ID {
		t.Fatalf("other account got proxy #%d, want shared #%d", p.ID, p2.ID)
	}
	slotRelease(p.ID)

	// 独占账号严格用绑定出口。
	p, ok, reason = pickProxyFor(0, a.ID, p1.ID)
	if !ok {
		t.Fatalf("owner strict pick failed: %s", reason)
	}
	if p.ID != p1.ID {
		t.Fatalf("owner got proxy #%d, want #%d", p.ID, p1.ID)
	}
	slotRelease(p.ID)
}

func TestExclusiveCookieCoolsWithProxy(t *testing.T) {
	a := addProxyTestAccount(t, "cooling-owner", "sap-c", "psid-c")
	p := Proxy{
		ID: 990003, Name: "cooling", URL: "http://127.0.0.1:19003",
		Enabled: true, FailCount: proxyFailThreshold, LastUsed: time.Now().Unix(),
	}
	withTestProxyPool(t, []Proxy{p}, func(c *RuntimeConfig) { c.ProxyCooldownMin = 120 })
	if err := accountBindProxy(a.ID, p.ID, true); err != nil {
		t.Fatalf("bind exclusive: %v", err)
	}
	t.Cleanup(func() { _ = accountUnbindProxy(a.ID) })

	fresh := accountByID(a.ID)
	st := accountProxyStatus(*fresh, time.Now())
	if st.State != "cooling" || st.RetryAfter <= 0 {
		t.Fatalf("status = %+v, want cooling with retry", st)
	}

	_, _, err := pickAccountForModel(ModelConfig{
		AllowedAccounts: map[int64]bool{a.ID: true},
	}, nil)
	var aue *AccountUnavailableError
	if !errors.As(err, &aue) || aue.Reason != "proxy_cooling" || !aue.PoolExhausted {
		t.Fatalf("pickAccountForModel err = %v, want pool-exhausted proxy_cooling", err)
	}

	_, ok, err := acquireSlotForAccount(fresh)
	if ok || !errors.As(err, &aue) || aue.Reason != "proxy_cooling" {
		t.Fatalf("acquireSlotForAccount ok=%v err=%v, want proxy_cooling", ok, err)
	}

	// 代理恢复正常后同一个号重新可用，且必须回到绑定出口。
	proxyMu.Lock()
	proxyCache[0].FailCount = 0
	proxyMu.Unlock()
	fresh = accountByID(a.ID)
	got, ok, err := acquireSlotForAccount(fresh)
	if !ok || err != nil {
		t.Fatalf("after reset: ok=%v err=%v", ok, err)
	}
	if got.ID != p.ID {
		t.Fatalf("after reset got proxy #%d, want #%d", got.ID, p.ID)
	}
	releaseSlot(got.ID)
}

func TestExclusiveCookieCoolsWhenSlotFull(t *testing.T) {
	a := addProxyTestAccount(t, "full-owner", "sap-d", "psid-d")
	p := Proxy{ID: 990004, Name: "full", URL: "http://127.0.0.1:19004", Enabled: true}
	withTestProxyPool(t, []Proxy{p}, func(c *RuntimeConfig) { c.PerIPConcurrent = 1 })
	if err := accountBindProxy(a.ID, p.ID, true); err != nil {
		t.Fatalf("bind exclusive: %v", err)
	}
	t.Cleanup(func() { _ = accountUnbindProxy(a.ID) })

	s := getSlot(p.ID)
	s.mu.Lock()
	s.inflight = 1
	s.mu.Unlock()

	fresh := accountByID(a.ID)
	st := accountProxyStatus(*fresh, time.Now())
	if st.State != "full" || st.RetryAfter <= 0 {
		t.Fatalf("status = %+v, want full with retry", st)
	}
	_, ok, err := acquireSlotForAccount(fresh)
	var aue *AccountUnavailableError
	if ok || !errors.As(err, &aue) || aue.Reason != "proxy_full" {
		t.Fatalf("ok=%v err=%v, want proxy_full", ok, err)
	}
}

func TestExclusiveBindingConflictAndDeleteGuard(t *testing.T) {
	a := addProxyTestAccount(t, "conflict-a", "sap-e", "psid-e")
	b := addProxyTestAccount(t, "conflict-b", "sap-f", "psid-f")
	p := Proxy{ID: 990005, Name: "conflict", URL: "http://127.0.0.1:19005", Enabled: true}
	withTestProxyPool(t, []Proxy{p}, nil)
	if err := accountBindProxy(a.ID, p.ID, true); err != nil {
		t.Fatalf("bind exclusive: %v", err)
	}
	t.Cleanup(func() { _ = accountUnbindProxy(a.ID) })

	err := accountBindProxy(b.ID, p.ID, true)
	var conflict *proxyBindingConflictError
	if !errors.As(err, &conflict) || conflict.OwnerID != a.ID {
		t.Fatalf("second exclusive bind err = %v, want conflict with owner #%d", err, a.ID)
	}

	err = proxyDelete(p.ID)
	var guard *proxyExclusiveDeleteError
	if !errors.As(err, &guard) || guard.AccountID != a.ID {
		t.Fatalf("proxyDelete err = %v, want exclusive delete guard", err)
	}

	// 非独占绑定允许共享同一个代理（旧行为）。
	if err := accountBindProxy(b.ID, p.ID, false); err != nil {
		t.Fatalf("shared bind should succeed: %v", err)
	}
	t.Cleanup(func() { _ = accountUnbindProxy(b.ID) })
}

func TestCookieAcctViewShowsIPCooling(t *testing.T) {
	a := CookieAccount{
		ID: 990101, Label: "view", Status: "enabled",
		Cookie:  "SAPISID=abc; __Secure-1PSID=def",
		ProxyID: 990006, ProxyExclusive: true,
	}
	p := Proxy{
		ID: 990006, Name: "view-proxy", URL: "http://127.0.0.1:19006",
		Enabled: true, FailCount: proxyFailThreshold, LastUsed: time.Now().Unix(),
	}
	withTestProxyPool(t, []Proxy{p}, nil)
	bindingMu.Lock()
	exclusiveOwnerByProxy[p.ID] = a.ID
	exclusiveProxyByAccount[a.ID] = p.ID
	bindingMu.Unlock()

	v := cookieAcctView(a)
	if v["available"] != false {
		t.Fatalf("available = %v, want false", v["available"])
	}
	if v["proxy_state"] != "cooling" {
		t.Fatalf("proxy_state = %v, want cooling", v["proxy_state"])
	}
	if v["proxy_retry_after"].(int) <= 0 {
		t.Fatalf("proxy_retry_after = %v, want >0", v["proxy_retry_after"])
	}
	if !strings.Contains(v["unavailable_reason"].(string), "熔断") {
		t.Fatalf("unavailable_reason = %v", v["unavailable_reason"])
	}
	if v["proxy_exclusive"] != true {
		t.Fatalf("proxy_exclusive = %v", v["proxy_exclusive"])
	}
}
