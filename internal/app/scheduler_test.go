package app

import (
	"math"
	"testing"
	"time"
)

// account_first 调度策略的钉子测试。
//
// 调度模型：账号 ↔ 出口粘绑，绑定的出口到限流线时**换账号不换出口** ——
// Google 眼里永远「一号一个固定 IP」，绝不出现同一个号从多个 IP 冒出来。
// 旧行为（可用性优先）保留为 account_first=false，这里把两种策略都钉死。

// setRt 临时改运行时配置，测试结束恢复。
func setRt(t *testing.T, f func(*RuntimeConfig)) {
	t.Helper()
	rtMu.Lock()
	saved := rtVal
	f(&rtVal)
	rtMu.Unlock()
	t.Cleanup(func() {
		rtMu.Lock()
		rtVal = saved
		rtMu.Unlock()
	})
}

// addTestProxy 建一个测试出口，测试结束删掉（proxyCreate 会同步刷内存缓存）。
func addTestProxy(t *testing.T, name string) int64 {
	t.Helper()
	id, err := proxyCreate(name, "http://192.0.2."+name[len(name)-1:]+":8080", 1)
	if err != nil {
		t.Fatalf("建测试代理失败: %v", err)
	}
	t.Cleanup(func() { _ = proxyDelete(id) })
	return id
}

// addTestAccount 建一个测试账号并绑定出口，测试结束删掉。
func addTestAccount(t *testing.T, label string, proxyID int64) *CookieAccount {
	t.Helper()
	id, err := accountAdd(label, "SAPISID=dummy; SID=x", "")
	if err != nil {
		t.Fatalf("插测试账号失败: %v", err)
	}
	bindAccountProxy(id, proxyID)
	t.Cleanup(func() { _ = accountDelete(id) })
	a := accountByID(id)
	if a == nil {
		t.Fatal("账号插入后查不到")
	}
	return a
}

// 绑定的出口满载时的两种策略：account_first=true 拒绝（调用方换号），false 漂移（旧行为）。
func TestAcquireExitForAccountBoundFull(t *testing.T) {
	idA := addTestProxy(t, "tA-1")
	idB := addTestProxy(t, "tB-2")
	acctA := addTestAccount(t, "acct-a", idA)
	acctB := addTestAccount(t, "acct-b", idB)

	// 并发上限=1：占住一个出口的唯一 slot 即到线
	setRt(t, func(c *RuntimeConfig) { c.PerIPConcurrent = 1 })
	if ok, _ := trySlotAcquire(idA); !ok {
		t.Fatal("占住出口 A 失败")
	}
	defer slotRelease(idA)

	// account_first=true：绑定的出口满了 → 拒绝，绝不漂到别的出口
	p, ok, needsBind, err := acquireExitForAccount(acctA, true)
	if ok || err != nil {
		t.Fatalf("account_first 下绑定出口满载应返回 ok=false, got ok=%v p=%+v err=%v", ok, p, err)
	}
	if needsBind {
		t.Error("拒绝时不应要求写绑定")
	}

	// account_first=true：B 没满 → 正常拿到 B 自己的绑定出口
	p, ok, _, err = acquireExitForAccount(acctB, true)
	if !ok || err != nil || p.ID != idB {
		t.Fatalf("account_first 下未满载的绑定出口应可用, got ok=%v p=%+v err=%v", ok, p, err)
	}
	slotRelease(p.ID)

	// account_first=false（旧行为）：A 满 → 漂到 B，可用性优先
	p, ok, needsBind, err = acquireExitForAccount(acctA, false)
	if !ok || err != nil || p.ID != idB {
		t.Fatalf("旧行为下绑定出口满载应漂移到 B, got ok=%v p=%+v err=%v", ok, p, err)
	}
	slotRelease(p.ID)
	// 漂移绝不改写绑定：好号的粘性不能被满载撞掉
	if got := accountByID(acctA.ID).ProxyID; got != idA {
		t.Errorf("漂移不应改写账号的绑定, proxy_id=%d want %d", got, idA)
	}
	_ = needsBind
}

// 没绑定过的号分新出口：优先挑没被别的 enabled 账号占的（一号一出口），
// 全满才退而求其次。
func TestPickProxyForNewBinding(t *testing.T) {
	idA := addTestProxy(t, "tA-3")
	idB := addTestProxy(t, "tB-4")
	addTestAccount(t, "acct-bound", idA) // 占住 A

	// A 已被占：新号应分到 B
	p, ok := pickProxyForNewBinding(boundProxyIDs())
	if !ok || p.ID != idB {
		t.Fatalf("新号应分到未被占用的 B, got ok=%v p=%+v", ok, p)
	}
	slotRelease(p.ID)

	// B 也满了（并发=1 占住）：第一轮全跳过/全满，第二轮退而求其次拿 A
	setRt(t, func(c *RuntimeConfig) { c.PerIPConcurrent = 1 })
	if ok, _ := trySlotAcquire(idB); !ok {
		t.Fatal("占住出口 B 失败")
	}
	defer slotRelease(idB)
	p, ok = pickProxyForNewBinding(boundProxyIDs())
	if !ok || p.ID != idA {
		t.Fatalf("全满时应退而求其次拿到 A, got ok=%v p=%+v", ok, p)
	}
	slotRelease(p.ID)
}

