package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const Version = "4.9.0"

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	body, _ := json.Marshal(data)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	w.Write(body)
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	var data []map[string]interface{}
	for name, c := range availableModels() {
		data = append(data, map[string]interface{}{
			"id":          name,
			"object":      "model",
			"created":     1700000000,
			"owned_by":    "google",
			"description": c.Desc,
		})
	}
	writeJSON(w, 200, map[string]interface{}{"object": "list", "data": data})
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	var modelNames []string
	for n := range availableModels() {
		modelNames = append(modelNames, n)
	}
	writeJSON(w, 200, map[string]interface{}{
		"status":  "ok",
		"version": Version,
		"models":  modelNames,
	})
}

func handleOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.WriteHeader(204)
}

// buildUsage 用 tiktoken 算 token 数，跟 requests 表里记的口径保持一致。
// responsesAPI=true 时用 /v1/responses 的字段名（input_tokens/output_tokens）。
func buildUsage(prompt, text string, responsesAPI bool) map[string]int {
	return buildUsageWithReasoning(prompt, text, "", responsesAPI)
}

// buildUsageWithReasoning 在 usage 里单列思考链的 token 数。
//
// 思考链**不计入** completion_tokens：它是模型自己的推理过程，客户端默认不展示，
// 算进去等于让用户为看不见的输出买单（下游 newapi 是按 completion_tokens 计费的）。
// 单列在 completion_tokens_details.reasoning_tokens 里，跟 OpenAI 的做法一致，
// 想计费的人自己加。
func buildUsageWithReasoning(prompt, text, reasoning string, responsesAPI bool) map[string]int {
	// 媒体产物是 base64 data URL，上百万字符。不计进 output token：那是二进制内容，
	// 按 token 计费等于让用户为看不见的字节买单，下游 newapi 按 output token 收钱。
	in, out := countTokens(prompt), countTokens(stripDataURLs(text))
	if responsesAPI {
		return map[string]int{
			"input_tokens":  in,
			"output_tokens": out,
			"total_tokens":  in + out,
		}
	}
	u := map[string]int{
		"prompt_tokens":     in,
		"completion_tokens": out,
		"total_tokens":      in + out,
	}
	if reasoning != "" {
		u["reasoning_tokens"] = countTokens(reasoning)
	}
	return u
}

// rejectUnsupported 检查客户端传了但我们兑现不了的字段。
//
// 只拦"静默忽略会让客户端拿到错误结果"的：n>1 少给候选、图片输入被丢掉会
// 让模型答非所问。采样类参数（temperature/top_p/max_tokens/...）上游根本
// 没有对应旋钮，收下忽略即可，报错反而会挡住正常客户端。
func rejectUnsupported(req map[string]interface{}, messages []map[string]interface{}) error {
	if n, ok := req["n"].(float64); ok && n > 1 {
		return fmt.Errorf("n=%d not supported: upstream returns a single candidate", int(n))
	}
	for _, m := range messages {
		parts, ok := m["content"].([]interface{})
		if !ok {
			continue
		}
		for _, c := range parts {
			cm, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			switch getStr(cm, "type") {
			case "image_url", "input_image":
				// 有 cookie 就能收：图会被上传成附件再引用。匿名不行 —— 传得上去，
				// 但一引用就被服务端拒，收下只会让客户端拿到一个看不懂的失败。
				if !hasCookie() {
					return fmt.Errorf("image input needs a Google account cookie: anonymous " +
						"uploads succeed but referencing them in a conversation is rejected " +
						"upstream. Add a cookie in the admin panel (Cookie pool)")
				}
			case "input_audio":
				return fmt.Errorf("audio input not supported")
			}
		}
	}
	return nil
}

