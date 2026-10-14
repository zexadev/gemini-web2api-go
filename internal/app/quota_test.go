package app

import (
	"strings"
	"testing"
	"time"
)

// 真实抓包的 jSf9Qc 响应（2026-10-14，配额耗尽现场）：高级档剩 1.04%、基础档剩 21%，
// 基础档重置时间与网页 UI 显示的「用量限额重置时间 15:40」完全吻合。
const quotaRawSample = ")]}'\n\n199\n" +
	`[["wrb.fr","jSf9Qc","[2,[[47879,0.01043113,2,[[1792550388,71087000]]],[1895,0.21,1,[[1791963588,70986000]]]],false]",null,null,null,"generic"],["di",468],["af.httprm",468,"-2498571107325625386",3]]` +
	"\n25\n[[\"e\",4,null,null,235]]\n"

func TestParseQuotaPayload(t *testing.T) {
	wins, err := parseQuotaPayload([]byte(quotaRawSample))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(wins) != 2 {
		t.Fatalf("应解析出 2 个配额窗口, got %d", len(wins))
	}
	prem, basic := wins[0], wins[1]
	if prem.WindowID != 47879 || prem.Tier != 2 {
		t.Errorf("高级档解析错误: %+v", prem)
	}
	if prem.Remaining < 0.0104 || prem.Remaining > 0.0105 {
		t.Errorf("高级档剩余应≈0.0104, got %v", prem.Remaining)
	}
	// 基础档重置 = 1791963588 → 北京时间当天 15:39:48（网页 UI 显示 15:40）
	if basic.WindowID != 1895 || basic.Remaining < 0.209 || basic.Remaining > 0.211 {
		t.Errorf("基础档解析错误: %+v", basic)
	}
	if got := time.Unix(basic.ResetAt, 0).Local().Format("15:04"); got != "15:39" {
		t.Errorf("基础档重置时间应≈15:39(显示 15:40), got %s", got)
	}
	if d := describeQuota(wins); d == "" || !strings.Contains(d, "档2") || !strings.Contains(d, "档1") {
		t.Errorf("describeQuota 输出异常: %q", d)
	}
}

// 降级判定矩阵：请求的登录态模型与自报名前缀不符 = 被静默降级；基础模型不判。
func TestDowngraded(t *testing.T) {
	cases := []struct {
		mc     ModelConfig
		report string
		want   bool
	}{
		{Models["gemini-4-argon"], "4 Argon", false},
		{Models["gemini-4-argon"], "4 Argon High", false},
		{Models["gemini-4-argon"], "3.5 Flash-Lite", true},
		{Models["gemini-4-argon"], "", false}, // 没自报的不判（响应被拒等场景）
		{Models["gemini-3.1-pro"], "3.1 Pro 扩展", false},
		{Models["gemini-3.1-pro"], "3.5 Flash-Lite", true},
		{Models["gemini-3.8-flash"], "3.8 Flash", false},
		{Models["gemini-3.8-flash"], "3.1 Pro", true},
		{Models["gemini-3.6-flash"], "3.5 Flash-Lite", false}, // 基础模型不受配额门控
	}
	for _, c := range cases {
		res := &StreamResult{UpstreamModel: c.report}
		if got := downgraded(c.mc, res); got != c.want {
			t.Errorf("downgraded(%s, %q) = %v, want %v", c.mc.HexID, c.report, got, c.want)
		}
		// nil res 不判
		if downgraded(c.mc, nil) {
			t.Errorf("nil res 不应判降级")
		}
	}
}

// 受限标记生命周期：打上 → 生效 → 45 分钟后自动过期。
func TestQuotaBlockLifecycle(t *testing.T) {
	id := int64(999999) // 不存在的账号，纯测标记
	markQuotaBlocked(id)
	if skip, _ := quotaSkipSet(Models["gemini-4-argon"]); !skip[id] {
		t.Error("打标后应在 skip 集合里")
	}
	if skip2, _ := quotaSkipSet(Models["gemini-3.6-flash"]); skip2[id] {
		t.Error("基础模型不应受配额门控")
	}
	if quotaBlockedCount() < 1 {
		t.Error("计数应 ≥1")
	}
	quotaUnblock(id)
	if skip3, _ := quotaSkipSet(Models["gemini-4-argon"]); skip3[id] {
		t.Error("解除后不应再在 skip 集合里")
	}
}
