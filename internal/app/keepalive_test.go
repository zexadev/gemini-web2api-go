package app

import (
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func setKeepaliveSec(t *testing.T, sec int) {
	t.Helper()
	rtMu.Lock()
	old := rtVal
	rtVal.StreamKeepaliveSec = sec
	rtMu.Unlock()
	t.Cleanup(func() { rtMu.Lock(); rtVal = old; rtMu.Unlock() })
}

// 空闲满一个间隔才 ping；正文在流时不 ping；Stop 之后不再 ping。
func TestStreamKeepalive(t *testing.T) {
	var pings atomic.Int32
	k := startKeepaliveEvery(200*time.Millisecond, func() { pings.Add(1) })
	time.Sleep(100 * time.Millisecond)
	if pings.Load() != 0 {
		t.Fatal("没空闲满一个间隔就 ping 了")
	}
	time.Sleep(250 * time.Millisecond)
	if pings.Load() == 0 {
		t.Fatal("空闲满一个间隔没 ping")
	}
	send := k.guard(func(string) {})
	before := pings.Load()
	for i := 0; i < 10; i++ { // 每 50ms 写一次正文，持续 500ms
		send("x")
		time.Sleep(50 * time.Millisecond)
	}
	if pings.Load() != before {
		t.Fatalf("正文在流时还 ping 了 %d 次", pings.Load()-before)
	}
	k.Stop()
	k.Stop() // 可重复调用
	after := pings.Load()
	time.Sleep(300 * time.Millisecond)
	if pings.Load() != after {
		t.Fatal("Stop 之后还在 ping")
	}

	var nilK *streamKeepalive // 设置为 0 时 startKeepalive 返回 nil
	if nilK.guard(nil) != nil {
		t.Fatal("nil 回调应原样返回")
	}
	nilK.Stop()
}

func TestStartKeepaliveOffByDefault(t *testing.T) {
	setKeepaliveSec(t, 0)
	if k := startKeepalive(func() {}); k != nil {
		k.Stop()
		t.Fatal("stream_keepalive_sec=0 时不该起保活")
	}
}

// /v1beta 流式：上游慢时先发空正文块保活，正文照常，拼起来跟不保活一样；
// 保活开流之后再失败，错误走流内最后一块，而不是 HTTP 502。
func TestStreamGenerateContentKeepalive(t *testing.T) {
	setKeepaliveSec(t, 1)
	saved := runGeminiCall
	t.Cleanup(func() { runGeminiCall = saved })
	var fail error
	runGeminiCall = func(g *geminiCall, onDelta, onReasoning func(string)) (string, []ToolCall, *StreamResult, error) {
		time.Sleep(2200 * time.Millisecond)
		if fail != nil {
			return "", nil, &StreamResult{}, fail
		}
		onDelta("Hello ")
		onDelta("world")
		return "Hello world", nil, &StreamResult{Emitted: "Hello world"}, nil
	}
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`

	w := httptest.NewRecorder()
	handleGeminiModelItem(w, httptest.NewRequest("POST",
		"/v1beta/models/gemini-3.6-flash:streamGenerateContent?alt=sse", strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("code %d %s", w.Code, w.Body)
	}
	chunks := sseChunks(t, w.Body.String())
	var text strings.Builder
	empty := 0
	for _, c := range chunks {
		for _, p := range chunkParts(c) {
			s := p.(map[string]interface{})["text"].(string)
			if s == "" {
				empty++
			}
			text.WriteString(s)
		}
	}
	if empty < 2 { // 2.2s 里至少 ping 一次，加上收尾那块的空 text
		t.Fatalf("没看到保活块: %s", w.Body)
	}
	if text.String() != "Hello world" {
		t.Fatalf("正文拼出来不对: %q", text.String())
	}

	fail = errors.New("upstream timeout")
	w = httptest.NewRecorder()
	handleGeminiModelItem(w, httptest.NewRequest("POST",
		"/v1beta/models/gemini-3.6-flash:streamGenerateContent?alt=sse", strings.NewReader(body)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("开流后失败应走流内错误: %d %s", w.Code, w.Body)
	}
}

// chat 的 Ping：先开流（role chunk），再发 SSE 注释行。
func TestSSEWriterPing(t *testing.T) {
	w := httptest.NewRecorder()
	s := newSSEWriter(w, "id", 0, "m")
	s.Ping()
	s.Ping()
	b := w.Body.String()
	if !s.Started() || w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatal("Ping 应先开流")
	}
	if strings.Count(b, `"role":"assistant"`) != 1 || strings.Count(b, ": ping\n\n") != 2 {
		t.Fatalf("got %q", b)
	}
}
