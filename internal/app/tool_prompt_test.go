package app

import (
	"strings"
	"testing"
)

// Long Agent instructions and earlier capability disclaimers must be followed
// by the external execution protocol, including for search-only tool lists.
func TestSearchToolBridgeFollowsHistory(t *testing.T) {
	tools := []map[string]interface{}{{"type": "function", "function": map[string]interface{}{
		"name": "web_search", "description": "Search current information",
		"parameters": map[string]interface{}{"type": "object"},
	}}}
	messages := []map[string]interface{}{
		{"role": "developer", "content": strings.Repeat("Agent instructions. ", 2500)},
		{"role": "assistant", "content": "当前环境未开启实时联网检索"},
		{"role": "user", "content": "请实际调用搜索工具"},
	}
	prompt, _ := messagesToPrompt(messages, tools, "auto")
	lastRequest := strings.LastIndex(prompt, "请实际调用搜索工具")
	bridge := strings.LastIndex(prompt, toolExecutionBridge)
	if bridge <= lastRequest {
		t.Fatal("external execution protocol must follow the latest request and stale disclaimer")
	}
	for _, want := range []string{"external Agent client", "not a Gemini native tool invocation", "search, browsing or network tool", "Respect explicit client permission requirements"} {
		if !strings.Contains(prompt[bridge:], want) {
			t.Errorf("search bridge lost %q", want)
		}
	}
	for _, choice := range []interface{}{nil, "none"} {
		activeTools := tools
		if choice == nil {
			activeTools = nil
		}
		p, _ := messagesToPrompt(messages, activeTools, choice)
		if strings.Contains(p, toolExecutionBridge) {
			t.Errorf("must not advertise tools without enabled definitions: choice=%v", choice)
		}
	}
}

func TestContinuationToolPolicy(t *testing.T) {
	tools := []map[string]interface{}{
		{"type": "function", "function": map[string]interface{}{"name": "web_search", "description": "schema-description-kept-upstream"}},
		{"type": "function", "function": map[string]interface{}{"name": "read_file"}},
	}
	reminder := toolContinuationReminder(tools, map[string]interface{}{"type": "function", "name": "web_search"})
	if !strings.Contains(reminder, `MUST call the tool "web_search"`) || !strings.Contains(reminder, `Allowed tool names: ["web_search"]`) {
		t.Fatalf("continuation lost forced Responses tool policy: %s", reminder)
	}
	for _, forbidden := range []string{"read_file", "schema-description-kept-upstream", "Available tools:"} {
		if strings.Contains(reminder, forbidden) {
			t.Errorf("continuation resent a filtered tool or schema: %q", forbidden)
		}
	}
	if toolContinuationReminder(tools, "none") != "" || toolContinuationReminder(nil, "auto") != "" {
		t.Fatal("disabled or absent tools must not receive an execution reminder")
	}
}
