package app

import (
	"encoding/json"
	"errors"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Proxy struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Enabled   bool   `json:"enabled"`
	Weight    int    `json:"weight"`
	FailCount int    `json:"fail_count"`
	LastUsed  int64  `json:"last_used"`
	LastError string `json:"last_error"`
	CreatedAt int64  `json:"created_at"`

	// 置信度调度状态（key-pool 算法，见 proxyRate）。内存态，不入库也不出 API：
	// 重启归零 = 全体出口回到「未探索」满权重，冷启动即可重新探索，符合该算法
	// 无状态的设计初衷；三个浮点数不值得加列迁移。
	Succ    float64   `json:"-"` // S：成功计数（读取时按半衰期衰减）
	SoftF   float64   `json:"-"` // F_soft：有过成功之后的失败（限流/抖动类）
	HardF   float64   `json:"-"` // F_hard：从未成功过的失败（被封/协议不兼容类）
	LastOut time.Time `json:"-"` // 最后一次成败时刻（算 Δt）
}

// 自适应衰减置信度调度（Adaptive Decaying Confidence Scheduler）。
// 算法来自 fastpkg/distance_matrix 的 docs/key-pool-algorithm.md，把「key」换成
// 「代理出口」落地：每个出口维护 S / F_soft / F_hard 三个指数衰减计数，选择率 R
// 由 Beta 后验 + 置信收缩 + 探测门控一条公式链算出——未探索的出口满权重、刚失败的
// 立刻掉到 ~0、冷却中的出口按 2% 地板权重周期性探测，旧失败随半衰期自行洗掉。
//
// 跟 account_first 粘绑的配合：绑定时按 R 加权**一次性**选中稳定出口，之后粘住
// 不动（高频流量自然集中在高 R 出口上）；出口劣化时 R 塌到 0，绑定号按
// account_first 换号让它歇着，门重新打开（g 随时间回升）后再服务，绑定不丢。
const (
	confHalfLifeSec = 300.0 // KeyRecoverySec：计数半衰期，λ = ln2/300
	confTau         = 2.0   // KeyConfidenceTau：置信质量爬升尺度
	confProbeSec    = 30.0  // KeyProbeSec：探测间隔基数 T₀
	confPhi         = 5.0   // 软硬失败回退指数 φ：T(F_b) = T₀·e^(F_b/φ)
	confSoftWeight  = 0.3   // w_f：软失败权重（F_b = F_hard + w_f·F_soft）
	confProbeFloor  = 0.02  // ρ_min：探测地板权重
	confStarveK     = 4.0   // 反饥饿系数 k：ε(N)=e^(−N/k)，仅单出口池生效
)

// confDecay 指数衰减：X' = X·e^(−λΔt)，λ = ln2/半衰期。
func confDecay(v, dtSec float64) float64 {
	if dtSec <= 0 {
		return v
	}
	return v * math.Exp(-math.Ln2/confHalfLifeSec*dtSec)
}

// proxyRate 出口在 now 时刻的选择率 R∈[0,1]。公式链逐条对齐 key-pool 文档：
//
//	ρ  = (S+2)/(S+F+3)            Beta 后验（a=2,b=1 → 未知出口乐观先验 ≈0.67）
//	η  = S + 0.3·F；α = 1−e^(−η/τ)  置信质量与爬升
//	R₀ = (1−α)·π + α·ρ             收缩；π=1 → 未探索出口满权重
//	T(F_b) = 30s·e^(F_b/φ)；g = 1−e^(−Δt/T(F_b))  探测门，刚失败时关着
//	g' = g + (1−g)·ε(N)            反饥饿：仅单出口池（N≤1）保底继续探测
//	普通出口（S>0 或 F_hard<0.5）：R = R₀ + 0.02·g'·(1−R₀)
//	纯探测出口（S=0 且 F_hard≥0.5）：R = 0.02·g'·(1−R₀)，门关着时为 0
//
// 文档里「F_hard 衰减到 1 以下回到普通」是离散计数语境；这里是连续衰减，F_hard=1
// 在 Δt>0 的瞬间就小于 1，判据会立刻失真。改成 0.5：一次硬失败至少撑过一个半衰期
// （5 分钟）才解除纯探测身份，语义等价于原文的「衰减到不到一次失败」。
func proxyRate(p *Proxy, now time.Time, poolSize int) float64 {
	dt := now.Sub(p.LastOut).Seconds()
	s := confDecay(p.Succ, dt)
	fs := confDecay(p.SoftF, dt)
	fh := confDecay(p.HardF, dt)
	f := s + fs + fh
	fb := fh + confSoftWeight*fs

	rho := (s + 2) / (f + 3)
	eta := s + confSoftWeight*f
	alpha := 1 - math.Exp(-eta/confTau)
	r0 := (1-alpha)*1.0 + alpha*rho

	tBackoff := confProbeSec * math.Exp(fb/confPhi)
	g := 1 - math.Exp(-dt/tBackoff)
	eps := 0.0
	if poolSize <= 1 {
		eps = math.Exp(-float64(poolSize) / confStarveK)
	}
	gp := g + (1-g)*eps

	if s > 0 || fh < 0.5 {
		return r0 + confProbeFloor*gp*(1-r0)
	}
	return confProbeFloor * gp * (1 - r0)
}

