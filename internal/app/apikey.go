package app

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
)

// API key for /v1/* endpoints.
//
// 现在支持多把 key：面板里的 DB key（api_keys 表）+ 启动参数/环境变量提供的
// 只读“部署 Key”。鉴权索引在 keys.go 的 keyStore 里，这里只保留对外兼容的单
// key 辅助函数和中间件。
//
// 旧接口（/admin/api/apikey 的 GET/POST/PATCH）继续操作“默认 Key”：优先取
// id 最小的 DB key，没有 DB key 时取部署 Key。这样老脚本不需要改。

var (
	// apiKeyLocked 表示存在部署 Key（--api-key / API_KEY）。旧面板用它显示
	// “部署锁定”；它不再代表所有 key 都不可改。
	apiKeyLocked bool
	apiKeyMu     sync.Mutex
)

const apiKeyPrefix = "sk-gemini-"

func newAPIKey() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return apiKeyPrefix + hex.EncodeToString(b)
}

// getAPIKey 返回默认 Key 的值，供旧管理接口和启动 banner 使用。
func getAPIKey() string {
	if k := defaultAPIKey(); k != nil {
		return k.Key
	}
	return ""
}

// rotateAPIKey 给默认 DB Key 换一个随机值。部署 Key / 没有 DB Key 时拒绝。
func rotateAPIKey() (string, error) {
	apiKeyMu.Lock()
	defer apiKeyMu.Unlock()
	cur := defaultAPIKey()
	if cur == nil || cur.Locked {
		return "", errMsg("api key is locked by --api-key flag or env, edit deployment to change")
	}
	k := newAPIKey()
	if taken, err := apiKeyValueTaken(k, cur.ID); err != nil {
		return "", err
	} else if taken {
		return "", errMsg("generated key collides with an existing key, retry")
	}
	if err := apiKeyUpdateRow(cur.ID, cur.Name, k, cur.Note, cur.Enabled, cur.AllowAll); err != nil {
		return "", err
	}
	reloadAPIKeyStore()
	return k, nil
}

// setAPIKey 设置默认 DB Key 的自定义值。部署 Key / 没有 DB Key 时拒绝。
func setAPIKey(k string) error {
	apiKeyMu.Lock()
	defer apiKeyMu.Unlock()
	cur := defaultAPIKey()
	if cur == nil || cur.Locked {
		return errMsg("api key is locked by --api-key flag or env")
	}
	k = strings.TrimSpace(k)
	if err := validateAPIKeyValue(k); err != nil {
		return err
	}
	if taken, err := apiKeyValueTaken(k, cur.ID); err != nil {
		return err
	} else if taken {
		return errMsg("api key already exists")
	}
	if err := apiKeyUpdateRow(cur.ID, cur.Name, k, cur.Note, cur.Enabled, cur.AllowAll); err != nil {
		return err
	}
	reloadAPIKeyStore()
	return nil
}

// requireAPIKey wraps a /v1/* handler with API key auth.
//
// Accepts: "Authorization: Bearer xxx" (OpenAI SDK default), "x-api-key: xxx",
// and Google SDK 的两种写法："x-goog-api-key: xxx" 头或 "?key=xxx" 查询参数
// （/v1beta 原生 Gemini 接口的客户端只会这么带，不认就一律 401）。
//
// 命中后把 *APIKey 放进 request context，下游据此限制可用账号。
func requireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dbKeys, hasDeploy := apiKeyCounts()
		if dbKeys == 0 && !hasDeploy {
			// 没有任何 key 配置：保持旧行为，放行（正常启动 initAPIKeys 会保证
			// 至少生成一把默认 Key，这里只覆盖测试/异常场景）。
			next(w, r)
			return
		}
		got := ""
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			got = strings.TrimSpace(h[7:])
		}
		if got == "" {
			got = strings.TrimSpace(r.Header.Get("x-api-key"))
		}
		if got == "" {
			got = strings.TrimSpace(r.Header.Get("x-goog-api-key"))
		}
		if got == "" {
			got = strings.TrimSpace(r.URL.Query().Get("key"))
		}
		k := authenticateAPIKey(got)
		if k == nil {
			writeJSON(w, 401, map[string]interface{}{
				"error": map[string]string{
					"message": "Invalid API key. Set Authorization: Bearer <key>, x-api-key, x-goog-api-key or ?key=.",
					"type":    "invalid_request_error",
					"code":    "invalid_api_key",
				},
			})
			return
		}
		next(w, r.WithContext(withAPIKeyContext(r.Context(), k)))
	}
}

// helpers ────────────────────────────────────────────────────────────────────

type errString string

func (e errString) Error() string { return string(e) }
func errMsg(s string) error       { return errString(s) }
