package app

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func responseInputFixture(t *testing.T, body string) []map[string]interface{} {
	t.Helper()
	var req map[string]interface{}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	return responsesToMessages(req)
}

// Responses replays each call as a top-level output item, before the optional
// text item. They must remain one assistant turn, with every call and result.
func TestResponsesInputStandaloneFunctionCalls(t *testing.T) {
	messages := responseInputFixture(t, `{"instructions":"be brief","input":[
		{"role":"user","content":"check both cities"},
		{"type":"reasoning","summary":[]},
		{"type":"function_call","call_id":"call_paris","name":"weather","arguments":"{\"city\":\"Paris\"}"},
		{"type":"function_call","call_id":"call_rome","name":"weather","arguments":"{\"city\":\"Rome\"}"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Checking."}]},
		{"type":"function_call_output","call_id":"call_rome","output":"Rome is sunny"},
		{"type":"function_call_output","call_id":"call_paris","output":"Paris is cloudy"}
	]}`)
	if len(messages) != 5 {
		t.Fatalf("want system, user, assistant and two tool results; got %#v", messages)
	}
	if getStr(messages[2], "role") != "assistant" || getStr(messages[2], "content") != "Checking." {
		t.Fatalf("assistant text lost or split: %#v", messages[2])
	}
	calls, ok := messages[2]["tool_calls"].([]interface{})
	if !ok || len(calls) != 2 {
		t.Fatalf("want two Chat-compatible tool calls, got %#v", messages[2]["tool_calls"])
	}
	for i, wantID := range []string{"call_paris", "call_rome"} {
		call := calls[i].(map[string]interface{})
		if getStr(call, "id") != wantID {
			t.Errorf("call %d id = %q, want %q", i, getStr(call, "id"), wantID)
		}
	}
	for i, wantID := range []string{"call_rome", "call_paris"} {
		m := messages[i+3]
		if getStr(m, "role") != "tool" || getStr(m, "name") != "weather" || getStr(m, "tool_call_id") != wantID {
			t.Errorf("tool result identity lost: %#v", m)
		}
	}
	prompt, _ := messagesToPrompt(messages, nil, nil)
	for _, want := range []string{"[System instruction]: be brief", "Checking.", `"name": "weather"`, `"city":"Paris"`, `"city":"Rome"`, "[Tool result for weather]: Rome is sunny", "[Tool result for weather]: Paris is cloudy"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("cold-start prompt lost %q: %s", want, prompt)
		}
	}
}

func TestResponsesInputAssistantReplayGrouping(t *testing.T) {
	standalone := responseInputFixture(t, `{"input":[
		{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},
		{"type":"reasoning","summary":[]},
		{"role":"assistant","content":[{"type":"output_text","text":"Looking "},{"type":"text","text":"now."}]}
	]}`)
	nested := responseInputFixture(t, `{"input":[
		{"role":"assistant","content":[{"type":"output_text","text":"Looking "},{"type":"text","text":"now."},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}]}
	]}`)
	if len(standalone) != 1 || len(nested) != 1 || !reflect.DeepEqual(standalone, nested) {
		t.Fatalf("equivalent assistant replay should normalize equally: standalone=%#v nested=%#v", standalone, nested)
	}
	if getStr(standalone[0], "content") != "Looking now." {
		t.Fatalf("assistant text blocks lost: %#v", standalone[0])
	}
}

func TestResponsesInputToolNamesAndExtraMessages(t *testing.T) {
	messages := responseInputFixture(t, `{"input":[
		{"type":"function_call_output","call_id":"call_1","output":[{"type":"text","text":"saved"}]},
		{"type":"function_call","call_id":"call_1","name":"write_file","arguments":"{}"},
		{"role":"user","content":[{"type":"input_text","text":"continue"}]},
		{"role":"user","content":"time context"},
		{"role":"developer","content":"environment context"},
		{"type":"reasoning","summary":[]},
		{"type":"unknown_item"}
	]}`)
	if len(messages) != 5 {
		t.Fatalf("must retain all real messages without phantom users: %#v", messages)
	}
	if getStr(messages[0], "name") != "write_file" || getStr(messages[0], "content") != "saved" {
		t.Fatalf("future call name or structured result lost: %#v", messages[0])
	}
	for i, want := range []string{"continue", "time context", "environment context"} {
		if getStr(messages[i+2], "content") != want {
			t.Errorf("extra message %d lost or reordered: %#v", i, messages[i+2])
		}
	}
}

func TestResponsesInputStringAndExplicitToolName(t *testing.T) {
	messages := responseInputFixture(t, `{"instructions":"be helpful","input":"hello"}`)
	want := []map[string]interface{}{{"role": "system", "content": "be helpful"}, {"role": "user", "content": "hello"}}
	if !reflect.DeepEqual(messages, want) {
		t.Fatalf("simple input changed: got %#v, want %#v", messages, want)
	}
	messages = responseInputFixture(t, `{"input":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","name":"explicit_name","output":"ok"}]}`)
	if len(messages) != 2 || getStr(messages[1], "name") != "explicit_name" {
		t.Fatalf("explicit tool result name should be preserved: %#v", messages)
	}
}

