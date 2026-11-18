package app

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// APIKey 是一把入站 API key。
//
// Accounts 的语义（重要）：
//   - nil       = allow_all，允许账号池里的全部账号；
//   - 非 nil    = 只允许 map 里为 true 的账号；空 map 表示一个账号都不允许，
//     此时该 key 只能走匿名能力（登录态模型会在模型列表里隐藏并在
//     请求时返回 403）。
type APIKey struct {
	ID        int64
	Name      string
	Key       string
	Hash      string // sha256(key) hex，鉴权按它查
	Enabled   bool
	AllowAll  bool
	Locked    bool // 部署 Key（--api-key / API_KEY），不落库、不可改
	Note      string
	CreatedAt int64
	Accounts  map[int64]bool
}

// apiKeyState 是进程内的只读快照。CRUD 后整表重建并原子替换，记录本身不再修改，
// 所以正在处理的请求拿着旧指针也不会看到半更新状态。
type apiKeyState struct {
	all        []*APIKey
	byHash     map[string]*APIKey
	deploy     *APIKey
	defaultKey *APIKey
}

var (
	keyStoreMu  sync.RWMutex
	keyStoreVal = &apiKeyState{byHash: map[string]*APIKey{}}
)

const (
	kvAPIKeysMigrated = "api_keys_migrated"
	apiKeyMinLen      = 20
	apiKeyMaxLen      = 1024
)

func keyDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// initAPIKeys 决定启动时的 key 集合。替代旧的 initAPIKey。
//
// 规则：
//  1. --api-key > API_KEY 生成内置部署 Key（ID=0，locked，允许全部账号，不落库）；
//  2. 没有部署 Key 时，把 kv("api_key") 迁移成一条 DB key（保留 kv 原值，便于回滚）；
//  3. 迁移只做一次（kv api_keys_migrated）；之后删光 DB key 不会把旧 key 复活，
//     但会生成一条新的默认 Key，避免“零 key = 开放鉴权”。
func initAPIKeys(flagKey string) string {
	deployValue := strings.TrimSpace(flagKey)
	if deployValue == "" {
		deployValue = strings.TrimSpace(os.Getenv("API_KEY"))
	}
	var deploy *APIKey
	if deployValue != "" {
		deploy = &APIKey{
			ID: 0, Name: "部署 Key", Key: deployValue, Hash: keyDigest(deployValue),
			Enabled: true, AllowAll: true, Locked: true,
			Note: "来自 --api-key / API_KEY，面板只读",
		}
	}

	keys, err := loadAPIKeysFromDB()
	if err != nil {
		logf("[keys] 读取 API Key 失败: %v", err)
		keys = nil
	}
	keys = migrateAPIKeys(keys, deploy)
	if len(keys) == 0 && deploy == nil {
		// 迁移/插入失败时不能把服务留在“零 key = 开放鉴权”状态。用一把只在
		// 本次进程有效的临时 Key 兜底，至少不裸奔；重启会换新值。
		k := newAPIKey()
		deploy = &APIKey{
			ID: 0, Name: "临时 Key", Key: k, Hash: keyDigest(k),
			Enabled: true, AllowAll: true, Locked: true,
			Note: "数据库写入失败，临时生成；重启会变",
		}
		logf("[keys] 没有可用的 API Key 且无法持久化，使用临时 Key（重启会变）")
	}

	state := buildAPIKeyState(keys, deploy)
	keyStoreMu.Lock()
	keyStoreVal = state
	keyStoreMu.Unlock()
	apiKeyLocked = deploy != nil
	if state.defaultKey != nil {
		return state.defaultKey.Key
	}
	return ""
}

// buildAPIKeyState 把记录整理成鉴权索引。DB 里理论上 key_hash 唯一；部署 Key 与
// DB Key 撞值属于配置错误，部署 Key 优先（它是锁定的，不能从面板改）。
func buildAPIKeyState(keys []*APIKey, deploy *APIKey) *apiKeyState {
	state := &apiKeyState{byHash: map[string]*APIKey{}, deploy: deploy}
	state.all = append(state.all, keys...)
	if deploy != nil {
		state.all = append(state.all, deploy)
	}
	for _, k := range keys {
		if k.Hash == "" {
			k.Hash = keyDigest(k.Key)
		}
		state.byHash[k.Hash] = k
		if state.defaultKey == nil {
			state.defaultKey = k
		}
	}
	if deploy != nil {
		if prev, ok := state.byHash[deploy.Hash]; ok {
			logf("[keys] 部署 Key 与 DB Key #%d 的值相同，鉴权按部署 Key 处理", prev.ID)
		}
		state.byHash[deploy.Hash] = deploy
		if state.defaultKey == nil {
			state.defaultKey = deploy
		}
	}
	return state
}

