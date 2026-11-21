package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const refusalSample = "I can help with many kinds of requests, but it appears this one would go against my [guidelines](https://gemini.google/policy-guidelines/). Is there something else I can try for you?"

func setRefusalCheck(t *testing.T, enabled bool) {
	t.Helper()
	old := rtCfg()
	t.Cleanup(func() { rtMu.Lock(); rtVal = old; rtMu.Unlock() })
	rtMu.Lock()
	rtVal.RefusalAsError = enabled
	rtMu.Unlock()
}

func refusalTestFrame(text string) string {
	inner := make([]interface{}, 5)
	inner[4] = []interface{}{[]interface{}{"rc_test123", []interface{}{text}}}
	b, _ := json.Marshal(inner)
	line, _ := json.Marshal([]interface{}{[]interface{}{"wrb.fr", nil, string(b)}})
	return string(line) + strings.Repeat(" ", 240)
}

func TestGeminiRefusalDetection(t *testing.T) {
	yes := []string{
		refusalSample,
		"I can help with all sorts of things, but that request may go against my [guidelines](https://gemini.google/policy-guidelines/).",
		"I'm sorry, it appears I can't help with this particular request, as it may go against my [guidelines](https://gemini.google/policy-guidelines/).",
		"Your request may go against my [guidelines](https://gemini.google/policy-guidelines/). Would you like to try something else?",
		"Sorry, I cannot assist with that because of my safety guidelines: https://gemini.google/policy-guidelines/",
		"I’m unable to fulfill this request under my [guidelines](https://gemini.google/policy-guidelines).",
		"抱歉，这个请求违反了我的指南：https://gemini.google/policy-guidelines/",
		" \n" + strings.ReplaceAll(refusalSample, " ", "\n") + "\r\n",
		"Read https://gemini.google/policy-guidelines/ for details.",
		"> " + refusalSample,
		"```text\n" + refusalSample + "\n```",
		"“" + refusalSample + "”的意思是模型拒绝了请求。",
		"Translation: " + refusalSample,
		"The model answered: " + refusalSample,
		"I can help translate this sentence: \"" + refusalSample + "\"",
		"Your request is fine. See https://gemini.google/policy-guidelines/",
		"I can help explain why requests go against https://gemini.google/policy-guidelines/.",
		"新的拒答措辞：https://gemini.google/policy-guidelines/?hl=zh#section",
		"\"https://gemini.google/policy-guidelines/\"",
		"`https://gemini.google/policy-guidelines/`",
		"链接：HTTP://GEMINI.GOOGLE/POLICY-GUIDELINES/",
		strings.Repeat("长回复", 1000) + " https://gemini.google/policy-guidelines/",
	}
	for _, s := range yes {
		if !isGeminiRefusal(s) {
			t.Errorf("missed policy URL: %s", s)
		}
	}
	no := []string{
		"正常回复", "I can't tell from this picture.",
		"Sorry, I cannot help with this request.", // Without the link, do not infer a refusal.
		"Sorry, I cannot help: https://gemini.google.evil.test/policy-guidelines/",
		"Sorry, I cannot help: https://gemini.google/policy-guidelines-other",
		"Sorry, I cannot help: https://gemini.google/policy-guidelines.other",
		"Sorry, I cannot help: https://gemini.google/policy-guidelines/other",
		"https://gemini.google/policy-guidelines/other?x=1",
		"https://gemini.google/policy-guidelines.html",
		"https://example.com/policy-guidelines/",
	}
	for _, s := range no {
		if isGeminiRefusal(s) {
			t.Errorf("false refusal: %s", s)
		}
	}
}

