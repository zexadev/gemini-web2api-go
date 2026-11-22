package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// Keep these tests on the actual HTTP handlers and conversation path. The only
// stub is the upstream exchange, so no request can leave the test process.
func stubFenceHandlerReply(t *testing.T, reply string) *int {
	t.Helper()
	resetConversationTestState(t)
	rtMu.Lock()
	rtVal.MultiTurn = true
	rtMu.Unlock()
	calls := new(int)
	previous := runConversationTurn
	runConversationTurn = func(_ string, _ ModelConfig, st *convState, onDelta, _ func(string)) (*StreamResult, error) {
		(*calls)++
		st.cid = "fence-handler-conversation"
		st.turn++
		if onDelta != nil {
			// Split the opening marker, JSON, and nested markdown across reads.
			for i := 0; i < len(reply); i += 7 {
				end := i + 7
				if end > len(reply) {
					end = len(reply)
				}
				onDelta(reply[i:end])
			}
		}
		return &StreamResult{Raw: conversationTestRaw(t, reply), Emitted: reply}, nil
	}
	t.Cleanup(func() { runConversationTurn = previous })
	return calls
}

func fenceHandlerRequest(t *testing.T, responses, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	req := map[string]interface{}{
		"model": "gemini-3.5-flash-lite", "stream": stream,
		"prompt_cache_key": "fence-handler-test",
	}
	function := map[string]interface{}{
		"name": "write_file", "parameters": map[string]interface{}{"type": "object"},
	}
	path := "/v1/chat/completions"
	handler := http.HandlerFunc(handleChatCompletions)
	if responses {
		path, handler = "/v1/responses", handleResponses
		req["input"] = []interface{}{map[string]interface{}{"role": "user", "content": "Create the requested document."}}
		req["tools"] = []interface{}{map[string]interface{}{
			"type": "function", "name": function["name"], "parameters": function["parameters"],
		}}
	} else {
		req["messages"] = []interface{}{map[string]interface{}{"role": "user", "content": "Create the requested document."}}
		req["tools"] = []interface{}{map[string]interface{}{"type": "function", "function": function}}
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer fence-handler-test-caller")
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}

func fenceHandlerObjects(t *testing.T, body string, stream bool) []map[string]interface{} {
	t.Helper()
	if !stream {
		var object map[string]interface{}
		if err := json.Unmarshal([]byte(body), &object); err != nil {
			t.Fatal(err)
		}
		return []map[string]interface{}{object}
	}
	var objects []map[string]interface{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var object map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &object); err != nil {
			t.Fatal(err)
		}
		objects = append(objects, object)
	}
	return objects
}

func TestToolFenceHandlersRejectMalformedRepliesAtomically(t *testing.T) {
	valid := "```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"safe.md\",\"content\":\"safe\"}}\n```\n"
	// A literal newline in a JSON string precedes a nested markdown fence. The
	// former fallback split at that fence and leaked this suffix as normal text.
	malformed := "```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"content\":\"broken\n```mermaid\\nLEAKED_SUFFIX\\n```\\n\"}}\n```"
	cases := []struct{ name, reply string }{
		{"invalid-json", malformed},
		{"missing-name", "```tool_call\n{\"arguments\":{\"content\":\"LEAKED_SUFFIX\"}}\n```"},
		{"unclosed-fence", "```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"content\":\"LEAKED_SUFFIX\"}}"},
		{"mixed-valid-invalid", valid + malformed},
	}
	for _, responses := range []bool{false, true} {
		api := "chat"
		if responses {
			api = "responses"
		}
		for _, stream := range []bool{false, true} {
			mode := "json"
			if stream {
				mode = "stream"
			}
			for _, tc := range cases {
				t.Run(api+"/"+mode+"/"+tc.name, func(t *testing.T) {
					calls := stubFenceHandlerReply(t, "Checking document.\n"+tc.reply)
					w := fenceHandlerRequest(t, responses, stream)
					body := w.Body.String()
					wantStatus := http.StatusBadGateway
					if stream {
						wantStatus = http.StatusOK // A permitted preamble has already opened SSE.
					}
					if w.Code != wantStatus {
						t.Fatalf("status=%d, want %d: %s", w.Code, wantStatus, body)
					}
					if *calls != 1 {
						t.Fatalf("malformed output must not trigger another exchange: %d", *calls)
					}
					for _, leaked := range []string{"LEAKED_SUFFIX", "mermaid", "tool_calls", "function_call", "response.completed"} {
						if strings.Contains(body, leaked) {
							t.Fatalf("malformed reply leaked %q or published executable/successful output: %s", leaked, body)
						}
					}
					failed := false
					var streamedText strings.Builder
					for _, object := range fenceHandlerObjects(t, body, stream) {
						if getStr(object, "type") == "response.output_text.delta" {
							streamedText.WriteString(getStr(object, "delta"))
						}
						if getStr(object, "type") == "response.failed" {
							response, _ := object["response"].(map[string]interface{})
							errorObject, _ := response["error"].(map[string]interface{})
							if getStr(errorObject, "code") != "tool_call_parse_error" {
								t.Fatalf("Responses must expose its typed parse failure: %#v", response)
							}
							failed = true
						}
						if object["error"] != nil {
							failed = true
						}
						if choices, ok := object["choices"].([]interface{}); ok {
							for _, raw := range choices {
								choice, _ := raw.(map[string]interface{})
								delta, _ := choice["delta"].(map[string]interface{})
								streamedText.WriteString(getStr(delta, "content"))
								if getStr(choice, "finish_reason") == "stop" || getStr(choice, "finish_reason") == "tool_calls" {
									t.Fatalf("failed reply must not carry a success finish reason: %#v", choice)
								}
							}
						}
					}
					if !failed {
						t.Fatalf("missing explicit failure: %s", body)
					}
					if stream && strings.TrimSpace(streamedText.String()) != "Checking document." {
						t.Fatalf("stream must preserve only the ordinary preamble, got %q", streamedText.String())
					}
					convMu.Lock()
					stored := len(convStore)
					convMu.Unlock()
					if stored != 0 {
						t.Fatalf("failed output must not publish a reusable conversation head: %d", stored)
					}
				})
			}
		}
	}
}

