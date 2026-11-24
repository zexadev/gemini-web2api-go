package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type conversationOptions struct {
	requestScope     string
	normalizeToolIDs bool            // Gemini permits clients to omit call/result IDs.
	recoverToolReply context.Context // Responses may regenerate an uncommitted capability disclaimer.
}

// conversationRequestScope separates local continuation state. prompt_cache_key
// is a client-supplied conversation namespace, not a Google prompt-cache control.
// Only a digest of credentials and caller identifiers is retained in memory.
func conversationRequestScope(r *http.Request, promptCacheKey, user string) string {
	credential := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		credential = strings.TrimSpace(h[7:])
	}
	for _, candidate := range []string{r.Header.Get("x-api-key"), r.Header.Get("x-goog-api-key"), r.URL.Query().Get("key")} {
		if credential == "" {
			credential = strings.TrimSpace(candidate)
		}
	}
	path := r.URL.Path
	for _, action := range []string{":generateContent", ":streamGenerateContent"} {
		path = strings.TrimSuffix(path, action)
	}
	b, _ := json.Marshal([]string{path, credential, promptCacheKey, user})
	return hashStr(string(b))
}

// Decode numbers without float64 rounding: distinct large integer arguments
// must remain distinct. Invalid argument strings are compared byte-for-byte.
func canonicalToolArguments(s string) string {
	if s == "" {
		return "{}"
	}
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	var v interface{}
	if d.Decode(&v) != nil {
		return s
	}
	var extra interface{}
	if d.Decode(&extra) != io.EOF {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return s
	}
	return string(b)
}

func conversationToolCalls(value interface{}) []ToolCall {
	b, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var calls []ToolCall
	if json.Unmarshal(b, &calls) != nil {
		return nil
	}
	for i := range calls {
		if calls[i].Type == "" {
			calls[i].Type = "function"
		}
		calls[i].Function.Arguments = canonicalToolArguments(calls[i].Function.Arguments)
	}
	return calls
}

func conversationCallMaps(calls []ToolCall) []interface{} {
	var result []interface{}
	for _, call := range calls {
		result = append(result, map[string]interface{}{
			"id": call.ID, "type": call.Type,
			"function": map[string]interface{}{"name": call.Function.Name, "arguments": call.Function.Arguments},
		})
	}
	return result
}

// Responses may replay one assistant turn as separate function_call/message
// items. Chat replays it as a single message. Normalize both to the same turn,
// including call identity/arguments rather than just the visible assistant text.
func normalizeConversationMessages(messages []map[string]interface{}) []map[string]interface{} {
	var result []map[string]interface{}
	names := map[string]string{}
	for _, source := range messages {
		role := getStr(source, "role")
		if role == "" {
			role = "user"
		}
		m := map[string]interface{}{"role": role, "content": contentToString(source["content"])}
		if role == "tool" && hasToolResultError(source) {
			m["is_error"] = true
		}
		for _, field := range []string{"name", "tool_call_id"} {
			if v := getStr(source, field); v != "" {
				m[field] = v
			}
		}
		calls := conversationToolCalls(source["tool_calls"])
		if len(calls) > 0 {
			m["tool_calls"] = conversationCallMaps(calls)
			for _, call := range calls {
				names[call.ID] = call.Function.Name
			}
		}
		if role == "tool" && getStr(m, "name") == "" {
			if name := names[getStr(m, "tool_call_id")]; name != "" {
				m["name"] = name
			}
		}
		if role == "assistant" && len(result) > 0 && getStr(result[len(result)-1], "role") == "assistant" {
			prev := result[len(result)-1]
			prev["content"] = getStr(prev, "content") + getStr(m, "content")
			if len(calls) > 0 {
				all := append(conversationToolCalls(prev["tool_calls"]), calls...)
				prev["tool_calls"] = conversationCallMaps(all)
			}
			continue
		}
		result = append(result, m)
	}
	return result
}

// Model/tool policy changes require a fresh upstream conversation because the
// retained history contains the original tool instructions and model state.
func convScopeKey(mc ModelConfig, tools []map[string]interface{}, choice interface{}, requestScope string) string {
	if len(tools) == 0 {
		tools = nil
	}
	mode, forced := parseToolChoice(choice)
	b, _ := json.Marshal(struct {
		Version              int
		Caller, Model        string
		Mode, Thinking, Tool int
		Account              int64
		Tools                []map[string]interface{}
		ToolMode, ForcedTool string
	}{1, requestScope, mc.HexID, mc.Mode, mc.thinkWire(), mc.Tool, mc.PinAccountID, tools, mode, forced})
	return hashStr(string(b))
}

// Incremental hashing is linear in history size, even with many new messages.
// JSON frames preserve message boundaries even when text contains separators.
func convPrefixKeys(messages []map[string]interface{}, scope string, normalizeToolIDs ...bool) []string {
	h := sha256.New()
	h.Write([]byte(scope + "\n"))
	var keys []string
	messages = normalizeConversationMessages(messages)
	if len(normalizeToolIDs) > 0 && normalizeToolIDs[0] {
		messages = conversationWithoutWireIDs(messages)
	}
	for _, m := range messages {
		b, _ := json.Marshal(m)
		h.Write(b)
		h.Write([]byte{'\n'})
		keys = append(keys, hex.EncodeToString(h.Sum(nil)))
	}
	return keys
}

// Gemini functionCall/functionResponse IDs are optional. Match their logical
// positions for fingerprinting only; preserve actual IDs in prompts/responses.
// Explicit result IDs still determine the association when results are reordered.
func conversationWithoutWireIDs(messages []map[string]interface{}) []map[string]interface{} {
	ids := map[string]string{}
	names := map[string][]string{}
	used := map[string]bool{}
	callIndex := 0
	for _, m := range messages {
		calls := conversationToolCalls(m["tool_calls"])
		for i := range calls {
			id := fmt.Sprintf("call_%d", callIndex)
			callIndex++
			if calls[i].ID != "" {
				ids[calls[i].ID] = id
			}
			names[calls[i].Function.Name] = append(names[calls[i].Function.Name], id)
			calls[i].ID = id
		}
		if len(calls) > 0 {
			m["tool_calls"] = conversationCallMaps(calls)
		}
		if getStr(m, "role") != "tool" {
			continue
		}
		wireID := getStr(m, "tool_call_id")
		id := ids[wireID]
		if id == "" && wireID == "" {
			for _, candidate := range names[getStr(m, "name")] {
				if !used[candidate] {
					id = candidate
					break
				}
			}
		}
		if id != "" {
			m["tool_call_id"] = id
			used[id] = true
		}
	}
	return messages
}

func conversationMessagesWithReply(messages []map[string]interface{}, text string, calls []ToolCall) []map[string]interface{} {
	reply := map[string]interface{}{"role": "assistant", "content": text}
	if len(calls) > 0 {
		reply["tool_calls"] = conversationCallMaps(calls)
	}
	full := append(append([]map[string]interface{}{}, messages...), reply)
	return normalizeConversationMessages(full)
}
