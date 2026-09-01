package app

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var (
	db     *sql.DB
	dbw    *dbx
	dbOnce sync.Once
)

const schema = `
CREATE TABLE IF NOT EXISTS proxies (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT NOT NULL,
    url          TEXT NOT NULL,
    enabled      INTEGER NOT NULL DEFAULT 1,
    weight       INTEGER NOT NULL DEFAULT 1,
    fail_count   INTEGER NOT NULL DEFAULT 0,
    last_used    INTEGER,
    last_error   TEXT,
    created_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS requests (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    ts              INTEGER NOT NULL,
    model           TEXT NOT NULL,
    upstream_model  TEXT,
    proxy_id        INTEGER,
    proxy_name      TEXT,
    account_id      INTEGER,
    account_label   TEXT,
    status          INTEGER NOT NULL,
    error           TEXT,
    ttfb_ms         INTEGER,
    total_ms        INTEGER NOT NULL,
    prompt_chars    INTEGER NOT NULL,
    response_chars  INTEGER NOT NULL,
    prompt_tokens   INTEGER NOT NULL,
    output_tokens   INTEGER NOT NULL,
    endpoint        TEXT,
    stream          INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_requests_ts ON requests(ts);
CREATE INDEX IF NOT EXISTS idx_requests_proxy ON requests(proxy_id);
CREATE INDEX IF NOT EXISTS idx_requests_model ON requests(model);

-- Hourly aggregate (永久保留，明细只留 30 天)
CREATE TABLE IF NOT EXISTS stats_hourly (
    bucket          INTEGER NOT NULL,    -- unix ts of hour start
    model           TEXT NOT NULL,
    proxy_id        INTEGER NOT NULL,    -- 0 = no proxy
    requests        INTEGER NOT NULL DEFAULT 0,
    successes       INTEGER NOT NULL DEFAULT 0,
    failures        INTEGER NOT NULL DEFAULT 0,
    total_ms        INTEGER NOT NULL DEFAULT 0,
    p50_ms          INTEGER NOT NULL DEFAULT 0,
    p95_ms          INTEGER NOT NULL DEFAULT 0,
    prompt_tokens   INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (bucket, model, proxy_id)
);
CREATE INDEX IF NOT EXISTS idx_hourly_bucket ON stats_hourly(bucket);

-- Daily aggregate (永久保留)
CREATE TABLE IF NOT EXISTS stats_daily (
    bucket          INTEGER NOT NULL,
    model           TEXT NOT NULL,
    proxy_id        INTEGER NOT NULL,
    requests        INTEGER NOT NULL DEFAULT 0,
    successes       INTEGER NOT NULL DEFAULT 0,
    failures        INTEGER NOT NULL DEFAULT 0,
    total_ms        INTEGER NOT NULL DEFAULT 0,
    p50_ms          INTEGER NOT NULL DEFAULT 0,
    p95_ms          INTEGER NOT NULL DEFAULT 0,
    prompt_tokens   INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (bucket, model, proxy_id)
);
CREATE INDEX IF NOT EXISTS idx_daily_bucket ON stats_daily(bucket);

CREATE TABLE IF NOT EXISTS sessions (
    token        TEXT PRIMARY KEY,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS kv (
    k TEXT PRIMARY KEY,
    v TEXT
);

-- Cookie 池：每行一个 Google 登录态账号（一整串 gemini.google.com cookie）。
-- 请求时按 last_used_at 最久优先挑一个 enabled 的，天然轮转 + 分散单 IP 上限。
CREATE TABLE IF NOT EXISTS accounts (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    label         TEXT NOT NULL DEFAULT '',      -- 用户可命名（一般填邮箱）
    cookie        TEXT NOT NULL,                 -- 完整 cookie 串 "k=v; k=v"
    status        TEXT NOT NULL DEFAULT 'enabled', -- enabled | disabled
    note          TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    last_used_at  INTEGER NOT NULL DEFAULT 0,     -- 上次被挑中发请求的时刻
    last_ok_at    INTEGER NOT NULL DEFAULT 0,     -- 上次请求成功的时刻
    last_error    TEXT NOT NULL DEFAULT '',
    fail_count    INTEGER NOT NULL DEFAULT 0,     -- 连续失败次数（成功归零）
    proxy_id      INTEGER NOT NULL DEFAULT 0      -- 绑定的出口，0 = 还没绑
);
CREATE INDEX IF NOT EXISTS idx_accounts_pick ON accounts(status, last_used_at);
`

