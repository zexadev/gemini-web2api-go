package app

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSendToolCallsIndex(t *testing.T) {
	rec := httptest.NewRecorder()
	s := &sseWriter{w: rec}

	s.SendToolCalls([]ToolCall{
		{ID: "a", Type: "function", Function: ToolCallFunction{Name: "f1", Arguments: `{"x":1}`}},
		{ID: "b", Type: "function", Function: ToolCallFunction{Name: "f2", Arguments: `{}`}},
	})

	chunk := rec.Body.String()
	if !strings.Contains(chunk, `"index":0`) {
		t.Fatalf("missing tool_call index:0 in %q", chunk)
	}
	// 两个 tool_call 各自的 index=0,1 必须都出现；"index":1 只可能来自第二个 tool_call
	if !strings.Contains(chunk, `"index":1`) {
		t.Fatalf("missing tool_call index:1 in %q", chunk)
	}
	if !strings.Contains(chunk, `"f1"`) || !strings.Contains(chunk, `"f2"`) {
		t.Fatalf("missing function names in %q", chunk)
	}
	t.Logf("OK: %s", chunk)
}