package app

import (
	"testing"
	"time"
)

// 档位 + 额度带是选号的唯一判据，逐个钉住：
//   - 只有确实还有额度才是 quotaHasRoom；
//   - 拿不到数据一律未知（不奖不罚），绝不把"看不见"当成"没额度"；
//   - 额度带决定"差距够不够明显"的改道粒度。
func TestQuotaRankOf(t *testing.T) {
	pct := func(n int) *QuotaMetric { return &QuotaMetric{RemainPct: n} }
	cases := []struct {
		name     string
		snap     QuotaSnapshot
		ok       bool
		wantTier int
		wantBand int
	}{
		{"没缓存 → 未知", QuotaSnapshot{}, false, quotaUnknown, 0},
		{"查询失败 → 未知", QuotaSnapshot{OK: false}, true, quotaUnknown, 0},
		{"两档都没数据 → 未知", QuotaSnapshot{OK: true}, true, quotaUnknown, 0},
		{"两档都充足，取更紧的那档分带（min 60 → 带 3）",
			QuotaSnapshot{OK: true, FiveHour: pct(80), Weekly: pct(60)}, true, quotaHasRoom, 3},
		{"5 小时见底 → 不足", QuotaSnapshot{OK: true, FiveHour: pct(5), Weekly: pct(60)}, true, quotaExhausted, 0},
		{"周额度见底 → 不足", QuotaSnapshot{OK: true, FiveHour: pct(80), Weekly: pct(3)}, true, quotaExhausted, 0},
		{"刚好等于阈值算够", QuotaSnapshot{OK: true, FiveHour: pct(quotaPickMinRemainPct), Weekly: pct(50)},
			true, quotaHasRoom, 0},
		{"只给了 5 小时且够", QuotaSnapshot{OK: true, FiveHour: pct(50)}, true, quotaHasRoom, 2},
		{"实测场景：21% 的号落带 1", QuotaSnapshot{OK: true, FiveHour: pct(21), Weekly: pct(65)},
			true, quotaHasRoom, 1},
		{"实测场景：99% 的号落带 4", QuotaSnapshot{OK: true, FiveHour: pct(99), Weekly: pct(100)},
			true, quotaHasRoom, 4},
	}
	for _, c := range cases {
		tier, band := quotaRankOf(c.snap, c.ok)
		if tier != c.wantTier || band != c.wantBand {
			t.Errorf("%s：quotaRankOf = (tier %d, band %d)，期望 (%d, %d)",
				c.name, tier, band, c.wantTier, c.wantBand)
		}
	}
}

// 重排的核心不变量：
//  1. 跨带才改道 —— 差得明显（21% vs 99%）必须让 99% 插到前面，即使轮转指向 21%；
//  2. 同带保持轮转 —— 55% vs 50% 不许改道（否则会把请求量全压到一个号上）；
//  3. 档位优先于额度带 —— 未知（没有数据）排在"已知不足"之前，但排在"有额度"之后；
//  4. 同带内的相对顺序按原顺序（稳定排序）；
//  5. 开关关掉时一行都不动。
func TestOrderAccountsByQuota(t *testing.T) {
	withQuotaCache(t, map[int64]quotaCacheEntry{
		1: freshQuota(21, 65),  // 带 1
		2: freshQuota(99, 100), // 带 4
		3: freshQuota(50, 50),  // 带 2
		4: freshQuota(55, 55),  // 带 2（和 3 同带）
		5: freshQuota(3, 80),   // 不足 → 档位 2
		// 6 故意没有缓存 → 未知 → 档位 1
	})

	// 1) 实测场景：轮转顺序是 [1, 2]（1 更久没用），但 2 明显更充足 → 必须先选 2
	got := orderAccountsByQuota([]CookieAccount{{ID: 1}, {ID: 2}}, true)
	if ids := accountIDs(got); !sameIDs(ids, []int64{2, 1}) {
		t.Fatalf("21%% vs 99%% 应改道到 99%%，实际顺序 %v", ids)
	}

	// 2) 同带不改道：55% 与 50% 都属于带 2 → 保持传入顺序
	got = orderAccountsByQuota([]CookieAccount{{ID: 4}, {ID: 3}}, true)
	if ids := accountIDs(got); !sameIDs(ids, []int64{4, 3}) {
		t.Fatalf("同带（55%% 与 50%%）不该改道，实际顺序 %v", ids)
	}

	// 3) 档位优先：有额度(3) → 未知(6) → 不足(5)
	got = orderAccountsByQuota([]CookieAccount{{ID: 5}, {ID: 6}, {ID: 3}}, true)
	if ids := accountIDs(got); !sameIDs(ids, []int64{3, 6, 5}) {
		t.Fatalf("档位顺序应为 有额度→未知→不足，实际 %v", ids)
	}

	// 4) 同带内的相对顺序按原顺序
	got = orderAccountsByQuota([]CookieAccount{{ID: 3}, {ID: 4}}, true)
	if ids := accountIDs(got); !sameIDs(ids, []int64{3, 4}) {
		t.Fatalf("同带应保持传入顺序，实际 %v", ids)
	}

	// 5) 关掉开关：原样
	off := orderAccountsByQuota([]CookieAccount{{ID: 1}, {ID: 2}}, false)
	if accountIDs(off)[0] != 1 {
		t.Errorf("开关关闭时不该重排，实际 %v", accountIDs(off))
	}
}

