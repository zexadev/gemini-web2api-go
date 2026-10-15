package app

// 网页端「用量额度」（Usage limits）——GET /admin/api/quota。
//
// rpc id 与响应结构来自对 gemini.google.com/usage 的逆向，并已用真实账号核对：
//   页面显示 5 小时窗口 63% / 每周 13%  ⇄  本接口返回同样的 63% / 13% 与同样的重置时间。
// 参考实现（MIT）：https://github.com/Nagi-ovo/voyager —— public/usage-observer.js
// 与 src/pages/content/usageStatus/usageParsing.ts（extractUsagePayload / parseUsageRpcResponse）。
//
// 协议：
//   POST https://gemini.google.com/_/BardChatUi/data/batchexecute
//        ?rpcids=jSf9Qc&bl=<bl>&hl=en&_reqid=<n>&rt=c&source-path=/usage
//   f.req=[[["jSf9Qc","[]",null,"generic"]]]  &at=<XSRF>
// 响应是标准 batchexecute 分帧，载荷里有一个数组，元素形如
//   [limit, fraction, period, [[resetEpoch, nanos]]]
// 其中 fraction 是「已用比例」(0..1)、period 1 = 5 小时滚动窗口、2 = 每周额度。
//
// 额度是**账号级**的（匿名没有），所以池里必须有可用 cookie。
//
// 额度是慢变量，但每次查询都要真实出网一次（占一个 cookie + 一条出口），
// 所以结果缓存 20 秒；**失败不写缓存**，下一次请求立刻重试。

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// QuotaMetric 是一档额度。
type QuotaMetric struct {
	UsedPercent int   `json:"used_percent"` // 已用百分比（0-150）
	RemainPct   int   `json:"remain_percent"`
	ResetAt     int64 `json:"reset_at"`     // unix 秒
	ResetInSec  int64 `json:"reset_in_sec"` // 距重置还有多少秒
}

// QuotaSnapshot 是 /admin/api/quota 的返回体。
type QuotaSnapshot struct {
	OK        bool         `json:"ok"`
	AccountID int64        `json:"account_id,omitempty"`
	Account   string       `json:"account,omitempty"`
	Proxy     string       `json:"proxy,omitempty"`
	Direct    bool         `json:"direct"`
	FiveHour  *QuotaMetric `json:"five_hour,omitempty"`
	Weekly    *QuotaMetric `json:"weekly,omitempty"`
	FetchedAt int64        `json:"fetched_at"`
	Cached    bool         `json:"cached,omitempty"`
	Error     string       `json:"error,omitempty"`
}

// quotaCacheEntry 是一次成功的额度快照，按账号缓存。
type quotaCacheEntry struct {
	snap QuotaSnapshot
	at   time.Time
}

var (
	// quotaMu 只保护下面两张表本身，**不覆盖网络请求**——
	// 面板会并发问好几个账号的额度，若整段持锁，N 个账号就要串行 N 次上游 RTT。
	quotaMu    sync.Mutex
	quotaCache = map[int64]quotaCacheEntry{}
	quotaLocks = map[int64]*sync.Mutex{}
)

// quotaCacheTTL 让插件即便每分钟轮询也不会把上游打爆。网页端自己显示「Updated just now」，
// 但额度是慢变量，20 秒足够新。
const quotaCacheTTL = 20 * time.Second

// quotaLock 取某个账号（0 = 自动选号）的单飞锁：同一账号并发请求只打一次上游。
func quotaLock(id int64) *sync.Mutex {
	quotaMu.Lock()
	defer quotaMu.Unlock()
	l, ok := quotaLocks[id]
	if !ok {
		l = new(sync.Mutex)
		quotaLocks[id] = l
	}
	return l
}

// quotaAccount 选出最该用的账号，**不改写 last_used_at**（额度查询不该推进轮转）。
func quotaAccount() (*CookieAccount, bool) {
	accts := accountList()
	cands := make([]CookieAccount, 0, len(accts))
	for _, a := range accts {
		if a.Status == "enabled" {
			cands = append(cands, a)
		}
	}
	if len(cands) == 0 {
		return nil, false
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].FailCount != cands[j].FailCount {
			return cands[i].FailCount < cands[j].FailCount
		}
		if cands[i].LastUsedAt != cands[j].LastUsedAt {
			return cands[i].LastUsedAt < cands[j].LastUsedAt
		}
		return cands[i].ID < cands[j].ID
	})
	return &cands[0], true
}

// quotaAccountByID 按 id 取账号，**不限启用状态**：
// 面板要看的恰恰是「被停用/正在冷却的那个号还剩多少额度」，所以这里不按 status 过滤。
// 但必须有 cookie —— 没有登录态就没有账号级额度可查。
func quotaAccountByID(id int64) (*CookieAccount, bool) {
	a := accountByID(id)
	if a == nil || strings.TrimSpace(a.Cookie) == "" {
		return nil, false
	}
	return a, true
}

