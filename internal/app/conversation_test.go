package app

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func conversationTestCall(id, name, arguments string) map[string]interface{} {
	return map[string]interface{}{"id": id, "type": "function", "function": map[string]interface{}{"name": name, "arguments": arguments}}
}

func TestCanonicalMessagesTracksToolIdentity(t *testing.T) {
	base := []map[string]interface{}{{"role": "assistant", "content": "", "tool_calls": []interface{}{conversationTestCall("call_1", "read_file", `{"path":"one"}`)}}}
	for _, tc := range []struct{ label, id, name, args string }{
		{"id", "call_2", "read_file", `{"path":"one"}`},
		{"name", "call_1", "write_file", `{"path":"one"}`},
		{"arguments", "call_1", "read_file", `{"path":"two"}`},
	} {
		t.Run(tc.label, func(t *testing.T) {
			changed := []map[string]interface{}{{"role": "assistant", "content": "", "tool_calls": []interface{}{conversationTestCall(tc.id, tc.name, tc.args)}}}
			if canonicalMessages(base) == canonicalMessages(changed) {
				t.Fatal("different tool call histories must not share a conversation")
			}
		})
	}
}

func TestCanonicalMessagesTracksToolResultAssociation(t *testing.T) {
	base := []map[string]interface{}{{"role": "tool", "content": "same result", "name": "read_file", "tool_call_id": "call_1"}}
	for _, tc := range []struct{ name, id string }{{"write_file", "call_1"}, {"read_file", "call_2"}} {
		changed := []map[string]interface{}{{"role": "tool", "content": "same result", "name": tc.name, "tool_call_id": tc.id}}
		if canonicalMessages(base) == canonicalMessages(changed) {
			t.Fatal("different tool result associations must not share a conversation")
		}
	}
}

func TestCanonicalMessagesDoesNotConfuseMessageBoundaries(t *testing.T) {
	one := []map[string]interface{}{{"role": "user", "content": "first\x1eassistant\x1fsecond"}}
	two := []map[string]interface{}{{"role": "user", "content": "first"}, {"role": "assistant", "content": "second"}}
	if canonicalMessages(one) == canonicalMessages(two) {
		t.Fatal("message text containing separators must not collide with another history")
	}
}

func TestCanonicalMessagesNormalizesOnlyToolArguments(t *testing.T) {
	first := []map[string]interface{}{{"role": "assistant", "content": "keep whitespace ", "tool_calls": []interface{}{conversationTestCall("call_1", "read_file", `{"path":"one","options":{"a":1,"b":2}}`)}}}
	equivalent := []map[string]interface{}{{"role": "assistant", "content": "keep whitespace ", "tool_calls": []interface{}{conversationTestCall("call_1", "read_file", ` { "options": {"b":2, "a":1}, "path": "one" } `)}}}
	if canonicalMessages(first) != canonicalMessages(equivalent) {
		t.Fatal("JSON argument formatting must not cause a false history miss")
	}
	equivalent[0]["content"] = "keep whitespace"
	if canonicalMessages(first) == canonicalMessages(equivalent) {
		t.Fatal("ordinary text whitespace must remain part of the exact history")
	}
	big := []map[string]interface{}{{"role": "assistant", "tool_calls": []interface{}{conversationTestCall("c", "n", `{"n":9007199254740992}`)}}}
	bigger := []map[string]interface{}{{"role": "assistant", "tool_calls": []interface{}{conversationTestCall("c", "n", `{"n":9007199254740993}`)}}}
	if canonicalMessages(big) == canonicalMessages(bigger) {
		t.Fatal("argument canonicalization must preserve integer precision")
	}
}

func TestCanonicalMessagesCoalescesAssistantReplay(t *testing.T) {
	call := conversationTestCall("call_1", "read_file", `{"path":"one"}`)
	combined := []map[string]interface{}{{"role": "assistant", "content": "Checking", "tool_calls": []interface{}{call}}}
	for _, split := range [][]map[string]interface{}{
		{{"role": "assistant", "tool_calls": []interface{}{call}}, {"role": "assistant", "content": "Checking"}},
		{{"role": "assistant", "content": "Checking"}, {"role": "assistant", "tool_calls": []interface{}{call}}},
	} {
		if canonicalMessages(combined) != canonicalMessages(split) {
			t.Fatal("separate assistant text and function call items must identify the same turn")
		}
	}
}

