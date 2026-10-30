package app

import (
	"testing"
	"time"
)

// 账号序号 → 请求路径前缀：0 是裸路径（默认号），N>0 是 /u/N。
func TestGeminiBase(t *testing.T) {
	cases := map[int]string{
		0: "https://gemini.google.com",
		1: "https://gemini.google.com/u/1",
		9: "https://gemini.google.com/u/9",
	}
	for n, want := range cases {
		if got := geminiBase(n); got != want {
			t.Errorf("geminiBase(%d) = %q, want %q", n, got, want)
		}
	}
}

// 同一串 cookie 的不同号要分开缓存 XSRF（token 跟号绑定，拿错号的发出去 400）；
// 作废时同串 cookie 的所有号一起清。
func TestXSRFCachePerAuthUser(t *testing.T) {
	const c = "SAPISID=x; __Secure-1PSID=y"
	if xsrfKey(c, 0) == xsrfKey(c, 1) {
		t.Fatal("不同序号的缓存键不能相同")
	}
	xsrfMu.Lock()
	xsrfCache[xsrfKey(c, 0)] = xsrfEntry{token: "t0", fetched: time.Now()}
	xsrfCache[xsrfKey(c, 1)] = xsrfEntry{token: "t1", fetched: time.Now()}
	xsrfMu.Unlock()

	if tok, _ := getXSRF(c, 0, ""); tok != "t0" {
		t.Errorf("号 0 拿到 %q", tok)
	}
	if tok, _ := getXSRF(c, 1, ""); tok != "t1" {
		t.Errorf("号 1 拿到 %q", tok)
	}

	invalidateXSRF(c)
	xsrfMu.Lock()
	_, ok0 := xsrfCache[xsrfKey(c, 0)]
	_, ok1 := xsrfCache[xsrfKey(c, 1)]
	xsrfMu.Unlock()
	if ok0 || ok1 {
		t.Errorf("作废后应全部清掉, 还剩 0:%v 1:%v", ok0, ok1)
	}
}

// 序号 0 不动请求头；N>0 把 Referer 和 X-Goog-AuthUser 对齐到第 N 个号。
func TestSetAuthUserHeaders(t *testing.T) {
	h := buildGeminiHeaders("", "", "")
	setAuthUserHeaders(h, 0)
	if h["Referer"] != "https://gemini.google.com/app" || h["X-Goog-AuthUser"] != "0" {
		t.Errorf("序号 0 不该改头: %v / %v", h["Referer"], h["X-Goog-AuthUser"])
	}
	setAuthUserHeaders(h, 2)
	if h["Referer"] != "https://gemini.google.com/u/2/app" || h["X-Goog-AuthUser"] != "2" {
		t.Errorf("序号 2: %v / %v", h["Referer"], h["X-Goog-AuthUser"])
	}
}

// 上传 start 地址：默认号不带参数，第 N 个号带 ?authuser=N。
func TestUploadStartURL(t *testing.T) {
	if got := uploadStartURL(0); got != uploadHost {
		t.Errorf("序号 0: %q", got)
	}
	if got := uploadStartURL(3); got != uploadHost+"?authuser=3" {
		t.Errorf("序号 3: %q", got)
	}
}
