package app

import (
	"strings"
	"testing"
)

func recoveryTestTools() []map[string]interface{} {
	return []map[string]interface{}{{"type": "function", "name": "web_search"}}
}

func TestToolRecoveryRequiresUncommittedCapabilityClaim(t *testing.T) {
	denial := "当前对话的外部搜索与工具调用额度已耗尽，无法继续联网检索。"
	for _, text := range []string{denial, "抱歉，我目前无法进行联网检索。", "当前会话未启用工具执行权限。", "目前工具调用次数剩余 0 次。", "Sorry, the tool budget is exhausted.", "I cannot use the search tool."} {
		if !shouldRecoverToolReply(nil, recoveryTestTools(), "auto", text, nil, false) {
			t.Errorf("uncommitted capability claim was not eligible: %s", text)
		}
	}
	for _, text := range []string{"好的，我会搜索。", "工具预算不足是一个可能的错误。", "当前项目需要检查工具配置。", "根据工具结果，额度已耗尽。", "当前工具额度尚未耗尽，可以继续使用。", "当前工具额度已耗尽的提示是误报。", "The current tool budget is not exhausted.", "", "答案是 42。当前工具额度已耗尽。"} {
		if shouldRecoverToolReply(nil, recoveryTestTools(), "auto", text, nil, false) {
			t.Errorf("ordinary text must not trigger recovery: %s", text)
		}
	}
	tests := []struct {
		name     string
		messages []map[string]interface{}
		tools    []map[string]interface{}
		choice   interface{}
		calls    []ToolCall
		emitted  bool
	}{
		{name: "already emitted", tools: recoveryTestTools(), emitted: true},
		{name: "real call", tools: recoveryTestTools(), calls: []ToolCall{{ID: "call_one"}}},
		{name: "tool result", tools: recoveryTestTools(), messages: []map[string]interface{}{{"role": "tool", "content": "quota exceeded"}}},
		{name: "successful tool result", tools: recoveryTestTools(), messages: []map[string]interface{}{{"role": "tool", "content": "success"}}},
		{name: "raw Responses result", tools: recoveryTestTools(), messages: []map[string]interface{}{{"type": "function_call_output", "output": "quota exceeded"}}},
		{name: "none", tools: recoveryTestTools(), choice: "none"},
		{name: "explicit budget", tools: recoveryTestTools(), messages: []map[string]interface{}{{"role": "system", "content": "Tool budget: 0"}}},
		{name: "explicit Chinese budget", tools: recoveryTestTools(), messages: []map[string]interface{}{{"role": "user", "content": "工具剩余次数：0"}}},
		{name: "explicit Chinese prohibition", tools: recoveryTestTools(), messages: []map[string]interface{}{{"role": "user", "content": "不要调用任何工具"}}},
		{name: "explicit English prohibition", tools: recoveryTestTools(), messages: []map[string]interface{}{{"role": "system", "content": "Do not use tools."}}},
		{name: "no definitions"},
		{name: "native search only", tools: []map[string]interface{}{{"type": "web_search"}}},
		{name: "custom only", tools: []map[string]interface{}{{"type": "custom", "name": "web_search"}}},
		{name: "missing forced function", tools: recoveryTestTools(), choice: map[string]interface{}{"type": "function", "name": "missing"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if shouldRecoverToolReply(test.messages, test.tools, test.choice, denial, test.calls, test.emitted) {
				t.Fatal("must not silently regenerate this response")
			}
		})
	}
	chatTools := []map[string]interface{}{{"type": "function", "function": map[string]interface{}{"name": "web_search"}}}
	if !shouldRecoverToolReply(nil, chatTools, map[string]interface{}{"type": "function", "name": "web_search"}, denial, nil, false) {
		t.Fatal("a real forced standard function should remain eligible")
	}
	if !canRecoverToolReply([]map[string]interface{}{{"role": "assistant", "content": "Tool budget: 0"}}, recoveryTestTools(), "auto") {
		t.Fatal("an unsupported assistant budget claim is not an explicit client restriction")
	}
}