func callGemini(prompt, latest string, mc ModelConfig, tools []map[string]interface{},
	images []pendingUpload, onDelta, onReasoning func(string)) (string, []ToolCall, *StreamResult, error) {
	res, err := streamGenerateWithFiles(prompt, latest, mc, images, onDelta, onReasoning)
	if err != nil {
		// res 非 nil：失败时它只带归属（哪个号 / 哪个出口），给 recordRequest 用
		return "", nil, res, err
	}
	text := extractResponseText(res.Raw)
	// 媒体模型（生图/音乐）：生成 200 了但产物字节没取回来，直接报错而不是返回一个
	// 只有文字没有图的半成品 —— 客户端要的就是那张图/那段乐。
	if mc.Tool == toolImage || mc.Tool == toolMusic {
		if len(res.Artifacts) == 0 {
			msg := res.MediaErr
			if msg == "" {
				msg = "媒体产物取回失败"
			}
			return "", nil, res, fmt.Errorf("media generation succeeded but artifact retrieval failed: %s", msg)
		}
		// 产物以 base64 data URL 追加到正文（可能没正文，只有图）。
		text = appendArtifactMarkdown(text, res.Artifacts)
		return text, nil, res, nil
	}
	if text == "" {
		// 上游拒绝时只回一个结束帧、没有内容帧（实测被拒时 raw 仅 216
		// 字节）。这种情况必须报错：以前会当成空回复返回 200 + content:null，
		// 客户端看不出请求其实失败了。
		// 注意不能用 BardErrorInfo 判错 —— 正常响应的结束帧里也带这个码。
		return "", nil, res, fmt.Errorf("upstream returned no content frame (raw %d bytes)", len(res.Raw))
	}
	var toolCalls []ToolCall
	if len(tools) > 0 {
		text, toolCalls = parseToolCalls(text)
	}
	return text, toolCalls, res, nil
}

