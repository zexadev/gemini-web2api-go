package app

// 多数据库方言支持：默认 sqlite，设了 SQL_DSN 环境变量则走 mysql / postgres。
//
// 为什么不是「加个 env 就行」：
//   - 占位符：sqlite/mysql 用 ?，postgres 用 $1/$2 —— 全部查询都受影响，靠 rebind 中心重写。
//   - 建表 DDL：AUTOINCREMENT / 类型 / TEXT 能不能当主键，三家都不一样 —— 靠 schema 转换。
//   - upsert：INSERT OR REPLACE(sqlite) / REPLACE INTO(mysql) / ON CONFLICT(pg) —— 靠 upsert() 生成。
// 业务代码里那 ~70 条 ? 查询一条不用改，方言差异全收敛在这个文件。

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

type dbDialect int

const (
	dialectSQLite dbDialect = iota
	dialectMySQL
	dialectPostgres
)

// curDialect 在 getDB() 里按 SQL_DSN 设定一次，之后全局只读。
var curDialect = dialectSQLite

// detectDSN 从 SQL_DSN 认方言，返回 database/sql 驱动名和连接串。
// SQL_DSN 一旦设置就必须是 mysql/pg（空则不会走到这里，仍是 sqlite）。认不出直接退出。
func detectDSN(dsn string) (dbDialect, string, string) {
	low := strings.ToLower(strings.TrimSpace(dsn))
	switch {
	case strings.HasPrefix(low, "postgres://"), strings.HasPrefix(low, "postgresql://"):
		return dialectPostgres, "postgres", dsn // lib/pq 直接吃 URL
	case strings.HasPrefix(low, "mysql://"):
		return dialectMySQL, "mysql", mysqlURLToDSN(dsn)
	case strings.Contains(dsn, "@tcp("):
		return dialectMySQL, "mysql", ensureMySQLParams(dsn) // go-sql-driver 原生 DSN
	default:
		fmt.Fprintf(os.Stderr, "[db] SQL_DSN 认不出方言（要 postgres:// 或 postgresql:// 或 mysql:// 开头，"+
			"或 go-sql-driver 的 user:pass@tcp(host:port)/db 形式）: %s\n", dsn)
		os.Exit(1)
		return dialectSQLite, "", ""
	}
}

// mysqlURLToDSN 把 mysql://user:pass@host:port/db?x=y 转成 go-sql-driver 的 user:pass@tcp(host:port)/db?x=y。
func mysqlURLToDSN(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[db] mysql DSN 解析失败: %v\n", err)
		os.Exit(1)
	}
	cred := ""
	if u.User != nil {
		cred = u.User.Username()
		if p, ok := u.User.Password(); ok {
			cred += ":" + p
		}
	}
	out := fmt.Sprintf("%s@tcp(%s)/%s", cred, u.Host, strings.TrimPrefix(u.Path, "/"))
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return ensureMySQLParams(out)
}

func ensureMySQLParams(dsn string) string {
	if !strings.Contains(dsn, "charset=") {
		if strings.Contains(dsn, "?") {
			dsn += "&charset=utf8mb4"
		} else {
			dsn += "?charset=utf8mb4"
		}
	}
	return dsn
}

// ── 占位符 shim：getDB() 返回 *dbx，业务代码照常 .Exec/.Query/.QueryRow/.Begin ──────

type dbx struct{ raw *sql.DB }

func (d *dbx) Exec(q string, a ...any) (sql.Result, error) { return d.raw.Exec(rebind(q), a...) }
func (d *dbx) Query(q string, a ...any) (*sql.Rows, error) { return d.raw.Query(rebind(q), a...) }
func (d *dbx) QueryRow(q string, a ...any) *sql.Row        { return d.raw.QueryRow(rebind(q), a...) }
func (d *dbx) Begin() (*txx, error) {
	t, err := d.raw.Begin()
	if err != nil {
		return nil, err
	}
	return &txx{t}, nil
}

type txx struct{ raw *sql.Tx }

func (t *txx) Exec(q string, a ...any) (sql.Result, error) { return t.raw.Exec(rebind(q), a...) }
func (t *txx) Query(q string, a ...any) (*sql.Rows, error) { return t.raw.Query(rebind(q), a...) }
func (t *txx) QueryRow(q string, a ...any) *sql.Row        { return t.raw.QueryRow(rebind(q), a...) }
func (t *txx) Commit() error                               { return t.raw.Commit() }
func (t *txx) Rollback() error                             { return t.raw.Rollback() }

