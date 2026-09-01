package app

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	sessionCookieName = "gemini_admin_session"
	sessionTTL        = 7 * 24 * time.Hour
)

func newSessionToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// authOK accepts either:
//  1. Cookie gemini_admin_session matching a valid sessions row
//  2. Authorization: Bearer <admin_token>
//  3. ?token=<admin_token> query param (for embedded HTML <a href> use)
func authOK(r *http.Request) bool {
	if cfg.AdminToken == "" {
		return true // unauthenticated mode (only safe behind 127.0.0.1)
	}
	if c, err := r.Cookie(sessionCookieName); err == nil && validSession(c.Value) {
		return true
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if subtle.ConstantTimeCompare([]byte(h[7:]), []byte(cfg.AdminToken)) == 1 {
			return true
		}
	}
	if t := r.URL.Query().Get("token"); t != "" {
		if subtle.ConstantTimeCompare([]byte(t), []byte(cfg.AdminToken)) == 1 {
			return true
		}
	}
	return false
}

func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authOK(r) {
			writeJSON(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad body"})
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	if cfg.AdminToken != "" && subtle.ConstantTimeCompare([]byte(req.Token), []byte(cfg.AdminToken)) != 1 {
		writeJSON(w, 401, map[string]string{"error": "invalid token"})
		return
	}
	tok := newSessionToken()
	createSession(tok, sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    tok,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		_, _ = getDB().Exec(`DELETE FROM sessions WHERE token=?`, c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// /admin/api/stats — KPI summary for last N hours (default 24).
func handleAdminStats(w http.ResponseWriter, r *http.Request) {
	hours := atoiDefault(r.URL.Query().Get("hours"), 24)
	since := time.Now().Unix() - int64(hours)*3600

	stats := map[string]interface{}{}

	var total, success, fail int64
	var sumMs, sumPT, sumOT int64
	_ = getDB().QueryRow(`SELECT COUNT(*),
        SUM(CASE WHEN status=200 THEN 1 ELSE 0 END),
        SUM(CASE WHEN status<>200 THEN 1 ELSE 0 END),
        COALESCE(SUM(total_ms),0),
        COALESCE(SUM(prompt_tokens),0),
        COALESCE(SUM(output_tokens),0)
        FROM requests WHERE ts >= ?`, since).Scan(&total, &success, &fail, &sumMs, &sumPT, &sumOT)

	stats["window_hours"] = hours
	stats["total_requests"] = total
	stats["success"] = success
	stats["failures"] = fail
	if total > 0 {
		stats["success_rate"] = float64(success) / float64(total)
		stats["avg_ms"] = sumMs / total
	} else {
		stats["success_rate"] = 0.0
		stats["avg_ms"] = 0
	}
	stats["prompt_tokens"] = sumPT
	stats["output_tokens"] = sumOT
	stats["error_breakdown"] = errorBreakdown(since)

	// per-model breakdown
	rows, _ := getDB().Query(`SELECT model, COUNT(*),
        SUM(CASE WHEN status=200 THEN 1 ELSE 0 END),
        COALESCE(AVG(total_ms),0),
        COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(output_tokens),0)
        FROM requests WHERE ts >= ? GROUP BY model ORDER BY 2 DESC`, since)
	var byModel []map[string]interface{}
	if rows != nil {
		for rows.Next() {
			var m string
			var c, ok int
			var avgMs float64
			var pt, ot int64
			if err := rows.Scan(&m, &c, &ok, &avgMs, &pt, &ot); err == nil {
				byModel = append(byModel, map[string]interface{}{
					"model":         m,
					"requests":      c,
					"success":       ok,
					"avg_ms":        int(avgMs),
					"prompt_tokens": pt,
					"output_tokens": ot,
				})
			}
		}
		rows.Close()
	}
	stats["by_model"] = byModel

	// per-proxy breakdown
	prows, _ := getDB().Query(`SELECT COALESCE(proxy_name,'(direct)') as p, COUNT(*),
        SUM(CASE WHEN status=200 THEN 1 ELSE 0 END),
        COALESCE(AVG(total_ms),0)
        FROM requests WHERE ts >= ? GROUP BY p ORDER BY 2 DESC`, since)
	var byProxy []map[string]interface{}
	if prows != nil {
		for prows.Next() {
			var p string
			var c, ok int
			var avgMs float64
			if err := prows.Scan(&p, &c, &ok, &avgMs); err == nil {
				byProxy = append(byProxy, map[string]interface{}{
					"proxy":    p,
					"requests": c,
					"success":  ok,
					"avg_ms":   int(avgMs),
				})
			}
		}
		prows.Close()
	}
	stats["by_proxy"] = byProxy

	writeJSON(w, 200, stats)
}

// /admin/api/timeseries?range=24h|7d|30d
func handleAdminTimeseries(w http.ResponseWriter, r *http.Request) {
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "24h"
	}
	var bucketSec int64
	var since int64
	now := time.Now().Unix()
	switch rng {
	case "24h":
		bucketSec = 3600
		since = now - 86400
	case "7d":
		bucketSec = 3600
		since = now - 7*86400
	case "30d":
		bucketSec = 86400
		since = now - 30*86400
	default:
		bucketSec = 3600
		since = now - 86400
	}

	var rows interface {
		Close() error
		Next() bool
		Scan(...interface{}) error
	}
	if bucketSec == 86400 {
		// pull from stats_daily
		r2, err := getDB().Query(`SELECT bucket, SUM(requests), SUM(successes), AVG(p50_ms), AVG(p95_ms),
            SUM(prompt_tokens), SUM(output_tokens)
            FROM stats_daily WHERE bucket >= ? GROUP BY bucket ORDER BY bucket`, since)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		rows = r2
	} else if rng == "7d" {
		// stats_hourly
		r2, err := getDB().Query(`SELECT bucket, SUM(requests), SUM(successes), AVG(p50_ms), AVG(p95_ms),
            SUM(prompt_tokens), SUM(output_tokens)
            FROM stats_hourly WHERE bucket >= ? GROUP BY bucket ORDER BY bucket`, since)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		rows = r2
	} else {
		// 24h: live aggregate from raw requests for fresh-out-of-the-oven curve
		r2, err := getDB().Query(`SELECT (ts - ts % 3600) as b,
            COUNT(*),
            SUM(CASE WHEN status=200 THEN 1 ELSE 0 END),
            0, 0,
            COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(output_tokens),0)
            FROM requests WHERE ts >= ? GROUP BY b ORDER BY b`, since)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		rows = r2
	}
	defer rows.Close()

	var series []map[string]interface{}
	for rows.Next() {
		var bucket int64
		var requests, successes int
		var p50, p95 float64
		var pt, ot int64
		if err := rows.Scan(&bucket, &requests, &successes, &p50, &p95, &pt, &ot); err == nil {
			series = append(series, map[string]interface{}{
				"bucket":        bucket,
				"requests":      requests,
				"successes":     successes,
				"p50_ms":        int(p50),
				"p95_ms":        int(p95),
				"prompt_tokens": pt,
				"output_tokens": ot,
			})
		}
	}
	writeJSON(w, 200, map[string]interface{}{"range": rng, "bucket_sec": bucketSec, "series": series})
}

// /admin/api/requests?limit=50&offset=0&model=...&status=...
func handleAdminRequests(w http.ResponseWriter, r *http.Request) {
	limit := atoiDefault(r.URL.Query().Get("limit"), 50)
	if limit > 500 {
		limit = 500
	}
	offset := atoiDefault(r.URL.Query().Get("offset"), 0)
	model := r.URL.Query().Get("model")
	status := r.URL.Query().Get("status")

	q := `SELECT id, ts, model, COALESCE(upstream_model,''), COALESCE(proxy_id,0), COALESCE(proxy_name,''),
        COALESCE(account_id,0), COALESCE(account_label,''),
        status, COALESCE(error,''), COALESCE(ttfb_ms,0), total_ms,
        prompt_chars, response_chars, prompt_tokens, output_tokens,
        COALESCE(endpoint,''), stream FROM requests WHERE 1=1`
	args := []interface{}{}
	if model != "" {
		q += " AND model=?"
		args = append(args, model)
	}
	if status == "ok" {
		q += " AND status=200"
	} else if status == "fail" {
		q += " AND status<>200"
	}
	q += " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := getDB().Query(q, args...)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	var list []map[string]interface{}
	for rows.Next() {
		var id, ts, proxyID, acctID, ttfb, totalMs int64
		var model, upstreamModel, proxyName, acctLabel, errStr, endpoint string
		var status, promptC, respC, promptT, outT, stream int
		if err := rows.Scan(&id, &ts, &model, &upstreamModel, &proxyID, &proxyName,
			&acctID, &acctLabel,
			&status, &errStr, &ttfb, &totalMs,
			&promptC, &respC, &promptT, &outT,
			&endpoint, &stream); err != nil {
			continue
		}
		list = append(list, map[string]interface{}{
			"id":             id,
			"ts":             ts,
			"model":          model,
			"upstream_model": upstreamModel,
			"proxy_id":       proxyID,
			"proxy_name":     proxyName,
			"account_id":     acctID,
			"account_label":  acctLabel,
			"status":         status,
			"error":          errStr,
			"ttfb_ms":        ttfb,
			"total_ms":       totalMs,
			"prompt_chars":   promptC,
			"response_chars": respC,
			"prompt_tokens":  promptT,
			"output_tokens":  outT,
			"endpoint":       endpoint,
			"stream":         stream == 1,
		})
	}

	var total int
	cq := "SELECT COUNT(*) FROM requests WHERE 1=1"
	cargs := []interface{}{}
	if model != "" {
		cq += " AND model=?"
		cargs = append(cargs, model)
	}
	if status == "ok" {
		cq += " AND status=200"
	} else if status == "fail" {
		cq += " AND status<>200"
	}
	_ = getDB().QueryRow(cq, cargs...).Scan(&total)

	writeJSON(w, 200, map[string]interface{}{
		"items":  list,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// /admin/api/proxies — GET list / POST create / PUT update / DELETE
func handleAdminProxies(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		writeJSON(w, 200, map[string]interface{}{"items": listProxies()})
	case "POST":
		var p struct {
			Name   string `json:"name"`
			URL    string `json:"url"`
			Weight int    `json:"weight"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &p); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		id, err := proxyCreate(p.Name, p.URL, p.Weight)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]interface{}{"id": id})
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

// /admin/api/proxies/{id}/{action} — PATCH update / DELETE delete / POST reset / POST toggle
func handleAdminProxyItem(w http.ResponseWriter, r *http.Request) {
	// /admin/api/proxies/123/(toggle|reset|update|delete)
	rest := strings.TrimPrefix(r.URL.Path, "/admin/api/proxies/")
	parts := strings.Split(rest, "/")
	if len(parts) < 1 {
		writeJSON(w, 404, map[string]string{"error": "missing id"})
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad id"})
		return
	}
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch r.Method {
	case "DELETE":
		if err := proxyDelete(id); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	case "POST":
		switch action {
		case "reset":
			if err := proxyResetFailures(id); err != nil {
				writeJSON(w, 500, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, 200, map[string]bool{"ok": true})
		case "toggle":
			// flip enabled
			cur := false
			for _, p := range listProxies() {
				if p.ID == id {
					cur = p.Enabled
					break
				}
			}
			newVal := !cur
			if err := proxyUpdate(id, "", "", &newVal, nil); err != nil {
				writeJSON(w, 500, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, 200, map[string]bool{"enabled": newVal})
		default:
			writeJSON(w, 400, map[string]string{"error": "unknown action"})
		}
	case "PATCH":
		var p struct {
			Name    string `json:"name"`
			URL     string `json:"url"`
			Weight  *int   `json:"weight"`
			Enabled *bool  `json:"enabled"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &p); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		if err := proxyUpdate(id, p.Name, p.URL, p.Enabled, p.Weight); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

// /admin/api/usage — 返回每个 IP slot 的当前限流用量。
func handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	usage := allSlotUsage()
	// 加上代理名字以便 UI 显示
	proxyNames := map[int64]string{}
	for _, p := range listProxies() {
		proxyNames[p.ID] = p.Name
	}
	out := make([]map[string]interface{}, 0, len(usage))
	for _, u := range usage {
		name := "直连"
		if u.ProxyID > 0 {
			if n, ok := proxyNames[u.ProxyID]; ok {
				name = n
			} else {
				name = "(已删除)"
			}
		}
		out = append(out, map[string]interface{}{
			"proxy_id":         u.ProxyID,
			"name":             name,
			"inflight":         u.Inflight,
			"rpm":              u.RPM,
			"rph":              u.RPH,
			"limit_concurrent": u.LimitConc,
			"limit_rpm":        u.LimitRPM,
			"limit_rph":        u.LimitRPH,
		})
	}
	writeJSON(w, 200, map[string]interface{}{"items": out})
}

func atoiDefault(s string, d int) int {
	if s == "" {
		return d
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return d
	}
	return n
}

// /admin/api/apikey — GET shows current key + lock status; POST rotate;
//
//	PATCH sets a custom key.
func handleAdminAPIKey(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		writeJSON(w, 200, map[string]interface{}{
			"key":    getAPIKey(),
			"locked": apiKeyLocked,
		})
	case "POST":
		// rotate
		k, err := rotateAPIKey()
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]interface{}{"key": k, "locked": apiKeyLocked})
	case "PATCH":
		var p struct {
			Key string `json:"key"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &p); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		if err := setAPIKey(p.Key); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

// /admin/api/test — 端到端连通性诊断。
//
// 走完整协议路径（chrome146 指纹 + 真 StreamGenerate）但：
//   - 不消耗限流 slot（admin 诊断不算业务流量）
//   - 不写入 requests 表（不污染业务统计）
//   - 返回详细原因码 + 延迟 + 上游响应片段
//
// 可选参数 ?proxy_id=N 测某个代理（id=0 测直连 / 留空 = 自动按调度规则挑一个）。
func handleAdminTest(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	proxyIDStr := q.Get("proxy_id")
	prompt := q.Get("prompt")
	if prompt == "" {
		prompt = "Reply with one short word."
	}

	var proxyURL, proxyName string
	var proxyID int64
	useDirect := false
	if proxyIDStr == "" {
		// 自动模式：仿照 acquireSlot 优先代理池里第一个 enabled
		for _, p := range listProxies() {
			if p.Enabled && p.FailCount < 5 {
				proxyURL = p.URL
				proxyName = p.Name
				proxyID = p.ID
				break
			}
		}
		if proxyURL == "" {
			useDirect = true
			proxyName = "直连"
		}
	} else {
		var err error
		proxyID, err = strconv.ParseInt(proxyIDStr, 10, 64)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad proxy_id"})
			return
		}
		if proxyID == 0 {
			useDirect = true
			proxyName = "直连"
		} else {
			found := false
			for _, p := range listProxies() {
				if p.ID == proxyID {
					proxyURL = p.URL
					proxyName = p.Name
					found = true
					break
				}
			}
			if !found {
				writeJSON(w, 404, map[string]string{"error": "proxy not found"})
				return
			}
		}
	}
	if useDirect {
		proxyURL = ""
	}

	t0 := time.Now()
	res := probeGemini(prompt, proxyURL)
	res.TotalMs = time.Since(t0).Milliseconds()
	res.ProxyID = proxyID
	res.ProxyName = proxyName
	res.UseDirect = useDirect

	writeJSON(w, 200, res)
}

// handleAdminConfig 读写运行时配置。GET 返回当前值 + 各项允许范围，
// PUT 校验后立刻生效并持久化到 kv。
func handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, map[string]interface{}{
			"config": rtCfg(),
			"models": modelNamesSorted(),
			// 部署期配置只读展示，改这些要动 docker-compose.yml 并重启
			"deploy": map[string]interface{}{
				"host":        cfg.Host,
				"port":        cfg.Port,
				"db_path":     cfg.DBPath,
				"cookie_file": cfg.CookieFile,
				"admin_auth":  cfg.AdminToken != "",
				// 面板的部署配置表要展示 Key 是否被启动参数锁死
				"api_key_locked": apiKeyLocked,
			},
		})
	case http.MethodPut:
		var next RuntimeConfig
		if err := json.NewDecoder(r.Body).Decode(&next); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		if err := saveRuntimeConfig(next); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]interface{}{"ok": true, "config": rtCfg()})
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

func modelNamesSorted() []string {
	var names []string
	for n := range availableModels() {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// classifyError 把 requests.error 归到几个可行动的类别。
//
// 分类的意义在于"看到之后该做什么不一样"：上游瞬时拒绝只能重试、代理层失败
// 该换代理或熔断、slot 满说明该加代理、协议错误才需要看代码。全混在一列原始
// 错误串里，排查时分不出是被限流了还是真出故障。
func classifyError(errStr string) string {
	switch {
	case errStr == "":
		return ""
	case strings.Contains(errStr, "no content frame"):
		// 上游返回 200 但一个内容帧都没有。实测是瞬时拒绝，隔几分钟自行恢复，
		// 跟频率/并发/累积次数都无关，重试即可。
		return "upstream_rejected"
	case strings.Contains(errStr, "slot full") || strings.Contains(errStr, "limit reached"):
		return "rate_limited_local"
	case strings.Contains(errStr, "upstream HTTP 302"), strings.Contains(errStr, "sorry"):
		return "blocked_sorry"
	case strings.Contains(errStr, "upstream HTTP 429"):
		return "upstream_429"
	case strings.Contains(errStr, "upstream HTTP"):
		return "upstream_http_error"
	case strings.Contains(errStr, "unknown model"), strings.Contains(errStr, "not supported"):
		return "bad_request"
	default:
		// 连不上代理 / DNS / TLS 握手失败之类
		return "network"
	}
}

// errorBreakdown 统计窗口内各类错误的次数。
func errorBreakdown(since int64) []map[string]interface{} {
	rows, err := getDB().Query(
		`SELECT COALESCE(error,''), COUNT(*) FROM requests
         WHERE ts >= ? AND status <> 200 GROUP BY error`, since)
	if err != nil || rows == nil {
		return nil
	}
	defer rows.Close()
	agg := map[string]int{}
	sample := map[string]string{}
	for rows.Next() {
		var e string
		var c int
		if rows.Scan(&e, &c) != nil {
			continue
		}
		k := classifyError(e)
		if k == "" {
			continue
		}
		agg[k] += c
		if _, ok := sample[k]; !ok {
			sample[k] = truncate(e, 120)
		}
	}
	var out []map[string]interface{}
	for k, c := range agg {
		out = append(out, map[string]interface{}{
			"kind": k, "count": c, "sample": sample[k],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["count"].(int) > out[j]["count"].(int)
	})
	return out
}
