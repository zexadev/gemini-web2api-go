package app

import (
	"sort"
	"sync"
	"time"
)

// quota_aware_pick（默认关）：挑号时优先选**额度更充足**的号。
//
// 为什么需要：现有选号是
//
//	ORDER BY fail_count ASC, last_used_at ASC, id ASC
//
// **完全不看额度**。池子里虽然有号，但轮到已经耗尽的那个时，请求照样发出去 ——
// 上游于是静默降级成更弱的模型（#41）。strict_model 是在结果出来后拦截，
// 这里是从根上不把请求发给快空了的号。
//
// 四条约束（都是踩过的坑）：
//
//  1. **绝不阻塞请求**。选号在请求热路径上，这里只读额度缓存；缓存冷了就按老规矩
//     （最久未用）选，同时**异步**补一次。绝不能在这里同步拉额度 —— 那是几百毫秒
//     到几秒的上游往返，会把每一个请求都拖慢。
//  2. **不确定 ≠ 没额度**。额度未知的号排在"已知不足"之前。否则缓存一冷，功能就
//     退化成"永远不碰没缓存的号"，比不做还糟。
//  3. **差距要明显才改道**：剩余额度按 quotaPickBandPct 分带，只有**跨带**才让更充足的
//     号插到前面，同一带内维持原来的轮转。理由见 quotaPickBandPct 的注释。
//  4. **全池都不足时仍然选出"最不空"的那个**，而不是让请求失败。
const (
	// quotaPickMinRemainPct 是"这个号还能用"的最低剩余额度。留 10% 余量：额度是按
	// 请求扣的，贴着 0% 选号很容易正好把请求发给一个刚好耗尽的号。
	quotaPickMinRemainPct = 10

	// quotaPickBandPct 是"谁更充足"的比较粒度：剩余额度按这个宽度分带，
	// 只有跨带才改道（带高的优先），同一带内维持原来的轮转。
	//
	// 两个极端都不对：
	//   - "谁多选谁"会把请求量全压到一个号上，破坏原有的均匀轮换；
	//   - "只有低于阈值才改道"实测不够：21% 与 99% 会被判成同一档而不改道，
	//     于是 21% 那个号会一直被用到快见底 —— 而"贴着 0%"正是会触发静默降级的窗口。
	//
	// 折中：差得明显（跨一个带）才改道。20 个百分点意味着"从 99% 掉到 21% 这种级别的
	// 差距一定改道"，而"55% 与 50%"这种差别会保持轮转。
	quotaPickBandPct = 20

	// quotaPickStaleTTL 之后缓存就算过期，过期快照当**未知**处理，同时触发一次异步补刷。
	//
	// 为什么敢放到 15 分钟这么久：
	//   - 额度带宽度是 20 个百分点，而额度只会**有请求时**才缓慢下降 —— 十几分钟前的
	//     数字足够用来做"跨带比较"；
	//   - 窗口同时是**补刷的节拍**：某号的数据超过这个窗口才会被异步补一次，所以
	//     持续有请求时 ≈ 每号 15 分钟一次上游调用（2 个号 = 8 次/小时），
	//     没有请求时**一次都不发**。把窗口设小（原先 90 秒）会让选号频繁处于
	//     "未知 → 退回轮转"的状态，实测表现为"隔一条请求才生效一次"。
	//
	// 代价（明说）：排序依据最多可能旧 15 分钟。最坏情况是两个号都快见底时，
	// 依据过期数据选错一个 —— 而这时无论选谁都接近触发降级，且下一条请求就会纠正。
	quotaPickStaleTTL = 15 * time.Minute

	// quotaWarmInterval 是同一个号最多多久补一次额度缓存，防止请求洪峰打爆上游。
	quotaWarmInterval = 60 * time.Second
)

// 优先级的三个档位（越小越优先）。
const (
	quotaHasRoom   = 0 // 额度充足
	quotaUnknown   = 1 // 没有可用数据：不奖也不罚
	quotaExhausted = 2 // 已知不足
)

