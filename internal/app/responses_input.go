package app

import (
	"encoding/json"
	"fmt"
)

// responsesToMessages normalizes output items replayed through Responses input
// into the Chat message shape used by prompt building and conversation matching.
// One assistant turn can be replayed as several call/text items in either order.
func responsesToMessages(req map[string]interface{}) []map[string]interface{} {
	var messages []map[string]interface{}
	if instr := getStr(req, "instructions"); instr != "" {
		messages = append(messages, map[string]interface{}{"role": "system", "content": instr})
	}
	if input, ok := req["input"].(string); ok {
		return append(messages, map[string]interface{}{"role": "user", "content": input})
	}
	input, _ := req["input"].([]interface{})

	// Results normally omit the function name. Scan all calls first so a result
	// can resolve its call_id even if its call appears later in the input array.
	callNames := map[string]string{}
	for _, raw := range input {
		item, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if getStr(item, "type") == "function_call" {
			callNames[responsesCallID(item)] = getStr(item, "name")
		}
		if getStr(item, "role") == "assistant" {
			parts, _ := item["content"].([]interface{})
			for _, rawPart := range parts {
				part, ok := rawPart.(map[string]interface{})
				if ok && getStr(part, "type") == "function_call" {
					callNames[responsesCallID(part)] = getStr(part, "name")
				}
			}
		}
	}

	callIndex := 0
	toolCall := func(item map[string]interface{}) interface{} {
		id := responsesCallID(item)
		if id == "" {
			id = fmt.Sprintf("call_%d", callIndex)
		}
		callIndex++
		args := getStr(item, "arguments")
		if args == "" {
			args = "{}"
			if value := item["arguments"]; value != nil {
				if _, isText := value.(string); !isText {
					if encoded, err := json.Marshal(value); err == nil {
						args = string(encoded)
					}
				}
			}
		}
		return map[string]interface{}{
			"id": id, "type": "function",
			"function": map[string]interface{}{"name": getStr(item, "name"), "arguments": args},
		}
	}
	appendAssistant := func(text string, calls []interface{}) {
		var assistant map[string]interface{}
		if len(messages) > 0 && getStr(messages[len(messages)-1], "role") == "assistant" {
			assistant = messages[len(messages)-1]
		} else {
			assistant = map[string]interface{}{"role": "assistant", "content": ""}
			messages = append(messages, assistant)
		}
		assistant["content"] = getStr(assistant, "content") + text
		if len(calls) > 0 {
			existing, _ := assistant["tool_calls"].([]interface{})
			assistant["tool_calls"] = append(existing, calls...)
		}
	}

	for _, raw := range input {
		if text, ok := raw.(string); ok {
			messages = append(messages, map[string]interface{}{"role": "user", "content": text})
			continue
		}
		item, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		switch getStr(item, "type") {
		case "reasoning":
			continue
		case "function_call":
			appendAssistant("", []interface{}{toolCall(item)})
			continue
		case "function_call_output":
			id, name := getStr(item, "call_id"), getStr(item, "name")
			if name == "" && id != "" {
				name = callNames[id]
			}
			content := contentToString(item["output"])
			if content == "" && item["output"] != nil {
				if _, text := item["output"].(string); !text {
					if encoded, err := json.Marshal(item["output"]); err == nil {
						content = string(encoded)
					}
				}
			}
			message := map[string]interface{}{
				"role": "tool", "tool_call_id": id, "name": name, "content": content,
			}
			if hasToolResultError(item) {
				message["is_error"] = true
			}
			messages = append(messages, message)
			continue
		case "", "message":
			// Ordinary input and replayed assistant message items are handled below.
		default:
			continue // Unknown output items are not empty user messages.
		}
		role := getStr(item, "role")
		if role == "assistant" {
			text := getStr(item, "content")
			var calls []interface{}
			parts, _ := item["content"].([]interface{})
			for _, rawPart := range parts {
				part, ok := rawPart.(map[string]interface{})
				if !ok {
					continue
				}
				switch getStr(part, "type") {
				case "output_text", "text", "input_text":
					text += getStr(part, "text")
				case "function_call":
					calls = append(calls, toolCall(part))
				}
			}
			appendAssistant(text, calls)
			continue
		}
		if role == "" {
			if _, hasContent := item["content"]; !hasContent {
				continue
			}
			role = "user"
		}
		messages = append(messages, map[string]interface{}{"role": role, "content": contentToString(item["content"])})
	}
	return messages
}

func responsesCallID(item map[string]interface{}) string {
	if id := getStr(item, "call_id"); id != "" {
		return id
	}
	return getStr(item, "id")
}