// 绑定的出口被删出池子后，旧绑定永远无法满足，必须允许重挑（不算漂移）。
func TestAcquireExitForAccountRebindAfterDelete(t *testing.T) {
	idA := addTestProxy(t, "tA-5")
	idB := addTestProxy(t, "tB-6")
	acct := addTestAccount(t, "acct-rebind", idA)
	if err := proxyDelete(idA); err != nil {
		t.Fatalf("删出口失败: %v", err)
	}
	loadProxies()

	p, ok, needsBind, err := acquireExitForAccount(acct, true)
	if !ok || err != nil || p.ID != idB {
		t.Fatalf("绑定出口已删应重挑到 B, got ok=%v p=%+v err=%v", ok, p, err)
	}
	if !needsBind {
		t.Error("重挑后应要求写新绑定")
	}
	slotRelease(p.ID)
}

// ---- key-pool 置信度调度（自适应衰减置信度调度器）----

// 公式链的关键行为钉死：未探索满权重、刚失败立即塌 0、冷却后以探测地板权重回归、
// 稳定出口接近满权重、F_hard 衰减过半自动解除「纯探测」身份。
func TestProxyRate(t *testing.T) {
	now := time.Now()
	rate := func(s, soft, hard float64, lastOut time.Time) float64 {
		p := Proxy{Succ: s, SoftF: soft, HardF: hard, LastOut: lastOut}
		return proxyRate(&p, now, 8)
	}
	if r := rate(0, 0, 0, time.Time{}); r < 0.999 {
		t.Errorf("未探索出口应满权重 R≈1, got %v", r)
	}
	if r := rate(0, 0, 1, now); r > 0.001 {
		t.Errorf("刚硬失败的纯探测出口门应关着 R≈0, got %v", r)
	}
	if r := rate(0, 0, 1, now.Add(-2*confProbeSec)); r <= 0 || r > confProbeFloor {
		t.Errorf("冷却中的出口应落在 (0, ρ_min] 探测区间, got %v", r)
	}
	// Beta 后验的诚实值：10 成 1 败 → ρ=6/7≈0.857，置信高 → R 收缩到 ρ 附近
	if r := rate(10, 1, 0, now); r < 0.8 || r > 0.95 {
		t.Errorf("稳定出口应收缩到 Beta 后验附近 (0.8,0.95), got %v", r)
	}
	// F_hard 衰减过半（一个半衰期 + 余量）后解除纯探测身份，回到普通出口公式
	p := Proxy{Succ: 0, SoftF: 0, HardF: 1, LastOut: now.Add(-(confHalfLifeSec + 60) * time.Second)}
	if r := proxyRate(&p, now, 8); r <= confProbeFloor {
		t.Errorf("F_hard 衰减过半后应回到普通公式（权重明显高于探测地板）, got %v", r)
	}
}

// 记账分类：从未成功 → 硬失败；有成功之后 → 软失败；成功累计 S。
func TestRecordProxyResultClassifies(t *testing.T) {
	id := addTestProxy(t, "tC-7")
	recordProxyResult(id, false, "boom")
	recordProxyResult(id, true, "")
	recordProxyResult(id, false, "throttled")
	proxyMu.RLock()
	var p *Proxy
	for i := range proxyCache {
		if proxyCache[i].ID == id {
			p = &proxyCache[i]
		}
	}
	proxyMu.RUnlock()
	if p == nil {
		t.Fatal("代理不在缓存里")
	}
	// 两次记账之间有毫秒级时间差，衰减会让计数比整数小一丝，用容差比较
	if math.Abs(p.HardF-1) > 0.01 || math.Abs(p.Succ-1) > 0.01 || math.Abs(p.SoftF-1) > 0.01 {
		t.Errorf("分类错误: S=%v F_soft=%v F_hard=%v, want 1/1/1", p.Succ, p.SoftF, p.HardF)
	}
}

// 加权抽样必须把刚失败的出口排到最后（好出口权重 1000+，坏出口 ≈0）。
func TestWeightedOrderSkipsBroken(t *testing.T) {
	good := addTestProxy(t, "tG-8")
	bad := addTestProxy(t, "tB-9")
	recordProxyResult(bad, false, "malformed h2") // 从未成功的硬失败，刚发生
	now := time.Now()
	proxyMu.RLock()
	var pg, pb Proxy
	for i := range proxyCache {
		if proxyCache[i].ID == good {
			pg = proxyCache[i]
		}
		if proxyCache[i].ID == bad {
			pb = proxyCache[i]
		}
	}
	proxyMu.RUnlock()
	order := weightedOrder([]Proxy{pg, pb}, now)
	if order[0].ID != good {
		t.Errorf("好出口应排第一, got %+v", order[0].Name)
	}
	// 选择率：坏的接近 0，好的接近 1
	if r := proxyRate(&pb, now, 2); r > 0.001 {
		t.Errorf("刚失败出口 R 应≈0, got %v", r)
	}
	if r := proxyRate(&pg, now, 2); r < 0.999 {
		t.Errorf("未探索出口 R 应≈1, got %v", r)
	}
}

// 绑定出口的置信门：刚失败（门关着）→ 拿不到 slot（调用方换号、绑定不动）；
// 门重开后恢复。这就是「出口劣化让号歇着、绑定不丢」的机制。
func TestAcquireBoundSlotConfidenceGate(t *testing.T) {
	id := addTestProxy(t, "tD-10")
	acct := addTestAccount(t, "acct-gate", id)
	recordProxyResult(id, false, "connection reset")
	if _, ok := acquireBoundSlot(id); ok {
		t.Error("置信门关着时不应拿到绑定出口的 slot")
	}
	recordProxyResult(id, true, "")
	p, ok := acquireBoundSlot(id)
	if !ok || p.ID != id {
		t.Errorf("门重开后应恢复, got ok=%v p=%+v", ok, p)
	}
	slotRelease(p.ID)
	_ = acct
}
