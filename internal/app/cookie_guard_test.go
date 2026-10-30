package app

import (
	"errors"
	"fmt"
	"io"
	"testing"
)

// 连续鉴权失败要自动停用，否则死号会一直排在挑号队列里，
// 每个请求都得先为它付一次 XSRF 往返才轮到报错。
func TestAutoDisableAfterAuthFailures(t *testing.T) {
	id, err := accountAdd("dead", "SAPISID=aaa; __Secure-1PSID=bbb", "")
	if err != nil {
		t.Fatalf("插账号失败: %v", err)
	}
	t.Cleanup(func() { _ = accountDelete(id) })

	for i := 1; i < maxCookieAuthFailures; i++ {
		markCookieByStatus(id, 401, "no SNlM0e")
		if a := accountByID(id); a == nil || a.Status != "enabled" {
			t.Fatalf("第 %d 次失败就被停用了，太早", i)
		}
	}
	markCookieByStatus(id, 401, "no SNlM0e")
	if a := accountByID(id); a == nil || a.Status != "disabled" {
		t.Errorf("连续 %d 次鉴权失败后应自动停用, got %+v", maxCookieAuthFailures, a)
	}
}

// 非鉴权类失败不该累加 fail_count，更不该导致停用 ——
// 住宅出口退化率很高，把 302/网络错误算进去会让好 cookie 被误伤成"失败最多"。
func TestNonAuthFailureDoesNotDisable(t *testing.T) {
	id, err := accountAdd("noisy", "SAPISID=ccc; __Secure-1PSID=ddd", "")
	if err != nil {
		t.Fatalf("插账号失败: %v", err)
	}
	t.Cleanup(func() { _ = accountDelete(id) })

	for i := 0; i < maxCookieAuthFailures+2; i++ {
		markCookieByStatus(id, 302, "sorry page")
		markCookieByStatus(id, 0, "network error")
	}
	a := accountByID(id)
	if a == nil || a.Status != "enabled" || a.FailCount != 0 {
		t.Errorf("非鉴权失败不该动健康度, got %+v", a)
	}
}

// 成功要清零，否则偶发失败会累积到停用阈值。
func TestSuccessResetsFailCount(t *testing.T) {
	id, err := accountAdd("flappy", "SAPISID=eee; __Secure-1PSID=fff", "")
	if err != nil {
		t.Fatalf("插账号失败: %v", err)
	}
	t.Cleanup(func() { _ = accountDelete(id) })

	markCookieByStatus(id, 401, "x")
	markCookieByStatus(id, 200, "")
	markCookieByStatus(id, 401, "x")
	markCookieByStatus(id, 401, "x")
	if a := accountByID(id); a == nil || a.Status != "enabled" {
		t.Errorf("中间成功过就该清零，不应停用, got %+v", a)
	}
}

// 刷新结果的身份必须跟库里那份一致才写回。
// 不校验的话，上游若在响应里换了整套会话，A 号的凭据会被静默写进 B 号那一行 ——
// 面板上显示的还是原标签，实际发出去的却是别人的会话。
func TestUpdateAccountCookieIdentityGuard(t *testing.T) {
	orig := "SAPISID=keep; __Secure-1PSID=same; SIDCC=old"
	id, err := accountAdd("guarded", orig, "")
	if err != nil {
		t.Fatalf("插账号失败: %v", err)
	}
	t.Cleanup(func() { _ = accountDelete(id) })

	// 只刷新 SIDCC：身份没变，应该写进去
	refreshed := "SAPISID=keep; __Secure-1PSID=same; SIDCC=new"
	updateAccountCookie(id, refreshed)
	if a := accountByID(id); a == nil || a.Cookie != refreshed {
		t.Errorf("同身份的刷新应写回, got %+v", a)
	}

	// 换了 SAPISID：不是同一个账号了，必须丢弃
	updateAccountCookie(id, "SAPISID=other; __Secure-1PSID=same; SIDCC=x")
	if a := accountByID(id); a == nil || a.Cookie != refreshed {
		t.Errorf("换了 SAPISID 应丢弃不写, got cookie=%q", a.Cookie)
	}

	// 换了 __Secure-1PSID：同理
	updateAccountCookie(id, "SAPISID=keep; __Secure-1PSID=other; SIDCC=x")
	if a := accountByID(id); a == nil || a.Cookie != refreshed {
		t.Errorf("换了 __Secure-1PSID 应丢弃不写, got cookie=%q", a.Cookie)
	}
}

// 取 XSRF 失败只有 cookie 自己的问题才计数。之前任何错误都当 401 记，
// 断网时 GET /app 连着 3 次 EOF，好号就被自动停用了。
func TestXSRFErrBlamesCookie(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"无 SNlM0e", fmt.Errorf("no SNlM0e in page (cookie expired or not signed in)"), true},
		{"401", newAppPageStatusError(401, ""), true},
		{"403", newAppPageStatusError(403, ""), true},
		{"302 跳登录", newAppPageStatusError(302, "https://accounts.google.com/ServiceLogin?continue=x"), true},
		{"302 跳 sorry", newAppPageStatusError(302, "https://www.google.com/sorry/index?continue=x&q=y"), false},
		{"302 无 Location", newAppPageStatusError(302, ""), false},
		{"429", newAppPageStatusError(429, ""), false},
		{"502", newAppPageStatusError(502, ""), false},
		{"EOF", fmt.Errorf(`Get "https://gemini.google.com/app": %w`, io.EOF), false},
		{"包一层的 sorry", fmt.Errorf("wrap: %w", newAppPageStatusError(302, "https://www.google.com/sorry/")), false},
		{"包一层的登录", fmt.Errorf("wrap: %w", newAppPageStatusError(302, "https://accounts.google.com/v3/signin")), true},
		{"nil", nil, false},
		{"其它", errors.New("proxy connect failed"), false},
	}
	for _, c := range cases {
		if got := xsrfErrBlamesCookie(c.err); got != c.want {
			t.Errorf("%s: got %v want %v (err=%v)", c.name, got, c.want, c.err)
		}
	}
}

// 错误文本要保持 "fetch /app: HTTP <code>" 前缀，别处按子串判 "HTTP 302"/"HTTP 429"。
func TestAppPageStatusErrorText(t *testing.T) {
	if s := newAppPageStatusError(302, "https://www.google.com/sorry/index?continue=x").Error(); s != "fetch /app: HTTP 302 -> www.google.com/sorry/index" {
		t.Errorf("got %q", s)
	}
	if s := newAppPageStatusError(429, "").Error(); s != "fetch /app: HTTP 429" {
		t.Errorf("got %q", s)
	}
}
