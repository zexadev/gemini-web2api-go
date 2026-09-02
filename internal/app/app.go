package app

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

func logf(format string, args ...interface{}) {
	if rtCfg().LogRequests {
		fmt.Fprintf(os.Stderr, "[%s] %s\n",
			time.Now().Format("15:04:05"),
			fmt.Sprintf(format, args...))
	}
}

// maskKey shows "sk-gemini-XXXX...YYYY" for banner. Empty stays empty.
func maskKey(k string) string {
	if k == "" {
		return "(disabled)"
	}
	if len(k) <= 12 {
		return "****"
	}
	return k[:12] + "..." + k[len(k)-4:]
}

// Run 是进程入口，由根目录的 main.go 调用。
func Run() {
	port := flag.Int("port", 0, "listening port (default: 8083 or config.json)")
	configPath := flag.String("config", "", "path to config.json")
	cookieFile := flag.String("cookie-file", "", "path to cookie file")
	proxy := flag.String("proxy", "", "HTTP proxy fallback when proxy pool is empty")
	impersonate := flag.String("impersonate", "", "TLS profile (chrome_146, chrome_133, firefox_147, ...)")
	dbPath := flag.String("db", "", "SQLite path (default: ./data/gemini.db)")
	adminToken := flag.String("admin-token", "", "admin token (empty = no auth, only safe on 127.0.0.1)")
	apiKey := flag.String("api-key", "", "API key for /v1/* (locked). Empty = use kv-table key (auto-gen on first boot, mutable from admin UI)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("gemini-web2api-go %s\n", Version)
		return
	}

	if err := loadConfig(*configPath); err != nil {
		fmt.Fprintf(os.Stderr, "config load error: %v\n", err)
		os.Exit(1)
	}
	if *port > 0 {
		cfg.Port = *port
	}
	if *cookieFile != "" {
		cfg.CookieFile = *cookieFile
	}
	if *proxy != "" {
		cfg.Proxy = *proxy
	}
	if *impersonate != "" {
		cfg.Impersonate = *impersonate
	}
	if *dbPath != "" {
		cfg.DBPath = *dbPath
	}
	if *adminToken != "" {
		cfg.AdminToken = *adminToken
	}
	if envTok := os.Getenv("ADMIN_TOKEN"); envTok != "" && cfg.AdminToken == "" {
		cfg.AdminToken = envTok
	}

	// boot DB and load proxy pool
	getDB()
	initRuntimeConfig() // 面板改过的运行时配置盖在启动配置之上
	loadProxies()
	seedProxiesFromConfig() // --proxy / 遗留静态代理并进代理池
	seedCookiesFromConfig() // --cookie-file / 遗留单 cookie 并进 cookie 池
	resolvedAPIKey := initAPIKey(*apiKey)
	initTokenizer()
	startScheduler()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", requireAPIKey(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "OPTIONS":
			handleOptions(w, r)
		case "GET":
			handleModels(w, r)
		default:
			writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		}
	}))
	mux.HandleFunc("/v1/chat/completions", requireAPIKey(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "OPTIONS":
			handleOptions(w, r)
		case "POST":
			handleChatCompletions(w, r)
		default:
			writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		}
	}))
	mux.HandleFunc("/v1/responses", requireAPIKey(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "OPTIONS":
			handleOptions(w, r)
		case "POST":
			handleResponses(w, r)
		default:
			writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		}
	}))
	// /v1/videos —— OpenAI(Sora) 形状的异步视频生成。POST 建任务，GET 轮询，GET .../content 下 MP4。
	mux.HandleFunc("/v1/videos", requireAPIKey(handleCreateVideo))
	mux.HandleFunc("/v1/videos/", requireAPIKey(handleVideoItem))
	// MCP over HTTP（Streamable HTTP）：跟 OpenAI 接口同进程同端口，暴露 web_search。
	// 用同一把 API key 鉴权，客户端配 Authorization: Bearer <key> 连这个 URL。
	mux.HandleFunc("/mcp", requireAPIKey(handleMCPHTTP))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			handleRoot(w, r)
		} else {
			writeJSON(w, 404, map[string]string{"error": "not found"})
		}
	})

	// ─── Admin UI + API ────────────────────────────────────────────────
	if cfg.AdminEnabled {
		mux.HandleFunc("/admin", handleAdminUI)
		mux.HandleFunc("/admin/", handleAdminUI)
		mux.HandleFunc("/admin/api/login", handleAdminLogin)
		mux.HandleFunc("/admin/api/logout", handleAdminLogout)
		mux.HandleFunc("/admin/api/stats", requireAuth(handleAdminStats))
		mux.HandleFunc("/admin/api/timeseries", requireAuth(handleAdminTimeseries))
		mux.HandleFunc("/admin/api/requests", requireAuth(handleAdminRequests))
		mux.HandleFunc("/admin/api/proxies", requireAuth(handleAdminProxies))
		mux.HandleFunc("/admin/api/proxies/", requireAuth(handleAdminProxyItem))
		mux.HandleFunc("/admin/api/apikey", requireAuth(handleAdminAPIKey))
		mux.HandleFunc("/admin/api/usage", requireAuth(handleAdminUsage))
		mux.HandleFunc("/admin/api/config", requireAuth(handleAdminConfig))
		mux.HandleFunc("/admin/api/cookies", requireAuth(handleAdminCookies))
		mux.HandleFunc("/admin/api/cookies/", requireAuth(handleAdminCookieItem))
		mux.HandleFunc("/admin/api/test", requireAuth(handleAdminTest))
	}

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // streaming responses can be long
	}

	cookieStatus := "none (anonymous)"
	if total, enabled := accountCount(); enabled > 0 {
		cookieStatus = fmt.Sprintf("%d/%d in pool", enabled, total)
	}
	proxyStatus := "none (direct)"
	if n := len(listProxies()); n > 0 {
		proxyStatus = fmt.Sprintf("%d in pool", n)
	}
	var modelNames []string
	for n := range availableModels() {
		modelNames = append(modelNames, n)
	}
	sort.Strings(modelNames)

	fmt.Printf("gemini-web2api-go v%s\n", Version)
	fmt.Printf("  Listening:   http://%s\n", addr)
	fmt.Printf("  Base URL:    http://localhost:%d/v1\n", cfg.Port)
	fmt.Printf("  API key:     %s%s\n", maskKey(resolvedAPIKey),
		map[bool]string{true: "  (locked by flag/env)", false: "  (mutable in admin UI)"}[apiKeyLocked])
	if cfg.AdminEnabled {
		authMode := "no auth (open)"
		if cfg.AdminToken != "" {
			authMode = "token auth"
		}
		fmt.Printf("  Admin UI:    http://localhost:%d/admin  (%s)\n", cfg.Port, authMode)
	}
	fmt.Printf("  DB:          %s\n", cfg.DBPath)
	fmt.Printf("  Models:      %v\n", modelNames)
	fmt.Printf("  Cookie:      %s\n", cookieStatus)
	fmt.Printf("  Proxy:       %s\n", proxyStatus)
	fmt.Printf("  Impersonate: %s\n", rtCfg().Impersonate)
	tokInfo := "chars/4 (fallback)"
	if tokenizerOK {
		tokInfo = "tiktoken cl100k_base"
	}
	fmt.Printf("  Tokenizer:   %s\n", tokInfo)
	fmt.Printf("  Per-IP 限流: 并发=%d / RPM=%d / RPH=%d\n",
		rtCfg().PerIPConcurrent, rtCfg().PerIPRPM, rtCfg().PerIPRPH)
	fmt.Printf("  Retry:       %dx / %ds\n", rtCfg().RetryAttempts, rtCfg().RetryDelaySec)
	fmt.Println()
	warnEnvProxyIgnored()

	if err := server.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}

