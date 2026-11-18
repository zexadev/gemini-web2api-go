package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// setTestAPIKeys 临时替换进程内的 key store，测试结束恢复。
func setTestAPIKeys(t *testing.T, keys ...*APIKey) {
	t.Helper()
	keyStoreMu.RLock()
	old := keyStoreVal
	keyStoreMu.RUnlock()

	state := &apiKeyState{byHash: map[string]*APIKey{}}
	for _, k := range keys {
		if k.Hash == "" {
			k.Hash = keyDigest(k.Key)
		}
		state.all = append(state.all, k)
		state.byHash[k.Hash] = k
		if state.defaultKey == nil {
			state.defaultKey = k
		}
	}
	keyStoreMu.Lock()
	keyStoreVal = state
	keyStoreMu.Unlock()
	t.Cleanup(func() {
		keyStoreMu.Lock()
		keyStoreVal = old
		keyStoreMu.Unlock()
	})
}

func TestRequireAPIKeyAcceptsGoogleSDKForms(t *testing.T) {
	setTestAPIKeys(t, &APIKey{
		ID: 1, Name: "test", Key: "test-key-0123456789abcdef",
		Enabled: true, AllowAll: true,
	})

	h := requireAPIKey(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	cases := []struct {
		name string
		set  func(r *http.Request)
		want int
	}{
		{"bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer test-key-0123456789abcdef") }, 204},
		{"x-api-key", func(r *http.Request) { r.Header.Set("x-api-key", "test-key-0123456789abcdef") }, 204},
		{"x-goog-api-key", func(r *http.Request) { r.Header.Set("x-goog-api-key", "test-key-0123456789abcdef") }, 204},
		{"query", func(r *http.Request) { r.URL.RawQuery = "key=test-key-0123456789abcdef&alt=sse" }, 204},
		{"wrong", func(r *http.Request) { r.Header.Set("x-goog-api-key", "nope") }, 401},
		{"none", func(r *http.Request) {}, 401},
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", "/v1beta/models/x:generateContent", nil)
		c.set(req)
		w := httptest.NewRecorder()
		h(w, req)
		if w.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, w.Code, c.want)
		}
	}
}

func TestRequireAPIKeyRejectsDisabled(t *testing.T) {
	setTestAPIKeys(t, &APIKey{
		ID: 7, Name: "disabled", Key: "disabled-key-0123456789",
		Enabled: false, AllowAll: true,
	})
	h := requireAPIKey(func(w http.ResponseWriter, r *http.Request) {
		t.Error("disabled key reached the handler")
		w.WriteHeader(204)
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer disabled-key-0123456789")
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != 401 {
		t.Fatalf("disabled key: got %d, want 401", w.Code)
	}
}
