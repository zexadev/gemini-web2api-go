package app

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
)

// 两种 http 客户端在「响应头没等到」和「读 body 读到一半」两处超时，都要认成上游超时；
// 新协议的超时靠 errUpstreamTimeout 包装；别的失败不算。
func TestIsUpstreamTimeout(t *testing.T) {
	slowHead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
	}))
	defer slowHead.Close()
	slowBody := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("first\n"))
		w.(http.Flusher).Flush()
		time.Sleep(3 * time.Second)
	}))
	defer slowBody.Close()

	std := &http.Client{Timeout: time.Second}
	tc, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), tls_client.WithTimeoutSeconds(1))
	if err != nil {
		t.Fatal(err)
	}
	stdGet := func(u string) error {
		resp, err := std.Get(u)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _, err = readBody(resp.Body, nil, time.Now())
		return err
	}
	tlsGet := func(u string) error {
		req, _ := fhttp.NewRequest("GET", u, nil)
		resp, err := tc.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _, err = readBody(resp.Body, nil, time.Now())
		return err
	}
	for name, get := range map[string]func(string) error{"stdlib": stdGet, "tls-client": tlsGet} {
		for where, u := range map[string]string{"head": slowHead.URL, "body": slowBody.URL} {
			err := get(u)
			if !isUpstreamTimeout(err) {
				t.Errorf("%s 在 %s 超时没认出来: %T %v", name, where, err, err)
			}
		}
	}

	if !isUpstreamTimeout(fmt.Errorf("processSession 超时（3m0s）: %w", errUpstreamTimeout)) {
		t.Error("新协议超时没认出来")
	}
	for _, e := range []error{nil, errUpstreamFailureReply, errors.New("EOF"),
		fmt.Errorf("upstream returned no content frame (raw 216 bytes)")} {
		if isUpstreamTimeout(e) {
			t.Errorf("不该算超时: %v", e)
		}
	}
	// 连不上（拒绝连接）不是超时
	slowHead.Close()
	if err := stdGet(slowHead.URL); isUpstreamTimeout(err) {
		t.Errorf("拒绝连接不该算超时: %v", err)
	}
}