// rebind 把 ? 顺序换成 $1,$2…（仅 postgres）。我们所有查询里都没有含 ? 的字符串字面量，顺序替换安全。
func rebind(q string) string {
	if curDialect != dialectPostgres {
		return q
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		} else {
			b.WriteByte(q[i])
		}
	}
	return b.String()
}

// insertID 执行 INSERT 并返回自增主键。pg（lib/pq）不支持 LastInsertId，改用 RETURNING id。
// query 里不要带结尾分号；主键列名固定为 id。
func insertID(query string, args ...any) (int64, error) {
	if curDialect == dialectPostgres {
		var id int64
		err := getDB().QueryRow(query+" RETURNING id", args...).Scan(&id)
		return id, err
	}
	res, err := getDB().Exec(query, args...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// upsert 生成方言相容的「插入或按主键覆盖」语句。cols 是全部列（含主键），pk 是主键列。
func upsert(table string, cols, pk []string) string {
	ph := "?" + strings.Repeat(",?", len(cols)-1)
	collist := strings.Join(cols, ", ")
	switch curDialect {
	case dialectMySQL:
		return fmt.Sprintf("REPLACE INTO %s(%s) VALUES(%s)", table, collist, ph)
	case dialectPostgres:
		pkset := map[string]bool{}
		for _, c := range pk {
			pkset[c] = true
		}
		var sets []string
		for _, c := range cols {
			if !pkset[c] {
				sets = append(sets, c+"=excluded."+c)
			}
		}
		return fmt.Sprintf("INSERT INTO %s(%s) VALUES(%s) ON CONFLICT(%s) DO UPDATE SET %s",
			table, collist, ph, strings.Join(pk, ","), strings.Join(sets, ", "))
	default: // sqlite
		return fmt.Sprintf("INSERT OR REPLACE INTO %s(%s) VALUES(%s)", table, collist, ph)
	}
}

// ── schema 按方言转换 ────────────────────────────────────────────────────────────

var (
	reAutoPK      = regexp.MustCompile(`INTEGER PRIMARY KEY AUTOINCREMENT`)
	reInteger     = regexp.MustCompile(`\bINTEGER\b`)
	reTextDefault = regexp.MustCompile(`TEXT NOT NULL DEFAULT ('[^']*')`)
	reModelText   = regexp.MustCompile(`model\s+TEXT NOT NULL`) // model 进了复合主键/索引，mysql 里 TEXT 不能当键
	reIdxIfExists = regexp.MustCompile(`(?i)CREATE INDEX IF NOT EXISTS`)
)

// schemaStatements 把 sqlite 版 schema 转成目标方言、切成单条语句（mysql/pg 驱动默认不支持多语句 Exec）。
func schemaStatements(d dbDialect) []string {
	s := schema
	switch d {
	case dialectMySQL:
		s = reAutoPK.ReplaceAllString(s, "BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY")
		// mysql 的 TEXT 不能直接做主键/索引/带 DEFAULT，短文本列改 VARCHAR。
		s = strings.ReplaceAll(s, "TEXT PRIMARY KEY", "VARCHAR(255) PRIMARY KEY")
		s = reModelText.ReplaceAllString(s, "model VARCHAR(255) NOT NULL")
		s = reTextDefault.ReplaceAllString(s, "VARCHAR(512) NOT NULL DEFAULT $1")
		s = reInteger.ReplaceAllString(s, "BIGINT")
		s = reIdxIfExists.ReplaceAllString(s, "CREATE INDEX") // mysql 不支持 CREATE INDEX IF NOT EXISTS
	case dialectPostgres:
		s = reAutoPK.ReplaceAllString(s, "BIGSERIAL PRIMARY KEY")
		s = reInteger.ReplaceAllString(s, "BIGINT")
	}
	// 先逐行剥掉 -- 行内注释再按 ; 切：注释里可能带分号（如 cookie 串 "k=v; k=v"），
	// 直接 split 会把语句从注释中间切断。本 schema 的字符串字面量里没有 --，直接截安全。
	var clean strings.Builder
	for _, ln := range strings.Split(s, "\n") {
		if i := strings.Index(ln, "--"); i >= 0 {
			ln = ln[:i]
		}
		clean.WriteString(ln)
		clean.WriteByte('\n')
	}
	var out []string
	for _, part := range strings.Split(clean.String(), ";") {
		if stmt := strings.TrimSpace(part); stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}