// Replay the handler's actual output array, as an Agent does after running
// parallel tools. The continuation must retain one upstream conversation and
// send every new result, even when a synthetic message follows the results.
func TestResponsesHandlerToolReplayContinuesConversation(t *testing.T) {
	resetConversationTestState(t)
	rtMu.Lock()
	rtVal.MultiTurn = true
	rtMu.Unlock()
	var prompts []string
	var turns []int
	stubConversationTestTurn(t, func(prompt string, _ ModelConfig, st *convState) (*StreamResult, error) {
		prompts = append(prompts, prompt)
		turns = append(turns, st.turn)
		st.cid = "handler-conversation"
		st.turn++
		text := "Both results received."
		if len(prompts) == 1 {
			text = "```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"one\"}}\n```\n```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"two\"}}\n```"
		}
		return &StreamResult{Raw: conversationTestRaw(t, text)}, nil
	})
	firstUser := map[string]interface{}{"role": "user", "content": "original-handler-question"}
	req := map[string]interface{}{
		"model":            "gemini-3.5-flash-lite",
		"instructions":     "stable-handler-system",
		"input":            []interface{}{firstUser},
		"prompt_cache_key": "handler-session",
		"stream":           true,
		"tools": []interface{}{map[string]interface{}{
			"type": "function", "name": "read_file", "parameters": map[string]interface{}{"type": "object"},
		}},
	}
	call := func() map[string]interface{} {
		t.Helper()
		body, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer test-caller")
		w := httptest.NewRecorder()
		handleResponses(w, r)
		if w.Code != 200 {
			t.Fatalf("handler status=%d: %s", w.Code, w.Body.String())
		}
		for _, line := range strings.Split(w.Body.String(), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event map[string]interface{}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				t.Fatal(err)
			}
			if getStr(event, "type") == "response.completed" {
				response, _ := event["response"].(map[string]interface{})
				return response
			}
		}
		t.Fatalf("missing successful terminal event: %s", w.Body.String())
		return nil
	}
	first := call()
	output, _ := first["output"].([]interface{})
	if len(output) != 2 {
		t.Fatalf("expected two real handler function calls, got %#v", output)
	}
	replay := append([]interface{}{firstUser}, output...)
	for i, raw := range output {
		item := raw.(map[string]interface{})
		if getStr(item, "type") != "function_call" || getStr(item, "call_id") == "" {
			t.Fatalf("expected function_call identity in actual output: %#v", item)
		}
		replay = append(replay, map[string]interface{}{
			"type": "function_call_output", "call_id": item["call_id"], "output": []string{"result-one", "result-two"}[i],
		})
	}
	replay = append(replay, map[string]interface{}{"role": "user", "content": "synthetic-time-context"})
	req["input"] = replay
	second := call()
	if getStr(second, "status") != "completed" || !reflect.DeepEqual(turns, []int{0, 1}) {
		t.Fatalf("tool replay must continue upstream turn 1, turns=%v response=%#v", turns, second)
	}
	for _, want := range []string{"[Tool result for read_file]: result-one", "[Tool result for read_file]: result-two", "synthetic-time-context"} {
		if !strings.Contains(prompts[1], want) {
			t.Errorf("continuation dropped %q: %s", want, prompts[1])
		}
	}
	for _, old := range []string{"original-handler-question", "stable-handler-system", "Available tools"} {
		if strings.Contains(prompts[1], old) {
			t.Errorf("continuation resent old context %q: %s", old, prompts[1])
		}
	}
}

func TestResponsesRequestNamespaceIsolation(t *testing.T) {
	request := httptest.NewRequest("POST", "/v1/responses", nil)
	request.Header.Set("Authorization", "Bearer private-test-credential")
	base := conversationRequestScope(request, "session-one", "user-one")
	if len(base) != 64 || strings.Contains(base, "private-test-credential") {
		t.Fatal("request namespace must retain a digest instead of raw credentials")
	}
	if again := conversationRequestScope(request, "session-one", "user-one"); again != base {
		t.Fatal("identical request identity must have a stable scope")
	}
	otherCredential := request.Clone(request.Context())
	otherCredential.Header.Set("Authorization", "Bearer another-test-credential")
	for label, scope := range map[string]string{
		"credential": conversationRequestScope(otherCredential, "session-one", "user-one"),
		"session":    conversationRequestScope(request, "session-two", "user-one"),
		"user":       conversationRequestScope(request, "session-one", "user-two"),
	} {
		if scope == base {
			t.Errorf("different %s must not share a request namespace", label)
		}
	}
}
