package app

// 配额探测：jSf9Qc 是 Gemini 网页端 /usage 页的数据源（batchexecute，空 payload）。
// 响应形状（2026-10-14 实测）：
//
//	[[ "wrb.fr","jSf9Qc","[2,[[47879,0.01043113,2,[[1792550388,71087000]]],
//	   [1895,0.21,1,[[1791963588,70986000]]]],false]", ...]]
//
// 每个内层条目 = 一个配额窗口：[窗口ID, 剩余比例(0~1), 档位, [[重置epoch秒, 纳秒]]]。
// 剩余比例的语义（remaining 而非 used）由配额耗尽现场佐证：降级发生时高级档报
// 0.0104（≈没剩多少），基础档 0.21；若语义是 used 则耗尽时不可能只有 1%。
// 窗口 ID（1895/47879）与具体模型的映射 Google 未给出，不要猜，按档位展示。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// QuotaWindow 是一个配额窗口的探测结果。
type QuotaWindow struct {
	WindowID  int     `json:"window_id"`
	Remaining float64 `json:"remaining"` // 0~1，剩余比例
	Tier      int     `json:"tier"`
	ResetAt   int64   `json:"reset_at"` // epoch 秒
}

// parseQuotaPayload 从 batchexecute 的原始响应里解析配额窗口。
// 响应是 chunked 拼接体：)]}‘ 头 + 逐段「字节长度行 + JSON 数组行」，必须逐行试解。
func parseQuotaPayload(raw []byte) ([]QuotaWindow, error) {
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ")]}'") {
			continue
		}
		if line[0] >= '0' && line[0] <= '9' { // 纯数字行是 chunk 字节长度，跳过
			continue
		}
		var lines [][]json.RawMessage
		if json.Unmarshal([]byte(line), &lines) != nil {
			continue
		}
		if wins, err := quotaFromLines(lines); err == nil {
			return wins, nil
		}
	}
	return nil, fmt.Errorf("响应里没有 jSf9Qc 条目")
}

func quotaFromLines(rpcs [][]json.RawMessage) ([]QuotaWindow, error) {
	for _, rpc := range rpcs {
		// 每个 RPC 元素本身就是头数组：["wrb.fr","jSf9Qc","<payload JSON 字符串>",...]
		if len(rpc) < 3 {
			continue
		}
		var rpcID, payloadStr string
		if json.Unmarshal(rpc[0], &rpcID) != nil || rpcID != "wrb.fr" {
			continue
		}
		if json.Unmarshal(rpc[1], &rpcID) != nil || rpcID != "jSf9Qc" {
			continue
		}
		if err := json.Unmarshal(rpc[2], &payloadStr); err != nil {
			return nil, err
		}
		var payload []json.RawMessage
		if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil || len(payload) < 2 {
			return nil, fmt.Errorf("jSf9Qc payload 形状不对")
		}
		var windows [][]json.RawMessage
		if err := json.Unmarshal(payload[1], &windows); err != nil {
			return nil, err
		}
		var out []QuotaWindow
		for _, w := range windows {
			if len(w) < 3 {
				continue
			}
			var id, tier int
			var frac float64
			if json.Unmarshal(w[0], &id) != nil || json.Unmarshal(w[1], &frac) != nil ||
				json.Unmarshal(w[2], &tier) != nil {
				continue
			}
			qw := QuotaWindow{WindowID: id, Remaining: frac, Tier: tier}
			// 重置时间: [[[sec, nanos]]]（可有可无）
			if len(w) > 3 {
				var resets [][]float64
				if json.Unmarshal(w[3], &resets) == nil && len(resets) > 0 && len(resets[0]) > 0 {
					qw.ResetAt = int64(resets[0][0])
				}
			}
			out = append(out, qw)
		}
		return out, nil
	}
	return nil, fmt.Errorf("该行不含 jSf9Qc")
}

