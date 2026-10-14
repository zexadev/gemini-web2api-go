package app

import (
	"net/http"
	"testing"
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