var (
	proxyMu    sync.RWMutex
	proxyCache []Proxy
)

// loadProxies 从 DB 刷新内存里的代理列表。
//
// 读一半失败时**保留上一次的池子**，绝不用半截结果覆盖。
// 旧写法有两个静默失效点：Scan 出错 continue（悄悄漏掉一个代理）、rows.Err()
// 完全不查（遍历中断当成正常读完）。两者都会让 proxyCache 变短甚至变空，而
// acquireSlot 用 len(proxyCache)==0 判断"没配代理池"，于是**池子一空就退回直连**
// —— 部署者的真实 IP 直接暴露给上游，日志上只看到偶发的直连请求。
//
// 这条路径每个请求都会走（recordProxyResult 结束就调），而 WAL 模式下并发
// UPDATE 期间 rows.Next() 完全可能返回 SQLITE_BUSY，所以"偶发"就是这么来的。
func loadProxies() {
	rows, err := getDB().Query(`SELECT id, name, url, enabled, weight, fail_count,
        COALESCE(last_used,0), COALESCE(last_error,''), created_at FROM proxies ORDER BY id`)
	if err != nil {
		logf("[proxy] 读取失败，保留上一次的代理池: %v", err)
		return
	}
	defer rows.Close()
	var list []Proxy
	for rows.Next() {
		var p Proxy
		var enabled int
		if err := rows.Scan(&p.ID, &p.Name, &p.URL, &enabled, &p.Weight, &p.FailCount,
			&p.LastUsed, &p.LastError, &p.CreatedAt); err != nil {
			logf("[proxy] 有行读不出来，保留上一次的代理池: %v", err)
			return
		}
		p.Enabled = enabled == 1
		list = append(list, p)
	}
	if err := rows.Err(); err != nil {
		logf("[proxy] 遍历中断，保留上一次的代理池: %v", err)
		return
	}
	// 置信度状态按 ID 平移到新列表上：这条路径每个请求后都会跑（recordProxyResult
	// 结束就调），不平移的话每次刷新都把 S/F 清零，刚失败过的坏出口立刻满权重复活。
	proxyMu.Lock()
	if len(proxyCache) > 0 {
		prev := make(map[int64]*Proxy, len(proxyCache))
		for i := range proxyCache {
			prev[proxyCache[i].ID] = &proxyCache[i]
		}
		for i := range list {
			if old, ok := prev[list[i].ID]; ok {
				list[i].Succ, list[i].SoftF, list[i].HardF, list[i].LastOut =
					old.Succ, old.SoftF, old.HardF, old.LastOut
			}
		}
	}
	proxyCache = list
	proxyMu.Unlock()
}

// 连续失败到这个次数就把代理熔断，等冷却期过了再放回池子。
const proxyFailThreshold = 5

