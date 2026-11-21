package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

const recoveryDenial = "当前对话的外部搜索与工具调用额度已耗尽（剩余 0 次）。这是未验证的正文。"
const recoveryLateDenial = "已查看安装文档（[README](https://example.invalid/README.md)），由于本轮工具调用次数已达上限，尚未在本地执行安装命令。"

func recoveryResultHistory(content string) []map[string]interface{} {
	return []map[string]interface{}{
		{"role": "user", "content": "Read the documentation, then check the local prerequisites."},
		{"role": "assistant", "tool_calls": []ToolCall{{ID: "call_fetch", Type: "function", Function: ToolCallFunction{Name: "web_fetch", Arguments: `{"url":"https://example.invalid"}`}}}},
		{"role": "tool", "tool_call_id": "call_fetch", "name": "web_fetch", "content": content},
	}
}

func TestGenerateToolReplyRecoversWithoutLeakingCandidate(t *testing.T) {
	messages := []map[string]interface{}{{"role": "system", "content": "Require approval before external writes."}, {"role": "user", "content": "Search the current sources."}}
	tools := recoveryTestTools()
	for _, choice := range []interface{}{"auto", "required", map[string]interface{}{"type": "function", "name": "web_search"}} {
		var sent strings.Builder
		attempts := 0
		block := "```tool_call\n{\"name\":\"web_search\",\"arguments\":{\"query\":\"current sources\"}}\n```"
		text, calls, res, err := generateToolReply(context.Background(), "initial prompt", messages, tools, choice, func(delta string) { sent.WriteString(delta) }, toolReplyOptions{inlineOnly: true},
			func(prompt string, delta func(string), retry bool) (*StreamResult, error) {
				attempts++
				if !strings.Contains(prompt, toolTranscriptHeader) || !strings.Contains(prompt, toolTranscriptFooter) {
					t.Fatal("eligible generations must distinguish the external transcript from native tools")
				}
				out := recoveryDenial
				if attempts == 1 && retry {
					t.Fatal("first generation cannot be a retry")
				}
				if attempts == 2 {
					if !retry || !strings.Contains(prompt, toolProtocolCorrection) || !strings.Contains(prompt, "Require approval before external writes.") {
						t.Fatal("correction must retain the real client permission requirements")
					}
					if mode, _ := parseToolChoice(choice); mode == "auto" && strings.Contains(prompt, "MUST call one of the tools") {
						t.Fatal("recovery must not change auto into required")
					}
					out = block
				}
				for _, r := range out {
					delta(string(r))
				}
				return &StreamResult{Raw: conversationTestRaw(t, out), TotalMs: 10}, nil
			})
		if err != nil || attempts != 2 || text != "" || len(calls) != 1 || calls[0].Function.Name != "web_search" || res.TotalMs != 20 {
			t.Fatalf("failed repaired generation: attempts=%d text=%q calls=%+v res=%+v err=%v", attempts, text, calls, res, err)
		}
		if sent.String() != block || strings.Contains(sent.String(), "额度已耗尽") {
			t.Fatalf("hidden candidate escaped or tool request changed: %q", sent.String())
		}
	}
}

func TestGenerateToolReplyRetryBoundAndCancellation(t *testing.T) {
	for _, cancelBeforeRetry := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		var sent strings.Builder
		attempts := 0
		text, calls, _, err := generateToolReply(ctx, "initial", nil, recoveryTestTools(), "auto", func(delta string) { sent.WriteString(delta) }, toolReplyOptions{inlineOnly: true},
			func(_ string, delta func(string), _ bool) (*StreamResult, error) {
				attempts++
				delta(recoveryDenial)
				if cancelBeforeRetry {
					cancel()
				}
				return &StreamResult{Raw: conversationTestRaw(t, recoveryDenial)}, nil
			})
		cancel()
		if cancelBeforeRetry {
			if attempts != 1 || !errors.Is(err, context.Canceled) || sent.Len() != 0 {
				t.Fatalf("cancelled caller must not start recovery: attempts=%d sent=%q err=%v", attempts, sent.String(), err)
			}
		} else {
			var protocolError *ToolProtocolError
			if attempts != 2 || !errors.As(err, &protocolError) || text != "" || len(calls) != 0 || sent.Len() != 0 {
				t.Fatalf("a repeated unverified disclaimer must fail without text or a third generation: attempts=%d sent=%q err=%v", attempts, sent.String(), err)
			}
		}
	}
}