// recordRequest writes one row of metadata to the requests table.
// Privacy: the prompt/response strings themselves are never persisted —
// only their length, model name, latency, status, and proxy info.
func recordRequest(endpoint, model, prompt, response string, res *StreamResult, status int, errStr string, stream bool) {
	// 媒体产物是超长 base64，剥掉再算 token/长度：既不让它污染统计，也免得在请求线程里
	// 对上百万字符跑 tiktoken 白白拖慢。
	response = stripDataURLs(response)
	r := &RequestRow{
		TS:            time.Now().Unix(),
		Model:         model,
		Status:        status,
		Error:         errStr,
		PromptChars:   len(prompt),
		ResponseChars: len(response),
		PromptTokens:  countTokens(prompt),
		OutputTokens:  countTokens(response),
		Endpoint:      endpoint,
	}
	if stream {
		r.Stream = 1
	}
	if res != nil {
		r.UpstreamModel = res.UpstreamModel
		r.TotalMs = res.TotalMs
		r.TTFBMs = &res.TTFBMs
		if res.ProxyID > 0 {
			r.ProxyID = &res.ProxyID
		}
		r.ProxyName = res.ProxyName
		if res.AccountID > 0 {
			r.AccountID = &res.AccountID
		}
		r.AccountLabel = res.AccountLabel
	}
	go insertRequest(r)
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, 400, map[string]interface{}{"error": map[string]string{"message": err.Error()}})
		return
	}
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, 400, map[string]interface{}{"error": map[string]string{"message": "invalid JSON"}})
		return
	}

	modelInput := getStr(req, "model")
	if modelInput == "" {
		modelInput = rtCfg().DefaultModel
	}
	modelName, modelCfg, err := resolveModel(modelInput)
	if err != nil {
		writeJSON(w, 400, map[string]interface{}{"error": map[string]string{"message": err.Error()}})
		return
	}

	messagesRaw, _ := req["messages"].([]interface{})
	var messages []map[string]interface{}
	for _, m := range messagesRaw {
		if mm, ok := m.(map[string]interface{}); ok {
			messages = append(messages, mm)
		}
	}

	toolsRaw, _ := req["tools"].([]interface{})
	var tools []map[string]interface{}
	for _, t := range toolsRaw {
		if tm, ok := t.(map[string]interface{}); ok {
			tools = append(tools, tm)
		}
	}

	if err := rejectUnsupported(req, messages); err != nil {
		writeJSON(w, 400, map[string]interface{}{"error": map[string]string{
			"message": err.Error(), "type": "invalid_request_error"}})
		return
	}

	// 图片先解出来带着，真正上传要等挑完账号和出口（见 streamGenerate）。
	images, err := collectImages(messages, "")
	if err != nil {
		writeJSON(w, 400, map[string]interface{}{"error": map[string]string{
			"message": err.Error(), "type": "invalid_request_error"}})
		return
	}

	prompt, latest := messagesToPrompt(messages, tools, req["tool_choice"])
	if strings.TrimSpace(prompt) == "" && len(images) == 0 {
		writeJSON(w, 400, map[string]interface{}{"error": map[string]string{"message": "empty prompt"}})
		return
	}

	stream, _ := req["stream"].(bool)
	includeUsage := false
	if so, ok := req["stream_options"].(map[string]interface{}); ok {
		includeUsage, _ = so["include_usage"].(bool)
	}

	cid := "chatcmpl-" + randHex(12)
	created := time.Now().Unix()

	// 带 tools 时正文过一道围栏闸门：```tool_call``` 块要完整文本才能解析，
	// 直接转发会把围栏原文推给客户端，所以只放行确定不在围栏里的部分。
	// 思考链跟围栏无关，两种情况都直接流。
	var sse *sseWriter
	var gate *toolFenceGate
	var onDelta, onReasoning func(string)
	if stream {
		sse = newSSEWriter(w, cid, created, modelName)
		onReasoning = sse.SendReasoning
		if len(tools) == 0 {
			onDelta = sse.SendContent
		} else {
			gate = newToolFenceGate(sse.SendContent)
			onDelta = gate.Push
		}
	}

	var text string
	var toolCalls []ToolCall
	var res *StreamResult
	if rtCfg().MultiTurn && len(images) == 0 && modelCfg.Tool == 0 {
		// 多轮：按历史前缀识别续接，命中就只发新消息、历史留服务端。带 tools 也走这条。
		text, toolCalls, res, err = callGeminiConv(messages, modelCfg, tools, req["tool_choice"], onDelta, onReasoning)
	} else {
		text, toolCalls, res, err = callGemini(prompt, latest, modelCfg, tools, images, onDelta, onReasoning)
	}
	if err != nil {
		recordRequest("chat.completions", modelName, prompt, "", res, 502, err.Error(), stream)
		if sse != nil && sse.Started() {
			sse.Fail(err) // 已经开流，HTTP 状态码改不了了
			return
		}
		if ptl, ok := err.(*PromptTooLongError); ok {
			// 明确报 400 而不是发出去让上游把用户的问题截掉 —— 那样客户端拿到的是
			// 一个答非所问的 200，根本看不出请求其实没送到。
			writeJSON(w, 400, map[string]interface{}{"error": map[string]string{
				"message": ptl.Error(), "type": "invalid_request_error",
				"code": "context_length_exceeded"}})
			return
		}
		if rle, ok := err.(*RateLimitError); ok {
			writeJSON(w, 429, map[string]interface{}{"error": map[string]string{
				"message": rle.Error(),
				"type":    "rate_limit_exceeded",
				"code":    "ip_slot_full",
			}})
			return
		}
		writeJSON(w, 502, map[string]interface{}{"error": map[string]string{"message": "upstream error: " + err.Error()}})
		return
	}

	msg := map[string]interface{}{"role": "assistant"}
	if text != "" {
		msg["content"] = text
	} else {
		msg["content"] = nil
	}
	// 思考链走 reasoning_content —— DeepSeek-R1 带起来的事实标准，newapi 和
	// 主流客户端都认，会渲染成可折叠的「思考过程」。只有 3.1 Pro 有，其余为空。
	if res != nil && res.Reasoning != "" {
		msg["reasoning_content"] = res.Reasoning
	}
	finish := "stop"
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
		finish = "tool_calls"
	}

	recordRequest("chat.completions", modelName, prompt, text, res, 200, "", stream)
	if stream {
		var usage map[string]int
		if includeUsage {
			usage = usageOf(prompt, text, res)
		}
		// 真流式已发出的不重发，只补尾巴；有 tools 时没走真流式，这里发全量。
		// 思考链同理，且要在正文之前补——保持「先思考后回答」的顺序。
		if res != nil && res.Reasoning != "" {
			if rest := remainingOf(res.Reasoning, res.EmittedReasoning); rest != "" {
				sse.SendReasoning(rest)
			}
		}
		// 补尾巴要跟**实际发给客户端的内容**比。走了闸门时 res.Emitted 含围栏
		// 原文，拿它比前缀会对不上，尾巴会整段丢掉。
		if rest := remainingOf(text, sentText(res, gate)); rest != "" {
			sse.SendContent(rest)
		}
		if len(toolCalls) > 0 {
			sse.SendToolCalls(toolCalls)
		}
		sse.Finish(finish, usage)
		return
	}

	writeJSON(w, 200, map[string]interface{}{
		"id":      cid,
		"object":  "chat.completion",
		"created": created,
		"model":   modelName,
		"choices": []map[string]interface{}{{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
		"usage": usageOf(prompt, text, res),
	})
}

// usageOf 是 buildUsageWithReasoning 的便捷包装，res 可能为 nil。
func usageOf(prompt, text string, res *StreamResult) map[string]int {
	r := ""
	if res != nil {
		r = res.Reasoning
	}
	return buildUsageWithReasoning(prompt, text, r, false)
}

// remainingText 返回最终文本里还没通过 onDelta 发出去的部分。
// 真流式下通常只剩末尾一点或为空；没走真流式时 Emitted 为空，返回全文。
func remainingText(text string, res *StreamResult) string {
	if res == nil {
		return text
	}
	return remainingOf(text, res.Emitted)
}

// sentText 返回本次实际发给客户端的正文。
// 没走围栏闸门时就是 deltaTracker 发出的那些；走了闸门时以闸门为准 ——
// 闸门扣掉了围栏，跟 res.Emitted 不是同一份文本。
func sentText(res *StreamResult, gate *toolFenceGate) string {
	if gate != nil {
		return gate.Sent()
	}
	if res == nil {
		return ""
	}
	return res.Emitted
}

// remainingOf 返回 full 里还没发出去的尾巴。emitted 为空时返回全文。
func remainingOf(full, emitted string) string {
	if emitted == "" {
		return full
	}
	if strings.HasPrefix(full, emitted) {
		return full[len(emitted):]
	}
	// 前缀对不上（上游中途改写过），已发的收不回，不再补发以免重复。
	return ""
}

// handleResponses implements OpenAI's /v1/responses (Codex CLI format).
func handleResponses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, 400, map[string]interface{}{"error": map[string]string{"message": err.Error()}})
		return
	}
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, 400, map[string]interface{}{"error": map[string]string{"message": "invalid JSON"}})
		return
	}

	modelInput := getStr(req, "model")
	if modelInput == "" {
		modelInput = rtCfg().DefaultModel
	}
	modelName, modelCfg, err := resolveModel(modelInput)
	if err != nil {
		writeJSON(w, 400, map[string]interface{}{"error": map[string]string{"message": err.Error()}})
		return
	}

	var messages []map[string]interface{}
	if instr := getStr(req, "instructions"); instr != "" {
		messages = append(messages, map[string]interface{}{"role": "system", "content": instr})
	}

	switch input := req["input"].(type) {
	case string:
		messages = append(messages, map[string]interface{}{"role": "user", "content": input})
	case []interface{}:
		for _, raw := range input {
			switch item := raw.(type) {
			case string:
				messages = append(messages, map[string]interface{}{"role": "user", "content": item})
			case map[string]interface{}:
				if t := getStr(item, "type"); t == "function_call_output" {
					messages = append(messages, map[string]interface{}{
						"role":         "tool",
						"tool_call_id": getStr(item, "call_id"),
						"name":         getStr(item, "name"),
						"content":      getStr(item, "output"),
					})
					continue
				}
				role := getStr(item, "role")
				if role == "assistant" || (getStr(item, "type") == "message" && role == "assistant") {
					var textAcc strings.Builder
					var tcList []map[string]interface{}
					if cp, ok := item["content"].([]interface{}); ok {
						for _, c := range cp {
							cm, ok := c.(map[string]interface{})
							if !ok {
								continue
							}
							switch getStr(cm, "type") {
							case "output_text":
								textAcc.WriteString(getStr(cm, "text"))
							case "function_call":
								tcList = append(tcList, cm)
							}
						}
					} else if s, ok := item["content"].(string); ok {
						textAcc.WriteString(s)
					}
					m := map[string]interface{}{"role": "assistant", "content": textAcc.String()}
					if len(tcList) > 0 {
						var tcs []map[string]interface{}
						for i, tc := range tcList {
							id := getStr(tc, "call_id")
							if id == "" {
								id = fmt.Sprintf("call_%d", i)
							}
							tcs = append(tcs, map[string]interface{}{
								"id":   id,
								"type": "function",
								"function": map[string]interface{}{
									"name":      getStr(tc, "name"),
									"arguments": getStr(tc, "arguments"),
								},
							})
						}
						m["tool_calls"] = tcs
					}
					messages = append(messages, m)
				} else {
					if role == "" {
						role = "user"
					}
					content := contentToString(item["content"])
					messages = append(messages, map[string]interface{}{"role": role, "content": content})
				}
			}
		}
	}

	toolsRaw, _ := req["tools"].([]interface{})
	var tools []map[string]interface{}
	for _, t := range toolsRaw {
		tm, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		// Normalize Responses API tool shape to Chat Completions shape.
		if getStr(tm, "type") == "function" && tm["function"] == nil {
			tools = append(tools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        getStr(tm, "name"),
					"description": getStr(tm, "description"),
					"parameters":  tm["parameters"],
				},
			})
		} else {
			tools = append(tools, tm)
		}
	}

	if err := rejectUnsupported(req, messages); err != nil {
		writeJSON(w, 400, map[string]interface{}{"error": map[string]string{
			"message": err.Error(), "type": "invalid_request_error"}})
		return
	}

	images, err := collectImages(messages, "")
	if err != nil {
		writeJSON(w, 400, map[string]interface{}{"error": map[string]string{
			"message": err.Error(), "type": "invalid_request_error"}})
		return
	}

	prompt, latest := messagesToPrompt(messages, tools, req["tool_choice"])
	if strings.TrimSpace(prompt) == "" && len(images) == 0 {
		writeJSON(w, 400, map[string]interface{}{"error": map[string]string{"message": "empty input"}})
		return
	}

	stream, _ := req["stream"].(bool)
	rid := "resp_" + randHex(16)
	mid := "msg_" + randHex(12)

	// Responses 流式协议要求先 response.output_item.added 声明 item，才能对它发
	// output_text.delta；漏了 Codex 这类严格客户端会报 "OutputTextDelta without active
	// item"。msgIndex/nextIdx 给每个 output item 分配序号，ensureMsg 惰性声明 message
	// item（首个 delta 时才发，纯工具调用轮不发空 message）。
	msgIndex := -1
	nextIdx := 0
	var ensureMsg func()

	// 流式要先把头和 response.created 发出去，才能边收边推 delta。
	// 代价是一旦开了流 HTTP 状态码就改不了了，上游失败只能用 response.failed
	// 事件告知 —— 跟 /v1/chat/completions 那条路的取舍一致。
	var writeEvent func(string, interface{})
	var emitDelta func(string)
	var gate *toolFenceGate
	var onDelta func(string)
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		writeEvent = func(eventType string, payload interface{}) {
			pj, _ := json.Marshal(payload)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, pj)
			if flusher != nil {
				flusher.Flush()
			}
		}
		writeEvent("response.created", map[string]interface{}{
			"type": "response.created",
			"response": map[string]interface{}{
				"id":     rid,
				"object": "response",
				"status": "in_progress",
				"model":  modelName,
				"output": []interface{}{},
			},
		})
		ensureMsg = func() {
			if msgIndex >= 0 {
				return
			}
			msgIndex = nextIdx
			nextIdx++
			writeEvent("response.output_item.added", map[string]interface{}{
				"type":         "response.output_item.added",
				"output_index": msgIndex,
				"item": map[string]interface{}{
					"id": mid, "type": "message", "role": "assistant",
					"status": "in_progress", "content": []interface{}{},
				},
			})
			writeEvent("response.content_part.added", map[string]interface{}{
				"type": "response.content_part.added", "item_id": mid,
				"output_index": msgIndex, "content_index": 0,
				"part": map[string]interface{}{"type": "output_text", "text": "", "annotations": []interface{}{}},
			})
		}
		emitDelta = func(d string) {
			ensureMsg()
			writeEvent("response.output_text.delta", map[string]interface{}{
				"type":          "response.output_text.delta",
				"item_id":       mid,
				"output_index":  msgIndex,
				"content_index": 0,
				"delta":         d,
			})
		}
		// 跟 chat 那条路同一套围栏闸门：带 tools 时只放行确定不在围栏里的部分。
		if len(tools) == 0 {
			onDelta = emitDelta
		} else {
			gate = newToolFenceGate(emitDelta)
			onDelta = gate.Push
		}
	}

	// onReasoning 传 nil：Responses API 有自己的 reasoning 事件形状，跟 chat 的
	// reasoning_content 不通用，这条路目前不暴露思考链。
	var text string
	var toolCalls []ToolCall
	var res *StreamResult
	if rtCfg().MultiTurn && len(images) == 0 && modelCfg.Tool == 0 {
		text, toolCalls, res, err = callGeminiConv(messages, modelCfg, tools, req["tool_choice"], onDelta, nil)
	} else {
		text, toolCalls, res, err = callGemini(prompt, latest, modelCfg, tools, images, onDelta, nil)
	}
	if err != nil {
		recordRequest("responses", modelName, prompt, "", res, 502, err.Error(), stream)
		if stream {
			writeEvent("response.failed", map[string]interface{}{
				"type": "response.failed",
				"response": map[string]interface{}{
					"id":     rid,
					"object": "response",
					"status": "failed",
					"model":  modelName,
					"error":  map[string]string{"message": err.Error()},
				},
			})
			return
		}
		if ptl, ok := err.(*PromptTooLongError); ok {
			// 明确报 400 而不是发出去让上游把用户的问题截掉 —— 那样客户端拿到的是
			// 一个答非所问的 200，根本看不出请求其实没送到。
			writeJSON(w, 400, map[string]interface{}{"error": map[string]string{
				"message": ptl.Error(), "type": "invalid_request_error",
				"code": "context_length_exceeded"}})
			return
		}
		if rle, ok := err.(*RateLimitError); ok {
			writeJSON(w, 429, map[string]interface{}{"error": map[string]string{
				"message": rle.Error(),
				"type":    "rate_limit_exceeded",
				"code":    "ip_slot_full",
			}})
			return
		}
		writeJSON(w, 502, map[string]interface{}{"error": map[string]string{"message": "upstream error: " + err.Error()}})
		return
	}

	var output []map[string]interface{}
	for _, tc := range toolCalls {
		output = append(output, map[string]interface{}{
			"type":      "function_call",
			"id":        tc.ID,
			"call_id":   tc.ID,
			"name":      tc.Function.Name,
			"arguments": tc.Function.Arguments,
			"status":    "completed",
		})
	}
	if text != "" || len(toolCalls) == 0 {
		output = append(output, map[string]interface{}{
			"type":   "message",
			"id":     mid,
			"role":   "assistant",
			"status": "completed",
			"content": []map[string]interface{}{{
				"type":        "output_text",
				"text":        text,
				"annotations": []interface{}{},
			}},
		})
	}

	recordRequest("responses", modelName, prompt, text, res, 200, "", stream)
	if stream {
		// 补上闸门扣住、或前缀 diff 跳过的尾巴，再发终态事件。
		if rest := remainingOf(text, sentText(res, gate)); rest != "" {
			emitDelta(rest)
		}
		for _, item := range output {
			switch item["type"] {
			case "function_call":
				// 工具调用 item 也要 added → done 包起来，Codex 才认。
				idx := nextIdx
				nextIdx++
				writeEvent("response.output_item.added", map[string]interface{}{
					"type": "response.output_item.added", "output_index": idx, "item": item,
				})
				writeEvent("response.function_call_arguments.done", map[string]interface{}{
					"type":      "response.function_call_arguments.done",
					"item_id":   item["id"],
					"call_id":   item["call_id"],
					"name":      item["name"],
					"arguments": item["arguments"],
				})
				writeEvent("response.output_item.done", map[string]interface{}{
					"type": "response.output_item.done", "output_index": idx, "item": item,
				})
			case "message":
				ensureMsg() // 没有 delta 但有正文时，这里补声明 message item
				if cps, ok := item["content"].([]map[string]interface{}); ok {
					for ci, cp := range cps {
						writeEvent("response.output_text.done", map[string]interface{}{
							"type":          "response.output_text.done",
							"item_id":       item["id"],
							"output_index":  msgIndex,
							"content_index": ci,
							"text":          cp["text"],
						})
						writeEvent("response.content_part.done", map[string]interface{}{
							"type": "response.content_part.done", "item_id": item["id"],
							"output_index": msgIndex, "content_index": ci,
							"part": map[string]interface{}{"type": "output_text", "text": cp["text"], "annotations": []interface{}{}},
						})
					}
				}
				writeEvent("response.output_item.done", map[string]interface{}{
					"type": "response.output_item.done", "output_index": msgIndex, "item": item,
				})
			}
		}
		respObj := map[string]interface{}{
			"id":     rid,
			"object": "response",
			"status": "completed",
			"model":  modelName,
			"output": output,
			"usage":  buildUsage(prompt, text, true),
		}
		writeEvent("response.completed", map[string]interface{}{
			"type":     "response.completed",
			"response": respObj,
		})
		return
	}

	writeJSON(w, 200, map[string]interface{}{
		"id":         rid,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     "completed",
		"model":      modelName,
		"output":     output,
		"usage":      buildUsage(prompt, text, true),
	})
}