// proxyUsable 判断这条代理现在能不能用。
//
// 熔断不是永久除名：被 Google 拦掉的出口实测 106-121 分钟就自动恢复，而旧写法
// 只认 fail_count<5，一旦超了就再也选不中，也就永远等不到一次成功把 fail_count
// 清零 —— 只能去面板手动重置。冷却期从 last_used（也就是最后一次尝试）算起，
// 放回去再失败一次就重新计时。
//
// cooldownMin<=0 表示关掉冷却，退回"熔断即永久除名"的旧行为。
func proxyUsable(p Proxy, now int64, cooldownMin int) bool {
	if !p.Enabled {
		return false
	}
	if p.FailCount < proxyFailThreshold {
		return true
	}
	if cooldownMin <= 0 {
		return false
	}
	return now-p.LastUsed >= int64(cooldownMin)*60
}

// kv 里记迁移/播种状态的键。
const (
	kvLegacyProxyDone = "legacy_static_proxy_migrated"
	kvSeededProxyID   = "seeded_proxy_id"
	kvSeededProxyURL  = "seeded_proxy_url"
)

// seedProxiesFromConfig 把启动参数和历史遗留的「静态代理」并进代理池。
//
// 以前代理有两个入口：代理池，和「设置」页那个单独的静态代理文本框（池空时才用）。
// 后者不是"简单版"而是**残废版** —— 它走的是 picked.ID=0 这条路，于是跟直连共用
// 同一个限流 slot、recordProxyResult 压根不会被调用，也就没有失败计数、没有熔断、
// 没有冷却、面板上看不到任何状态。池子里放一个，处处严格更好。
//
// 现在请求路径只认池子，这个函数负责把旧入口的值搬进来。
func seedProxiesFromConfig() {
	migrateLegacyStaticProxy()
	syncSeededProxy()
}

// migrateLegacyStaticProxy 一次性把 kv 里遗留的静态代理搬进池子。
//
// 两条保命规则，都是针对"用户的库已经在跑"这个前提：
//
//  1. **入池成功才标记完成**，失败就原样留着下次再试。旧版的
//     validateRuntimeConfig 对这个字段零校验，用户完全可能存的是 `1.2.3.4:8080`
//     这种缺 scheme 的值，而 proxyCreate 会拒收它 —— 先清值再入池的话，用户升级
//     后代理凭空消失，流量全转直连然后被上游拦。
//  2. 用**独立的迁移标记**，不去改写 runtime_config 那个 JSON。少动一次已有数据
//     就少一分把别的字段写坏的风险；顺带回滚到旧版时那条静态代理还在，行为不变。
func migrateLegacyStaticProxy() {
	if kvGet(kvLegacyProxyDone) == "1" {
		return
	}
	v := strings.TrimSpace(legacyStaticProxy())
	if v == "" {
		_ = kvSet(kvLegacyProxyDone, "1") // 本来就没有，标记掉免得每次启动都解析一遍
		return
	}
	if !poolHasProxyURL(v) {
		if _, err := proxyCreate("原静态代理", v, 1); err != nil {
			logf("[proxy] 「设置」页的静态代理迁入池子失败，原值保留、下次启动重试: %v", err)
			return
		}
		logf("[proxy] 「设置」页的静态代理已迁入代理池")
	}
	_ = kvSet(kvLegacyProxyDone, "1")
}

// legacyStaticProxy 只读地取 kv 里遗留的 runtime_config.proxy。
// RuntimeConfig 已经没有这个字段了，所以只能直接看原始 JSON。
func legacyStaticProxy() string {
	raw := kvGet(runtimeConfigKey)
	if raw == "" {
		return ""
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return ""
	}
	v, _ := m["proxy"].(string)
	return v
}