// quotaProxy 挑出口：优先账号绑定的那个，否则代理池里第一个健康的，池空则直连。
// 与 handleAdminTest 同款（旁路查询，不占限流 slot）。
func quotaProxy(preferID int64) (proxyURL, proxyName string, direct bool) {
	for _, p := range listProxies() {
		if !p.Enabled || p.FailCount >= proxyFailThreshold {
			continue
		}
		if preferID != 0 && p.ID != preferID {
			continue
		}
		return p.URL, p.Name, false
	}
	if preferID != 0 {
		for _, p := range listProxies() {
			if p.Enabled && p.FailCount < proxyFailThreshold {
				return p.URL, p.Name, false
			}
		}
	}
	return "", "直连", true
}

// fetchQuotaRPC 真打一次上游拿额度。
//
// preferID != 0 时查指定账号（管理面板多号池逐号查额度时用），否则沿用自动选号。
func fetchQuotaRPC(preferID int64) QuotaSnapshot {
	snap := QuotaSnapshot{FetchedAt: time.Now().Unix()}

	var acct *CookieAccount
	var ok bool
	if preferID != 0 {
		acct, ok = quotaAccountByID(preferID)
		if !ok {
			// 失败也回填 AccountID：调用方靠它确认这份结果是不是自己问的那个号，
			// 拿不到匹配值时就不该把数字挂到这个账号名下。
			snap.AccountID = preferID
			snap.Error = fmt.Sprintf("账号 #%d 不存在或没有 cookie", preferID)
			return snap
		}
	} else {
		acct, ok = quotaAccount()
		if !ok {
			snap.Error = "cookie 池里没有启用的账号：额度是账号级的，先挂 Google cookie（面板 Cookie 池）"
			return snap
		}
	}
	snap.AccountID, snap.Account = acct.ID, acct.Label
	proxyURL, proxyName, direct := quotaProxy(acct.ProxyID)
	snap.Proxy, snap.Direct = proxyName, direct

	xsrf, err := getXSRF(acct.Cookie, proxyURL)
	if err != nil || xsrf == "" {
		snap.Error = "取 XSRF token 失败（cookie 可能已失效，面板点「检测」确认）: " + errText(err)
		return snap
	}

	// f.req 逐字对照 Voyager 抓包：args 是空数组的字符串 "[]"，不是 ""。
	form := url.Values{}
	form.Set("f.req", `[[["jSf9Qc","[]",null,"generic"]]]`)
	form.Set("at", xsrf)

	reqid := time.Now().UnixNano() % 1000000
	endpoint := fmt.Sprintf(
		"https://gemini.google.com/_/BardChatUi/data/batchexecute?rpcids=jSf9Qc&bl=%s&hl=en&_reqid=%d&rt=c&source-path=%%2Fusage",
		currentBL(proxyURL), reqid)

	headers := buildGeminiHeaders(acct.Cookie, extractSAPISID(acct.Cookie), "")
	delete(headers, "x-goog-ext-525001261-jspb") // batchexecute 不带模型 header

	status, _, body, err := uploadPost(endpoint, headers, []byte(form.Encode()), proxyURL)
	if err != nil {
		snap.Error = "网络错误: " + err.Error()
		return snap
	}
	if status != 200 {
		snap.Error = fmt.Sprintf("上游 HTTP %d（登录态可能已过期）", status)
		return snap
	}

	five, weekly, perr := parseQuotaBody(string(body))
	if perr != nil {
		snap.Error = perr.Error()
		return snap
	}
	now := time.Now().Unix()
	hydrate := func(m *QuotaMetric) *QuotaMetric {
		if m == nil {
			return nil
		}
		if m.ResetAt > now {
			m.ResetInSec = m.ResetAt - now
		}
		m.RemainPct = 100 - m.UsedPercent
		if m.RemainPct < 0 {
			m.RemainPct = 0
		}
		return m
	}
	snap.FiveHour, snap.Weekly = hydrate(five), hydrate(weekly)
	if snap.FiveHour == nil && snap.Weekly == nil {
		snap.Error = "响应里没有可识别的额度档位（上游可能改版）"
		return snap
	}
	snap.OK = true
	return snap
}

// getQuota 带缓存地取额度。accountID = 0 表示自动选号。
func getQuota(force bool, accountID int64) QuotaSnapshot {
	lock := quotaLock(accountID)
	lock.Lock()
	defer lock.Unlock()

	quotaMu.Lock()
	entry, cached := quotaCache[accountID]
	quotaMu.Unlock()
	if cached && !force && time.Since(entry.at) < quotaCacheTTL {
		c := entry.snap
		c.Cached = true
		return c
	}

	snap := fetchQuotaRPC(accountID)
	if !snap.OK { // 失败不写缓存，下次立刻重试
		return snap
	}
	quotaMu.Lock()
	quotaCache[accountID] = quotaCacheEntry{snap: snap, at: time.Now()}
	quotaMu.Unlock()
	return snap
}