// quotaCachePeek 只读额度缓存（含新鲜度判断），**绝不触发网络请求**。
// 选号路径只能用这个，不能用会出网的 getQuota。
func quotaCachePeek(id int64) (QuotaSnapshot, bool) {
	quotaMu.Lock()
	entry, ok := quotaCache[id]
	quotaMu.Unlock()
	if !ok || time.Since(entry.at) > quotaPickStaleTTL {
		return QuotaSnapshot{}, false
	}
	return entry.snap, true
}

// quotaMinRemain 取两档里**更紧**的那一档作为剩余量：5 小时窗口或周额度任一快见底，
// 这个号都不该被优先选中。两档都没有数据时 has=false。
func quotaMinRemain(snap QuotaSnapshot) (int, bool) {
	best, has := 0, false
	for _, m := range []*QuotaMetric{snap.FiveHour, snap.Weekly} {
		if m == nil {
			continue
		}
		if !has || m.RemainPct < best {
			best, has = m.RemainPct, true
		}
	}
	return best, has
}

// quotaRankOf 给出 (档位, 额度带)：排序规则 = 档位升序 → 带降序 → 保持传入顺序（轮转）。
// 纯函数，便于单测。
func quotaRankOf(snap QuotaSnapshot, ok bool) (tier, band int) {
	if !ok || !snap.OK {
		return quotaUnknown, 0
	}
	remain, has := quotaMinRemain(snap)
	if !has {
		return quotaUnknown, 0
	}
	if remain < quotaPickMinRemainPct {
		return quotaExhausted, remain / quotaPickBandPct
	}
	return quotaHasRoom, remain / quotaPickBandPct
}

// quotaRank 读缓存 + 判档/分带。
func quotaRank(id int64) (tier, band int) {
	snap, ok := quotaCachePeek(id)
	return quotaRankOf(snap, ok)
}

// orderAccountsByQuota 把候选**稳定**重排：档位优先，其次额度带，同带内保持传入顺序
// （也就是原来那条 SQL 的最久未用优先）。开关关掉时原样返回。
func orderAccountsByQuota(accounts []CookieAccount, enabled bool) []CookieAccount {
	if !enabled || len(accounts) < 2 {
		return accounts
	}
	type ranked struct {
		account    CookieAccount
		tier, band int
	}
	rows := make([]ranked, len(accounts))
	for i, a := range accounts {
		tier, band := quotaRank(a.ID)
		rows[i] = ranked{account: a, tier: tier, band: band}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].tier != rows[j].tier {
			return rows[i].tier < rows[j].tier
		}
		return rows[i].band > rows[j].band
	})
	for i := range rows {
		accounts[i] = rows[i].account
	}
	return accounts
}

var (
	quotaWarmMu sync.Mutex
	quotaWarmAt = map[int64]time.Time{}
)

// prepareQuotaWarmth 在选号前扫一遍候选：缓存冷/过期的就异步补一次。
// 调用方不等它 —— 这次仍按老规矩选，下一次请求就有额度数据了。
func prepareQuotaWarmth(accounts []CookieAccount) {
	for _, a := range accounts {
		if _, ok := quotaCachePeek(a.ID); ok {
			continue
		}
		warmQuotaAsync(a.ID)
	}
}

// warmQuotaAsync 异步补一个号的额度缓存。每号每 quotaWarmInterval 最多一次；
// 并发重复调用由 getQuota 自己的 single-flight 锁合并成一次上游请求。
func warmQuotaAsync(id int64) {
	quotaWarmMu.Lock()
	if last, seen := quotaWarmAt[id]; seen && time.Since(last) < quotaWarmInterval {
		quotaWarmMu.Unlock()
		return
	}
	quotaWarmAt[id] = time.Now()
	quotaWarmMu.Unlock()

	go func() {
		// 已停用/已删掉的号别去问上游（额度查询本身允许查停用号，见 quotaAccountByID，
		// 但选号补数据只对"可能被选中"的号有意义）。
		if _, ok := quotaAccountByID(id); !ok {
			return
		}
		_ = getQuota(false, id)
	}()
}