// syncSeededProxy 让池子里跟着 --proxy / config.json 走一条记录。
//
// 启动参数是**声明式**的：值变了就更新同一条，而不是再加一条。按 URL 去重的写法
// 挡不住这个 —— 用户把 compose 里的代理换掉，旧那条会留在池子里继续 enabled、
// 继续接流量，成了一条谁也不知道还在用的僵尸出口。
//
// 值没变时**完全不碰池子**：用户在面板上对这条记录的增删改停用，都以面板为准。
func syncSeededProxy() {
	url := strings.TrimSpace(cfg.Proxy)
	prev := kvGet(kvSeededProxyURL)
	if url == prev {
		return
	}
	dropSeededProxy(prev)
	if url == "" {
		_ = kvSet(kvSeededProxyURL, "")
		_ = kvSet(kvSeededProxyID, "")
		logf("[proxy] --proxy 已从启动参数移除，对应的池子记录一并撤下")
		return
	}
	if poolHasProxyURL(url) {
		// 用户自己已经在面板加过同一个出口，不重复建，只记下来
		_ = kvSet(kvSeededProxyURL, url)
		_ = kvSet(kvSeededProxyID, "")
		return
	}
	id, err := proxyCreate("启动参数", url, 1)
	if err != nil {
		logf("[proxy] --proxy 入池失败: %v", err)
		return // 不记 URL，下次启动还会再试
	}
	_ = kvSet(kvSeededProxyURL, url)
	_ = kvSet(kvSeededProxyID, strconv.FormatInt(id, 10))
	logf("[proxy] --proxy / config.json 的代理已加入代理池")
}

// dropSeededProxy 撤掉上一次由启动参数建的那条。
// 只在这条记录**还是我们建时那个 URL** 时才删 —— 用户在面板上把它改成别的出口了，
// 就说明他接管了这条记录，不该被启动参数的变更连坐删掉。
func dropSeededProxy(prevURL string) {
	idStr := kvGet(kvSeededProxyID)
	if idStr == "" || prevURL == "" {
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		return
	}
	proxyMu.RLock()
	var found *Proxy
	for i := range proxyCache {
		if proxyCache[i].ID == id {
			p := proxyCache[i]
			found = &p
			break
		}
	}
	proxyMu.RUnlock()
	if found == nil || found.URL != prevURL {
		return
	}
	if err := proxyDelete(id); err != nil {
		logf("[proxy] 撤下旧的启动参数代理失败: %v", err)
	}
}

// poolHasProxyURL 池子里有没有这个 URL。
func poolHasProxyURL(url string) bool {
	proxyMu.RLock()
	defer proxyMu.RUnlock()
	for _, p := range proxyCache {
		if p.URL == url {
			return true
		}
	}
	return false
}

// proxyPoolEmpty 池子里还有没有任何出口记录（不管可用不可用）。
func proxyPoolEmpty() bool {
	proxyMu.RLock()
	defer proxyMu.RUnlock()
	return len(proxyCache) == 0
}

// pickProxyWithCapacity 找一个可用（enabled + 没熔断或已过冷却）且限流没满的代理。
// 返回 (proxy, ok)。所有代理都不可用或都满时返回 ok=false。
//
// 跟旧的 pickProxy 区别：会问 trySlotAcquire 看 slot 是否有容量；
// 调用方拿到的 slot 必须配套调 slotRelease(proxy.ID)。
func pickProxyWithCapacity() (Proxy, bool) { return pickProxyPreferring(0) }

// acquireBoundSlot 只尝试 id 指定的那一个出口：可用、置信门开着且有限流容量就占住
// 返回，否则 (zero,false) —— **绝不退而求其次**。account_first 调度的地基：
// 绑定的出口到了限流线或置信门关着（刚失败过、探测窗未开），调用方的正确反应是
// 换下一个账号，而不是让这个号从别人的出口出去。绑定本身不动——门随时间重开
// （g = 1−e^(−Δt/T(F_b))），出口恢复后这个号自然回到它自己的出口上。
func acquireBoundSlot(proxyID int64) (Proxy, bool) {
	proxyMu.RLock()
	defer proxyMu.RUnlock()
	now := time.Now()
	for i := range proxyCache {
		p := &proxyCache[i]
		if p.ID != proxyID {
			continue
		}
		if !proxyUsable(*p, now.Unix(), rtCfg().ProxyCooldownMin) {
			return Proxy{}, false
		}
		// 置信门：R 低于探测地板 ρ_min = 门关着（纯探测态的出口恒低于地板）。
		// 此时这个号歇着而不是换出口——门随时间重开：F_hard 半衰期衰减过半后回到
		// 普通公式 R≈0.93，或某次探测成功攒下 S。绑定不动，号也不背锅。
		if proxyRate(p, now, len(proxyCache)) < confProbeFloor {
			return Proxy{}, false
		}
		if ok, _ := trySlotAcquire(proxyID); ok {
			return *p, true
		}
		return Proxy{}, false
	}
	return Proxy{}, false // 绑定的出口已不在池子里（被删了）
}

