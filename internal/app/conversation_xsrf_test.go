package app

import (
	"errors"
	"io"
	"testing"
)

func TestConversationXSRFUsesRotatedCookieOnlyForSameSession(t *testing.T) {
	const old = "SAPISID=account; __Secure-1PSID=session; __Secure-1PSIDTS=old; OSID=present"
	const rotated = "SAPISID=account; __Secure-1PSID=session; __Secure-1PSIDTS=new; OSID=present"
	signedOut := errors.New("no SNlM0e in page (cookie expired or not signed in)")
	proxy := Proxy{ID: 1, URL: "http://same-egress.invalid"}

	for _, tt := range []struct {
		name   string
		cookie string
		first  error
		change func(*convState)
		retry  bool
	}{
		{name: "rotated credentials", cookie: rotated, first: signedOut, retry: true},
		{name: "valid old credentials", cookie: rotated},
		{name: "no rotation", cookie: old, first: signedOut},
		{name: "transport failure", cookie: rotated, first: io.EOF},
		{name: "changed identity", cookie: "SAPISID=account; __Secure-1PSID=another; OSID=present", first: signedOut},
		{name: "changed authuser", cookie: rotated, first: signedOut, change: func(c *convState) { c.authuser = 2 }},
		{name: "changed exit", cookie: rotated, first: signedOut, change: func(c *convState) { c.proxyURL = "http://other.invalid" }},
		{name: "first turn", cookie: rotated, first: signedOut, change: func(c *convState) { c.turn = 0 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			id, err := accountAdd("xsrf-test", tt.cookie, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = accountDelete(id) })
			conv := &convState{cookie: old, accountID: id, isLogin: true, turn: 1, cid: "original-conversation", proxyID: proxy.ID, proxyURL: proxy.URL}
			if tt.change != nil {
				tt.change(conv)
			}
			calls := 0
			token, err := conversationXSRF(conv, proxy, func(cookie string, authuser int, proxyURL string) (string, error) {
				calls++
				if authuser != conv.authuser || proxyURL != proxy.URL {
					t.Fatal("authentication request changed account index or egress")
				}
				if calls == 1 && cookie == old {
					return "old-token", tt.first
				}
				if calls == 2 && cookie == rotated {
					return "refreshed-token", nil
				}
				t.Fatal("unexpected authentication attempt")
				return "", nil
			})
			if conv.cid != "original-conversation" {
				t.Fatal("authentication changed the upstream conversation")
			}
			if tt.retry {
				if err != nil || token != "refreshed-token" || calls != 2 || conv.cookie != rotated || conv.sapisid != "account" {
					t.Fatalf("expected refreshed credentials after two attempts, got %d attempts and error %v", calls, err)
				}
			} else if err != tt.first || calls != 1 || conv.cookie != old {
				t.Fatalf("expected the first result and unchanged credentials, got %d attempts and error %v", calls, err)
			}
		})
	}
}