// loadAPIKeysFromDB 读全部 key 和它们的账号白名单。
func loadAPIKeysFromDB() ([]*APIKey, error) {
	rows, err := getDB().Query(
		`SELECT id, name, key, key_hash, enabled, allow_all, note, created_at
		 FROM api_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var keys []*APIKey
	byID := map[int64]*APIKey{}
	for rows.Next() {
		var k APIKey
		var enabled, allowAll int
		if err := rows.Scan(&k.ID, &k.Name, &k.Key, &k.Hash, &enabled, &allowAll, &k.Note, &k.CreatedAt); err != nil {
			continue
		}
		k.Enabled = enabled == 1
		k.AllowAll = allowAll == 1
		k.Accounts = map[int64]bool{}
		keys = append(keys, &k)
		byID[k.ID] = &k
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	arows, err := getDB().Query(`SELECT key_id, account_id FROM api_key_accounts ORDER BY key_id, account_id`)
	if err != nil {
		return nil, err
	}
	defer arows.Close()
	for arows.Next() {
		var keyID, accountID int64
		if err := arows.Scan(&keyID, &accountID); err != nil {
			continue
		}
		if k := byID[keyID]; k != nil {
			k.Accounts[accountID] = true
		}
	}
	return keys, arows.Err()
}

// migrateAPIKeys 执行一次性的 kv("api_key") 迁移。返回最终要装进 store 的列表。
func migrateAPIKeys(keys []*APIKey, deploy *APIKey) []*APIKey {
	// 部署 Key 存在时保持旧语义：env/flag 压过 kv，且不迁移、不写标记。
	// 这样以后去掉环境变量，旧版 kv key 还能按原逻辑回落。
	if deploy != nil {
		return keys
	}
	if kvGet(kvAPIKeysMigrated) == "1" {
		if len(keys) == 0 {
			if k, err := createDefaultAPIKey("自动生成（此前所有 Key 都被删除）"); err == nil {
				keys = append(keys, k)
				// 同步 kv，旧版回滚时还能用这把新默认 Key。
				_ = kvSet("api_key", k.Key)
			} else {
				logf("[keys] 自动生成默认 Key 失败: %v", err)
			}
		}
		return keys
	}

	old := strings.TrimSpace(kvGet("api_key"))
	if old != "" {
		if !apiKeyValueInList(keys, old) {
			id, err := apiKeyInsert("默认 Key", old, "从旧版单 Key 迁移", true, true)
			if err != nil {
				// 迁移失败不写标记，下次启动继续试。保留 kv 原值，旧版回滚仍可用。
				logf("[keys] 迁移旧 API Key 失败，下次启动重试: %v", err)
				return keys
			}
			keys = append(keys, &APIKey{
				ID: id, Name: "默认 Key", Key: old, Hash: keyDigest(old),
				Enabled: true, AllowAll: true, Note: "从旧版单 Key 迁移",
				CreatedAt: time.Now().Unix(),
			})
		}
		_ = kvSet(kvAPIKeysMigrated, "1")
		return keys
	}

	if len(keys) == 0 {
		k, err := createDefaultAPIKey("首次启动自动生成")
		if err != nil {
			logf("[keys] 自动生成默认 Key 失败: %v", err)
			return keys
		}
		keys = append(keys, k)
		// 回滚兼容：旧版只认 kv("api_key")，把默认 Key 同步过去。
		_ = kvSet("api_key", k.Key)
	}
	_ = kvSet(kvAPIKeysMigrated, "1")
	return keys
}

func createDefaultAPIKey(note string) (*APIKey, error) {
	k := newAPIKey()
	id, err := apiKeyInsert("默认 Key", k, note, true, true)
	if err != nil {
		return nil, err
	}
	return &APIKey{
		ID: id, Name: "默认 Key", Key: k, Hash: keyDigest(k),
		Enabled: true, AllowAll: true, Note: note, CreatedAt: time.Now().Unix(),
	}, nil
}

func apiKeyValueInList(keys []*APIKey, value string) bool {
	want := keyDigest(strings.TrimSpace(value))
	for _, k := range keys {
		if k.Hash == want || k.Key == value {
			return true
		}
	}
	return false
}

// reloadAPIKeyStore 从库里重读 key 集合，保留部署 Key。CRUD/账号删除后调用。
func reloadAPIKeyStore() {
	keys, err := loadAPIKeysFromDB()
	if err != nil {
		logf("[keys] 重读 API Key 失败，保留旧快照: %v", err)
		return
	}
	keyStoreMu.RLock()
	deploy := keyStoreVal.deploy
	keyStoreMu.RUnlock()
	state := buildAPIKeyState(keys, deploy)
	keyStoreMu.Lock()
	keyStoreVal = state
	keyStoreMu.Unlock()
}

func currentAPIKeys() []*APIKey {
	keyStoreMu.RLock()
	defer keyStoreMu.RUnlock()
	out := make([]*APIKey, len(keyStoreVal.all))
	copy(out, keyStoreVal.all)
	return out
}

func defaultAPIKey() *APIKey {
	keyStoreMu.RLock()
	defer keyStoreMu.RUnlock()
	return keyStoreVal.defaultKey
}

// defaultAPIKeyLocked 表示默认 Key 是不是部署 Key（面板不能改它的值）。
func defaultAPIKeyLocked() bool {
	k := defaultAPIKey()
	return k != nil && k.Locked
}

func apiKeyByID(id int64) *APIKey {
	keyStoreMu.RLock()
	defer keyStoreMu.RUnlock()
	for _, k := range keyStoreVal.all {
		if k.ID == id {
			return k
		}
	}
	return nil
}

func apiKeyCounts() (database int, deploy bool) {
	keyStoreMu.RLock()
	defer keyStoreMu.RUnlock()
	for _, k := range keyStoreVal.all {
		if k.Locked {
			deploy = true
		} else {
			database++
		}
	}
	return
}

// authenticateAPIKey 按候选值查 key。返回 nil 表示没匹配上（或没有 key 配置，
// 由调用方决定是否放行）。
func authenticateAPIKey(candidate string) *APIKey {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return nil
	}
	hash := keyDigest(candidate)
	keyStoreMu.RLock()
	k := keyStoreVal.byHash[hash]
	// 先按 hash 找到候选，再对完整值做常量时间比较，避免 hash 碰撞/时序差异。
	ok := k != nil && k.Enabled && subtle.ConstantTimeCompare([]byte(candidate), []byte(k.Key)) == 1
	keyStoreMu.RUnlock()
	if !ok {
		return nil
	}
	return k
}

// ── 请求上下文 ────────────────────────────────────────────────────────────────

type apiKeyContextKey struct{}

func withAPIKeyContext(ctx context.Context, k *APIKey) context.Context {
	return context.WithValue(ctx, apiKeyContextKey{}, k)
}

func keyFromContext(ctx context.Context) *APIKey {
	if ctx == nil {
		return nil
	}
	k, _ := ctx.Value(apiKeyContextKey{}).(*APIKey)
	return k
}

// ── 账号白名单 ────────────────────────────────────────────────────────────────

// allowedByMap nil = 全部允许；非 nil（含空 map）= 只允许 map 里为 true 的。
func allowedByMap(allowed map[int64]bool, id int64) bool {
	if allowed == nil {
		return true
	}
	return allowed[id]
}

// withKeyAccess 把 key 的身份/白名单挂到本次请求的 ModelConfig 上。
func withKeyAccess(mc ModelConfig, k *APIKey) ModelConfig {
	if k == nil {
		return mc
	}
	mc.KeyID = k.ID
	mc.KeyName = k.Name
	if !k.AllowAll {
		// 空 map 也要保留（表示一个都不允许），不能归一成 nil。
		mc.AllowedAccounts = k.Accounts
	}
	return mc
}

// hasCookieForKey 判断这个 key 有没有至少一个 enabled 的可用账号。
func hasCookieForKey(k *APIKey) bool {
	if k == nil || k.AllowAll {
		return hasCookie()
	}
	if len(k.Accounts) == 0 {
		return false
	}
	for _, a := range accountList() {
		if a.Status == "enabled" && k.Accounts[a.ID] {
			return true
		}
	}
	return false
}

// availableModelsForKey 按 key 的账号白名单过滤登录态模型。
// 匿名可用的模型始终暴露；需要登录但 key 没有可用账号的模型不暴露，
// 免得客户端以为能用、请求时才失败。
func availableModelsForKey(k *APIKey) map[string]ModelConfig {
	all := availableModels()
	if k == nil || k.AllowAll {
		return all
	}
	login := hasCookieForKey(k)
	out := make(map[string]ModelConfig, len(all))
	for name, mc := range all {
		if modelNeedsLogin(mc) && !login {
			continue
		}
		out[name] = mc
	}
	return out
}

// keyCanUseModel 判断 key 能不能使用这个模型。只拦“需要登录态但 key 没有可用
// 账号”的情况；匿名可用模型不受账号白名单限制（这是产品确认的语义）。
func keyCanUseModel(k *APIKey, mc ModelConfig) error {
	if k == nil || k.AllowAll {
		return nil
	}
	if !modelNeedsLogin(mc) {
		return nil
	}
	if hasCookieForKey(k) {
		return nil
	}
	return &KeyAccountForbiddenError{KeyName: k.Name, Model: mc.Upstream,
		Detail: "this key has no enabled account in its allowlist for a signed-in model"}
}

// resolveModelForKey 先按全局规则解析，再套 key 的账号白名单。
func resolveModelForKey(modelName string, k *APIKey) (string, ModelConfig, error) {
	name, mc, err := resolveModel(modelName)
	if err != nil {
		return "", ModelConfig{}, err
	}
	if err := keyCanUseModel(k, mc); err != nil {
		return "", ModelConfig{}, err
	}
	return name, withKeyAccess(mc, k), nil
}

// ── DB CRUD ──────────────────────────────────────────────────────────────────

func validateAPIKeyValue(k string) error {
	k = strings.TrimSpace(k)
	if len(k) < apiKeyMinLen {
		return fmt.Errorf("api key too short (min %d chars)", apiKeyMinLen)
	}
	if len(k) > apiKeyMaxLen {
		return fmt.Errorf("api key too long (max %d chars)", apiKeyMaxLen)
	}
	return nil
}

func apiKeyInsert(name, key, note string, enabled, allowAll bool) (int64, error) {
	key = strings.TrimSpace(key)
	en, al := 0, 0
	if enabled {
		en = 1
	}
	if allowAll {
		al = 1
	}
	return insertID(
		`INSERT INTO api_keys(name, key, key_hash, enabled, allow_all, note, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		strings.TrimSpace(name), key, keyDigest(key), en, al, strings.TrimSpace(note), time.Now().Unix())
}

func apiKeyUpdateRow(id int64, name, key, note string, enabled, allowAll bool) error {
	en, al := 0, 0
	if enabled {
		en = 1
	}
	if allowAll {
		al = 1
	}
	key = strings.TrimSpace(key)
	if key == "" {
		_, err := getDB().Exec(
			`UPDATE api_keys SET name=?, enabled=?, allow_all=?, note=? WHERE id=?`,
			strings.TrimSpace(name), en, al, strings.TrimSpace(note), id)
		return err
	}
	_, err := getDB().Exec(
		`UPDATE api_keys SET name=?, key=?, key_hash=?, enabled=?, allow_all=?, note=? WHERE id=?`,
		strings.TrimSpace(name), key, keyDigest(key), en, al, strings.TrimSpace(note), id)
	return err
}

func apiKeyDelete(id int64) error {
	if id <= 0 {
		return nil
	}
	_, _ = getDB().Exec(`DELETE FROM api_key_accounts WHERE key_id=?`, id)
	_, err := getDB().Exec(`DELETE FROM api_keys WHERE id=?`, id)
	return err
}

// apiKeySetAccounts 全量替换一个 key 的账号白名单。
// 关联表只有两列主键，不能用 upsert（PG 会生成空的 DO UPDATE SET），
// 所以用事务里 DELETE + INSERT。
func apiKeySetAccounts(id int64, ids []int64) error {
	tx, err := getDB().Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM api_key_accounts WHERE key_id=?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	seen := map[int64]bool{}
	for _, accountID := range ids {
		if accountID <= 0 || seen[accountID] {
			continue
		}
		seen[accountID] = true
		if _, err := tx.Exec(`INSERT INTO api_key_accounts(key_id, account_id) VALUES (?,?)`, id, accountID); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// apiKeyValueTaken 判断 key 值是否已被另一条记录占用。
func apiKeyValueTaken(key string, excludeID int64) (bool, error) {
	// 部署 Key 不在 DB 里，但它的值同样不能被 DB Key 复用，否则鉴权会按部署
	// Key 处理，DB 那条永远匹配不上。
	if apiKeyValueIsDeploy(key) {
		return true, nil
	}
	hash := keyDigest(strings.TrimSpace(key))
	var id int64
	err := getDB().QueryRow(`SELECT id FROM api_keys WHERE key_hash=?`, hash).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return id != excludeID, nil
}

func apiKeyValueIsDeploy(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	keyStoreMu.RLock()
	defer keyStoreMu.RUnlock()
	d := keyStoreVal.deploy
	return d != nil && subtle.ConstantTimeCompare([]byte(key), []byte(d.Key)) == 1
}

// apiKeyAccountIDs 返回一个 key 的白名单（排序后），给管理面板展示。
// allow_all 时也返回已保存的关联：用户切回“只允许勾选账号”时能恢复上次的选择。
func apiKeyAccountIDs(k *APIKey) []int64 {
	if k == nil || len(k.Accounts) == 0 {
		return []int64{}
	}
	ids := make([]int64, 0, len(k.Accounts))
	for id := range k.Accounts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