func TestRefusalCallGate(t *testing.T) {
	old := generateGeminiFiles
	t.Cleanup(func() { generateGeminiFiles = old })
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			setRefusalCheck(t, enabled)
			var emitted strings.Builder
			generateGeminiFiles = func(_, _ string, _ ModelConfig, _ []pendingUpload, delta, reason func(string)) (*StreamResult, error) {
				if delta != nil {
					delta("I can help ")
					delta(strings.TrimPrefix(refusalSample, "I can help "))
				}
				if reason != nil {
					reason("internal reasoning")
				}
				return &StreamResult{Raw: refusalTestFrame(refusalSample), Emitted: refusalSample, EmittedReasoning: "internal reasoning"}, nil
			}
			text, _, res, err := callGeminiChecked("safe prompt", "safe prompt", Models["gemini-3.6-flash"], nil, nil,
				func(s string) { emitted.WriteString(s) }, func(s string) { emitted.WriteString(s) }, enabled)
			if enabled {
				if !isGeminiRefusalError(err) || text != "" || emitted.Len() != 0 || res.Emitted != "" {
					t.Fatalf("refusal escaped: text=%q emitted=%q err=%v", text, emitted.String(), err)
				}
			} else if err != nil || text != refusalSample || emitted.Len() == 0 {
				t.Fatalf("disabled changed behavior: %v %q", err, text)
			}
		})
	}
}

func TestRefusalHTTPAndNormalStreaming(t *testing.T) {
	setRefusalCheck(t, true)
	old := generateGeminiFiles
	t.Cleanup(func() { generateGeminiFiles = old })
	for _, tc := range []struct {
		text    string
		blocked bool
	}{
		{refusalSample, true},
		{"未知新措辞 https://gemini.google/policy-guidelines/", true},
		{"引用：\"https://gemini.google/policy-guidelines/\"", true},
		{strings.Repeat("长回复", 400) + " https://gemini.google/policy-guidelines/", true},
		{"正常回复", false},
		{"```tool_call\n{\"name\":\"get_time\",\"arguments\":{}}\n```", false},
	} {
		text := tc.text
		generateGeminiFiles = func(_, _ string, _ ModelConfig, _ []pendingUpload, delta, reason func(string)) (*StreamResult, error) {
			if delta != nil {
				delta(text)
			}
			return &StreamResult{Raw: refusalTestFrame(text), Emitted: text}, nil
		}
		for _, stream := range []bool{false, true} {
			for _, endpoint := range []string{"chat", "responses", "gemini"} {
				w := httptest.NewRecorder()
				tools := `[{"type":"function","function":{"name":"get_time","parameters":{"type":"object"}}}]`
				switch endpoint {
				case "chat":
					handleChatCompletions(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"gemini-3.6-flash","stream":%v,"messages":[{"role":"user","content":"hi"}],"tools":%s}`, stream, tools))))
				case "responses":
					handleResponses(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"gemini-3.6-flash","stream":%v,"input":"hi","tools":%s}`, stream, tools))))
				case "gemini":
					action := "generateContent"
					if stream {
						action = "streamGenerateContent?alt=sse"
					}
					handleGeminiModelItem(w, httptest.NewRequest("POST", "/v1beta/models/gemini-3.6-flash:"+action, strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"get_time"}]}]}`)))
				}
				if tc.blocked {
					if w.Code != 422 || !strings.Contains(w.Body.String(), geminiRefusalCode) || strings.Contains(w.Body.String(), "policy-guidelines") {
						t.Fatalf("%s stream=%v refusal: %d %s", endpoint, stream, w.Code, w.Body)
					}
				} else if w.Code != 200 || strings.Contains(w.Body.String(), "```tool_call") {
					t.Fatalf("%s stream=%v text=%q extracted=%q valid output: %d %s", endpoint, stream, text, extractResponseText(refusalTestFrame(text)), w.Code, w.Body)
				}
			}
		}
	}
}

