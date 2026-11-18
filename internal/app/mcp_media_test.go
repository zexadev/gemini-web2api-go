package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func mcpCall(t *testing.T, method string, params any) map[string]interface{} {
	t.Helper()
	raw, _ := json.Marshal(params)
	resp := dispatchMCP(&rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: method, Params: raw}, nil)
	if resp == nil || resp.Error != nil {
		t.Fatalf("%s: %+v", method, resp)
	}
	out, _ := json.Marshal(resp.Result)
	var m map[string]interface{}
	_ = json.Unmarshal(out, &m)
	return m
}

func TestMCPListsMediaTools(t *testing.T) {
	m := mcpCall(t, "tools/list", map[string]any{})
	var names []string
	for _, it := range m["tools"].([]interface{}) {
		names = append(names, it.(map[string]interface{})["name"].(string))
	}
	got := strings.Join(names, ",")
	if got != "web_search,generate_image,generate_music,create_video_task,get_video_status" {
		t.Fatalf("tools = %s", got)
	}
}

func TestMCPMediaToolNeedsPrompt(t *testing.T) {
	m := mcpCall(t, "tools/call", map[string]any{"name": "generate_image", "arguments": map[string]any{"prompt": "  "}})
	if m["isError"] != true {
		t.Fatalf("empty prompt should be a tool error: %v", m)
	}
}

// 产物按 mime 映射成 MCP 原生内容类型；没有对应类型的走 embedded resource。
func TestMCPMediaContent(t *testing.T) {
	cases := map[string]string{"image/png": "image", "audio/mpeg": "audio", "video/mp4": "resource"}
	for mime, want := range cases {
		c := mcpMediaContent(MediaArtifact{Mime: mime, Data: []byte("xyz")})
		if c["type"] != want {
			t.Errorf("%s -> %v, want %s", mime, c["type"], want)
		}
		switch want {
		case "resource":
			r := c["resource"].(map[string]interface{})
			if r["blob"] != "eHl6" || r["mimeType"] != mime {
				t.Errorf("resource = %v", r)
			}
		default:
			if c["data"] != "eHl6" || c["mimeType"] != mime {
				t.Errorf("%s content = %v", mime, c)
			}
		}
	}
}

func TestMCPVideoTools(t *testing.T) {
	m := mcpCall(t, "tools/call", map[string]any{"name": "create_video_task", "arguments": map[string]any{"prompt": "a fox", "size": "1:1"}})
	if m["isError"] != true {
		t.Fatalf("unsupported size should be a tool error: %v", m)
	}
	m = mcpCall(t, "tools/call", map[string]any{"name": "get_video_status", "arguments": map[string]any{"id": "video_nope"}})
	if m["isError"] != true {
		t.Fatalf("unknown id should be a tool error: %v", m)
	}

	putVideoJob(&videoJob{ID: "video_mcp1", Object: "video", Model: "gemini-video", Status: "completed",
		CreatedAt: time.Now().Unix(), mp4: []byte("mp4"), mime: "video/mp4"})
	// 不要视频时只回状态文本
	m = mcpCall(t, "tools/call", map[string]any{"name": "get_video_status", "arguments": map[string]any{"id": "video_mcp1"}})
	content := m["content"].([]interface{})
	if len(content) != 1 || !strings.Contains(content[0].(map[string]interface{})["text"].(string), `"status":"completed"`) {
		t.Fatalf("status only: %v", m)
	}
	// include_video=true 时附上 MP4
	m = mcpCall(t, "tools/call", map[string]any{"name": "get_video_status", "arguments": map[string]any{"id": "video_mcp1", "include_video": true}})
	content = m["content"].([]interface{})
	if len(content) != 2 || content[1].(map[string]interface{})["type"] != "resource" {
		t.Fatalf("with video: %v", m)
	}
}
