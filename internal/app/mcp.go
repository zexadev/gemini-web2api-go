package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// MCP（Model Context Protocol）服务器：把 Gemini 网页端的联网搜索暴露成
// web_search 工具，生图 / 音乐暴露成 generate_image / generate_music，视频暴露成
// create_video_task / get_video_status（见 mcp_media.go），
// 供 Claude Desktop / Claude Code / Cursor 等 MCP 客户端调用。
//
// 传输走 HTTP（Streamable HTTP），挂在后端现有 --port 上的 /mcp，跟 OpenAI 接口
// 同一个进程、同一个端口——部署成服务器后远程客户端连 URL 就能用，复用现成的
// 账号池 / 代理池 / 限流。（没做 stdio 那种客户端拉起本地子进程的传输。）
//
// 手写 JSON-RPC 2.0 而不引第三方 SDK：协议面很小（initialize / tools/list /
// tools/call），手写没有依赖、跟单二进制的风格一致。
//
// 搜索走 streamGenerate，白嫖现有的代理池 / 限流 / 重试 / 防封。匿名即可搜，
// 所以不依赖 cookie 池；媒体工具跟 chat 里的 gemini-image / gemini-music 一样要登录态。

const mcpProtocolVersion = "2025-06-18"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// handleMCPHTTP 是 MCP 的 HTTP 传输（Streamable HTTP），挂在后端 `/mcp` 上，跟
// OpenAI 接口同一个进程、同一个端口，复用现有的账号池 / 代理池 / 限流。
//
// 客户端 POST 一条 JSON-RPC 消息，我们回一条 application/json 响应。工具场景是
// 纯请求-响应，不需要服务端主动推送，所以不开 SSE 流：GET 直接回 405。
// 通知类（无 id）按规范回 202 空体。
func handleMCPHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Mcp-Session-Id, MCP-Protocol-Version")
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
		// 不提供服务端主动推送的 SSE 流；规范允许这么回。
		w.Header().Set("Allow", "POST")
		http.Error(w, "this MCP endpoint is POST-only (no server-initiated stream)", http.StatusMethodNotAllowed)
		return
	case http.MethodPost:
		// 落到下面处理
	default:
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeMCPHTTPError(w, nil, -32700, "read error")
		return
	}
	var req rpcRequest
	if json.Unmarshal(body, &req) != nil {
		writeMCPHTTPError(w, nil, -32700, "parse error")
		return
	}
	resp := dispatchMCP(&req)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted) // 通知：无响应体
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(resp)
	w.Write(b)
}

func writeMCPHTTPError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
	w.Write(b)
}

func dispatchMCP(req *rpcRequest) *rpcResponse {
	// 没有 id 的是通知（notifications/initialized 等），不回响应。
	if len(req.ID) == 0 {
		return nil
	}
	ok := func(result interface{}) *rpcResponse {
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
	}
	fail := func(code int, msg string) *rpcResponse {
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: code, Message: msg}}
	}

	switch req.Method {
	case "initialize":
		// 回显客户端请求的协议版本（拿不到就用我们支持的）。
		ver := mcpProtocolVersion
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(req.Params, &p) == nil && p.ProtocolVersion != "" {
			ver = p.ProtocolVersion
		}
		return ok(map[string]interface{}{
			"protocolVersion": ver,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]interface{}{"name": "gemini-search-mcp", "version": Version},
		})

	case "tools/list":
		return ok(map[string]interface{}{"tools": []interface{}{
			webSearchToolDef(), generateImageToolDef(), generateMusicToolDef(),
			createVideoTaskToolDef(), getVideoStatusToolDef()}})

	case "tools/call":
		var p struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if json.Unmarshal(req.Params, &p) != nil {
			return fail(-32602, "invalid params")
		}
		switch p.Name {
		case "web_search":
			query := strings.TrimSpace(getStr(p.Arguments, "query"))
			if query == "" {
				return ok(toolTextError("query 不能为空"))
			}
			text, err := mcpWebSearch(query)
			if err != nil {
				return ok(toolTextError("搜索失败: " + err.Error()))
			}
			return ok(map[string]interface{}{
				"content": []interface{}{map[string]interface{}{"type": "text", "text": text}},
			})
		case "generate_image", "generate_music":
			prompt := strings.TrimSpace(getStr(p.Arguments, "prompt"))
			if prompt == "" {
				return ok(toolTextError("prompt 不能为空"))
			}
			model, refs := "gemini-image", mcpStrings(p.Arguments["image_urls"])
			if p.Name == "generate_music" {
				model, refs = "gemini-music", nil
			}
			result, err := mcpGenerateMedia(model, prompt, refs)
			if err != nil {
				return ok(toolTextError("生成失败: " + err.Error()))
			}
			return ok(result)
		case "create_video_task":
			result, err := mcpCreateVideo(p.Arguments)
			if err != nil {
				return ok(toolTextError("建任务失败: " + err.Error()))
			}
			return ok(result)
		case "get_video_status":
			j := getVideoJob(strings.TrimSpace(getStr(p.Arguments, "id")))
			if j == nil {
				return ok(toolTextError("video task not found"))
			}
			withVideo, _ := p.Arguments["include_video"].(bool)
			return ok(mcpVideoJobResult(j, withVideo))
		default:
			return fail(-32602, "unknown tool: "+p.Name)
		}

	case "ping":
		return ok(map[string]interface{}{})

	default:
		return fail(-32601, "method not found: "+req.Method)
	}
}

// webSearchToolDef 是 web_search 工具的定义（含 JSON Schema）。
func webSearchToolDef() map[string]interface{} {
	return map[string]interface{}{
		"name": "web_search",
		"description": "Search the web via Google Gemini and return a synthesized answer " +
			"with source links. Use for current events, facts, or anything needing " +
			"up-to-date information from the internet.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "The search query or question to research.",
				},
			},
			"required": []interface{}{"query"},
		},
	}
}

// mcpWebSearch 执行一次联网搜索，返回"答案 + 来源清单"的文本。
func mcpWebSearch(query string) (string, error) {
	mc, ok := Models[rtCfg().DefaultModel]
	if !ok || mc.Retired != "" {
		mc = Models[fallbackDefaultModel]
	}
	res, err := streamGenerate(query, query, mc, nil, nil)
	if err != nil {
		return "", err
	}
	text := extractResponseText(res.Raw)
	if text == "" {
		return "", fmt.Errorf("上游没有返回内容")
	}
	sources := extractGrounding(res.Raw)
	var b strings.Builder
	b.WriteString(text)
	if len(sources) > 0 {
		b.WriteString("\n\n---\nSources:\n")
		for i, s := range sources {
			title := s.Title
			if title == "" {
				title = s.URL
			}
			fmt.Fprintf(&b, "%d. %s — %s\n", i+1, title, s.URL)
		}
	}
	return b.String(), nil
}

func toolTextError(msg string) map[string]interface{} {
	return map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text", "text": msg}},
		"isError": true,
	}
}