// pickProxyForNewBinding 给还没绑定出口的账号挑一个新出口并占上 slot——
// 这是「一号一出口」的唯一决策点，所以按置信度加权：稳定出口被绑定选中后，
// 这个号的全部流量都固定在它上面（account_first 粘绑把一次高权重选择放大成
// 持续的流量倾斜）。
//
// 三轮收敛：① 没被别的 enabled 账号占的、置信门开着的（首选）；② 门开着的（一号
// 一出口让位于可用性）；③ 全部可用的，含门关着的探测出口（聊胜于无）。
func pickProxyForNewBinding(taken map[int64]bool) (Proxy, bool) {
	cooldown := rtCfg().ProxyCooldownMin
	now := time.Now()
	proxyMu.RLock()
	var pool []Proxy
	for _, p := range proxyCache {
		if proxyUsable(p, now.Unix(), cooldown) {
			pool = append(pool, p)
		}
	}
	proxyMu.RUnlock()
	if len(pool) == 0 {
		return Proxy{}, false
	}
	good := make([]Proxy, 0, len(pool))
	for _, p := range pool {
		if proxyRate(&p, now, len(pool)) >= confProbeFloor { // 门开着的才值得绑
			good = append(good, p)
		}
	}
	for pass := 0; pass < 2; pass++ {
		cand := good
		if pass == 0 {
			cand = make([]Proxy, 0, len(good))
			for _, p := range good {
				if !taken[p.ID] {
					cand = append(cand, p)
				}
			}
		}
		if len(cand) == 0 {
			continue
		}
		for _, p := range weightedOrder(cand, now) {
			if ok, _ := trySlotAcquire(p.ID); ok {
				return p, true
			}
		}
	}
	// 门关着的也在最后兜底（权重序）：池子只剩坏出口时，探测比拒绝强
	for _, p := range weightedOrder(pool, now) {
		if ok, _ := trySlotAcquire(p.ID); ok {
			return p, true
		}
	}
	return Proxy{}, false
}

// pickProxyPreferring 优先挑 preferID 那个出口，它不可用或没容量时才轮询别的。
//
// 为什么要粘住：cookie 池和代理池各自独立轮转的话，同一个 Google 账号会在几十个
// 出口 IP 之间来回跳 —— 这在 Google 眼里正是账号共享的特征。粘不住时宁可换出口
// 也不排队等，可用性优先。
//
// account_first 调度（见 acquireExitForAccount）下，带账号的请求不再走这里 ——
// 绑定的出口满载时换的是账号；这里只剩匿名请求（preferID=0）和 fallback 路径在用。
func pickProxyPreferring(preferID int64) (Proxy, bool) {
	proxyMu.RLock()
	defer proxyMu.RUnlock()
	if len(proxyCache) == 0 {
		return Proxy{}, false
	}
	now := time.Now().Unix()
	cooldown := rtCfg().ProxyCooldownMin
	var pool []Proxy
	for _, p := range proxyCache {
		if proxyUsable(p, now, cooldown) {
			pool = append(pool, p)
		}
	}
	if len(pool) == 0 {
		return Proxy{}, false
	}
	if preferID > 0 {
		for _, p := range pool {
			if p.ID == preferID {
				if ok, _ := trySlotAcquire(p.ID); ok {
					return p, true
				}
				break // 绑的那个满了，往下走正常轮询
			}
		}
	}
	// 加权不放回抽样：W = weight·R·1000。稳定出口吃到大流量，刚失败的 R≈0 基本
	// 抽不到，冷却中的以 ~2% 地板权重被周期性探测。旧的纯轮询对好坏出口一视同仁，
	// 坏出口会持续分走四分之一的请求。
	for _, p := range weightedOrder(pool, time.Now()) {
		if ok, _ := trySlotAcquire(p.ID); ok {
			return p, true
		}
	}
	return Proxy{}, false
}

