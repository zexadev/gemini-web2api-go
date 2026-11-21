package app

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// Gemini clients may omit function-call IDs when replaying contents. Exercise
// the real generate/stream handlers across three turns so regenerated adapter
// IDs cannot silently force each new tool result into a fresh conversation.
func TestGeminiHandlerContinuesToolHistoryWithoutCallIDs(t *testing.T) {
	resetConversationTestState(t)
	rtMu.Lock()
	rtVal.MultiTurn = true
	rtMu.Unlock()
	var prompts []string
	var turns []int
	stubConversationTestTurn(t, func(prompt string, _ ModelConfig, st *convState) (*StreamResult, error) {
		prompts = append(prompts, prompt)
		turns = append(turns, st.turn)
		st.cid = "gemini-handler-conversation"
		st.turn++
		text := "Third turn complete."
		switch len(prompts) {
		case 1:
			text = "```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"one\"}}\n```\n```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"two\"}}\n```"
		case 2:
			text = "Both Gemini tool results received."
		}
		return &StreamResult{Raw: conversationTestRaw(t, text)}, nil
	})
	firstUser := map[string]interface{}{"role": "user", "parts": []interface{}{map[string]interface{}{"text": "original-gemini-question"}}}
	contents := []interface{}{firstUser}
	req := map[string]interface{}{
		"systemInstruction": map[string]interface{}{"parts": []interface{}{map[string]interface{}{"text": "stable-gemini-system"}}},
		"tools": []interface{}{map[string]interface{}{"functionDeclarations": []interface{}{
			map[string]interface{}{"name": "read_file", "parameters": map[string]interface{}{"type": "object"}},
		}}},
	}
	call := func(action string) []interface{} {
		t.Helper()
		req["contents"] = contents
		body, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		path := "/v1beta/models/gemini-3.5-flash-lite:" + action
		if action == "streamGenerateContent" {
			path += "?alt=sse"
		}
		r := httptest.NewRequest("POST", path, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer gemini-test-caller")
		w := httptest.NewRecorder()
		handleGeminiModelItem(w, r)
		if w.Code != 200 {
			t.Fatalf("%s handler status=%d: %s", action, w.Code, w.Body.String())
		}
		var chunks []map[string]interface{}
		if action == "streamGenerateContent" {
			chunks = sseChunks(t, w.Body.String())
		} else {
			var chunk map[string]interface{}
			if err := json.Unmarshal(w.Body.Bytes(), &chunk); err != nil {
				t.Fatal(err)
			}
			chunks = append(chunks, chunk)
		}
		var parts []interface{}
		for _, chunk := range chunks {
			parts = append(parts, chunkParts(chunk)...)
		}
		return parts
	}
	firstParts := call("generateContent")
	if len(firstParts) != 2 {
		t.Fatalf("want two actual Gemini function calls, got %#v", firstParts)
	}
	var results []interface{}
	for i, raw := range firstParts {
		part := raw.(map[string]interface{})
		fc, ok := part["functionCall"].(map[string]interface{})
		if !ok || getStr(fc, "name") != "read_file" {
			t.Fatalf("missing actual Gemini functionCall: %#v", part)
		}
		// SDKs commonly replay only name/args, although our response also offers id.
		delete(fc, "id")
		results = append(results, map[string]interface{}{"functionResponse": map[string]interface{}{
			"name": fc["name"], "response": map[string]interface{}{"result": []string{"gemini-result-one", "gemini-result-two"}[i]},
		}})
	}
	contents = append(contents,
		map[string]interface{}{"role": "model", "parts": firstParts},
		map[string]interface{}{"role": "user", "parts": results},
	)
	secondParts := call("streamGenerateContent")
	contents = append(contents,
		map[string]interface{}{"role": "model", "parts": secondParts},
		map[string]interface{}{"role": "user", "parts": []interface{}{map[string]interface{}{"text": "final-gemini-followup"}}},
	)
	call("generateContent")
	if !reflect.DeepEqual(turns, []int{0, 1, 2}) {
		t.Fatalf("ID-less tool replay and action changes must retain one upstream conversation, turns=%v", turns)
	}
	for _, want := range []string{"gemini-result-one", "gemini-result-two"} {
		if !strings.Contains(prompts[1], want) {
			t.Errorf("second turn dropped %q: %s", want, prompts[1])
		}
	}
	if !strings.Contains(prompts[2], "final-gemini-followup") {
		t.Errorf("third turn dropped final user message: %s", prompts[2])
	}
	for i := 1; i < len(prompts); i++ {
		for _, old := range []string{"original-gemini-question", "stable-gemini-system", "Available tools"} {
			if strings.Contains(prompts[i], old) {
				t.Errorf("turn %d resent old context %q: %s", i+1, old, prompts[i])
			}
		}
	}
}