// warnEnvProxyIgnored 在设了代理环境变量、但代理池是空的时候提醒一句。
//
// Linux 上的惯例是 HTTP 客户端自动读 HTTP_PROXY / ALL_PROXY（Go 标准库有现成的
// http.ProxyFromEnvironment），我们用的是显式 http.ProxyURL，一个都不读。这是
// 有意的：宿主机上随手一个 export 会悄悄改变出口 IP，而面板仍显示直连，排查时
// 会被带偏。但"没生效且毫无反馈"同样难查 —— 有用户按 systemd drop-in 的常规做法
// 注入了这几个变量，折腾很久才发现要在面板配。所以不读归不读，得说一声。
func warnEnvProxyIgnored() {
	// 大小写两种写法都查，但只报一个名字：Windows 的环境变量大小写不敏感，
	// 全列出来会变成 "HTTPS_PROXY / https_proxy" 这种看着像两个变量的噪音。
	var set []string
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		if os.Getenv(k) != "" || os.Getenv(strings.ToLower(k)) != "" {
			set = append(set, k)
		}
	}
	if len(set) == 0 || len(listProxies()) > 0 {
		return
	}
	fmt.Printf("  ⚠ 检测到代理环境变量 %s，但本程序**不读**它们，当前走直连。\n"+
		"    请在面板「代理池」添加，或用 --proxy 启动参数（它会在启动时导入代理池）。\n\n",
		strings.Join(set, " / "))
}