// weightedOrder 按 W = weight·R·1000 做不放回加权抽样，返回抽取顺序（稳定出口在前，
// 近零权重的最后才被试到）。权重全零时退回随机均匀序，保证不会卡死。
func weightedOrder(pool []Proxy, now time.Time) []Proxy {
	rest := make([]Proxy, len(pool))
	copy(rest, pool)
	out := make([]Proxy, 0, len(rest))
	for len(rest) > 0 {
		total := 0.0
		ws := make([]float64, len(rest))
		for i := range rest {
			ws[i] = float64(rest[i].Weight) * proxyRate(&rest[i], now, len(pool)) * 1000
			total += ws[i]
		}
		if total <= 0 {
			rand.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
			return append(out, rest...)
		}
		r := rand.Float64() * total
		idx := len(rest) - 1
		for i, w := range ws {
			r -= w
			if r <= 0 {
				idx = i
				break
			}
		}
		out = append(out, rest[idx])
		rest = append(rest[:idx], rest[idx+1:]...)
	}
	return out
}

// recordProxyResult 回写一次请求的结果，并同步更新内存里那一条。
//
// 只改内存里的那一条，不整表重读：这个函数每个请求都会调，重读一次就是一次全表
// SELECT，而它自己刚发起过 UPDATE —— 高并发下正是这对读写在 WAL 上撞出 SQLITE_BUSY，
// 也就是代理池被读空、请求退回直连的触发条件。
//
// 代价是内存里的 FailCount++ 和 DB 的 fail_count+1 各算各的，别的进程直接改库会
// 让两边漂移。单进程持有这个库，重启也会从 DB 重新加载，可以接受。
func recordProxyResult(id int64, success bool, errStr string) {
	if id == 0 {
		return
	}
	now := time.Now()
	nowUnix := now.Unix()
	if success {
		_, _ = getDB().Exec(`UPDATE proxies SET fail_count=0, last_used=?, last_error='' WHERE id=?`, nowUnix, id)
	} else {
		_, _ = getDB().Exec(`UPDATE proxies SET fail_count=fail_count+1, last_used=?, last_error=? WHERE id=?`,
			nowUnix, errStr, id)
	}
	proxyMu.Lock()
	for i := range proxyCache {
		if proxyCache[i].ID != id {
			continue
		}
		p := &proxyCache[i]
		p.LastUsed = nowUnix
		// 置信度记账：先按 Δt 衰减旧计数，再按 key-pool 的分类规则累加——
		// 有过成功（衰减后 S>0）→ 软失败（限流/抖动类）；从未成功 → 硬失败
		// （IP 被封 / 协议不兼容）。成功清零软硬失败？不：算法靠衰减自然洗掉
		// 历史失败，成功只加 S —— 让「偶尔抖一下的稳定出口」和「彻底坏死」的
		// 分数路径不同。
		dt := now.Sub(p.LastOut).Seconds()
		p.Succ = confDecay(p.Succ, dt)
		p.SoftF = confDecay(p.SoftF, dt)
		p.HardF = confDecay(p.HardF, dt)
		if success {
			p.Succ++
		} else if p.Succ > 0 {
			p.SoftF++
		} else {
			p.HardF++
		}
		p.LastOut = now
		if success {
			p.FailCount = 0
			p.LastError = ""
		} else {
			p.FailCount++
			p.LastError = errStr
		}
		break
	}
	proxyMu.Unlock()
}

// proxyConfidenceReset 面板「重置」时把置信度一并归零：让出口以「未探索」满权重
// 重新接受考验，而不是带着半衰期没洗完的失败分数继续被探测门压着。
func proxyConfidenceReset(id int64) {
	proxyMu.Lock()
	defer proxyMu.Unlock()
	for i := range proxyCache {
		if proxyCache[i].ID == id {
			proxyCache[i].Succ, proxyCache[i].SoftF, proxyCache[i].HardF = 0, 0, 0
			proxyCache[i].LastOut = time.Time{}
			return
		}
	}
}

// CRUD ───────────────────────────────────────────────────────────────────────