// fetchQuotaWindows 拉一个账号的配额窗口。xsrf 必须来自同一会话（at token 绑 cookie）。
func fetchQuotaWindows(cookie, xsrf, proxyURL string) ([]QuotaWindow, error) {
	if strings.TrimSpace(xsrf) == "" {
		return nil, fmt.Errorf("缺 at token")
	}
	endpoint := fmt.Sprintf(
		"https://gemini.google.com/_/BardChatUi/data/batchexecute?rpcids=jSf9Qc&source-path=%%2Fusage&bl=%s&hl=zh-CN&rt=c",
		currentBL(proxyURL))
	body := fmt.Sprintf("f.req=%s&at=%s",
		`%5B%5B%5B%22jSf9Qc%22%2C%22%5B%5D%22%2Cnull%2C%22generic%22%5D%5D%5D`,
		strings.ReplaceAll(xsrf, ":", "%3A"))
	statusCode, raw, _, _, err := doGeminiRequest(endpoint, body, map[string]string{
		"Cookie":       cookie,
		"Content-Type": "application/x-www-form-urlencoded;charset=UTF-8",
		"Origin":       "https://gemini.google.com",
		"Referer":      "https://gemini.google.com/usage",
	}, proxyURL, nil)
	if err != nil {
		return nil, err
	}
	if statusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", statusCode)
	}
	return parseQuotaPayload(raw)
}

// describeQuota 把配额窗口翻译成人话（面板「检测」结果里直接展示）。
// 重置时间：今天的只显示时刻，跨天的带上日期。
func describeQuota(wins []QuotaWindow) string {
	if len(wins) == 0 {
		return ""
	}
	var parts []string
	today := time.Now().Local().Format("2006-01-02")
	for _, w := range wins {
		reset := "未知"
		if w.ResetAt > 0 {
			t := time.Unix(w.ResetAt, 0).Local()
			if t.Format("2006-01-02") == today {
				reset = t.Format("15:04")
			} else {
				reset = t.Format("01-02 15:04")
			}
		}
		parts = append(parts, fmt.Sprintf("档%d 剩%.0f%%（%s 重置）", w.Tier, w.Remaining*100, reset))
	}
	return strings.Join(parts, "；")
}

// probeAccountQuota 给面板/配额中枢用：占一个出口、取 XSRF、拉配额窗口。
// 出口选择与 checkAccountCookie 同口径（绑定出口优先，且只从绑定出口测才有意义）。
// 返回的 proxyName 用于排查「配额是否按出口变化」。
func probeAccountQuota(a CookieAccount) ([]QuotaWindow, string, string) {
	var picked Proxy
	var err error
	if a.ProxyID > 0 && proxyUsableByID(a.ProxyID) {
		var ok bool
		picked, ok = acquireBoundSlot(a.ProxyID)
		if !ok {
			return nil, "", "绑定的出口不可用或已到限流线，稍后再试"
		}
	} else {
		var ok bool
		picked, ok, err = acquireSlot(a.ProxyID)
		if !ok {
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			return nil, "", "拿不到出口：" + msg
		}
	}
	defer releaseSlot(picked.ID)
	proxyURL := picked.URL
	name := picked.Name
	if name == "" {
		name = "直连"
	}
	invalidateXSRF(a.Cookie)
	xsrfTok, err := getXSRF(a.Cookie, proxyURL)
	if err != nil {
		return nil, name, explainCookieFailure(err)
	}
	wins, qerr := fetchQuotaWindows(a.Cookie, xsrfTok, proxyURL)
	if qerr != nil {
		return nil, name, "配额拉取失败：" + qerr.Error()
	}
	return wins, name, ""
}

// handleV1Quota — GET /v1/quota：全部 enabled 账号的配额窗口，配额中枢聚合用。
// 并发探测（每个号 = 1 次 XSRF + 1 次 jSf9Qc，约 1-2 秒）。
func handleV1Quota(w http.ResponseWriter, r *http.Request) {
	accounts := accountList()
	type result struct {
		id     int64
		label  string
		wins   []QuotaWindow
		proxy  string
		detail string
	}
	results := make([]result, len(accounts))
	var wg sync.WaitGroup
	for i, a := range accounts {
		wg.Add(1)
		go func(i int, a CookieAccount) {
			defer wg.Done()
			wins, proxyName, detail := probeAccountQuota(a)
			results[i] = result{id: a.ID, label: a.Label, wins: wins, proxy: proxyName, detail: detail}
		}(i, a)
	}
	wg.Wait()
	out := make([]map[string]interface{}, 0, len(results))
	for _, r := range results {
		entry := map[string]interface{}{
			"id": r.id, "label": r.label, "proxy": r.proxy,
			"ok": len(r.wins) > 0, "quota": r.wins,
		}
		if r.detail != "" {
			entry["detail"] = r.detail
		}
		out = append(out, entry)
	}
	writeJSON(w, 200, map[string]interface{}{"object": "list", "data": out})
}

