package app

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// sseWriter 把 chat.completion.chunk 按 OpenAI 规范的顺序写出去：
// 先 delta{role} → 若干 delta{content} → delta{} + finish_reason → 可选 usage → [DONE]。
//
// header 是懒发的：第一次真要写 chunk 时才 WriteHeader(200)。这样上游在出第一个
// 字之前就失败的话，还能退回正常的 502 JSON，而不是留给客户端半个流。
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	id      string
	created int64
	model   string
	started bool
}

func newSSEWriter(w http.ResponseWriter, id string, created int64, model string) *sseWriter {
	f, _ := w.(http.Flusher)
	return &sseWriter{w: w, flusher: f, id: id, created: created, model: model}
}

func (s *sseWriter) Started() bool { return s.started }

// start 发送 SSE 响应头和首个 delta{"role":"assistant"} chunk。幂等。
func (s *sseWriter) start() {
	if s.started {
		return
	}
	s.started = true
	s.w.Header().Set("Content-Type", "text/event-stream")
	s.w.Header().Set("Cache-Control", "no-cache")
	s.w.Header().Set("Connection", "keep-alive")
	s.w.Header().Set("Access-Control-Allow-Origin", "*")
	s.w.WriteHeader(200)
	s.chunk(map[string]interface{}{"role": "assistant"}, nil, nil)
}

func (s *sseWriter) chunk(delta map[string]interface{}, finish interface{}, usage map[string]int) {
	c := map[string]interface{}{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []map[string]interface{}{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		c["usage"] = usage
	}
	b, _ := json.Marshal(c)
	fmt.Fprintf(s.w, "data: %s\n\n", b)
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// SendContent 发一段正文增量，供 streamGenerate 的 onDelta 回调直接使用。
// SendReasoning 推思考链增量。上游先把思考链推完才开始推正文，所以客户端会
// 先看到「思考过程」再看到答案，跟网页端的观感一致。
func (s *sseWriter) SendReasoning(delta string) {
	if delta == "" {
		return
	}
	s.start()
	s.chunk(map[string]interface{}{"reasoning_content": delta}, nil, nil)
}

func (s *sseWriter) SendContent(delta string) {
	if delta == "" {
		return
	}
	s.start()
	s.chunk(map[string]interface{}{"content": delta}, nil, nil)
}

func (s *sseWriter) SendToolCalls(tcs []ToolCall) {
	s.start()
	// 流式 delta 里每个 tool_call 必须带 index，客户端靠它把分片的 tool_call 拼起来
	// （OpenAI 流式规范要求）。ToolCall 结构本身没有 index，这里按顺序补上。
	out := make([]map[string]interface{}, len(tcs))
	for i, tc := range tcs {
		out[i] = map[string]interface{}{
			"index": i,
			"id":    tc.ID,
			"type":  tc.Type,
			"function": map[string]interface{}{
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
			},
		}
	}
	s.chunk(map[string]interface{}{"tool_calls": out}, nil, nil)
}

// Finish 收尾：空 delta + finish_reason，可选 usage chunk，最后 [DONE]。
func (s *sseWriter) Finish(reason string, usage map[string]int) {
	s.start()
	s.chunk(map[string]interface{}{}, reason, nil)
	if usage != nil {
		// OpenAI 的 usage chunk：choices 为空数组
		c := map[string]interface{}{
			"id": s.id, "object": "chat.completion.chunk",
			"created": s.created, "model": s.model,
			"choices": []map[string]interface{}{},
			"usage":   usage,
		}
		b, _ := json.Marshal(c)
		fmt.Fprintf(s.w, "data: %s\n\n", b)
	}
	fmt.Fprintf(s.w, "data: [DONE]\n\n")
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// Fail 在已经开流之后出错时用：发一个带 error 的 chunk 再收尾。
// 此时 HTTP 状态码已经是 200，改不了了。
func (s *sseWriter) Fail(err error) {
	s.start()
	c := map[string]interface{}{
		"id": s.id, "object": "chat.completion.chunk",
		"created": s.created, "model": s.model,
		"choices": []map[string]interface{}{},
		"error":   map[string]string{"message": err.Error(), "type": "upstream_error"},
	}
	b, _ := json.Marshal(c)
	fmt.Fprintf(s.w, "data: %s\n\n", b)
	fmt.Fprintf(s.w, "data: [DONE]\n\n")
	if s.flusher != nil {
		s.flusher.Flush()
	}
}
