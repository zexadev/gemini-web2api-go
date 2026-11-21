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

	fhttp "github.com/bogdanfinn/fhttp"
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

// getTLSClient returns a tls-client for direct connections only; its fingerprint follows
// effectiveImpersonate() (default Chrome 146, Firefox_147 when the firefox toggle is on).
// For proxied connections we use stdlib (see getStdlibClient) — tls-client's SOCKS5
// implementation is fragile compared to net/http.
func getTLSClient() tls_client.HttpClient {
	imp := effectiveImpersonate()
	// 缓存 key 带上超时：超时是建客户端时定死的，面板改了 request_timeout_sec
	// 要换一个新客户端才生效（只按 imp 缓存时要重启进程才生效）。
	timeout := rtCfg().RequestTimeout
	key := fmt.Sprintf("%s|%d", imp, timeout)
	clientCacheMu.RLock()
	if c, ok := clientCache[key]; ok {
		clientCacheMu.RUnlock()
		return c
	}
	clientCacheMu.RUnlock()

	clientCacheMu.Lock()
	defer clientCacheMu.Unlock()
	if c, ok := clientCache[key]; ok {
		return c
	}
	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(timeout),
		tls_client.WithClientProfile(resolveProfile(imp)),
		tls_client.WithNotFollowRedirects(),
	}
	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[client] tls-client init failed: %v\n", err)
		os.Exit(1)
	}
	clientCache[key] = client
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
	stdlibClientCache sync.Map // proxyURL|超时秒数 -> *http.Client
)

// getStdlibClient returns an http.Client routed through proxyURL.
// proxyURL must not be empty (caller checks).
func getStdlibClient(proxyURL string) *http.Client {
	// 同 getTLSClient：超时进 key，面板改超时后换新客户端。
	timeout := rtCfg().RequestTimeout
	key := fmt.Sprintf("%s|%d", proxyURL, timeout)
	if cached, ok := stdlibClientCache.Load(key); ok {
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
		Timeout:   time.Duration(timeout) * time.Second,
		Transport: t,
		// 跟 tls-client 一致：不自动跟随重定向（302 是诊断信号）
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	stdlibClientCache.Store(key, c)
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

// FirefoxUA 是走 Firefox/Zen 指纹时的 UA 模板。版本号对齐底层 TLS profile（tls-client 的
// Firefox_147），不报更高的 157——UA 报的大版本跟 TLS 握手指纹对不上反而更可疑。
const FirefoxUA = "Mozilla/5.0 (X11; Linux x86_64; rv:147.0) Gecko/20100101 Firefox/147.0"

// isFirefoxProfile 判断这次该不该走 Firefox 指纹。两个来源，任一成立即是：
//  1. firefox_fingerprint 实验开关打开（设置页里显式 opt-in，见 config.go）；
//  2. impersonate 字符串本身就填成了 firefox*/zen（老用法，向后兼容）。
//
// TLS profile（getTLSClient→effectiveImpersonate）和应用层 header（applyBrowserHeaders）
// 认的是同一个判据，两层不会错位成「Firefox UA + Chrome 握手」。
func isFirefoxProfile() bool {
	if rtCfg().FirefoxFingerprint {
		return true
	}
	p := strings.ToLower(rtCfg().Impersonate)
	return strings.HasPrefix(p, "firefox") || p == "zen"
}

// effectiveImpersonate 是真正拿去 resolveProfile 的指纹名。开着 Firefox 实验开关时强制
// firefox_147（跟 FirefoxUA 的大版本对齐），否则用用户配的 impersonate（默认 chrome_146）。
func effectiveImpersonate() string {
	if rtCfg().FirefoxFingerprint {
		return "firefox_147"
	}
	return rtCfg().Impersonate
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

// chromeClientHints 是 Chrome 指纹下要带的三个 Sec-CH-UA 头；Firefox 不发这组。
func chromeClientHints() map[string]string {
	return map[string]string{
		"Sec-CH-UA":          `"Chromium";v="146", "Google Chrome";v="146", "Not?A_Brand";v="24"`,
		"Sec-CH-UA-Mobile":   "?0",
		"Sec-CH-UA-Platform": `"Windows"`,
	}
}

// fetchSite 按目标主机算 Sec-Fetch-Site：所有请求都假定由 gemini.google.com 页面发起。
// 浏览器的规则是同源 same-origin、同一可注册域（google.com）same-site、其余 cross-site。
// 以前一律写 same-origin，发往 accounts / clients6（同站）和 lh3.googleusercontent.com、
// content-push.googleapis.com、用户给的外部图片（跨站）的请求都跟真浏览器对不上。
func fetchSite(host string) string {
	host = strings.ToLower(host)
	switch {
	case host == "gemini.google.com":
		return "same-origin"
	case host == "google.com" || strings.HasSuffix(host, ".google.com"):
		return "same-site"
	default:
		return "cross-site"
	}
}

// writeBrowserHeaders 是浏览器应用层 header 的唯一来源，stdlib 和 fhttp 两种请求都经它写入，
// 两条传输路径不会再各写一份、改一处漏一处。host 是请求的目标主机，用来算 Sec-Fetch-Site。
func writeBrowserHeaders(host string, set func(k, v string), del func(k string)) {
	set("User-Agent", currentBrowserUA())
	if isFirefoxProfile() {
		for k := range chromeClientHints() {
			del(k)
		}
	} else {
		for k, v := range chromeClientHints() {
			set(k, v)
		}
	}
	set("Sec-Fetch-Dest", "empty")
	set("Sec-Fetch-Mode", "cors")
	set("Sec-Fetch-Site", fetchSite(host))
}

// applyBrowserHeaders 给 stdlib 请求填上对应 impersonate 配置的应用层 header。
func applyBrowserHeaders(req *http.Request) {
	writeBrowserHeaders(req.URL.Hostname(), req.Header.Set, req.Header.Del)
}

// applyBrowserFHeaders 给 fhttp (tls-client 直连) 请求填上对应 impersonate 配置的应用层 header。
func applyBrowserFHeaders(req *fhttp.Request) {
	writeBrowserHeaders(req.URL.Hostname(), req.Header.Set, req.Header.Del)
}
