package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// /admin/api/keys —— 多 API Key 管理。
//
// 列表只返回脱敏值；完整 key 只在创建/轮换/显式 reveal 时返回，避免每次打开面板
// 都把全部凭证放进 DOM。部署 Key（id=0）只读，但允许 reveal。

func adminKeyView(k *APIKey) map[string]interface{} {
	if k == nil {
		return nil
	}
	ids := apiKeyAccountIDs(k)
	name := k.Name
	if name == "" {
		name = fmt.Sprintf("Key #%d", k.ID)
	}
	return map[string]interface{}{
		"id":            k.ID,
		"name":          name,
		"masked_key":    maskKey(k.Key),
		"enabled":       k.Enabled,
		"locked":        k.Locked,
		"allow_all":     k.AllowAll,
		"account_ids":   ids,
		"account_count": len(ids),
		"note":          k.Note,
		"created_at":    k.CreatedAt,
	}
}

// handleAdminKeys — GET 列表 / POST 新增。
func handleAdminKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		keys := currentAPIKeys()
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].Locked != keys[j].Locked {
				return keys[i].Locked
			}
			return keys[i].ID < keys[j].ID
		})
		items := make([]map[string]interface{}, 0, len(keys))
		for _, k := range keys {
			items = append(items, adminKeyView(k))
		}
		writeJSON(w, 200, map[string]interface{}{"items": items, "total": len(items)})
	case http.MethodPost:
		var p struct {
			Name       string  `json:"name"`
			Key        string  `json:"key"`
			Enabled    *bool   `json:"enabled"`
			AllowAll   *bool   `json:"allow_all"`
			Note       string  `json:"note"`
			AccountIDs []int64 `json:"account_ids"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &p); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		key := strings.TrimSpace(p.Key)
		if key == "" {
			key = newAPIKey()
		}
		if err := validateAPIKeyValue(key); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if taken, err := apiKeyValueTaken(key, 0); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		} else if taken {
			writeJSON(w, 409, map[string]string{"error": "api key already exists"})
			return
		}
		if err := validateKeyAccountIDs(p.AccountIDs); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		enabled := true
		if p.Enabled != nil {
			enabled = *p.Enabled
		}
		allowAll := true
		if p.AllowAll != nil {
			allowAll = *p.AllowAll
		}
		id, err := apiKeyInsert(p.Name, key, p.Note, enabled, allowAll)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if len(p.AccountIDs) > 0 {
			if err := apiKeySetAccounts(id, p.AccountIDs); err != nil {
				_ = apiKeyDelete(id)
				writeJSON(w, 500, map[string]string{"error": err.Error()})
				return
			}
		}
		reloadAPIKeyStore()
		logf("[keys] 新增 API Key #%d name=%q allow_all=%v", id, strings.TrimSpace(p.Name), allowAll)
		// 完整 key 只在这个响应里返回一次。
		writeJSON(w, 200, map[string]interface{}{"id": id, "key": key})
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

// handleAdminKeyItem — /admin/api/keys/{id}[/action]
//
//	PATCH          改 name/key/enabled/allow_all/note/account_ids
//	DELETE         删除
//	POST .../rotate  换随机 key
//	POST .../toggle  启用/停用
//	POST .../reveal  返回完整 key
func handleAdminKeyItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/api/keys/")
	parts := strings.Split(rest, "/")
	if len(parts) < 1 || parts[0] == "" {
		writeJSON(w, 404, map[string]string{"error": "missing id"})
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id < 0 {
		writeJSON(w, 400, map[string]string{"error": "bad id"})
		return
	}
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	k := apiKeyByID(id)
	if k == nil {
		writeJSON(w, 404, map[string]string{"error": "key not found"})
		return
	}
	readOnly := func() bool {
		if k.Locked {
			writeJSON(w, 409, map[string]string{"error": "deploy key is read-only; it comes from --api-key / API_KEY"})
			return true
		}
		return false
	}

	switch r.Method {
	case http.MethodDelete:
		if readOnly() {
			return
		}
		if err := apiKeyDelete(id); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		reloadAPIKeyStore()
		logf("[keys] 删除 API Key #%d", id)
		writeJSON(w, 200, map[string]bool{"ok": true})
	case http.MethodPost:
		switch action {
		case "rotate":
			if readOnly() {
				return
			}
			key := newAPIKey()
			if taken, err := apiKeyValueTaken(key, id); err != nil {
				writeJSON(w, 500, map[string]string{"error": err.Error()})
				return
			} else if taken {
				writeJSON(w, 500, map[string]string{"error": "generated key collides, retry"})
				return
			}
			if err := apiKeyUpdateRow(id, k.Name, key, k.Note, k.Enabled, k.AllowAll); err != nil {
				writeJSON(w, 500, map[string]string{"error": err.Error()})
				return
			}
			reloadAPIKeyStore()
			writeJSON(w, 200, map[string]interface{}{"key": key})
		case "toggle":
			if readOnly() {
				return
			}
			next := !k.Enabled
			if err := apiKeyUpdateRow(id, k.Name, "", k.Note, next, k.AllowAll); err != nil {
				writeJSON(w, 500, map[string]string{"error": err.Error()})
				return
			}
			reloadAPIKeyStore()
			writeJSON(w, 200, map[string]interface{}{"enabled": next})
		case "reveal":
			writeJSON(w, 200, map[string]interface{}{"key": k.Key})
		default:
			writeJSON(w, 400, map[string]string{"error": "unknown action"})
		}
	case http.MethodPatch:
		if readOnly() {
			return
		}
		var p struct {
			Name       *string  `json:"name"`
			Key        *string  `json:"key"`
			Enabled    *bool    `json:"enabled"`
			AllowAll   *bool    `json:"allow_all"`
			Note       *string  `json:"note"`
			AccountIDs *[]int64 `json:"account_ids"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &p); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		name, note := k.Name, k.Note
		enabled, allowAll := k.Enabled, k.AllowAll
		key := ""
		if p.Name != nil {
			name = *p.Name
		}
		if p.Note != nil {
			note = *p.Note
		}
		if p.Enabled != nil {
			enabled = *p.Enabled
		}
		if p.AllowAll != nil {
			allowAll = *p.AllowAll
		}
		if p.Key != nil {
			key = strings.TrimSpace(*p.Key)
			if key == "" {
				writeJSON(w, 400, map[string]string{"error": "api key cannot be empty"})
				return
			}
			if err := validateAPIKeyValue(key); err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
			if taken, err := apiKeyValueTaken(key, id); err != nil {
				writeJSON(w, 500, map[string]string{"error": err.Error()})
				return
			} else if taken {
				writeJSON(w, 409, map[string]string{"error": "api key already exists"})
				return
			}
		}
		if p.AccountIDs != nil {
			if err := validateKeyAccountIDs(*p.AccountIDs); err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
		}
		if err := apiKeyUpdateRow(id, name, key, note, enabled, allowAll); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if p.AccountIDs != nil {
			if err := apiKeySetAccounts(id, *p.AccountIDs); err != nil {
				writeJSON(w, 500, map[string]string{"error": err.Error()})
				return
			}
		}
		reloadAPIKeyStore()
		writeJSON(w, 200, map[string]bool{"ok": true})
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

// validateKeyAccountIDs 拒绝不存在的账号 id，避免面板选到已删账号后静默失效。
func validateKeyAccountIDs(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	exists := map[int64]bool{}
	for _, a := range accountList() {
		exists[a.ID] = true
	}
	var missing []int64
	for _, id := range ids {
		if id > 0 && !exists[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("account id(s) not found: %v", missing)
	}
	return nil
}