func resetConversationTestState(t *testing.T) {
	t.Helper()
	convMu.Lock()
	oldStore := convStore
	convStore = map[string]*convState{}
	convMu.Unlock()
	oldRT := rtCfg()
	rtMu.Lock()
	rtVal.AnonFirst = true
	rtVal.StrictModel = false
	rtMu.Unlock()
	t.Cleanup(func() {
		convMu.Lock()
		convStore = oldStore
		convMu.Unlock()
		rtMu.Lock()
		rtVal = oldRT
		rtMu.Unlock()
	})
}

func conversationTestRaw(t *testing.T, text string) string {
	t.Helper()
	inner := make([]interface{}, 44)
	inner[1] = []interface{}{"c_test", "r_test"}
	inner[4] = []interface{}{[]interface{}{"rc_test", []interface{}{text}, nil, nil, nil, nil, nil, nil, []interface{}{2}, "en"}}
	inner[39], inner[42], inner[43] = "cf41b0e0dd7d53e5", "3.5 Flash-Lite", true
	body, err := json.Marshal(inner)
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal([]interface{}{[]interface{}{"wrb.fr", nil, string(body)}})
	if err != nil {
		t.Fatal(err)
	}
	return string(line)
}

func stubConversationTestTurn(t *testing.T, f func(string, ModelConfig, *convState) (*StreamResult, error)) {
	t.Helper()
	old := runConversationTurn
	runConversationTurn = func(prompt string, mc ModelConfig, conv *convState, _, _ func(string)) (*StreamResult, error) {
		return f(prompt, mc, conv)
	}
	t.Cleanup(func() { runConversationTurn = old })
}

func conversationTestTools() []map[string]interface{} {
	return []map[string]interface{}{{"type": "function", "function": map[string]interface{}{"name": "read_file", "parameters": map[string]interface{}{"type": "object"}}}}
}

func TestConversationScopeSeparatesModelToolsAndRequests(t *testing.T) {
	mc := Models["gemini-3.5-flash-lite"]
	tools := conversationTestTools()
	base := convScopeKey(mc, tools, "auto", "thread-one")
	otherModel, otherThinking := mc, mc
	otherModel.Mode++
	otherThinking.ThinkWire = thinkingExtended
	otherTools := conversationTestTools()
	otherTools[0]["function"].(map[string]interface{})["description"] = "changed schema"
	for label, key := range map[string]string{
		"model":    convScopeKey(otherModel, tools, "auto", "thread-one"),
		"thinking": convScopeKey(otherThinking, tools, "auto", "thread-one"),
		"tools":    convScopeKey(mc, otherTools, "auto", "thread-one"),
		"choice":   convScopeKey(mc, tools, "required", "thread-one"),
		"request":  convScopeKey(mc, tools, "auto", "thread-two"),
	} {
		if key == base {
			t.Errorf("%s must start a different conversation scope", label)
		}
	}
}

func TestConversationTakeSelectsLongestProperPrefixAndCopiesState(t *testing.T) {
	resetConversationTestState(t)
	keys := []string{"short", "long", "whole"}
	st := &convState{cid: "long-conversation", turn: 2, accountID: 9, proxyID: 4}
	convPut("short", &convState{cid: "short-conversation", turn: 1})
	convPut("long", st)
	convPut("whole", &convState{cid: "must-not-use-whole-request"})
	st.cid = "mutated-after-put"
	got, prefix := convTake(keys)
	if got == nil || prefix != 2 || got.cid != "long-conversation" || got.accountID != 9 || got.proxyID != 4 {
		t.Fatalf("longest prefix and original state must be used, got %+v prefix=%d", got, prefix)
	}
	convMu.Lock()
	_, stillPresent := convStore["long"]
	_, wholePresent := convStore["whole"]
	convMu.Unlock()
	if stillPresent || !wholePresent {
		t.Fatal("take must consume only the selected proper prefix")
	}
}