func TestGenerateToolReplyPreservesRealToolLimit(t *testing.T) {
	messages := recoveryResultHistory("Error: the host denied the call because the tool budget is exhausted")
	var sent strings.Builder
	attempts := 0
	text, _, _, err := generateToolReply(context.Background(), "initial", messages, recoveryTestTools(), "auto", func(delta string) { sent.WriteString(delta) }, toolReplyOptions{inlineOnly: true},
		func(_ string, delta func(string), _ bool) (*StreamResult, error) {
			attempts++
			delta(recoveryDenial)
			if sent.String() != recoveryDenial {
				t.Fatal("a real tool result must disable prefix buffering as well as recovery")
			}
			return &StreamResult{Raw: conversationTestRaw(t, recoveryDenial)}, nil
		})
	if err != nil || attempts != 1 || text != recoveryDenial {
		t.Fatalf("tool result was overridden: attempts=%d err=%v", attempts, err)
	}
}

func TestGenerateToolReplyRetriesLateDisclaimerWithToolHistory(t *testing.T) {
	for _, repeatedCall := range []bool{false, true} {
		messages := recoveryResultHistory("HTTP 200: real tool result")
		attempts := 0
		_, calls, _, err := generateToolReply(context.Background(), "incremental prompt", messages, recoveryTestTools(), "auto",
			func(string) { t.Fatal("unchecked post-tool text reached the response") }, toolReplyOptions{inlineOnly: true},
			func(prompt string, delta func(string), retry bool) (*StreamResult, error) {
				attempts++
				if delta != nil {
					t.Fatal("the full post-tool candidate must be checked before streaming")
				}
				out := recoveryLateDenial
				if retry {
					if !strings.Contains(prompt, "HTTP 200: real tool result") || !strings.Contains(prompt, toolProtocolCorrection) {
						t.Fatal("the correction must retain the successful result")
					}
					out = "```tool_call\n{\"name\":\"web_search\",\"arguments\":{\"query\":\"prerequisites\"}}\n```"
					if repeatedCall {
						out = "```tool_call\n{\"name\":\"web_fetch\",\"arguments\":{\"url\":\"https://example.invalid\"}}\n```"
					}
				}
				return &StreamResult{Raw: conversationTestRaw(t, out)}, nil
			})
		var protocolError *ToolProtocolError
		if attempts != 2 || (repeatedCall && (!errors.As(err, &protocolError) || len(calls) != 0)) ||
			(!repeatedCall && (err != nil || len(calls) != 1 || calls[0].Function.Name != "web_search")) {
			t.Fatalf("post-tool recovery failed: repeated=%v attempts=%d calls=%v err=%v", repeatedCall, attempts, calls, err)
		}
	}
}

func TestConversationRecoveryUsesExistingHeadForOversizedToolHistory(t *testing.T) {
	resetConversationTestState(t)
	withBudget(t, 4000)
	messages := recoveryResultHistory(strings.Repeat("verified documentation ", 400))
	old := runConversationTurn
	t.Cleanup(func() { runConversationTurn = old })
	var turns []int
	runConversationTurn = func(prompt string, _ ModelConfig, state *convState, delta, _ func(string)) (*StreamResult, error) {
		turns = append(turns, state.turn)
		if delta != nil {
			t.Fatal("late disclaimer must not be streamed")
		}
		out := recoveryLateDenial
		if len(turns) == 1 {
			state.cid, state.rid = "existing-context", "first-reply"
			if !strings.Contains(prompt, "verified documentation") {
				t.Fatal("the initial transcript lost the tool result")
			}
		} else {
			if state.cid != "existing-context" || state.rid != "first-reply" || prompt != toolProtocolCorrection {
				t.Fatal("the correction must continue the same history without resending tool results")
			}
			out = "```tool_call\n{\"name\":\"web_search\",\"arguments\":{\"query\":\"prerequisites\"}}\n```"
		}
		state.turn++
		return &StreamResult{Raw: conversationTestRaw(t, out)}, nil
	}
	_, calls, _, err := callGeminiConv(messages, Models["gemini-3.5-flash-lite"], recoveryTestTools(), "auto", func(string) {}, nil,
		conversationOptions{requestScope: "oversize-result", recoverToolReply: context.Background()})
	if err != nil || len(calls) != 1 || len(turns) != 2 || turns[0] != 0 || turns[1] != 1 {
		t.Fatalf("oversized history correction: turns=%v calls=%v err=%v", turns, calls, err)
	}
}

