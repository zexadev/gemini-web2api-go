package app

import (
	"net/http"
	"strings"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
)

func TestBrowserHeadersProfileSwitch(t *testing.T) {
	old := rtCfg()
	defer func() {
		rtMu.Lock()
		rtVal = old
		rtMu.Unlock()
	}()

	// 1. Test Chrome impersonation
	rtMu.Lock()
	rtVal.Impersonate = "chrome_146"
	rtMu.Unlock()

	req, _ := http.NewRequest("GET", "https://gemini.google.com/app", nil)
	applyChromeHeaders(req)

	if req.Header.Get("User-Agent") != ChromeUA {
		t.Errorf("expected ChromeUA for chrome_146, got %s", req.Header.Get("User-Agent"))
	}
	if req.Header.Get("Sec-CH-UA") == "" {
		t.Errorf("expected Sec-CH-UA to be set for chrome_146")
	}

	reqF, _ := fhttp.NewRequest("GET", "https://gemini.google.com/app", nil)
	applyBrowserFHeaders(reqF)
	if reqF.Header.Get("User-Agent") != ChromeUA {
		t.Errorf("expected ChromeUA on fhttp for chrome_146, got %s", reqF.Header.Get("User-Agent"))
	}

	// 2. Test Firefox impersonation
	rtMu.Lock()
	rtVal.Impersonate = "firefox_147"
	rtMu.Unlock()

	reqFf, _ := http.NewRequest("GET", "https://gemini.google.com/app", nil)
	// Even if dummy Sec-CH-UA was previously set, it should be wiped
	reqFf.Header.Set("Sec-CH-UA", "dummy")
	reqFf.Header.Set("Sec-CH-UA-Mobile", "?0")
	reqFf.Header.Set("Sec-CH-UA-Platform", "Windows")
	applyChromeHeaders(reqFf)

	if reqFf.Header.Get("User-Agent") != FirefoxUA {
		t.Errorf("expected FirefoxUA for firefox_147, got %s", reqFf.Header.Get("User-Agent"))
	}
	if reqFf.Header.Get("Sec-CH-UA") != "" {
		t.Errorf("expected Sec-CH-UA to be empty for firefox, got %s", reqFf.Header.Get("Sec-CH-UA"))
	}
	if reqFf.Header.Get("Sec-CH-UA-Mobile") != "" {
		t.Errorf("expected Sec-CH-UA-Mobile to be empty for firefox")
	}
	if reqFf.Header.Get("Sec-CH-UA-Platform") != "" {
		t.Errorf("expected Sec-CH-UA-Platform to be empty for firefox")
	}

	// 3. Test Zen profile mapping
	rtMu.Lock()
	rtVal.Impersonate = "zen"
	rtMu.Unlock()
	if !isFirefoxProfile() {
		t.Errorf("expected isFirefoxProfile() to be true for 'zen'")
	}
	if currentBrowserUA() != FirefoxUA {
		t.Errorf("expected FirefoxUA for 'zen'")
	}
}

// TestFirefoxFingerprintToggle 锁住实验开关的不变量：开关是独立于 impersonate 字符串的
// 第二个来源，开着时强制 Firefox（TLS profile 和 header 两层一起），关着时还原成 impersonate。
func TestFirefoxFingerprintToggle(t *testing.T) {
	old := rtCfg()
	defer func() {
		rtMu.Lock()
		rtVal = old
		rtMu.Unlock()
	}()

	// 开关关 + impersonate=chrome_146：全程 Chrome。
	rtMu.Lock()
	rtVal.Impersonate = "chrome_146"
	rtVal.FirefoxFingerprint = false
	rtMu.Unlock()
	if isFirefoxProfile() {
		t.Error("toggle 关 + chrome_146 不该判定为 Firefox")
	}
	if got := effectiveImpersonate(); got != "chrome_146" {
		t.Errorf("toggle 关时 effectiveImpersonate 应为 chrome_146，得到 %s", got)
	}

	// 开关开：即便 impersonate 仍是 chrome_146，也强制 Firefox（TLS profile + header 两层）。
	rtMu.Lock()
	rtVal.FirefoxFingerprint = true
	rtMu.Unlock()
	if !isFirefoxProfile() {
		t.Error("toggle 开时应判定为 Firefox，即使 impersonate=chrome_146")
	}
	if got := effectiveImpersonate(); got != "firefox_147" {
		t.Errorf("toggle 开时 effectiveImpersonate 应为 firefox_147，得到 %s", got)
	}
	if currentBrowserUA() != FirefoxUA {
		t.Error("toggle 开时 currentBrowserUA 应为 FirefoxUA")
	}
	req, _ := http.NewRequest("GET", "https://gemini.google.com/app", nil)
	req.Header.Set("Sec-CH-UA", "dummy")
	applyChromeHeaders(req)
	if req.Header.Get("User-Agent") != FirefoxUA {
		t.Errorf("toggle 开时应发 FirefoxUA，得到 %s", req.Header.Get("User-Agent"))
	}
	if req.Header.Get("Sec-CH-UA") != "" {
		t.Error("toggle 开时应去掉 Sec-CH-UA")
	}
}

// Sec-Fetch-Site 按目标主机算，发起方固定是 gemini.google.com 页面。
func TestFetchSite(t *testing.T) {
	cases := map[string]string{
		"gemini.google.com":                "same-origin",
		"GEMINI.google.com":                "same-origin",
		"accounts.google.com":              "same-site",
		"geminiweb-pa.clients6.google.com": "same-site",
		"google.com":                       "same-site",
		"lh3.googleusercontent.com":        "cross-site",
		"content-push.googleapis.com":      "cross-site",
		"example.com":                      "cross-site",
		"evilgoogle.com":                   "cross-site",
	}
	for host, want := range cases {
		if got := fetchSite(host); got != want {
			t.Errorf("fetchSite(%q) = %q, want %q", host, got, want)
		}
	}
	req, _ := fhttp.NewRequest("GET", "https://lh3.googleusercontent.com/gg-dl/x", nil)
	applyBrowserFHeaders(req)
	if got := req.Header.Get("Sec-Fetch-Site"); got != "cross-site" {
		t.Errorf("下载链应是 cross-site，得到 %q", got)
	}
}

// UA / Sec-CH-UA 报的版本要跟 TLS 指纹实际用的 Chrome 版本一致。
func TestChromeVersionFollowsProfile(t *testing.T) {
	old := rtCfg()
	defer func() { rtMu.Lock(); rtVal = old; rtMu.Unlock() }()
	cases := map[string]string{"chrome_133": "133", "chrome_146": "146", "chrome_157": "146", "": "146"}
	for imp, want := range cases {
		rtMu.Lock()
		rtVal.Impersonate = imp
		rtVal.FirefoxFingerprint = false
		rtMu.Unlock()
		req, _ := http.NewRequest("GET", "https://gemini.google.com/app", nil)
		applyBrowserHeaders(req)
		if ua := req.Header.Get("User-Agent"); !strings.Contains(ua, "Chrome/"+want+".0.0.0") {
			t.Errorf("%q: UA %q 应报 Chrome/%s", imp, ua, want)
		}
		if ch := req.Header.Get("Sec-CH-UA"); !strings.Contains(ch, `"Google Chrome";v="`+want+`"`) {
			t.Errorf("%q: Sec-CH-UA %q 应报 %s", imp, ch, want)
		}
	}
}
