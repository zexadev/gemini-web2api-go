package app

import "testing"

func TestMergeResponseHistoryIncrementalAndFull(t *testing.T) {
	previous := []map[string]interface{}{
		{"role": "system", "content": "be concise"},
		{"role": "user", "content": "first"},
		{"role": "assistant", "content": "reply"},
	}
	incremental := []map[string]interface{}{
		{"role": "system", "content": "be concise"},
		{"role": "user", "content": "second"},
	}
	merged := mergeResponseHistory(previous, incremental)
	if got, want := canonicalMessages(merged), canonicalMessages([]map[string]interface{}{
		{"role": "system", "content": "be concise"},
		{"role": "user", "content": "first"},
		{"role": "assistant", "content": "reply"},
		{"role": "user", "content": "second"},
	}); got != want {
		t.Fatalf("incremental merge = %q, want %q", got, want)
	}

	full := append(append([]map[string]interface{}{}, previous...), incremental[1])
	fullMerged := mergeResponseHistory(previous, full)
	if got, want := canonicalMessages(fullMerged), canonicalMessages(full); got != want {
		t.Fatalf("full history merge = %q, want %q", got, want)
	}
}

func TestResponseHistoryPutGetCopiesAndToolCalls(t *testing.T) {
	id := "resp_test_history"
	responseMu.Lock()
	delete(responseStore, id)
	responseMu.Unlock()
	t.Cleanup(func() {
		responseMu.Lock()
		delete(responseStore, id)
		responseMu.Unlock()
	})

	input := []map[string]interface{}{{"role": "user", "content": "run it"}}
	responseHistoryPut(id, input, "", []ToolCall{{
		ID: "call_1", Type: "function",
		Function: ToolCallFunction{Name: "shell", Arguments: `{"command":"pwd"}`},
	}})
	got := responseHistoryGet(id)
	if len(got) != 2 {
		t.Fatalf("history length = %d, want 2", len(got))
	}
	if got[1]["role"] != "assistant" {
		t.Fatalf("stored assistant role = %#v", got[1]["role"])
	}
	calls, ok := got[1]["tool_calls"].([]interface{})
	if !ok || len(calls) != 1 {
		t.Fatalf("stored tool calls = %#v", got[1]["tool_calls"])
	}

	got[0]["content"] = "mutated"
	gotAgain := responseHistoryGet(id)
	if gotAgain[0]["content"] != "run it" {
		t.Fatalf("stored history was mutated through getter: %#v", gotAgain[0]["content"])
	}
}