func TestGenerateToolReplyRejectsOversizedColdRetry(t *testing.T) {
	withBudget(t, len(toolTranscriptPrompt("small incremental prompt"))+100)
	messages := []map[string]interface{}{{"role": "user", "content": strings.Repeat("complete-history", 400)}}
	attempts := 0
	var sent strings.Builder
	_, _, _, err := generateToolReply(context.Background(), "small incremental prompt", messages, recoveryTestTools(), "auto", func(delta string) { sent.WriteString(delta) }, toolReplyOptions{inlineOnly: true},
		func(_ string, delta func(string), _ bool) (*StreamResult, error) {
			attempts++
			delta(recoveryDenial)
			return &StreamResult{Raw: conversationTestRaw(t, recoveryDenial)}, nil
		})
	var oversized *PromptTooLongError
	if !errors.As(err, &oversized) || attempts != 1 || sent.Len() != 0 || oversized.Bytes <= oversized.Budget {
		t.Fatalf("cold retry must not silently truncate complete history: attempts=%d sent=%q err=%v", attempts, sent.String(), err)
	}
}

func TestGenerateToolReplyRejectsOversizedInitialTranscript(t *testing.T) {
	withBudget(t, 100)
	attempts := 0
	_, _, _, err := generateToolReply(context.Background(), "small prompt", nil, recoveryTestTools(), "auto", nil, toolReplyOptions{inlineOnly: true},
		func(_ string, _ func(string), _ bool) (*StreamResult, error) {
			attempts++
			return nil, nil
		})
	var oversized *PromptTooLongError
	if !errors.As(err, &oversized) || attempts != 0 || oversized.Bytes <= oversized.Budget {
		t.Fatalf("wrapped initial prompt must fit before any network call: attempts=%d err=%v", attempts, err)
	}
}

func TestGenerateToolReplyLeavesAttachmentBudgetToSource(t *testing.T) {
	withBudget(t, 100)
	attempts := 0
	_, calls, _, err := generateToolReply(context.Background(), strings.Repeat("context", 100), nil, recoveryTestTools(), "auto", nil, toolReplyOptions{},
		func(prompt string, _ func(string), _ bool) (*StreamResult, error) {
			attempts++
			if len(prompt) <= 100 {
				t.Fatal("full context must reach the attachment-capable source")
			}
			out := "```tool_call\n{\"name\":\"web_search\",\"arguments\":{\"query\":\"current sources\"}}\n```"
			return &StreamResult{Raw: conversationTestRaw(t, out)}, nil
		})
	if err != nil || attempts != 1 || len(calls) != 1 {
		t.Fatalf("attachment-capable source must own its budget: attempts=%d calls=%+v err=%v", attempts, calls, err)
	}
}

func TestConversationRecoveryPublishesOnlyCorrectedHead(t *testing.T) {
	resetConversationTestState(t)
	old := runConversationTurn
	t.Cleanup(func() { runConversationTurn = old })
	attempts := 0
	var turns []int
	var sent strings.Builder
	runConversationTurn = func(prompt string, _ ModelConfig, state *convState, delta, _ func(string)) (*StreamResult, error) {
		attempts++
		turns = append(turns, state.turn)
		out := recoveryDenial
		switch attempts {
		case 1:
			state.accountID, state.proxyID, state.cid = 17, 4, "hidden-candidate"
		case 2:
			if state.cid != "" || state.turn != 0 || state.accountID != 17 || state.proxyID != 4 || !strings.Contains(prompt, toolProtocolCorrection) {
				t.Fatal("recovery must reset upstream history while retaining account and egress")
			}
			state.cid = "corrected-conversation"
			out = "```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"one\"}}\n```"
		case 3:
			if state.cid != "corrected-conversation" || !strings.Contains(prompt, "real-file-result") || strings.Contains(prompt, toolProtocolCorrection) || strings.Contains(prompt, "original-question") {
				t.Fatal("client replay must continue the corrected head using only new tool results")
			}
			out = "The file says real-file-result."
		default:
			t.Fatal("unexpected generation after completion")
		}
		state.turn++
		if delta != nil {
			delta(out)
		}
		return &StreamResult{Raw: conversationTestRaw(t, out)}, nil
	}
	messages := []map[string]interface{}{{"role": "user", "content": "original-question"}}
	opt := conversationOptions{requestScope: "recovery-thread", recoverToolReply: context.Background()}
	text, calls, _, err := callGeminiConv(messages, Models["gemini-3.5-flash-lite"], conversationTestTools(), "auto", func(delta string) { sent.WriteString(delta) }, nil, opt)
	if err != nil || len(calls) != 1 || strings.Contains(sent.String(), "额度") {
		t.Fatalf("corrected first reply: text=%q calls=%+v sent=%q err=%v", text, calls, sent.String(), err)
	}
	convMu.Lock()
	heads := len(convStore)
	convMu.Unlock()
	if heads != 1 {
		t.Fatalf("hidden candidate must not publish its own continuation head: heads=%d", heads)
	}
	messages = append(conversationMessagesWithReply(messages, text, calls), map[string]interface{}{
		"role": "tool", "tool_call_id": calls[0].ID, "content": "real-file-result",
	})
	_, followupCalls, _, err := callGeminiConv(messages, Models["gemini-3.5-flash-lite"], conversationTestTools(), "auto", nil, nil, opt)
	if err != nil || len(followupCalls) != 0 || len(turns) != 3 || turns[0] != 0 || turns[1] != 0 || turns[2] != 1 {
		t.Fatalf("corrected replay failed: turns=%v calls=%+v err=%v", turns, followupCalls, err)
	}
}