func TestConversationTakeRejectsExpiredState(t *testing.T) {
	resetConversationTestState(t)
	convMu.Lock()
	convStore["expired"] = &convState{cid: "old", updated: time.Now().Add(-convTTL - time.Second)}
	convMu.Unlock()
	if got, prefix := convTake([]string{"expired", "suffix"}); got != nil || prefix != 0 {
		t.Fatalf("expired state must cold-start, got %+v prefix=%d", got, prefix)
	}
	convMu.Lock()
	_, kept := convStore["expired"]
	convMu.Unlock()
	if kept {
		t.Fatal("expired entry should be removed")
	}
}

func TestConversationTakeHasOneConcurrentOwner(t *testing.T) {
	resetConversationTestState(t)
	convPut("head", &convState{cid: "one-conversation"})
	var wg sync.WaitGroup
	owners := make(chan bool, 20)
	for i := 0; i < cap(owners); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _ := convTake([]string{"head", "suffix"})
			owners <- got != nil
		}()
	}
	wg.Wait()
	close(owners)
	n := 0
	for owner := range owners {
		if owner {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("one upstream conversation must have exactly one owner, got %d", n)
	}
}

func TestCallGeminiConvSendsAllNewToolAndUserMessages(t *testing.T) {
	resetConversationTestState(t)
	mc := Models["gemini-3.5-flash-lite"]
	tools := conversationTestTools()
	first := []map[string]interface{}{{"role": "system", "content": "stable-system"}, {"role": "user", "content": "original-question"}}
	var prompts []string
	var turns []int
	stubConversationTestTurn(t, func(prompt string, _ ModelConfig, st *convState) (*StreamResult, error) {
		prompts = append(prompts, prompt)
		turns = append(turns, st.turn)
		if len(prompts) == 1 {
			st.accountID, st.proxyID = 17, 4
		} else if st.accountID != 17 || st.proxyID != 4 || st.cid != "same-conversation" {
			t.Fatalf("continuation must retain account, proxy and upstream id: %+v", st)
		}
		st.cid = "same-conversation"
		st.turn++
		text := "finished"
		if len(prompts) == 1 {
			text = "Checking\n```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"one\"}}\n```\n```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"two\"}}\n```"
		}
		return &StreamResult{Raw: conversationTestRaw(t, text)}, nil
	})
	text, calls, _, err := callGeminiConv(first, mc, tools, "auto", nil, nil, conversationOptions{requestScope: "thread"})
	if err != nil || len(calls) != 2 {
		t.Fatalf("initial tool turn: %q %+v %v", text, calls, err)
	}
	callItems := make([]interface{}, len(calls))
	for i, c := range calls {
		callItems[i] = conversationTestCall(c.ID, c.Function.Name, c.Function.Arguments)
	}
	replay := append(append([]map[string]interface{}{}, first...),
		map[string]interface{}{"role": "assistant", "content": text, "tool_calls": callItems},
		map[string]interface{}{"role": "tool", "tool_call_id": calls[0].ID, "content": "result-one"},
		map[string]interface{}{"role": "tool", "tool_call_id": calls[1].ID, "content": "result-two"},
		map[string]interface{}{"role": "user", "content": "time-context-message"},
		map[string]interface{}{"role": "user", "content": "continue-now"})
	_, _, _, err = callGeminiConv(replay, mc, tools, "auto", nil, nil, conversationOptions{requestScope: "thread"})
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || turns[0] != 0 || turns[1] != 1 {
		t.Fatalf("expected same upstream conversation, turns=%v", turns)
	}
	for _, want := range []string{"result-one", "result-two", "time-context-message", "continue-now", "[Tool result for read_file]"} {
		if !strings.Contains(prompts[1], want) {
			t.Errorf("continuation dropped %q: %s", want, prompts[1])
		}
	}
	for _, old := range []string{"stable-system", "original-question", "Available tools"} {
		if strings.Contains(prompts[1], old) {
			t.Errorf("continuation resent old context %q", old)
		}
	}
}

