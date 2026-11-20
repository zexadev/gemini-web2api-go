package app

import (
	"net/http"
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