// 窗口内的旧快照必须继续可用：10 分钟前的数据（90 秒窗口那一版会判成"未知"）现在仍要生效。
// 否则选号会频繁退回"未知 → 轮转"，实测表现就是"隔一条请求才按额度生效一次"。
// 这条同时是**策略下限**：谁把窗口改到 10 分钟以下，这个测试就会失败，先想清楚再改。
func TestQuotaCacheKeepsSnapshotInsideWindow(t *testing.T) {
	withQuotaCache(t, map[int64]quotaCacheEntry{
		1: {
			snap: QuotaSnapshot{OK: true, FiveHour: &QuotaMetric{RemainPct: 80}, Weekly: &QuotaMetric{RemainPct: 80}},
			at:   time.Now().Add(-10 * time.Minute),
		},
	})
	snap, ok := quotaCachePeek(1)
	if !ok || !snap.OK {
		t.Fatal("10 分钟前的快照应当仍在有效窗口内")
	}
	if tier, band := quotaRank(1); tier != quotaHasRoom || band != 4 {
		t.Errorf("10 分钟前的 80%% 快照 = (tier %d, band %d)，期望 (有额度, 4)", tier, band)
	}
}

// 超出窗口的快照必须当未知（并在选号时触发一次异步补刷）：不能拿很久以前的数据当事实。
func TestQuotaCacheStaleBecomesUnknown(t *testing.T) {
	withQuotaCache(t, map[int64]quotaCacheEntry{
		9: {
			snap: QuotaSnapshot{OK: true, FiveHour: &QuotaMetric{RemainPct: 90}, Weekly: &QuotaMetric{RemainPct: 90}},
			at:   time.Now().Add(-(quotaPickStaleTTL + time.Second)),
		},
	})
	if _, ok := quotaCachePeek(9); ok {
		t.Error("过期快照应视为未知")
	}
	if tier, band := quotaRank(9); tier != quotaUnknown || band != 0 {
		t.Errorf("过期快照 = (tier %d, band %d)，期望 (未知, 0)", tier, band)
	}
}

// 读缓存绝不出网：缓存为空时 peek 立刻返回 false（选号在请求热路径上，不能有网络往返）。
func TestQuotaCachePeekIsCacheOnly(t *testing.T) {
	withQuotaCache(t, map[int64]quotaCacheEntry{})
	if _, ok := quotaCachePeek(12345); ok {
		t.Error("空缓存应返回 false")
	}
}

func freshQuota(five, week int) quotaCacheEntry {
	return quotaCacheEntry{
		snap: QuotaSnapshot{
			OK:       true,
			FiveHour: &QuotaMetric{RemainPct: five},
			Weekly:   &QuotaMetric{RemainPct: week},
		},
		at: time.Now(),
	}
}

// withQuotaCache 预置额度缓存，测试结束复原，免得影响别的用例。
func withQuotaCache(t *testing.T, entries map[int64]quotaCacheEntry) {
	t.Helper()
	quotaMu.Lock()
	old := quotaCache
	quotaCache = entries
	quotaMu.Unlock()
	t.Cleanup(func() {
		quotaMu.Lock()
		quotaCache = old
		quotaMu.Unlock()
	})
}

func accountIDs(accounts []CookieAccount) []int64 {
	out := make([]int64, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, a.ID)
	}
	return out
}

func sameIDs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
