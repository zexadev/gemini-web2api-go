package app

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// 带工具调用历史的请求翻成 prompt 后，函数声明、上一轮的调用和工具结果都得在里面，
// 并且跟 OpenAI 那边同一套 ```tool_call``` 围栏写法。
func TestGeminiAPIToMessagesFunctionCalling(t *testing.T) {
	body := `{
	  "systemInstruction": {"parts": [{"text": "be brief"}]},
	  "contents": [
	    {"role": "user", "parts": [{"text": "weather in Paris?"}]},
	    {"role": "model", "parts": [{"functionCall": {"name": "get_weather", "args": {"city": "Paris"}}}]},
	    {"role": "user", "parts": [{"functionResponse": {"name": "get_weather", "response": {"result": "18C sunny"}}}]}
	  ],
	  "tools": [{"functionDeclarations": [
	    {"name": "get_weather", "description": "Get weather", "parameters": {"type": "OBJECT", "properties": {"city": {"type": "STRING"}}}},
	    {"name": "get_time", "description": "Get time"}
	  ]}],
	  "toolConfig": {"functionCallingConfig": {"mode": "ANY", "allowedFunctionNames": ["get_weather"]}}
	}`
	var req geminiAPIRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	messages, tools, choice, err := geminiAPIToMessages(&req)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("allowedFunctionNames should filter tools, got %d", len(tools))
	}
	if m, _ := choice.(map[string]interface{}); m == nil {
		t.Fatalf("ANY + one allowed name should force that function, got %v", choice)
	}
	prompt, _ := messagesToPrompt(messages, tools, choice)
	for _, want := range []string{
		"[System instruction]: be brief",
		`You MUST call the tool "get_weather"`,
		"weather in Paris?",
		"```tool_call\n{\"name\": \"get_weather\", \"arguments\": {\"city\":\"Paris\"}}\n```",
		"[Tool result for get_weather]: 18C sunny",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "get_time") {
		t.Error("filtered-out function leaked into the prompt")
	}
}

func TestGeminiAPIToMessagesModes(t *testing.T) {
	for mode, want := range map[string]string{"NONE": "none", "ANY": "required", "AUTO": "auto", "": "auto"} {
		req := geminiAPIRequest{}
		req.ToolConfig.FunctionCallingConfig.Mode = mode
		_, _, choice, _ := geminiAPIToMessages(&req)
		if choice != want {
			t.Errorf("mode %q -> %v, want %s", mode, choice, want)
		}
	}
}

