package app

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// keyCookieWants 是判断一条 cookie 是否"复制全了"要看的关键项。
//
// 抓包实测浏览器每次发往 gemini.google.com 的有 30 个 cookie，其中 16 个是
// 每请求必带的登录态项。这里只挑最能反映"复制全没全"的几个来展示，不是说
// 别的不重要 —— 我们把整串原样转发，少一个就是少发一个。
var keyCookieWants = []string{"SID", "HSID", "SSID", "APISID", "SAPISID",
	"__Secure-1PSID", "__Secure-1PSIDTS", "__Secure-1PAPISID", "__Secure-1PSIDCC"}

// cookieAcctView 是账号的脱敏视图：**不含完整 cookie 值**，只回状态摘要。
// 凭证没必要从服务端再发回浏览器一次（跟旧 handleAdminCookie 同原则）。
func cookieAcctView(a CookieAccount) map[string]interface{} {
	names := cookieNames(a.Cookie)
	nameSet := map[string]bool{}
	for _, n := range names {
		nameSet[n] = true
	}
	var key []string
	for _, w := range keyCookieWants {
		if nameSet[w] {
			key = append(key, w)
		}
	}
	tail := ""
	if s := extractSAPISID(a.Cookie); len(s) >= 4 {
		tail = s[len(s)-4:]
	}
	return map[string]interface{}{
		"id":           a.ID,
		"label":        a.Label,
		"status":       a.Status,
		"note":         a.Note,
		"created_at":   a.CreatedAt,
		"last_used_at": a.LastUsedAt,
		"last_ok_at":   a.LastOkAt,
		"last_error":   a.LastError,
		"fail_count":   a.FailCount,
		"cookie_count": len(names),
		"key_cookies":  key,
		"sapisid_tail": tail,
		"has_1psidts":  nameSet["__Secure-1PSIDTS"],
		"proxy_id":     a.ProxyID,
		"proxy_name":   proxyNameByID(a.ProxyID),
	}
}

// handleAdminCookies — GET 列出池 / POST 新增一条。
func handleAdminCookies(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		accts := accountList()
		items := make([]map[string]interface{}, 0, len(accts))
		for _, a := range accts {
			items = append(items, cookieAcctView(a))
		}
		total, enabled := accountCount()
		writeJSON(w, 200, map[string]interface{}{
			"items": items, "total": total, "enabled": enabled,
		})
	case http.MethodPost:
		var p struct {
			Label  string `json:"label"`
			Cookie string `json:"cookie"`
			Note   string `json:"note"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &p); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		id, err := accountAdd(p.Label, p.Cookie, p.Note)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		logf("[cookies] 新增账号 #%d label=%q", id, strings.TrimSpace(p.Label))
		writeJSON(w, 200, map[string]interface{}{"id": id})
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

// handleAdminCookieItem — /admin/api/cookies/{id}[/toggle]
//
//	DELETE          删除
//	POST .../toggle 翻转 enabled/disabled
//	PATCH           改 label / note / status
func handleAdminCookieItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/api/cookies/")
	parts := strings.Split(rest, "/")
	if len(parts) < 1 || parts[0] == "" {
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
	case http.MethodDelete:
		if err := accountDelete(id); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		logf("[cookies] 删除账号 #%d", id)
		writeJSON(w, 200, map[string]bool{"ok": true})
	case http.MethodPost:
		if action == "rotate" {
			for _, a := range accountList() {
				if a.ID == id {
					iv, names, err := rotateAccount(a)
					if err != nil {
						writeJSON(w, 200, map[string]interface{}{"ok": false, "detail": err.Error()})
						return
					}
					detail := "保活成功"
					if len(names) > 0 {
						detail = "保活成功，刷新了 " + strings.Join(names, ", ")
					}
					writeJSON(w, 200, map[string]interface{}{
						"ok": true, "detail": detail, "refreshed": names, "next_sec": int(iv.Seconds())})
					return
				}
			}
			writeJSON(w, 404, map[string]string{"error": "account not found"})
			return
		}
		if action == "check" {
			for _, a := range accountList() {
				if a.ID == id {
					writeJSON(w, 200, checkAccountCookie(a))
					return
				}
			}
			writeJSON(w, 404, map[string]string{"error": "account not found"})
			return
		}
		if action != "toggle" {
			writeJSON(w, 400, map[string]string{"error": "unknown action"})
			return
		}
		cur := ""
		for _, a := range accountList() {
			if a.ID == id {
				cur = a.Status
				break
			}
		}
		next := "enabled"
		if cur == "enabled" {
			next = "disabled"
		}
		if err := accountSetStatus(id, next); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]interface{}{"status": next})
	case http.MethodPatch:
		var p struct {
			Label  *string `json:"label"`
			Note   *string `json:"note"`
			Status *string `json:"status"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &p); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		if p.Status != nil {
			if err := accountSetStatus(id, *p.Status); err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
		}
		if p.Label != nil || p.Note != nil {
			// 只改传了的字段：没传的沿用当前值。
			var curLabel, curNote string
			for _, a := range accountList() {
				if a.ID == id {
					curLabel, curNote = a.Label, a.Note
					break
				}
			}
			if p.Label != nil {
				curLabel = *p.Label
			}
			if p.Note != nil {
				curNote = *p.Note
			}
			if err := accountUpdateMeta(id, curLabel, curNote); err != nil {
				writeJSON(w, 500, map[string]string{"error": err.Error()})
				return
			}
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}
