package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// resetKeyState 清空 key 表和 kv 迁移状态，并保存/恢复进程内 store。
func resetKeyState(t *testing.T) {
	t.Helper()
	keyStoreMu.RLock()
	old := keyStoreVal
	keyStoreMu.RUnlock()
	oldLocked := apiKeyLocked
	oldEnv, hadEnv := os.LookupEnv("API_KEY")
	_ = os.Unsetenv("API_KEY")

	_, _ = getDB().Exec(`DELETE FROM api_key_accounts`)
	_, _ = getDB().Exec(`DELETE FROM api_keys`)
	_ = kvSet("api_key", "")
	_ = kvSet(kvAPIKeysMigrated, "")
	keyStoreMu.Lock()
	keyStoreVal = &apiKeyState{byHash: map[string]*APIKey{}}
	keyStoreMu.Unlock()
	apiKeyLocked = false

	t.Cleanup(func() {
		_, _ = getDB().Exec(`DELETE FROM api_key_accounts`)
		_, _ = getDB().Exec(`DELETE FROM api_keys`)
		_ = kvSet("api_key", "")
		_ = kvSet(kvAPIKeysMigrated, "")
		keyStoreMu.Lock()
		keyStoreVal = old
		keyStoreMu.Unlock()
		apiKeyLocked = oldLocked
		if hadEnv {
			_ = os.Setenv("API_KEY", oldEnv)
		} else {
			_ = os.Unsetenv("API_KEY")
		}
	})
}

func TestAPIKeyMigrationFromKV(t *testing.T) {
	resetKeyState(t)
	const old = "old-key-0123456789abcdef"
	if err := kvSet("api_key", old); err != nil {
		t.Fatal(err)
	}
	if got := initAPIKeys(""); got != old {
		t.Fatalf("initAPIKeys = %q, want old kv key", got)
	}
	if authenticateAPIKey(old) == nil {
		t.Fatal("migrated key does not authenticate")
	}
	keys, err := loadAPIKeysFromDB()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Key != old || !keys[0].AllowAll {
		t.Fatalf("migrated keys = %+v", keys)
	}
	// 再启动一次不能重复迁移。
	_ = initAPIKeys("")
	keys, _ = loadAPIKeysFromDB()
	if len(keys) != 1 {
		t.Fatalf("migration not idempotent: %d keys", len(keys))
	}
}

func TestDeployKeyCoexistsWithDBKey(t *testing.T) {
	resetKeyState(t)
	const deploy = "deploy-key-0123456789abcdef"
	_ = initAPIKeys(deploy)
	if authenticateAPIKey(deploy) == nil {
		t.Fatal("deploy key does not authenticate")
	}
	if k := apiKeyByID(0); k == nil || !k.Locked || !k.AllowAll {
		t.Fatalf("deploy key = %+v", k)
	}
	const dbKey = "db-key-0123456789abcdef"
	id, err := apiKeyInsert("db", dbKey, "", true, true)
	if err != nil {
		t.Fatal(err)
	}
	reloadAPIKeyStore()
	if authenticateAPIKey(dbKey) == nil {
		t.Fatal("db key does not authenticate alongside deploy key")
	}
	if k := apiKeyByID(id); k == nil || k.Locked {
		t.Fatalf("db key = %+v", k)
	}
}

func TestKeyAccountWhitelist(t *testing.T) {
	a := addProxyTestAccount(t, "key-a", "sap-key-a", "psid-key-a")
	b := addProxyTestAccount(t, "key-b", "sap-key-b", "psid-key-b")

	key := &APIKey{
		ID: 101, Name: "team-a", Key: "team-a-key-0123456789",
		Enabled: true, AllowAll: false, Accounts: map[int64]bool{a.ID: true},
	}
	setTestAPIKeys(t, key)
	if !hasCookieForKey(key) {
		t.Fatal("key should have an enabled allowed account")
	}
	got, ok := pickCookieAccountFiltered(nil, key.Accounts)
	if !ok || got.ID != a.ID {
		t.Fatalf("pick = %+v ok=%v, want account #%d", got, ok, a.ID)
	}
	if err := keyCanUseModel(key, Models["gemini-4-carbon"]); err != nil {
		t.Fatalf("allowed account should pass login model: %v", err)
	}

	empty := &APIKey{
		ID: 102, Name: "empty", Key: "empty-key-0123456789",
		Enabled: true, AllowAll: false, Accounts: map[int64]bool{},
	}
	setTestAPIKeys(t, empty)
	if hasCookieForKey(empty) {
		t.Fatal("empty allowlist should have no enabled account")
	}
	if err := keyCanUseModel(empty, Models["gemini-4-carbon"]); err == nil {
		t.Fatal("empty allowlist should be forbidden for a login model")
	}
	models := availableModelsForKey(empty)
	if _, ok := models["gemini-4-carbon"]; ok {
		t.Fatal("login model should be hidden for a key with no accounts")
	}
	if _, ok := models["gemini-3.6-flash"]; !ok {
		t.Fatal("anonymous model should stay visible")
	}

	_ = b // 账号 B 存在但不允许，确保过滤不是“有号就行”
}

