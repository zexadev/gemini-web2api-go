package app

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

var (
	clientCache   = map[string]tls_client.HttpClient{}
	clientCacheMu sync.RWMutex
)

// resolveProfile maps a config string to a tls-client ClientProfile.
// Defaults to Chrome_146 (matches Python v2's curl_cffi chrome146).
func resolveProfile(name string) profiles.ClientProfile {
	switch strings.ToLower(name) {
	case "chrome_120":
		return profiles.Chrome_120
	case "chrome_124":
		return profiles.Chrome_124
	case "chrome_131":
		return profiles.Chrome_131
	case "chrome_133":
		return profiles.Chrome_133
	case "chrome_144":
		return profiles.Chrome_144
	case "chrome_146", "chrome146":
		return profiles.Chrome_146
	case "firefox_120":
		return profiles.Firefox_120
	case "firefox_123":
		return profiles.Firefox_123
	case "firefox", "firefox_147", "firefox147", "firefox_157", "zen":
		return profiles.Firefox_147
	case "safari_16_0":
		return profiles.Safari_16_0
	case "safari_ios_17_0":
		return profiles.Safari_IOS_17_0
	default:
		return profiles.Chrome_146
	}
}

// getTLSClient returns a tls-client (Chrome 146 fingerprint) for direct connections only.
// For proxied connections we use stdlib (see getStdlibClient) — tls-client's SOCKS5
// implementation is fragile compared to net/http.
func getTLSClient() tls_client.HttpClient {
	clientCacheMu.RLock()
	if c, ok := clientCache[rtCfg().Impersonate]; ok {
		clientCacheMu.RUnlock()
		return c
	}
	clientCacheMu.RUnlock()

	clientCacheMu.Lock()
	defer clientCacheMu.Unlock()
	if c, ok := clientCache[rtCfg().Impersonate]; ok {
		return c
	}
	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(rtCfg().RequestTimeout),
		tls_client.WithClientProfile(resolveProfile(rtCfg().Impersonate)),
		tls_client.WithNotFollowRedirects(),
	}
	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[client] tls-client init failed: %v\n", err)
		os.Exit(1)
	}
	clientCache[rtCfg().Impersonate] = client
	return client
}

// ─── Stdlib HTTP client for proxied requests ─────────────────────────────────
//
// 走代理时改用 stdlib 而不是 tls-client，因为：
// 1. stdlib 的 http.ProxyURL 原生支持 socks5:// / socks5h:// / http:// / https://，
//    已知能过我们手头的代理。
// 2. tls-client 自带的 SOCKS 实现在某些代理端会 EOF（已实测）。
//
// 代价要写清楚：走代理时 Google 看到的是**我们自己的 TLS 指纹**，即 Go 标准库的，
// 不是 Chrome 146 的。HTTP CONNECT 只建隧道，TLS 是我们跟目标端到端握的。
// JA3 回显实测：stdlib 直连和过代理是同一个 JA3（03117a8e…），而同一个代理下换成
// tls-client 就变成另一个（2d25c563…）——两条判据都说明代理不介入 TLS。
//
// 但这不构成换回 tls-client 的理由：同时起跑的对照里 tls-client Chrome_146 打到
// 111 次被拦、Go stdlib 103 次，差 8 次；而同配置换个出口的方差能到 36%
// （151 vs 111）。指纹差异真实存在，对封禁阈值没有可观测影响。
//
// 不走代理时仍然用 getTLSClient（保留 utls/chrome146 真指纹优势）。

var (
	stdlibClientCache sync.Map // proxyURL -> *http.Client
)

// getStdlibClient returns an http.Client routed through proxyURL.
// proxyURL must not be empty (caller checks).
func getStdlibClient(proxyURL string) *http.Client {
	if cached, ok := stdlibClientCache.Load(proxyURL); ok {
		return cached.(*http.Client)
	}
	t := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
		// 走代理时不强求 HTTP/2，部分代理不支持。
		ForceAttemptHTTP2: false,
	}
	if u, err := url.Parse(proxyURL); err == nil {
		t.Proxy = http.ProxyURL(u)
	}
	c := &http.Client{
		Timeout:   time.Duration(rtCfg().RequestTimeout) * time.Second,
		Transport: t,
		// 跟 tls-client 一致：不自动跟随重定向（302 是诊断信号）
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	stdlibClientCache.Store(proxyURL, c)
	return c
}

// loadCookie reads the cookie file (Netscape one-line format or JSON).
// loadCookie 返回 (cookie 串, SAPISID)。只有旁路探测在用 —— 正式请求那条路
// 在 streamGenerate 里自己挑号，因为它还要挨个换号和绑定出口。
//
// cookie 只有 cookie 池一个来源：挑一个 enabled 账号（最久未用优先，自动轮转
// 分散单 IP 上限）。池空 = 匿名。原来那条「池空回落单 cookie」的路径已经取消，
// 它的值在启动时被 seedCookiesFromConfig 并进池子了。
//
// 第三个返回值是池里那条记录的 ID，请求结束后拿它调 markCookieByStatus 回写健康度。
func loadCookie() (cookie, sapisid string) {
	if a, ok := pickCookieAccount(); ok {
		return a.Cookie, extractSAPISID(a.Cookie)
	}
	return "", ""
}

func makeSAPISIDHash(sapisid string) string {
	ts := time.Now().Unix()
	h := sha1.Sum([]byte(fmt.Sprintf("%d %s https://gemini.google.com", ts, sapisid)))
	return fmt.Sprintf("SAPISIDHASH %d_%s", ts, hex.EncodeToString(h[:]))
}

// ChromeUA 是给 stdlib 走代理时用的 Chrome 146 真实 UA 模板。
const ChromeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

// FirefoxUA 是给 stdlib / tls-client 走 Firefox/Zen 指纹时的 UA 模板（对齐 Linux x86_64 Zen/Firefox Gecko 157）。
const FirefoxUA = "Mozilla/5.0 (X11; Linux x86_64; rv:157.0) Gecko/20100101 Firefox/157.0"

func isFirefoxProfile() bool {
	p := strings.ToLower(rtCfg().Impersonate)
	return strings.HasPrefix(p, "firefox") || p == "zen"
}

func currentBrowserUA() string {
	if isFirefoxProfile() {
		return FirefoxUA
	}
	return ChromeUA
}

// applyChromeHeaders 给 stdlib 请求填上应用层 header（根据 impersonate 配置自动适配 Chrome 或 Firefox）。
func applyChromeHeaders(req *http.Request) {
	applyBrowserHeaders(req)
}

// applyBrowserHeaders 给 stdlib 请求填上对应 impersonate 配置的应用层 header。
func applyBrowserHeaders(req *http.Request) {
	if isFirefoxProfile() {
		req.Header.Set("User-Agent", FirefoxUA)
		req.Header.Del("Sec-CH-UA")
		req.Header.Del("Sec-CH-UA-Mobile")
		req.Header.Del("Sec-CH-UA-Platform")
		req.Header.Set("Sec-Fetch-Dest", "empty")
		req.Header.Set("Sec-Fetch-Mode", "cors")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		return
	}
	req.Header.Set("User-Agent", ChromeUA)
	req.Header.Set("Sec-CH-UA", `"Chromium";v="146", "Google Chrome";v="146", "Not?A_Brand";v="24"`)
	req.Header.Set("Sec-CH-UA-Mobile", "?0")
	req.Header.Set("Sec-CH-UA-Platform", `"Windows"`)
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
}