func getDB() *dbx {
	dbOnce.Do(func() {
		var driver, connStr, sqliteDir string
		if dsnEnv := strings.TrimSpace(os.Getenv("SQL_DSN")); dsnEnv != "" {
			curDialect, driver, connStr = detectDSN(dsnEnv)
		} else {
			curDialect = dialectSQLite
			driver = "sqlite"
			path := cfg.DBPath
			if path == "" {
				path = "./data/gemini.db"
			}
			sqliteDir = filepath.Dir(path)
			if err := os.MkdirAll(sqliteDir, 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "[db] 建目录 %s 失败: %v\n%s", sqliteDir, err, dbPermHint(sqliteDir))
				os.Exit(1)
			}
			connStr = fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
		}
		conn, err := sql.Open(driver, connStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[db] open failed: %v\n", err)
			os.Exit(1)
		}
		conn.SetMaxOpenConns(8)
		conn.SetMaxIdleConns(4)
		conn.SetConnMaxLifetime(0)
		// mysql/pg 驱动默认不接受多语句 Exec，按方言逐条建表。
		for _, stmt := range schemaStatements(curDialect) {
			if _, err := conn.Exec(stmt); err != nil {
				// mysql 的 CREATE INDEX 没有 IF NOT EXISTS，重启时重复报错，容忍。
				if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(stmt)), "CREATE INDEX") {
					continue
				}
				// sqlite 最常见 SQLITE_CANTOPEN(14)：目录能进但写不了，判据+解法一起打。
				hint := ""
				if curDialect == dialectSQLite {
					hint = dbPermHint(sqliteDir)
				}
				fmt.Fprintf(os.Stderr, "[db] 初始化失败: %v\n  语句: %s\n%s", err, stmt, hint)
				os.Exit(1)
			}
		}
		// 老 sqlite 库的列迁移。新库/mysql/pg 不需要（跑了也只是报错忽略），只在 sqlite 上做。
		if curDialect == dialectSQLite {
			_, _ = conn.Exec(`ALTER TABLE requests ADD COLUMN upstream_model TEXT`)
			_, _ = conn.Exec(`ALTER TABLE requests ADD COLUMN account_id INTEGER`)
			_, _ = conn.Exec(`ALTER TABLE requests ADD COLUMN account_label TEXT`)
			_, _ = conn.Exec(`ALTER TABLE accounts ADD COLUMN proxy_id INTEGER NOT NULL DEFAULT 0`)
			_, _ = conn.Exec(`ALTER TABLE requests DROP COLUMN prompt_preview`)
			_, _ = conn.Exec(`ALTER TABLE requests DROP COLUMN response_preview`)
		}
		db = conn
		dbw = &dbx{conn}
		logf("[db] opened dialect=%d driver=%s", curDialect, driver)
	})
	return dbw
}

// Request rows ───────────────────────────────────────────────────────────────

type RequestRow struct {
	ID            int64  `json:"id"`
	TS            int64  `json:"ts"`
	Model         string `json:"model"`
	UpstreamModel string `json:"upstream_model"`
	ProxyID       *int64 `json:"proxy_id"`
	ProxyName     string `json:"proxy_name"`
	AccountID     *int64 `json:"account_id"`
	AccountLabel  string `json:"account_label"`
	Status        int    `json:"status"`
	Error         string `json:"error"`
	TTFBMs        *int64 `json:"ttfb_ms"`
	TotalMs       int64  `json:"total_ms"`
	PromptChars   int    `json:"prompt_chars"`
	ResponseChars int    `json:"response_chars"`
	PromptTokens  int    `json:"prompt_tokens"`
	OutputTokens  int    `json:"output_tokens"`
	Endpoint      string `json:"endpoint"`
	Stream        int    `json:"stream"`
}

func insertRequest(r *RequestRow) {
	_, err := getDB().Exec(`INSERT INTO requests
        (ts, model, upstream_model, proxy_id, proxy_name, account_id, account_label,
         status, error, ttfb_ms, total_ms,
         prompt_chars, response_chars, prompt_tokens, output_tokens,
         endpoint, stream)
        VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.TS, r.Model, r.UpstreamModel, r.ProxyID, r.ProxyName, r.AccountID, r.AccountLabel,
		r.Status, r.Error, r.TTFBMs, r.TotalMs,
		r.PromptChars, r.ResponseChars, r.PromptTokens, r.OutputTokens,
		r.Endpoint, r.Stream)
	if err != nil {
		logf("[db] insert request failed: %v", err)
	}
}

// Sessions ───────────────────────────────────────────────────────────────────

func createSession(token string, ttl time.Duration) {
	now := time.Now().Unix()
	_, err := getDB().Exec(upsert("sessions", []string{"token", "created_at", "expires_at"}, []string{"token"}),
		token, now, now+int64(ttl.Seconds()))
	if err != nil {
		logf("[db] session insert failed: %v", err)
	}
}

func validSession(token string) bool {
	if token == "" {
		return false
	}
	var exp int64
	err := getDB().QueryRow(`SELECT expires_at FROM sessions WHERE token=?`, token).Scan(&exp)
	if err != nil {
		return false
	}
	return time.Now().Unix() < exp
}

// KV helpers ─────────────────────────────────────────────────────────────────

func kvGet(k string) string {
	var v string
	if err := getDB().QueryRow(`SELECT v FROM kv WHERE k=?`, k).Scan(&v); err != nil {
		return ""
	}
	return v
}

func kvSet(k, v string) error {
	_, err := getDB().Exec(upsert("kv", []string{"k", "v"}, []string{"k"}), k, v)
	return err
}

// dbPermHint 在数据库打不开时给出可操作的提示。
//
// 判据：容器镜像是 distroless + USER nonroot（uid 65532），而 bind mount 会用宿主
// 目录的属主整个盖掉镜像里 /data 的属主。宿主上目录不存在时 Docker 自动建、属主是
// root，于是容器里的 65532 写不进去 —— 实测这就是 `unable to open database file (14)`
// 的成因（同一条命令只把属主 chown 成 65532 就能起来）。
func dbPermHint(dir string) string {
	uid := os.Getuid() // Windows 上返回 -1，那边不涉及这个问题
	return fmt.Sprintf(""+
		"      当前进程 uid=%d，写不进目录 %s。\n"+
		"      Docker 部署最常见的原因是 bind mount 的宿主目录属主是 root，\n"+
		"      而镜像以 nonroot(65532) 运行。两种解法：\n"+
		"        1) 改用具名卷（推荐）：volumes 写 gw2a-data:/data\n"+
		"        2) 保留 bind mount 就把属主改过来：sudo chown -R 65532:65532 ./data\n",
		uid, dir)
}