func TestRestrictedKeyDoesNotFallBackToAnonymous(t *testing.T) {
	a := addProxyTestAccount(t, "key-nofallback", "sap-nofb", "psid-nofb")
	key := &APIKey{
		ID: 103, Name: "no-fallback", Key: "no-fallback-key-0123456789",
		Enabled: true, AllowAll: false, Accounts: map[int64]bool{a.ID: true},
	}
	setTestAPIKeys(t, key)

	mc := Models["gemini-image"]
	mc.KeyID, mc.KeyName, mc.AllowedAccounts = key.ID, key.Name, key.Accounts
	// 账号已经试过并被 exclude：受限 key 下不能再退成匿名，必须报不可用。
	_, _, err := pickAccountForModel(mc, map[int64]bool{a.ID: true})
	var aue *AccountUnavailableError
	if !errors.As(err, &aue) || !aue.PoolExhausted {
		t.Fatalf("err = %v, want pool-exhausted AccountUnavailableError", err)
	}
}

func TestConversationKeysAreScoped(t *testing.T) {
	msgs := []map[string]interface{}{
		{"role": "user", "content": "hello"},
		{"role": "assistant", "content": "hi"},
		{"role": "user", "content": "again"},
	}
	if convParentKey(msgs, 1) == convParentKey(msgs, 2) {
		t.Fatal("parent key must include API key id")
	}
	if convChildKey(msgs, "answer", 1) == convChildKey(msgs, "answer", 2) {
		t.Fatal("child key must include API key id")
	}
	if convParentKey(msgs, 1) == "" {
		t.Fatal("parent key should not be empty")
	}
}

func TestVideoJobKeyOwnership(t *testing.T) {
	j := &videoJob{ID: "video_x", keyID: 7}
	if !videoJobAllowed(j, &APIKey{ID: 7}) {
		t.Fatal("same key should access the job")
	}
	if videoJobAllowed(j, &APIKey{ID: 8}) {
		t.Fatal("another key must not access the job")
	}
	if videoJobAllowed(j, nil) {
		t.Fatal("no-key context must not access a keyed job")
	}
	j0 := &videoJob{ID: "video_y", keyID: 0}
	if !videoJobAllowed(j0, nil) {
		t.Fatal("no-key context should access a job created without a key")
	}
	if !videoJobAllowed(j0, &APIKey{ID: 0, Locked: true}) {
		t.Fatal("deploy key should access a job created by it")
	}
}

func TestAdminKeyListMasksAndReveal(t *testing.T) {
	resetKeyState(t)
	_ = initAPIKeys("deploy-key-0123456789abcdef")

	const full = "team-key-0123456789abcdef"
	req := httptest.NewRequest("POST", "/admin/api/keys",
		strings.NewReader(`{"name":"team","key":"`+full+`","allow_all":true}`))
	w := httptest.NewRecorder()
	handleAdminKeys(w, req)
	if w.Code != 200 {
		t.Fatalf("create: HTTP %d %s", w.Code, w.Body.String())
	}
	var created struct {
		ID  int64  `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID <= 0 || created.Key != full {
		t.Fatalf("create response = %+v", created)
	}

	req = httptest.NewRequest("GET", "/admin/api/keys", nil)
	w = httptest.NewRecorder()
	handleAdminKeys(w, req)
	if strings.Contains(w.Body.String(), full) {
		t.Fatal("list leaked the full key")
	}
	if !strings.Contains(w.Body.String(), "masked_key") {
		t.Fatal("list missing masked_key")
	}

	req = httptest.NewRequest("POST", fmt.Sprintf("/admin/api/keys/%d/reveal", created.ID), nil)
	w = httptest.NewRecorder()
	handleAdminKeyItem(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), full) {
		t.Fatalf("reveal: HTTP %d %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest("DELETE", fmt.Sprintf("/admin/api/keys/%d", created.ID), nil)
	w = httptest.NewRecorder()
	handleAdminKeyItem(w, req)
	if w.Code != 200 {
		t.Fatalf("delete: HTTP %d %s", w.Code, w.Body.String())
	}
	if apiKeyByID(created.ID) != nil {
		t.Fatal("key still in store after delete")
	}
}

func TestSchemaIncludesAPIKeyTables(t *testing.T) {
	mysql := strings.Join(schemaStatements(dialectMySQL), "\n")
	if !strings.Contains(mysql, "api_keys") || !strings.Contains(mysql, "api_key_accounts") {
		t.Fatal("schema missing api key tables")
	}
	if !strings.Contains(mysql, "VARCHAR(64) NOT NULL UNIQUE") {
		t.Fatal("key_hash should be VARCHAR(64) UNIQUE for MySQL")
	}
	if strings.Contains(mysql, "AUTOINCREMENT") {
		t.Fatal("MySQL schema still has AUTOINCREMENT")
	}
	pg := strings.Join(schemaStatements(dialectPostgres), "\n")
	if !strings.Contains(pg, "BIGSERIAL PRIMARY KEY") {
		t.Fatal("PG schema missing BIGSERIAL")
	}
}