func proxyCreate(name, url string, weight int) (int64, error) {
	if name == "" || url == "" {
		return 0, errors.New("name and url required")
	}
	if err := validateProxyURL(url); err != nil {
		return 0, err
	}
	if weight <= 0 {
		weight = 1
	}
	id, err := insertID(`INSERT INTO proxies(name, url, enabled, weight, created_at)
        VALUES (?,?,?,?,?)`, name, url, 1, weight, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	loadProxies()
	return id, nil
}

// validateProxyURL 校验代理 URL 协议。
// 支持 http / https / socks5 / socks5h。
//
// scheme 按大小写不敏感比：URL 的 scheme 本来就不区分大小写，url.Parse 会统一转小写，
// 所以 HTTP:// 在 4.0.0 那条不做校验的静态代理路径上是能正常工作的。校验若大小写
// 敏感，升级时就会把这种值拦下来 —— 用户什么都没改，代理却不工作了。
func validateProxyURL(s string) error {
	low := strings.ToLower(s)
	for _, p := range []string{"http://", "https://", "socks5://", "socks5h://"} {
		if strings.HasPrefix(low, p) {
			return nil
		}
	}
	return errors.New("代理 URL 必须以 http:// / https:// / socks5:// / socks5h:// 开头")
}

func proxyUpdate(id int64, name, url string, enabled *bool, weight *int) error {
	q := `UPDATE proxies SET `
	args := []interface{}{}
	parts := []string{}
	if name != "" {
		parts = append(parts, "name=?")
		args = append(args, name)
	}
	if url != "" {
		parts = append(parts, "url=?")
		args = append(args, url)
	}
	if enabled != nil {
		v := 0
		if *enabled {
			v = 1
		}
		parts = append(parts, "enabled=?")
		args = append(args, v)
	}
	if weight != nil {
		parts = append(parts, "weight=?")
		args = append(args, *weight)
	}
	if len(parts) == 0 {
		return nil
	}
	q += joinComma(parts) + " WHERE id=?"
	args = append(args, id)
	_, err := getDB().Exec(q, args...)
	if err == nil {
		loadProxies()
	}
	return err
}

func proxyDelete(id int64) error {
	_, err := getDB().Exec(`DELETE FROM proxies WHERE id=?`, id)
	if err == nil {
		loadProxies()
	}
	return err
}

func proxyResetFailures(id int64) error {
	_, err := getDB().Exec(`UPDATE proxies SET fail_count=0, last_error='' WHERE id=?`, id)
	proxyConfidenceReset(id) // 面板「重置」= 置信度一并归零，以未探索满权重重新考验
	if err == nil {
		loadProxies()
	}
	return err
}

func listProxies() []Proxy {
	proxyMu.RLock()
	defer proxyMu.RUnlock()
	out := make([]Proxy, len(proxyCache))
	copy(out, proxyCache)
	return out
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// proxyNameByID 按 id 找代理名，找不到返回空串（0 = 还没绑 / 直连）。
func proxyNameByID(id int64) string {
	if id <= 0 {
		return ""
	}
	proxyMu.RLock()
	defer proxyMu.RUnlock()
	for _, p := range proxyCache {
		if p.ID == id {
			return p.Name
		}
	}
	return ""
}

// proxyUsableByID 这个出口现在还能不能用（存在 + enabled + 没熔断或已过冷却）。
// 判断账号绑定的出口是不是还有效，无效才该重新绑。
func proxyUsableByID(id int64) bool {
	if id <= 0 {
		return false
	}
	proxyMu.RLock()
	defer proxyMu.RUnlock()
	now := time.Now().Unix()
	cooldown := rtCfg().ProxyCooldownMin
	for _, p := range proxyCache {
		if p.ID == id {
			return proxyUsable(p, now, cooldown)
		}
	}
	return false
}

// proxyURLByID 按 id 取代理 URL，找不到返回空串（直连）。
func proxyURLByID(id int64) string {
	if id <= 0 {
		return ""
	}
	proxyMu.RLock()
	defer proxyMu.RUnlock()
	for _, p := range proxyCache {
		if p.ID == id {
			return p.URL
		}
	}
	return ""
}