func TestRefusalInvalidatesConversation(t *testing.T) {
	setRefusalCheck(t, true)
	old := generateGeminiConversation
	t.Cleanup(func() { generateGeminiConversation = old })
	messages := []map[string]interface{}{{"role": "user", "content": "earlier"}, {"role": "assistant", "content": "answer"}, {"role": "user", "content": "hi"}}
	key := convParentKey(messages)
	convPut(key, &convState{updated: time.Now()})
	t.Cleanup(func() { convDelete(key); convDelete(convChildKey(messages, refusalSample)) })
	generateGeminiConversation = func(_ string, _ ModelConfig, _ *convState, delta, reason func(string)) (*StreamResult, error) {
		if delta != nil {
			delta(refusalSample)
		}
		return &StreamResult{Raw: refusalTestFrame(refusalSample), Emitted: refusalSample}, nil
	}
	var emitted strings.Builder
	_, _, _, err := callGeminiConv(messages, Models["gemini-3.6-flash"], nil, nil, func(s string) { emitted.WriteString(s) }, nil, true)
	if !isGeminiRefusalError(err) || emitted.Len() != 0 || convGet(key) != nil || convGet(convChildKey(messages, refusalSample)) != nil {
		t.Fatal("refusal leaked or poisoned cached conversation")
	}
}

func TestRefusalSettingPersistsAndIsInUI(t *testing.T) {
	oldCfg, oldRuntime := cfg, rtCfg()
	oldKV := kvGet(runtimeConfigKey)
	t.Cleanup(func() { cfg = oldCfg; kvSet(runtimeConfigKey, oldKV); rtMu.Lock(); rtVal = oldRuntime; rtMu.Unlock() })
	cfg = defaultConfig()
	kvSet(runtimeConfigKey, `{"refusal_as_error":true}`)
	initRuntimeConfig()
	if !rtCfg().RefusalAsError {
		t.Fatal("saved switch missing after initialization")
	}
	kvSet(runtimeConfigKey, `{"refusal_as_error":false}`)
	initRuntimeConfig()
	if rtCfg().RefusalAsError {
		t.Fatal("saved false ignored")
	}
	if defaultConfig().RefusalAsError {
		t.Fatal("new switch must be opt-in")
	}
	ui, err := os.ReadFile("admin_ui/index.html")
	if err != nil || !strings.Contains(string(ui), "k: 'refusal_as_error'") {
		t.Fatal("UI save would drop setting")
	}
}

// Changing the switch while reading a request must not let headers/keepalive
// use one setting and the final refusal check use another setting.
type refusalSwitchReader struct {
	io.Reader
	enabled bool
}

func (r refusalSwitchReader) Read(p []byte) (int, error) {
	rtMu.Lock()
	rtVal.RefusalAsError = r.enabled
	rtMu.Unlock()
	return r.Reader.Read(p)
}

func TestRefusalSwitchIsSnapshotPerRequest(t *testing.T) {
	old := generateGeminiFiles
	t.Cleanup(func() { generateGeminiFiles = old })
	generateGeminiFiles = func(_, _ string, _ ModelConfig, _ []pendingUpload, delta, reason func(string)) (*StreamResult, error) {
		if delta != nil {
			delta(refusalSample)
		}
		return &StreamResult{Raw: refusalTestFrame(refusalSample), Emitted: refusalSample}, nil
	}
	for _, enabled := range []bool{false, true} {
		for _, endpoint := range []string{"chat", "responses", "gemini"} {
			setRefusalCheck(t, enabled)
			w := httptest.NewRecorder()
			body := `{"model":"gemini-3.6-flash","stream":true,"messages":[{"role":"user","content":"hi"}],"input":"hi","contents":[{"parts":[{"text":"hi"}]}]}`
			r := httptest.NewRequest("POST", "/", refusalSwitchReader{strings.NewReader(body), !enabled})
			switch endpoint {
			case "chat":
				handleChatCompletions(w, r)
			case "responses":
				handleResponses(w, r)
			case "gemini":
				handleStreamGenerateContent(w, r, "gemini-3.6-flash")
			}
			want := 200
			if enabled {
				want = 422
			}
			if w.Code != want || (enabled && strings.Contains(w.Body.String(), "policy-guidelines")) {
				t.Fatalf("%s enabled=%v: got %d %s", endpoint, enabled, w.Code, w.Body)
			}
		}
	}
}