// handleAdminQuota — GET /admin/api/quota[?account_id=N][?refresh=1]
//
//	{"ok":true,"account":"my-account","account_id":2,"five_hour":{"used_percent":63,...},"weekly":{...}}
//
// account_id 用于按账号查询：多号池逐号看「这个号还剩多少额度」时用得到
// （被停用/冷却中的号也能查）。不传时按自动选号。
func handleAdminQuota(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		accountID, err := parseAccountID(q.Get("account_id"))
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad account_id"})
			return
		}
		writeJSON(w, 200, getQuota(q.Get("refresh") == "1", accountID))
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

// parseAccountID 解析 ?account_id=N：空串表示自动选号（返回 0），非法值返回错误。
func parseAccountID(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("bad account_id: %q", raw)
	}
	return id, nil
}

// parseQuotaBody 从 batchexecute 原始响应里抽出 5 小时 / 每周两档额度。
//
// 沿用 Voyager 的判据（extractUsagePayload）：不假设固定下标，而是遍历整个载荷，
// 找出「元素形如 [limit, fraction, period, [[epoch, nanos]]]」的那一组——
// 上游将来加新的额度桶也不会整段解析失败。
func parseQuotaBody(raw string) (five, weekly *QuotaMetric, err error) {
	for _, payload := range quotaPayloads(raw) {
		arr, ok := payload.([]interface{})
		if !ok {
			continue
		}
		var metrics []*QuotaMetric
		var periods []float64
		for _, bucket := range arr {
			items, ok := bucket.([]interface{})
			if !ok {
				continue
			}
			var got []*QuotaMetric
			var gotP []float64
			for _, it := range items {
				m, p, ok := readQuotaMetric(it)
				if !ok {
					continue
				}
				got = append(got, m)
				gotP = append(gotP, p)
			}
			if len(got) > 0 {
				metrics, periods = got, gotP
				break
			}
		}
		if len(metrics) == 0 {
			continue
		}
		for i, p := range periods {
			switch int(p) {
			case 1: // 5 小时滚动窗口
				if five == nil {
					five = metrics[i]
				}
			case 2: // 每周
				if weekly == nil {
					weekly = metrics[i]
				}
			}
		}
		if five != nil || weekly != nil {
			return five, weekly, nil
		}
	}
	return nil, nil, fmt.Errorf("额度字段解析失败：响应结构可能与预期不符（上游改版）")
}

// quotaPayloads 取出 batchexecute 分帧里每个 wrb.fr 帧的载荷（inner JSON 解出来的对象）。
func quotaPayloads(raw string) []interface{} {
	var out []interface{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[[") {
			continue
		}
		var frame []interface{}
		if json.Unmarshal([]byte(line), &frame) != nil {
			continue
		}
		for _, part := range frame {
			parts, ok := part.([]interface{})
			if !ok || len(parts) < 3 || parts[0] != "wrb.fr" {
				continue
			}
			s, ok := parts[2].(string)
			if !ok {
				continue
			}
			var payload interface{}
			if json.Unmarshal([]byte(s), &payload) == nil {
				out = append(out, payload)
			}
		}
	}
	return out
}

// readQuotaMetric 判据同 Voyager 的 readMetric：
// [limit, fraction(0..1.5), period, [[resetEpoch, nanos]]]。
func readQuotaMetric(v interface{}) (*QuotaMetric, float64, bool) {
	m, ok := v.([]interface{})
	if !ok || len(m) < 4 {
		return nil, 0, false
	}
	frac, ok := m[1].(float64)
	if !ok || frac < 0 || frac > 1.5 {
		return nil, 0, false
	}
	period, ok := m[2].(float64)
	if !ok {
		return nil, 0, false
	}
	rw, ok := m[3].([]interface{})
	if !ok || len(rw) == 0 {
		return nil, 0, false
	}
	head, ok := rw[0].([]interface{})
	if !ok || len(head) == 0 {
		return nil, 0, false
	}
	epoch, ok := head[0].(float64)
	if !ok || epoch < 1_600_000_000 {
		return nil, 0, false
	}
	return &QuotaMetric{UsedPercent: int(math.Round(frac * 100)), ResetAt: int64(epoch)}, period, true
}

// errText 把可能为 nil 的 error 变成可读文本。
func errText(err error) string {
	if err == nil {
		return "未知原因"
	}
	return err.Error()
}