func TestCallGeminiConvPlainChatAndConsumedHeads(t *testing.T) {
	resetConversationTestState(t)
	mc := Models["gemini-3.5-flash-lite"]
	first := []map[string]interface{}{{"role": "user", "content": "first-question"}}
	var turns []int
	stubConversationTestTurn(t, func(_ string, _ ModelConfig, st *convState) (*StreamResult, error) {
		turns = append(turns, st.turn)
		st.turn++
		return &StreamResult{Raw: conversationTestRaw(t, "first-answer")}, nil
	})
	if _, _, _, err := callGeminiConv(first, mc, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	next := append(append([]map[string]interface{}{}, first...), map[string]interface{}{"role": "assistant", "content": "first-answer"}, map[string]interface{}{"role": "user", "content": "follow-up"})
	if _, _, _, err := callGeminiConv(next, mc, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := callGeminiConv(next, mc, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(turns) != 3 || turns[0] != 0 || turns[1] != 1 || turns[2] != 0 {
		t.Fatalf("ordinary follow-up should hit once, stale fork must cold-start: %v", turns)
	}
}

func TestCallGeminiConvChangedHistoryOrScopeStartsFresh(t *testing.T) {
	for _, change := range []string{"system", "model", "tools", "choice", "scope"} {
		t.Run(change, func(t *testing.T) {
			resetConversationTestState(t)
			mc := Models["gemini-3.5-flash-lite"]
			tools, choice, scope := conversationTestTools(), interface{}("auto"), "one"
			first := []map[string]interface{}{{"role": "system", "content": "stable"}, {"role": "user", "content": "question"}}
			var turns []int
			stubConversationTestTurn(t, func(_ string, _ ModelConfig, st *convState) (*StreamResult, error) {
				turns = append(turns, st.turn)
				st.turn++
				return &StreamResult{Raw: conversationTestRaw(t, "answer")}, nil
			})
			if _, _, _, err := callGeminiConv(first, mc, tools, choice, nil, nil, conversationOptions{requestScope: scope}); err != nil {
				t.Fatal(err)
			}
			next := []map[string]interface{}{{"role": "system", "content": "stable"}, {"role": "user", "content": "question"}, {"role": "assistant", "content": "answer"}, {"role": "user", "content": "next"}}
			switch change {
			case "system":
				next[0]["content"] = "changed"
			case "model":
				mc.Mode++
			case "tools":
				tools[0]["function"].(map[string]interface{})["description"] = "changed"
			case "choice":
				choice = "required"
			case "scope":
				scope = "two"
			}
			if _, _, _, err := callGeminiConv(next, mc, tools, choice, nil, nil, conversationOptions{requestScope: scope}); err != nil {
				t.Fatal(err)
			}
			if len(turns) != 2 || turns[1] != 0 {
				t.Fatalf("changed %s must cold-start: %v", change, turns)
			}
		})
	}
}

func TestCallGeminiConvFailedContinuationIsNotReused(t *testing.T) {
	resetConversationTestState(t)
	mc := Models["gemini-3.5-flash-lite"]
	first := []map[string]interface{}{{"role": "user", "content": "original-question"}}
	var turns []int
	stubConversationTestTurn(t, func(_ string, _ ModelConfig, st *convState) (*StreamResult, error) {
		turns = append(turns, st.turn)
		st.turn++ // Even a failed request may already have advanced upstream.
		if len(turns) == 2 {
			return &StreamResult{}, errors.New("lost upstream response")
		}
		return &StreamResult{Raw: conversationTestRaw(t, "answer")}, nil
	})
	if _, _, _, err := callGeminiConv(first, mc, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	next := append(append([]map[string]interface{}{}, first...), map[string]interface{}{"role": "assistant", "content": "answer"}, map[string]interface{}{"role": "user", "content": "next"})
	if _, _, _, err := callGeminiConv(next, mc, nil, nil, nil, nil); err == nil {
		t.Fatal("expected upstream failure")
	}
	if _, _, _, err := callGeminiConv(next, mc, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(turns) != 3 || turns[1] != 1 || turns[2] != 0 {
		t.Fatalf("failure must invalidate consumed head: %v", turns)
	}
}

func TestCallGeminiConvEmptyContinuationIsNotReused(t *testing.T) {
	resetConversationTestState(t)
	mc := Models["gemini-3.5-flash-lite"]
	first := []map[string]interface{}{{"role": "user", "content": "original-question"}}
	var turns []int
	stubConversationTestTurn(t, func(_ string, _ ModelConfig, st *convState) (*StreamResult, error) {
		turns = append(turns, st.turn)
		st.turn++
		if len(turns) == 2 {
			return &StreamResult{Raw: conversationTestRaw(t, "")}, nil
		}
		return &StreamResult{Raw: conversationTestRaw(t, "answer")}, nil
	})
	if _, _, _, err := callGeminiConv(first, mc, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	next := append(append([]map[string]interface{}{}, first...), map[string]interface{}{"role": "assistant", "content": "answer"}, map[string]interface{}{"role": "user", "content": "next"})
	if _, _, _, err := callGeminiConv(next, mc, nil, nil, nil, nil); err == nil {
		t.Fatal("empty upstream result must fail")
	}
	if _, _, _, err := callGeminiConv(next, mc, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(turns) != 3 || turns[1] != 1 || turns[2] != 0 {
		t.Fatalf("empty response must invalidate consumed head: %v", turns)
	}
}

func TestGeminiConversationFingerprintNormalizesOptionalWireIDs(t *testing.T) {
	original := []map[string]interface{}{
		{"role": "user", "content": "read one"},
		{"role": "assistant", "tool_calls": []interface{}{conversationTestCall("server_call", "read_file", `{"path":"one"}`)}},
		{"role": "tool", "tool_call_id": "server_call", "name": "read_file", "content": "result"},
	}
	replayed := []map[string]interface{}{
		{"role": "user", "content": "read one"},
		{"role": "assistant", "tool_calls": []interface{}{conversationTestCall("generated_on_replay", "read_file", `{"path":"one"}`)}},
		{"role": "tool", "name": "read_file", "content": "result"},
	}
	beforeOriginal, beforeReplay := canonicalMessages(original), canonicalMessages(replayed)
	first := convPrefixKeys(original, "gemini-thread", true)
	second := convPrefixKeys(replayed, "gemini-thread", true)
	if len(first) != 3 || len(second) != 3 || first[2] != second[2] {
		t.Fatalf("Gemini optional IDs must match the same logical call/result: %v vs %v", first, second)
	}
	strictFirst := convPrefixKeys(original, "openai-thread")
	strictSecond := convPrefixKeys(replayed, "openai-thread")
	if strictFirst[2] == strictSecond[2] {
		t.Fatal("OpenAI histories must retain explicit call IDs for matching")
	}
	if beforeOriginal != canonicalMessages(original) || beforeReplay != canonicalMessages(replayed) {
		t.Fatal("fingerprinting must not rewrite public call IDs or input messages")
	}
}

func TestGeminiConversationFingerprintKeepsExplicitResultAssociation(t *testing.T) {
	makeHistory := func(firstID, secondID string) []map[string]interface{} {
		return []map[string]interface{}{
			{"role": "assistant", "tool_calls": []interface{}{
				conversationTestCall("call_one", "read_file", `{"path":"one"}`),
				conversationTestCall("call_two", "read_file", `{"path":"two"}`),
			}},
			{"role": "tool", "tool_call_id": firstID, "name": "read_file", "content": "result-A"},
			{"role": "tool", "tool_call_id": secondID, "name": "read_file", "content": "result-B"},
		}
	}
	ordered := convPrefixKeys(makeHistory("call_one", "call_two"), "gemini-thread", true)
	swapped := convPrefixKeys(makeHistory("call_two", "call_one"), "gemini-thread", true)
	if ordered[len(ordered)-1] == swapped[len(swapped)-1] {
		t.Fatal("result text associated with a different explicit call must not hit")
	}
}