func TestGeminiAPIToMessagesMedia(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[
	  {"text":"what is this"},
	  {"inlineData":{"mimeType":"image/png","data":"AAAA"}},
	  {"fileData":{"mimeType":"video/mp4","fileUri":"https://x/v.mp4"}}]}]}`
	var req geminiAPIRequest
	_ = json.Unmarshal([]byte(body), &req)
	messages, _, _, err := geminiAPIToMessages(&req)
	if err != nil {
		t.Fatal(err)
	}
	parts := messages[0]["content"].([]interface{})
	if len(parts) != 3 || parts[1].(map[string]interface{})["type"] != "image_url" ||
		parts[2].(map[string]interface{})["type"] != "video_url" {
		t.Fatalf("media parts = %v", parts)
	}

	for _, bad := range []string{
		`{"contents":[{"parts":[{"inlineData":{"mimeType":"application/pdf","data":"AAAA"}}]}]}`,
		`{"contents":[{"parts":[{"fileData":{"mimeType":"image/png","fileUri":"files/abc"}}]}]}`,
	} {
		var r geminiAPIRequest
		_ = json.Unmarshal([]byte(bad), &r)
		if _, _, _, err := geminiAPIToMessages(&r); err == nil {
			t.Errorf("should reject: %s", bad)
		}
	}
}

func stubGeminiCall(t *testing.T, text string, calls []ToolCall, res *StreamResult, err error) *geminiCall {
	t.Helper()
	var seen geminiCall
	saved := runGeminiCall
	runGeminiCall = func(g *geminiCall, onDelta, onReasoning func(string)) (string, []ToolCall, *StreamResult, error) {
		seen = *g
		return text, calls, res, err
	}
	t.Cleanup(func() { runGeminiCall = saved })
	return &seen
}

func TestGenerateContentResponse(t *testing.T) {
	calls := []ToolCall{{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "get_weather", Arguments: `{"city":"Paris"}`}}}
	stubGeminiCall(t, "let me check", calls, &StreamResult{Reasoning: "thinking..."}, nil)

	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],
	  "tools":[{"functionDeclarations":[{"name":"get_weather"}]}],
	  "generationConfig":{"thinkingConfig":{"includeThoughts":true},"temperature":0.2}}`
	w := httptest.NewRecorder()
	handleGeminiModelItem(w, httptest.NewRequest("POST", "/v1beta/models/gemini-3.6-flash:generateContent", strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var out struct {
		Candidates []struct {
			Content struct {
				Role  string                   `json:"role"`
				Parts []map[string]interface{} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata map[string]int `json:"usageMetadata"`
		ModelVersion  string         `json:"modelVersion"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	c := out.Candidates[0]
	if c.Content.Role != "model" || c.FinishReason != "STOP" || out.ModelVersion != "gemini-3.6-flash" {
		t.Fatalf("bad envelope: %s", w.Body)
	}
	if len(c.Content.Parts) != 3 || c.Content.Parts[0]["thought"] != true || c.Content.Parts[1]["text"] != "let me check" {
		t.Fatalf("parts = %v", c.Content.Parts)
	}
	fc := c.Content.Parts[2]["functionCall"].(map[string]interface{})
	if fc["name"] != "get_weather" || fc["args"].(map[string]interface{})["city"] != "Paris" {
		t.Fatalf("functionCall = %v", fc)
	}
	if out.UsageMetadata["thoughtsTokenCount"] == 0 ||
		out.UsageMetadata["totalTokenCount"] != out.UsageMetadata["promptTokenCount"]+
			out.UsageMetadata["candidatesTokenCount"]+out.UsageMetadata["thoughtsTokenCount"] {
		t.Fatalf("usage = %v", out.UsageMetadata)
	}
}

// 没要 includeThoughts 时不回思考摘要；媒体产物走 inlineData，不再拼成 data URL 文本。
func TestGenerateContentHidesThoughtsAndReturnsInlineData(t *testing.T) {
	res := &StreamResult{Reasoning: "hidden", Artifacts: []MediaArtifact{{Mime: "image/png", Data: []byte("xyz")}}}
	stubGeminiCall(t, "![image](data:image/png;base64,eHl6)", nil, res, nil)
	w := httptest.NewRecorder()
	handleGeminiModelItem(w, httptest.NewRequest("POST", "/v1beta/models/gemini-3.6-flash:generateContent",
		strings.NewReader(`{"contents":[{"parts":[{"text":"draw"}]}]}`)))
	s := w.Body.String()
	if w.Code != 200 || strings.Contains(s, "hidden") || strings.Contains(s, "data:image") ||
		!strings.Contains(s, `"inlineData":{"data":"eHl6","mimeType":"image/png"}`) {
		t.Fatalf("%d %s", w.Code, s)
	}
}

func TestGenerateContentErrors(t *testing.T) {
	stubGeminiCall(t, "", nil, nil, &ModelDowngradedError{Requested: "3.1 Pro", Reported: "3.5 Flash-Lite"})
	w := httptest.NewRecorder()
	handleGeminiModelItem(w, httptest.NewRequest("POST", "/v1beta/models/gemini-3.6-flash:generateContent",
		strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`)))
	if w.Code != 429 || !strings.Contains(w.Body.String(), `"status":"RESOURCE_EXHAUSTED"`) {
		t.Fatalf("downgrade: %d %s", w.Code, w.Body)
	}

	w = httptest.NewRecorder()
	handleGeminiModelItem(w, httptest.NewRequest("POST", "/v1beta/models/no-such-model:generateContent",
		strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`)))
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"status":"INVALID_ARGUMENT"`) {
		t.Fatalf("unknown model: %d %s", w.Code, w.Body)
	}

	w = httptest.NewRecorder()
	handleGeminiModelItem(w, httptest.NewRequest("POST", "/v1beta/models/gemini-3.6-flash:embedContent", strings.NewReader(`{}`)))
	if w.Code != 404 {
		t.Fatalf("unsupported action: %d", w.Code)
	}
}

func TestGeminiModelsAndCountTokens(t *testing.T) {
	w := httptest.NewRecorder()
	handleGeminiModels(w, httptest.NewRequest("GET", "/v1beta/models", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"models/gemini-3.6-flash"`) {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	handleGeminiModelItem(w, httptest.NewRequest("GET", "/v1beta/models/gemini-3.6-flash", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "streamGenerateContent") {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	handleGeminiModelItem(w, httptest.NewRequest("POST", "/v1beta/models/gemini-3.6-flash:countTokens",
		strings.NewReader(`{"contents":[{"parts":[{"text":"hello world"}]}]}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"totalTokens":`) {
		t.Fatalf("countTokens: %d %s", w.Code, w.Body)
	}
}

// stubGeminiStream 模拟上游边出边推：先推思考，再分几段推正文（含工具围栏）。
func stubGeminiStream(t *testing.T, reasoning string, deltas []string, err error) {
	t.Helper()
	saved := runGeminiCall
	runGeminiCall = func(g *geminiCall, onDelta, onReasoning func(string)) (string, []ToolCall, *StreamResult, error) {
		res := &StreamResult{Reasoning: reasoning}
		if onReasoning != nil && reasoning != "" {
			onReasoning(reasoning)
			res.EmittedReasoning = reasoning
		}
		full := ""
		for _, d := range deltas {
			onDelta(d)
			full += d
		}
		res.Emitted = full
		if err != nil {
			return "", nil, res, err
		}
		text, calls := full, []ToolCall(nil)
		if len(g.tools) > 0 {
			text, calls = parseToolCalls(full)
		}
		return text, calls, res, nil
	}
	t.Cleanup(func() { runGeminiCall = saved })
}

func sseChunks(t *testing.T, body string) []map[string]interface{} {
	t.Helper()
	var out []map[string]interface{}
	for _, ev := range strings.Split(body, "\r\n\r\n") {
		ev = strings.TrimSpace(ev)
		if ev == "" {
			continue
		}
		if !strings.HasPrefix(ev, "data: ") {
			t.Fatalf("bad SSE event %q", ev)
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(ev[6:]), &m); err != nil {
			t.Fatalf("bad chunk %q: %v", ev, err)
		}
		out = append(out, m)
	}
	return out
}

func chunkParts(c map[string]interface{}) []interface{} {
	cand := c["candidates"].([]interface{})[0].(map[string]interface{})
	return cand["content"].(map[string]interface{})["parts"].([]interface{})
}

func TestStreamGenerateContentSSEWithFunctionCall(t *testing.T) {
	stubGeminiStream(t, "hmm", []string{"Checking ", "now.\n```tool", "_call\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Paris\"}}\n```"}, nil)
	body := `{"contents":[{"parts":[{"text":"weather?"}]}],
	  "tools":[{"functionDeclarations":[{"name":"get_weather"}]}],
	  "generationConfig":{"thinkingConfig":{"includeThoughts":true}}}`
	w := httptest.NewRecorder()
	handleGeminiModelItem(w, httptest.NewRequest("POST",
		"/v1beta/models/gemini-3.6-flash:streamGenerateContent?alt=sse", strings.NewReader(body)))
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	if strings.Contains(w.Body.String(), "tool_call") {
		t.Fatalf("fence leaked to the client: %s", w.Body)
	}
	chunks := sseChunks(t, w.Body.String())
	first := chunkParts(chunks[0])[0].(map[string]interface{})
	if first["thought"] != true || first["text"] != "hmm" {
		t.Fatalf("first chunk should be the thought: %v", first)
	}
	var text string
	for _, c := range chunks {
		for _, p := range chunkParts(c) {
			pm := p.(map[string]interface{})
			if pm["thought"] == nil {
				if s, ok := pm["text"].(string); ok {
					text += s
				}
			}
		}
	}
	if strings.TrimSpace(text) != "Checking now." {
		t.Fatalf("streamed text = %q", text)
	}
	last := chunks[len(chunks)-1]
	cand := last["candidates"].([]interface{})[0].(map[string]interface{})
	if cand["finishReason"] != "STOP" || last["usageMetadata"] == nil {
		t.Fatalf("last chunk = %v", last)
	}
	var fc map[string]interface{}
	for _, p := range chunkParts(last) {
		if m, ok := p.(map[string]interface{})["functionCall"].(map[string]interface{}); ok {
			fc = m
		}
	}
	if fc == nil || fc["name"] != "get_weather" {
		t.Fatalf("functionCall missing from last chunk: %v", last)
	}
}

// 不带 alt=sse 时是一个 JSON 数组；不要 includeThoughts 时思考不出现。
func TestStreamGenerateContentJSONArray(t *testing.T) {
	stubGeminiStream(t, "secret", []string{"Hel", "lo"}, nil)
	w := httptest.NewRecorder()
	handleGeminiModelItem(w, httptest.NewRequest("POST", "/v1beta/models/gemini-3.6-flash:streamGenerateContent",
		strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`)))
	var arr []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &arr); err != nil {
		t.Fatalf("not a JSON array: %v\n%s", err, w.Body)
	}
	if len(arr) != 3 || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("chunks = %s", w.Body)
	}
}

func TestStreamGenerateContentErrors(t *testing.T) {
	// 开流前出错：正常的 HTTP 状态码 + Google 错误体。
	stubGeminiStream(t, "", nil, &ModelDowngradedError{Requested: "3.1 Pro", Reported: "3.5 Flash-Lite"})
	w := httptest.NewRecorder()
	handleGeminiModelItem(w, httptest.NewRequest("POST", "/v1beta/models/gemini-3.6-flash:streamGenerateContent?alt=sse",
		strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`)))
	if w.Code != 429 || !strings.Contains(w.Body.String(), "RESOURCE_EXHAUSTED") {
		t.Fatalf("before start: %d %s", w.Code, w.Body)
	}
	// 开流后出错：状态码已经是 200，错误作为最后一块发出。
	stubGeminiStream(t, "", []string{"partial"}, errMsg("boom"))
	w = httptest.NewRecorder()
	handleGeminiModelItem(w, httptest.NewRequest("POST", "/v1beta/models/gemini-3.6-flash:streamGenerateContent?alt=sse",
		strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`)))
	chunks := sseChunks(t, w.Body.String())
	if w.Code != 200 || len(chunks) != 2 || chunks[1]["error"] == nil {
		t.Fatalf("after start: %d %s", w.Code, w.Body)
	}
}

// google-genai 3.x 默认打 /v1/models/{model}:action，要跟 /v1beta 一样处理；
// 不带冒号的 /v1/models/{id} 不归 Gemini 接口，照旧 404。
func TestV1GeminiAction(t *testing.T) {
	stubGeminiCall(t, "hi", nil, &StreamResult{}, nil)
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	w := httptest.NewRecorder()
	handleV1GeminiAction(w, httptest.NewRequest("POST", "/v1/models/gemini-3.6-flash:generateContent", strings.NewReader(body)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"candidates"`) {
		t.Fatalf("generateContent: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	handleV1GeminiAction(w, httptest.NewRequest("GET", "/v1/models/gemini-3.6-flash", nil))
	if w.Code != 404 {
		t.Fatalf("不带冒号应 404: %d %s", w.Code, w.Body)
	}
}