// handleV1Usage — GET /v1/usage?hours=N：token 消耗统计（API key 鉴权），
// 供配额中枢的「Token 消耗」卡聚合。形状与 /admin/api/stats 一致。
func handleV1Usage(w http.ResponseWriter, r *http.Request) {
	hours := 24
	if v := r.URL.Query().Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			hours = n
		}
	}
	if hours > 8760 {
		hours = 8760
	}
	writeJSON(w, 200, usageStats(hours))
}

// ---- 配额感知路由 ----
//
// Google 对无效/耗尽的配额不报错、只把生成静默降级成 Flash-Lite（客户端从状态码
// 完全看不出来）。所以路由层要自己感知：生成完成后核对服务端自报的模型名，发现
// 降级就给该账号打「高级配额受限」标记；之后需要登录态模型的挑号直接绕开它，
// 还没往客户端吐过字的请求换号重试。标记 45 分钟自动过期——配额窗口是滚动的，
// 过期后自然重试，若仍耗尽会再次打上（等效指数退避）。

var (
	quotaMu      sync.Mutex
	quotaBlocked = map[int64]time.Time{} // accountID → 受限至时刻
)

const quotaBlockDur = 45 * time.Minute

// expectedUpstreamPrefix 每个登录态模型应回报的显示名前缀（「4 Argon High」
// 「3.1 Pro 扩展」这类后缀变体都用前缀匹配兜住）。
func expectedUpstreamPrefix(mc ModelConfig) string {
	switch mc.HexID {
	case hexArgon4:
		return "4 Argon"
	case hexPro31:
		return "3.1 Pro"
	case hexFlash38:
		return "3.8 Flash"
	case hexFlash36:
		return "3.6 Flash"
	case hexFlashLite:
		return "3.5 Flash-Lite"
	}
	return ""
}

// downgraded 判定一次成功响应是否被静默降级：请求的模型需要登录态，但服务端
// 自报了别家的模型（最常见就是回落 3.5 Flash-Lite）。只对登录态模型生效——
// 基础模型的响应本来就不受配额门控。
func downgraded(mc ModelConfig, res *StreamResult) bool {
	if !modelNeedsLogin(mc) || res == nil || res.UpstreamModel == "" {
		return false
	}
	prefix := expectedUpstreamPrefix(mc)
	return prefix != "" && !strings.HasPrefix(res.UpstreamModel, prefix)
}

// quotaUnblock 手动解除受限标记（探测发现配额恢复时用）。
func quotaUnblock(id int64) {
	quotaMu.Lock()
	delete(quotaBlocked, id)
	quotaMu.Unlock()
}

// markQuotaBlocked 给账号打高级配额受限标记。
func markQuotaBlocked(id int64) {
	quotaMu.Lock()
	quotaBlocked[id] = time.Now().Add(quotaBlockDur)
	quotaMu.Unlock()
}

// quotaSkipSet 返回受限账号集合（供挑号绕开），顺带清掉过期标记。
// 只对需要登录态的请求返回非空——基础模型不挑号。
func quotaSkipSet(mc ModelConfig) (map[int64]bool, int) {
	if !modelNeedsLogin(mc) {
		return nil, 0
	}
	quotaMu.Lock()
	defer quotaMu.Unlock()
	now := time.Now()
	out := map[int64]bool{}
	n := 0
	for id, until := range quotaBlocked {
		if until.After(now) {
			out[id] = true
			n++
		} else {
			delete(quotaBlocked, id)
		}
	}
	return out, n
}

// quotaBlockedCount 当前受限（未过期）账号数。
func quotaBlockedCount() int {
	quotaMu.Lock()
	defer quotaMu.Unlock()
	now := time.Now()
	n := 0
	for id, until := range quotaBlocked {
		if until.After(now) {
			n++
		} else {
			delete(quotaBlocked, id)
		}
	}
	return n
}

// mergeSkip 合并两个跳过集合；b 为空时直接复用 a，避免热路径上的分配。
func mergeSkip(a, b map[int64]bool) map[int64]bool {
	if len(b) == 0 {
		return a
	}
	out := make(map[int64]bool, len(a)+len(b))
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

// enabledAccountCount 当前 enabled 账号数。
func enabledAccountCount() int {
	_, n := accountCount()
	return n
}
