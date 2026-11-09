package app

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Google 原生 Gemini API 形状的接口（/v1beta/models/...），给只会说 Google 格式的
// 客户端用：Google 的 genai SDK、Bifrost 等网关的 Google provider。
//
// 只做格式翻译：请求翻成 OpenAI messages / tools，走跟 /v1/chat/completions 同一条
// callGemini 链，结果再翻回 candidates / usageMetadata。function calling 也是同一套
// ```tool_call``` 围栏（上游没有协议层的工具调用，见 buildPrompt），这里只负责把
// functionDeclarations / functionCall / functionResponse 跟它互转。
//
// generationConfig 里的采样参数（temperature / topP / maxOutputTokens …）上游没有
// 对应旋钮，收下忽略，跟 OpenAI 那边一致。

// geminiAPIRequest 是 generateContent 请求体里我们用得到的部分。
type geminiAPIRequest struct {
	Contents          []geminiContent `json:"contents"`
	SystemInstruction json.RawMessage `json:"systemInstruction"`
	Tools             []struct {
		FunctionDeclarations []struct {
			Name                 string      `json:"name"`
			Description          string      `json:"description"`
			Parameters           interface{} `json:"parameters"`
			ParametersJSONSchema interface{} `json:"parametersJsonSchema"`
		} `json:"functionDeclarations"`
	} `json:"tools"`
	ToolConfig struct {
		FunctionCallingConfig struct {
			Mode                 string   `json:"mode"`
			AllowedFunctionNames []string `json:"allowedFunctionNames"`
		} `json:"functionCallingConfig"`
	} `json:"toolConfig"`
	GenerationConfig struct {
		CandidateCount int `json:"candidateCount"`
		ThinkingConfig struct {
			IncludeThoughts bool `json:"includeThoughts"`
		} `json:"thinkingConfig"`
	} `json:"generationConfig"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text       string `json:"text,omitempty"`
	Thought    bool   `json:"thought,omitempty"`
	InlineData *struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	} `json:"inlineData,omitempty"`
	FileData *struct {
		MimeType string `json:"mimeType"`
		FileURI  string `json:"fileUri"`
	} `json:"fileData,omitempty"`
	FunctionCall *struct {
		ID   string                 `json:"id"`
		Name string                 `json:"name"`
		Args map[string]interface{} `json:"args"`
	} `json:"functionCall,omitempty"`
	FunctionResponse *struct {
		ID       string      `json:"id"`
		Name     string      `json:"name"`
		Response interface{} `json:"response"`
	} `json:"functionResponse,omitempty"`
}

// geminiAPIToMessages 把 Google 请求翻成 OpenAI 的 messages / tools / tool_choice，
// 后面就能原样交给 messagesToPrompt / collectImages / callGemini。
func geminiAPIToMessages(req *geminiAPIRequest) ([]map[string]interface{}, []map[string]interface{}, interface{}, error) {
	var messages []map[string]interface{}
	if sys := systemInstructionText(req.SystemInstruction); sys != "" {
		messages = append(messages, map[string]interface{}{"role": "system", "content": sys})
	}

	for _, c := range req.Contents {
		var texts []string
		var media []interface{}
		var calls []interface{}
		for _, p := range c.Parts {
			switch {
			case p.FunctionCall != nil:
				args, _ := json.Marshal(p.FunctionCall.Args)
				if p.FunctionCall.Args == nil {
					args = []byte("{}")
				}
				id := p.FunctionCall.ID
				if id == "" {
					id = "call_" + randHex(8)
				}
				calls = append(calls, map[string]interface{}{
					"id": id, "type": "function",
					"function": map[string]interface{}{"name": p.FunctionCall.Name, "arguments": string(args)},
				})
			case p.FunctionResponse != nil:
				// 每个 functionResponse 是一条 tool 消息；一个 content 里可能有好几条（并行调用）。
				messages = append(messages, map[string]interface{}{
					"role":         "tool",
					"tool_call_id": p.FunctionResponse.ID,
					"name":         p.FunctionResponse.Name,
					"content":      functionResponseText(p.FunctionResponse.Response),
				})
			case p.InlineData != nil:
				part, err := mediaPart(p.InlineData.MimeType,
					"data:"+p.InlineData.MimeType+";base64,"+p.InlineData.Data)
				if err != nil {
					return nil, nil, nil, err
				}
				media = append(media, part)
			case p.FileData != nil:
				uri := p.FileData.FileURI
				if !strings.HasPrefix(uri, "http://") && !strings.HasPrefix(uri, "https://") {
					return nil, nil, nil, fmt.Errorf("fileData.fileUri %q not supported: only http(s) links "+
						"(there is no Files API here; send the bytes as inlineData instead)", uri)
				}
				part, err := mediaPart(p.FileData.MimeType, uri)
				if err != nil {
					return nil, nil, nil, err
				}
				media = append(media, part)
			case p.Thought:
				// 历史里回传的思考摘要：不再喂给模型，跟 OpenAI 那边不回传 reasoning_content 一致。
			default:
				if p.Text != "" {
					texts = append(texts, p.Text)
				}
			}
		}
		text := strings.Join(texts, "\n")

		if c.Role == "model" {
			if text == "" && len(calls) == 0 {
				continue
			}
			m := map[string]interface{}{"role": "assistant", "content": text}
			if len(calls) > 0 {
				m["tool_calls"] = calls
			}
			messages = append(messages, m)
			continue
		}
		// user（以及旧写法里放 functionResponse 的 "function" 角色）。
		if len(media) > 0 {
			content := media
			if text != "" {
				content = append([]interface{}{map[string]interface{}{"type": "text", "text": text}}, media...)
			}
			messages = append(messages, map[string]interface{}{"role": "user", "content": content})
		} else if text != "" {
			messages = append(messages, map[string]interface{}{"role": "user", "content": text})
		}
	}

	fc := req.ToolConfig.FunctionCallingConfig
	allowed := map[string]bool{}
	for _, n := range fc.AllowedFunctionNames {
		allowed[n] = true
	}
	var tools []map[string]interface{}
	for _, t := range req.Tools {
		for _, d := range t.FunctionDeclarations {
			if len(allowed) > 0 && !allowed[d.Name] {
				continue
			}
			params := d.Parameters
			if params == nil {
				params = d.ParametersJSONSchema
			}
			tools = append(tools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name": d.Name, "description": d.Description, "parameters": params,
				},
			})
		}
	}

	var toolChoice interface{} = "auto"
	switch strings.ToUpper(fc.Mode) {
	case "NONE":
		toolChoice = "none"
	case "ANY":
		toolChoice = "required"
		if len(fc.AllowedFunctionNames) == 1 {
			toolChoice = map[string]interface{}{"type": "function",
				"function": map[string]interface{}{"name": fc.AllowedFunctionNames[0]}}
		}
	}
	return messages, tools, toolChoice, nil
}

// systemInstructionText 取 systemInstruction 的文字：标准写法是 Content（{parts:[{text}]}），
// 也兼容直接给字符串。
func systemInstructionText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var c geminiContent
	if json.Unmarshal(raw, &c) != nil {
		return ""
	}
	var bits []string
	for _, p := range c.Parts {
		if p.Text != "" {
			bits = append(bits, p.Text)
		}
	}
	return strings.Join(bits, "\n")
}

// functionResponseText 把 functionResponse.response 转成工具结果文本。
// 只有一个字符串字段（{"output": "..."} / {"result": "..."} 这类最常见的包法）时取那个
// 字符串本身，formatToolResult 才认得出里面的退出码；其余原样转 JSON。
func functionResponseText(resp interface{}) string {
	if m, ok := resp.(map[string]interface{}); ok && len(m) == 1 {
		for _, v := range m {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	if s, ok := resp.(string); ok {
		return s
	}
	b, _ := json.Marshal(resp)
	return string(b)
}

// mediaPart 按 mime 把图片 / 视频转成 OpenAI content 里的 image_url / video_url。
// 上游只收这两类附件（见 newMediaUpload），别的类型直接报错，免得被静默丢掉。
func mediaPart(mime, src string) (map[string]interface{}, error) {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": src}}, nil
	case strings.HasPrefix(mime, "video/"):
		return map[string]interface{}{"type": "video_url", "video_url": map[string]interface{}{"url": src}}, nil
	}
	return nil, fmt.Errorf("%s input not supported: only image/* and video/* parts", mime)
}

// geminiStatus 把 HTTP 状态码映射成 Google 错误体里的 status 字符串。
func geminiStatus(code int) string {
	switch code {
	case 400:
		return "INVALID_ARGUMENT"
	case 401:
		return "UNAUTHENTICATED"
	case 404:
		return "NOT_FOUND"
	case 405:
		return "METHOD_NOT_ALLOWED"
	case 429:
		return "RESOURCE_EXHAUSTED"
	}
	return "UNAVAILABLE"
}

func writeGeminiError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]interface{}{"error": map[string]interface{}{
		"code": code, "message": msg, "status": geminiStatus(code)}})
}

// writeGeminiCallError 跟 OpenAI 那两条路同一套分流：降级 429、超长 400、限流 429、其余 502。
func writeGeminiCallError(w http.ResponseWriter, err error) {
	switch e := err.(type) {
	case *ModelDowngradedError:
		if e.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(e.RetryAfter.Seconds()))))
		}
		writeGeminiError(w, 429, e.Error())
	case *PromptTooLongError:
		writeGeminiError(w, 400, e.Error())
	case *RateLimitError:
		writeGeminiError(w, 429, e.Error())
	default:
		writeGeminiError(w, 502, "upstream error: "+err.Error())
	}
}

func geminiModelInfo(name string, c ModelConfig) map[string]interface{} {
	return map[string]interface{}{
		"name":                       "models/" + name,
		"baseModelId":                name,
		"version":                    Version,
		"displayName":                name,
		"description":                c.Desc,
		"supportedGenerationMethods": []string{"generateContent", "streamGenerateContent", "countTokens"},
	}
}

// handleGeminiModels: GET /v1beta/models，跟 /v1/models 同一份清单。
func handleGeminiModels(w http.ResponseWriter, r *http.Request) {
	if r.Method == "OPTIONS" {
		handleOptions(w, r)
		return
	}
	if r.Method != "GET" {
		writeGeminiError(w, 405, "method not allowed")
		return
	}
	all := availableModels()
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)
	models := make([]interface{}, 0, len(names))
	for _, n := range names {
		models = append(models, geminiModelInfo(n, all[n]))
	}
	writeJSON(w, 200, map[string]interface{}{"models": models})
}

// handleV1GeminiAction 把 /v1/models/{model}:{action} 交给 Gemini 接口。google-genai 3.0.0
// 起，自定义 base_url 时默认 api_version 是 v1，请求打到 /v1/models/…:generateContent
// 而不是 /v1beta。只接带冒号的动作：不带冒号的 GET /v1/models/{id} 是 OpenAI 的
// 「查单个模型」形状，两边响应体不同，不混用，照旧 404。
func handleV1GeminiAction(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(strings.TrimPrefix(r.URL.Path, "/v1/models/"), ":") {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	handleGeminiModelItem(w, r)
}

// handleGeminiModelItem 分发 /v1beta/models/{model}[:action]（以及 /v1/models/{model}:action）。
func handleGeminiModelItem(w http.ResponseWriter, r *http.Request) {
	if r.Method == "OPTIONS" {
		handleOptions(w, r)
		return
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/v1beta/models/"), "/v1/models/")
	model, action := rest, ""
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		model, action = rest[:i], rest[i+1:]
	}
	switch {
	case action == "" && r.Method == "GET":
		name, c, err := resolveModel(model)
		if err != nil {
			writeGeminiError(w, 404, err.Error())
			return
		}
		writeJSON(w, 200, geminiModelInfo(name, c))
	case r.Method != "POST":
		writeGeminiError(w, 405, "method not allowed")
	case action == "generateContent":
		handleGenerateContent(w, r, model)
	case action == "streamGenerateContent":
		handleStreamGenerateContent(w, r, model)
	case action == "countTokens":
		handleGeminiCountTokens(w, r)
	default:
		writeGeminiError(w, 404, fmt.Sprintf("unsupported method %q", action))
	}
}

// parseGeminiRequest 读请求体并翻成 OpenAI messages。出错时已经写好 400。
func parseGeminiRequest(w http.ResponseWriter, r *http.Request) (*geminiAPIRequest, []map[string]interface{},
	[]map[string]interface{}, interface{}, bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeGeminiError(w, 400, err.Error())
		return nil, nil, nil, nil, false
	}
	var req geminiAPIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeGeminiError(w, 400, "invalid JSON: "+err.Error())
		return nil, nil, nil, nil, false
	}
	messages, tools, toolChoice, err := geminiAPIToMessages(&req)
	if err != nil {
		writeGeminiError(w, 400, err.Error())
		return nil, nil, nil, nil, false
	}
	return &req, messages, tools, toolChoice, true
}

// handleGeminiCountTokens: 用跟 usage 同一个 tiktoken 口径数拼好的 prompt。
// 只是估算——上游没有给 token 数的接口。
func handleGeminiCountTokens(w http.ResponseWriter, r *http.Request) {
	_, messages, tools, toolChoice, ok := parseGeminiRequest(w, r)
	if !ok {
		return
	}
	prompt, _ := messagesToPrompt(messages, tools, toolChoice)
	writeJSON(w, 200, map[string]interface{}{"totalTokens": countTokens(prompt)})
}

// geminiTurn 是一次生成的结果，翻成 candidates 用。
type geminiTurn struct {
	text      string
	reasoning string
	calls     []ToolCall
	artifacts []MediaArtifact
}

// newGeminiTurn 从 callGemini 的结果里取出要回给客户端的东西。
// 媒体模型的产物 callGemini 已经以 data URL 拼进正文（那是给 OpenAI 客户端的写法），
// 这里改回原生 inlineData：正文重新从 raw 取，产物单独成 part。
func newGeminiTurn(text string, calls []ToolCall, res *StreamResult) geminiTurn {
	t := geminiTurn{text: text, calls: calls}
	if res != nil {
		t.reasoning = res.Reasoning
		if len(res.Artifacts) > 0 {
			t.artifacts = res.Artifacts
			t.text = strings.TrimSpace(extractResponseText(res.Raw))
		}
	}
	return t
}

// callParts 把工具调用翻成 functionCall parts。
func callParts(calls []ToolCall) []interface{} {
	var parts []interface{}
	for _, tc := range calls {
		args := map[string]interface{}{}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		parts = append(parts, map[string]interface{}{"functionCall": map[string]interface{}{
			"id": tc.ID, "name": tc.Function.Name, "args": args}})
	}
	return parts
}

func artifactParts(arts []MediaArtifact) []interface{} {
	var parts []interface{}
	for _, a := range arts {
		parts = append(parts, map[string]interface{}{"inlineData": map[string]interface{}{
			"mimeType": a.Mime, "data": base64.StdEncoding.EncodeToString(a.Data)}})
	}
	return parts
}

// geminiUsage 按 Google 的口径：candidatesTokenCount 不含思考，totalTokenCount 含。
func geminiUsage(prompt string, t geminiTurn) map[string]int {
	in, out := countTokens(prompt), countTokens(t.text)
	u := map[string]int{"promptTokenCount": in, "candidatesTokenCount": out, "totalTokenCount": in + out}
	if t.reasoning != "" {
		th := countTokens(t.reasoning)
		u["thoughtsTokenCount"] = th
		u["totalTokenCount"] += th
	}
	return u
}

func geminiCandidate(parts []interface{}, finish string) map[string]interface{} {
	c := map[string]interface{}{"content": map[string]interface{}{"role": "model", "parts": parts}, "index": 0}
	if finish != "" {
		c["finishReason"] = finish
	}
	return c
}

// prepareGemini 是 generateContent / streamGenerateContent 共用的前半段：
// 解析、选模型、收图、拼 prompt。出错时已经写好响应。
type geminiCall struct {
	req        *geminiAPIRequest
	modelName  string
	modelCfg   ModelConfig
	messages   []map[string]interface{}
	tools      []map[string]interface{}
	toolChoice interface{}
	images     []pendingUpload
	prompt     string
	latest     string
}

func prepareGemini(w http.ResponseWriter, r *http.Request, model string) (*geminiCall, bool) {
	req, messages, tools, toolChoice, ok := parseGeminiRequest(w, r)
	if !ok {
		return nil, false
	}
	modelName, modelCfg, err := resolveModel(model)
	if err != nil {
		writeGeminiError(w, 400, err.Error())
		return nil, false
	}
	check := map[string]interface{}{}
	if n := req.GenerationConfig.CandidateCount; n > 1 {
		check["n"] = float64(n)
	}
	if err := rejectUnsupported(check, messages); err != nil {
		writeGeminiError(w, 400, err.Error())
		return nil, false
	}
	images, err := collectImages(messages, "")
	if err != nil {
		writeGeminiError(w, 400, err.Error())
		return nil, false
	}
	prompt, latest := messagesToPrompt(messages, tools, toolChoice)
	if strings.TrimSpace(prompt) == "" && len(images) == 0 {
		writeGeminiError(w, 400, "contents is empty")
		return nil, false
	}
	return &geminiCall{req: req, modelName: modelName, modelCfg: modelCfg, messages: messages,
		tools: tools, toolChoice: toolChoice, images: images, prompt: prompt, latest: latest}, true
}

// run 调上游；多轮开关的分流跟 /v1/chat/completions 一致。
func (g *geminiCall) run(onDelta, onReasoning func(string)) (string, []ToolCall, *StreamResult, error) {
	if rtCfg().MultiTurn && len(g.images) == 0 && g.modelCfg.Tool == 0 {
		return callGeminiConv(g.messages, g.modelCfg, g.tools, g.toolChoice, onDelta, onReasoning)
	}
	return callGemini(g.prompt, g.latest, g.modelCfg, g.tools, g.images, onDelta, onReasoning)
}

// runGeminiCall 是调上游那一步，测试里替换成桩。
var runGeminiCall = func(g *geminiCall, onDelta, onReasoning func(string)) (string, []ToolCall, *StreamResult, error) {
	return g.run(onDelta, onReasoning)
}

func handleGenerateContent(w http.ResponseWriter, r *http.Request, model string) {
	g, ok := prepareGemini(w, r, model)
	if !ok {
		return
	}
	text, calls, res, err := runGeminiCall(g, nil, nil)
	if err != nil {
		status := 502
		if _, ok := err.(*ModelDowngradedError); ok {
			status = 429
		}
		recordRequest("gemini.generateContent", g.modelName, g.prompt, "", res, status, err.Error(), false)
		writeGeminiCallError(w, err)
		return
	}
	recordRequest("gemini.generateContent", g.modelName, g.prompt, text, res, 200, "", false)

	t := newGeminiTurn(text, calls, res)
	var parts []interface{}
	// 思考摘要只在客户端要了（thinkingConfig.includeThoughts）时给，跟 Google 一致。
	if t.reasoning != "" && g.req.GenerationConfig.ThinkingConfig.IncludeThoughts {
		parts = append(parts, map[string]interface{}{"text": t.reasoning, "thought": true})
	}
	if t.text != "" {
		parts = append(parts, map[string]interface{}{"text": t.text})
	}
	parts = append(parts, callParts(t.calls)...)
	parts = append(parts, artifactParts(t.artifacts)...)
	if len(parts) == 0 {
		parts = append(parts, map[string]interface{}{"text": ""})
	}
	writeJSON(w, 200, map[string]interface{}{
		"candidates":    []interface{}{geminiCandidate(parts, "STOP")},
		"usageMetadata": geminiUsage(g.prompt, t),
		"modelVersion":  g.modelName,
		"responseId":    randHex(16),
	})
}

// geminiStream 写 streamGenerateContent 的响应。两种帧格式：
//   - ?alt=sse：SSE，每块一行 "data: <json>"（SDK 默认就带这个参数）
//   - 不带：一个 JSON 数组，元素逐块写出（Google REST 的原始写法）
//
// 跟 sseWriter 一样第一块才发响应头：开流前出的错还能回正常的 HTTP 状态码。
type geminiStream struct {
	w       http.ResponseWriter
	flusher http.Flusher
	sse     bool
	n       int // 已写出的块数
	model   string
	rid     string
}

func (s *geminiStream) Started() bool { return s.n > 0 }

func (s *geminiStream) write(obj interface{}) {
	b, _ := json.Marshal(obj)
	if s.n == 0 {
		if s.sse {
			s.w.Header().Set("Content-Type", "text/event-stream")
			s.w.Header().Set("Cache-Control", "no-cache")
			s.w.Header().Set("X-Accel-Buffering", "no") // 关掉反代对 SSE 的缓冲，见 sse.go
		} else {
			s.w.Header().Set("Content-Type", "application/json")
		}
		s.w.Header().Set("Access-Control-Allow-Origin", "*")
		s.w.WriteHeader(200)
		if !s.sse {
			io.WriteString(s.w, "[")
		}
	}
	switch {
	case s.sse:
		fmt.Fprintf(s.w, "data: %s\r\n\r\n", b)
	case s.n > 0:
		fmt.Fprintf(s.w, ",\r\n%s", b)
	default:
		s.w.Write(b)
	}
	s.n++
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func (s *geminiStream) chunk(parts []interface{}, finish string, usage map[string]int) {
	c := map[string]interface{}{
		"candidates":   []interface{}{geminiCandidate(parts, finish)},
		"modelVersion": s.model,
		"responseId":   s.rid,
	}
	if usage != nil {
		c["usageMetadata"] = usage
	}
	s.write(c)
}

func (s *geminiStream) text(d string) {
	s.chunk([]interface{}{map[string]interface{}{"text": d}}, "", nil)
}

// ping 保活（见 keepalive.go）：发一块空正文。不用 SSE 注释行，因为这条路还有
// JSON 数组写法，而且 Google 的 SDK 按行 json 解析 data，注释行会让它解析失败；
// 空 text 块是合法的 Gemini 响应块，客户端拼接正文时等于没有。
func (s *geminiStream) ping() {
	s.text("")
}

func (s *geminiStream) thought(d string) {
	s.chunk([]interface{}{map[string]interface{}{"text": d, "thought": true}}, "", nil)
}

// fail 在已开流后报错：HTTP 状态码改不了了，就把 Google 形状的错误体当最后一块发出去。
func (s *geminiStream) fail(err error) {
	s.write(map[string]interface{}{"error": map[string]interface{}{
		"code": 502, "message": err.Error(), "status": geminiStatus(502)}})
	s.end()
}

func (s *geminiStream) end() {
	if !s.sse && s.n > 0 {
		io.WriteString(s.w, "]")
	}
}

func handleStreamGenerateContent(w http.ResponseWriter, r *http.Request, model string) {
	g, ok := prepareGemini(w, r, model)
	if !ok {
		return
	}
	f, _ := w.(http.Flusher)
	st := &geminiStream{w: w, flusher: f, sse: r.URL.Query().Get("alt") == "sse",
		model: g.modelName, rid: randHex(16)}
	includeThoughts := g.req.GenerationConfig.ThinkingConfig.IncludeThoughts

	// 带工具时正文过围栏闸门，跟 OpenAI 两条路一样：围栏原文不能推给客户端。
	var gate *toolFenceGate
	onDelta := st.text
	if len(g.tools) > 0 {
		gate = newToolFenceGate(st.text)
		onDelta = gate.Push
	}
	var onReasoning func(string)
	if includeThoughts {
		onReasoning = st.thought
	}
	ka := startKeepalive(st.ping)
	onDelta, onReasoning = ka.guard(onDelta), ka.guard(onReasoning)

	text, calls, res, err := runGeminiCall(g, onDelta, onReasoning)
	ka.Stop() // 之后的收尾回到单 goroutine 写；Started() 也要在保活停下后再看
	if err != nil {
		status := 502
		if _, ok := err.(*ModelDowngradedError); ok {
			status = 429
		}
		recordRequest("gemini.streamGenerateContent", g.modelName, g.prompt, "", res, status, err.Error(), true)
		if st.Started() {
			st.fail(err)
			return
		}
		writeGeminiCallError(w, err)
		return
	}
	recordRequest("gemini.streamGenerateContent", g.modelName, g.prompt, text, res, 200, "", true)

	t := newGeminiTurn(text, calls, res)
	// 补发真流式没推到的尾巴：思考在前、正文在后，保持「先思考后回答」的顺序。
	if includeThoughts && res != nil && t.reasoning != "" {
		if rest := remainingOf(t.reasoning, res.EmittedReasoning); rest != "" {
			st.thought(rest)
		}
	}
	var parts []interface{}
	if rest := remainingOf(t.text, sentText(res, gate)); rest != "" {
		parts = append(parts, map[string]interface{}{"text": rest})
	}
	parts = append(parts, callParts(t.calls)...)
	parts = append(parts, artifactParts(t.artifacts)...)
	if len(parts) == 0 {
		parts = append(parts, map[string]interface{}{"text": ""})
	}
	st.chunk(parts, "STOP", geminiUsage(g.prompt, t))
	st.end()
}