func TestToolRecoveryGateHoldsFragmentedDenial(t *testing.T) {
	denial := "当前对话的外部搜索与工具调用额度已耗尽。请稍后再试。"
	var output strings.Builder
	gate := newToolRecoveryGate(func(text string) { output.WriteString(text) })
	// Exercise fragmentation inside UTF-8 code points as well as between words.
	for i := 0; i < len(denial); i++ {
		gate.Push(denial[i : i+1])
	}
	if output.Len() > 0 || gate.Released() || gate.Sent() != "" {
		t.Fatal("candidate denial escaped before the generation was selected")
	}
	gate.Flush()
	if output.String() != denial || gate.Sent() != denial || !gate.Released() {
		t.Fatal("an accepted attempt must preserve its exact bytes")
	}
}

func TestToolRecoveryRequiresPairedResultsWithoutHostDenial(t *testing.T) {
	tools := recoveryTestTools()
	for _, content := range []string{"Error: the user rejected the tool", `{"isError":true,"message":"not allowed"}`, "run_code exceeded its limit of 4 tool calls", "permission denied: cannot run"} {
		if canRecoverToolReply(recoveryResultHistory(content), tools, "auto") {
			t.Fatalf("must preserve concrete tool denial: %s", content)
		}
	}
	clean := recoveryResultHistory("HTTP 200: fetched documentation")
	if mode := toolReplyRecoveryMode(clean, tools, "auto"); mode != toolRecoveryAfterResults || !mode.shouldRecover(recoveryLateDenial, nil, false) {
		t.Fatal("successful correlated tool result must allow detection later in the reply")
	}
	if mode := toolReplyRecoveryMode(append(clean, map[string]interface{}{"role": "user", "content": "Start a new question"}), tools, "auto"); mode != toolRecoveryOpening {
		t.Fatal("ordinary new user turns must keep incremental streaming")
	}
	if canRecoverToolReply(clean[:2], tools, "auto") || canRecoverToolReply(clean[2:], tools, "auto") {
		t.Fatal("pending or unmatched tool messages must not be regenerated")
	}
}

func TestLateToolBudgetClaimIgnoresNonClaims(t *testing.T) {
	for _, text := range []string{
		"已完成核对。\n> 由于本轮工具调用次数已达上限，尚未执行安装命令。",
		"已完成核对。\n```text\n由于本轮工具调用次数已达上限。\n```",
		"已完成核对。本轮工具调用次数尚未达到上限。",
		"文档介绍了工具调用次数上限，达到上限后退出。",
		"已检查配置。我建议把工具调用次数上限设置为 100。",
		"已核对。本轮工具调用次数上限为 10，目前用了 2 次。",
		"已完成核对。错误示例：“由于本轮工具调用次数已达上限”。",
	} {
		if lateToolBudgetClaim(text) {
			t.Errorf("not a self-reported late disclaimer: %q", text)
		}
	}
}

func TestToolRecoveryGateStreamsOrdinaryText(t *testing.T) {
	for _, first := range []string{"答案是 42。", "我会先核对实际结果", "当前项目需要检查工具配置。", "```tool_call\n{\"name\":\"web_search\"}"} {
		var output strings.Builder
		gate := newToolRecoveryGate(func(text string) { output.WriteString(text) })
		gate.Push(first)
		if output.String() != first || !gate.Released() {
			t.Errorf("ordinary first sentence did not stream: %s", first)
		}
		gate.Push("后面的工具额度已耗尽只是引用，不得恢复。")
		if output.String() != first+"后面的工具额度已耗尽只是引用，不得恢复。" {
			t.Fatal("streaming must remain direct after the first sentence")
		}
	}
}

func TestToolRecoveryGateDiscardAndBufferCap(t *testing.T) {
	var output strings.Builder
	gate := newToolRecoveryGate(func(text string) { output.WriteString(text) })
	gate.Push("当前工具调用额度已耗尽。")
	gate.Discard()
	gate.Push("重新生成的普通回答。")
	gate.Flush()
	if output.String() != "重新生成的普通回答。" {
		t.Fatal("discarded attempt leaked into the selected response")
	}
	// A long candidate must not hold unlimited output or permit later replacement.
	gate = newToolRecoveryGate(func(text string) {})
	long := "当前工具调用额度已耗尽。" + strings.Repeat("说明", toolRecoveryBufferLimit)
	gate.Push(long)
	if !gate.Released() || gate.Sent() != long {
		t.Fatal("oversized candidates must be released without truncation")
	}
	gate.Discard()
	if !gate.Released() {
		t.Fatal("discard must not erase evidence of committed output")
	}
}