func TestResponsesRecoveryKeepsRejectedTextOutOfSSE(t *testing.T) {
	resetConversationTestState(t)
	rtMu.Lock()
	rtVal.MultiTurn = true
	rtMu.Unlock()
	old := runConversationTurn
	t.Cleanup(func() { runConversationTurn = old })
	attempts := 0
	runConversationTurn = func(_ string, _ ModelConfig, state *convState, delta, _ func(string)) (*StreamResult, error) {
		attempts++
		text := recoveryDenial
		if attempts == 2 {
			text = "```tool_call\n{\"name\":\"web_search\",\"arguments\":{\"query\":\"current sources\"}}\n```"
		}
		state.cid, state.turn = "recovered-handler", state.turn+1
		delta(text)
		return &StreamResult{Raw: conversationTestRaw(t, text)}, nil
	}
	r := httptest.NewRequest("POST", "/v1/responses", bytes.NewBufferString(`{
		"model":"gemini-3.5-flash-lite","stream":true,"input":"Search current sources.",
		"tools":[{"type":"function","name":"web_search","parameters":{"type":"object"}}]
	}`))
	w := httptest.NewRecorder()
	handleResponses(w, r)
	if w.Code != 200 || attempts != 2 || strings.Contains(w.Body.String(), "额度") {
		t.Fatalf("handler leaked or failed recovery: code=%d attempts=%d body=%s", w.Code, attempts, w.Body.String())
	}
	declared := false
	completed := false
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		switch getStr(event, "type") {
		case "response.output_text.delta":
			t.Fatal("discarded disclaimer must not create a text item or text delta")
		case "response.output_item.added":
			item, _ := event["item"].(map[string]interface{})
			if getStr(item, "type") == "function_call" && getStr(item, "name") == "web_search" {
				declared = true
			}
		case "response.function_call_arguments.delta":
			if !declared {
				t.Fatal("recovered function arguments must follow item declaration")
			}
		case "response.completed":
			completed = true
		}
	}
	if !declared || !completed {
		t.Fatal("recovered response must still declare its call and complete the stream")
	}
}

func TestResponsesRepeatedDisclaimerReturnsProtocolFailure(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(map[bool]string{true: "stream", false: "nonstream"}[stream], func(t *testing.T) {
			resetConversationTestState(t)
			rtMu.Lock()
			rtVal.MultiTurn = true
			rtMu.Unlock()
			old := runConversationTurn
			t.Cleanup(func() { runConversationTurn = old })
			attempts := 0
			runConversationTurn = func(_ string, _ ModelConfig, state *convState, delta, _ func(string)) (*StreamResult, error) {
				attempts++
				state.cid, state.turn = "rejected-handler", state.turn+1
				if delta != nil {
					delta(recoveryDenial)
				}
				return &StreamResult{Raw: conversationTestRaw(t, recoveryDenial)}, nil
			}
			payload, _ := json.Marshal(map[string]interface{}{
				"model": "gemini-3.5-flash-lite", "stream": stream, "input": "Search current sources.",
				"tools": []map[string]interface{}{{"type": "function", "name": "web_search", "parameters": map[string]interface{}{"type": "object"}}},
			})
			r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(payload))
			w := httptest.NewRecorder()
			handleResponses(w, r)
			body := w.Body.String()
			if attempts != 2 || strings.Contains(body, "额度") || strings.Contains(body, "response.completed") || strings.Contains(body, "response.output_text.delta") || strings.Contains(body, "response.output_item.added") {
				t.Fatalf("rejected candidates must not become assistant output: attempts=%d body=%s", attempts, body)
			}
			if stream {
				if w.Code != 200 || !strings.Contains(body, "event: response.failed") || !strings.Contains(body, `"code":"tool_protocol_error"`) {
					t.Fatalf("stream must terminate with protocol failure: code=%d body=%s", w.Code, body)
				}
			} else if w.Code != 502 || !strings.Contains(body, `"type":"upstream_protocol_error"`) || !strings.Contains(body, `"code":"tool_protocol_error"`) {
				t.Fatalf("nonstream must return a protocol error: code=%d body=%s", w.Code, body)
			}
			convMu.Lock()
			heads := len(convStore)
			convMu.Unlock()
			if heads != 0 {
				t.Fatalf("failed candidates must not publish a conversation head: heads=%d", heads)
			}
		})
	}
}