func TestToolFenceHandlersPreserveValidNestedMarkdownCalls(t *testing.T) {
	content := "Document\n```mermaid\nflowchart LR\n A --> B\n```\nDone."
	call, err := json.Marshal(map[string]interface{}{
		"name": "write_file", "arguments": map[string]interface{}{"path": "safe.md", "content": content},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, responses := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			name := "chat/json"
			if responses {
				name = "responses/json"
			}
			if stream {
				name = strings.TrimSuffix(name, "/json") + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				stubFenceHandlerReply(t, "Checking document.\n```tool_call\n"+string(call)+"\n```")
				w := fenceHandlerRequest(t, responses, stream)
				if w.Code != http.StatusOK {
					t.Fatalf("valid nested markdown failed: %d %s", w.Code, w.Body.String())
				}
				var function map[string]interface{}
				for _, object := range fenceHandlerObjects(t, w.Body.String(), stream) {
					if responses {
						if getStr(object, "type") == "response.completed" {
							object, _ = object["response"].(map[string]interface{})
						} else if stream {
							continue
						}
						items, _ := object["output"].([]interface{})
						for _, raw := range items {
							item, _ := raw.(map[string]interface{})
							if getStr(item, "type") == "function_call" {
								function = item
							}
						}
					} else {
						choices, _ := object["choices"].([]interface{})
						for _, raw := range choices {
							choice, _ := raw.(map[string]interface{})
							field := "message"
							if stream {
								field = "delta"
							}
							message, _ := choice[field].(map[string]interface{})
							calls, _ := message["tool_calls"].([]interface{})
							for _, rawCall := range calls {
								tool, _ := rawCall.(map[string]interface{})
								function, _ = tool["function"].(map[string]interface{})
							}
						}
					}
				}
				if getStr(function, "name") != "write_file" {
					t.Fatalf("expected a structured write_file call: %s", w.Body.String())
				}
				var arguments map[string]interface{}
				if err := json.Unmarshal([]byte(getStr(function, "arguments")), &arguments); err != nil {
					t.Fatal(err)
				}
				if arguments["content"] != content || arguments["path"] != "safe.md" {
					t.Fatalf("nested markdown arguments changed: %#v", arguments)
				}
			})
		}
	}
}

func TestToolFenceFailedContinuationDiscardsAdvancedHead(t *testing.T) {
	resetConversationTestState(t)
	mc := Models["gemini-3.5-flash-lite"]
	tools := conversationTestTools()
	first := []map[string]interface{}{{"role": "user", "content": "First task."}}
	var turns []int
	stubConversationTestTurn(t, func(_ string, _ ModelConfig, st *convState) (*StreamResult, error) {
		turns = append(turns, st.turn)
		st.cid = "advanced-fence-conversation"
		st.turn++
		reply := "Initial answer."
		if len(turns) == 2 {
			reply = "Checking document.\n```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"one\"}}\n```\n```tool_call\n{\"arguments\":{\"path\":\"two\"}}\n```"
		}
		return &StreamResult{Raw: conversationTestRaw(t, reply)}, nil
	})
	if _, _, _, err := callGeminiConv(first, mc, tools, "auto", nil, nil); err != nil {
		t.Fatal(err)
	}
	next := append(append([]map[string]interface{}{}, first...),
		map[string]interface{}{"role": "assistant", "content": "Initial answer."},
		map[string]interface{}{"role": "user", "content": "Next task."})
	_, calls, _, err := callGeminiConv(next, mc, tools, "auto", nil, nil)
	var parseError *ToolCallParseError
	if !errors.As(err, &parseError) || len(calls) != 0 {
		t.Fatalf("malformed continuation must fail atomically with typed error: calls=%+v err=%v", calls, err)
	}
	if _, _, _, err := callGeminiConv(next, mc, tools, "auto", nil, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(turns, []int{0, 1, 0}) {
		t.Fatalf("failed continuation must discard the consumed upstream head: %v", turns)
	}
}
