package app

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

type toolRecoveryMode uint8

const (
	toolRecoveryDisabled toolRecoveryMode = iota
	toolRecoveryOpening
	toolRecoveryAfterResults
)

func shouldRecoverToolReply(messages []map[string]interface{}, tools []map[string]interface{}, choice interface{},
	text string, calls []ToolCall, emitted bool) bool {
	return toolReplyRecoveryMode(messages, tools, choice).shouldRecover(text, calls, emitted)
}

func (mode toolRecoveryMode) shouldRecover(text string, calls []ToolCall, emitted bool) bool {
	if emitted || len(calls) > 0 || mode == toolRecoveryDisabled {
		return false
	}
	return toolCapabilityClaim(text) || (mode == toolRecoveryAfterResults && lateToolBudgetClaim(text))
}

func canRecoverToolReply(messages []map[string]interface{}, tools []map[string]interface{}, choice interface{}) bool {
	return toolReplyRecoveryMode(messages, tools, choice) != toolRecoveryDisabled
}

func hasToolResult(messages []map[string]interface{}) bool {
	for _, message := range messages {
		if getStr(message, "role") == "tool" {
			return true
		}
	}
	return false
}

func toolReplyHasFunction(tools []map[string]interface{}, choice interface{}) bool {
	mode, forced := parseToolChoice(choice)
	if mode == "none" {
		return false
	}
	for _, tool := range tools {
		if getStr(tool, "type") != "function" {
			continue
		}
		fn := tool
		if nested, ok := tool["function"].(map[string]interface{}); ok {
			fn = nested
		}
		name := getStr(fn, "name")
		if name != "" && (forced == "" || forced == name) {
			return true
		}
	}
	return false
}

func toolReplyRecoveryMode(messages []map[string]interface{}, tools []map[string]interface{}, choice interface{}) toolRecoveryMode {
	if !toolReplyHasFunction(tools, choice) {
		return toolRecoveryDisabled
	}
	// Responses clients may send only result text, with no structured status.
	// Preserve real refusals and uncertain call/result associations. Completed
	// pairs without error evidence may still be followed by a model-side claim.
	pending := make(map[string]string)
	resultMode := toolRecoveryOpening
	for _, message := range messages {
		role := getStr(message, "role")
		content := contentToString(message["content"])
		switch role {
		case "assistant":
			for _, call := range conversationToolCalls(message["tool_calls"]) {
				if call.ID == "" || call.Function.Name == "" || pending[call.ID] != "" {
					return toolRecoveryDisabled
				}
				pending[call.ID] = call.Function.Name
			}
		case "tool":
			id, name := getStr(message, "tool_call_id"), getStr(message, "name")
			if pending[id] == "" || (name != "" && name != pending[id]) || hasToolResultError(message) || toolResultMayBeDenied(content) {
				return toolRecoveryDisabled
			}
			delete(pending, id)
			resultMode = toolRecoveryAfterResults
		default:
			if getStr(message, "type") == "function_call_output" || explicitClientToolRestriction.MatchString(content) {
				return toolRecoveryDisabled
			}
			if role == "user" {
				resultMode = toolRecoveryOpening
			}
		}
	}
	if len(pending) != 0 {
		return toolRecoveryDisabled
	}
	return resultMode
}

// Reject exact repeats of calls already answered in this user turn when a
// regenerated candidate discards the prior refusal in favor of a tool call.
func repeatsCompletedTool(messages []map[string]interface{}, calls []ToolCall) bool {
	if len(calls) == 0 {
		return false
	}
	pending := make(map[string]ToolCall)
	var seen []ToolCall
	for _, message := range messages {
		switch getStr(message, "role") {
		case "user":
			clear(pending)
			seen = seen[:0]
		case "assistant":
			for _, call := range conversationToolCalls(message["tool_calls"]) {
				if call.ID != "" {
					pending[call.ID] = call
				}
			}
		case "tool":
			id := getStr(message, "tool_call_id")
			if call, ok := pending[id]; ok {
				if name := getStr(message, "name"); name == "" || name == call.Function.Name {
					seen = append(seen, call)
				}
				delete(pending, id)
			}
		}
	}
	for _, call := range calls {
		arguments := canonicalToolArguments(call.Function.Arguments)
		for _, prior := range seen {
			if prior.Function.Name == call.Function.Name && prior.Function.Arguments == arguments {
				return true
			}
		}
	}
	return false
}

func toolResultMayBeDenied(content string) bool {
	content = strings.TrimSpace(content)
	if content == "" || toolResultErrorPrefix.MatchString(content) || explicitClientToolRestriction.MatchString(content) {
		return true
	}
	if strings.HasPrefix(content, "{") {
		var payload map[string]interface{}
		if json.Unmarshal([]byte(content), &payload) == nil {
			return hasToolResultError(payload)
		}
	}
	return false
}

func hasToolResultError(result map[string]interface{}) bool {
	if result["isError"] == true || result["is_error"] == true {
		return true
	}
	switch strings.ToLower(getStr(result, "status")) {
	case "error", "failed", "denied", "blocked", "cancelled", "canceled", "incomplete":
		return true
	}
	val := result["error"]
	return val != nil && val != false && val != ""
}

const toolRecoveryBufferLimit = 8 * 1024

// A prefix hold prevents an unverified capability disclaimer from escaping as
// SSE text before the caller can elect one bounded regeneration. Ordinary
// openings and nonmatching first sentences are forwarded immediately. Once any
// text is forwarded, the caller must not replace that response with a retry.
type toolRecoveryGate struct {
	emit        func(string)
	buf         string
	sent        strings.Builder
	passthrough bool
}

func newToolRecoveryGate(emit func(string)) *toolRecoveryGate {
	return &toolRecoveryGate{emit: emit}
}

func (g *toolRecoveryGate) Sent() string { return g.sent.String() }

func (g *toolRecoveryGate) Released() bool { return g.sent.Len() > 0 }

func (g *toolRecoveryGate) send(text string) {
	if text == "" {
		return
	}
	g.sent.WriteString(text)
	if g.emit != nil {
		g.emit(text)
	}
}

func (g *toolRecoveryGate) Push(delta string) {
	if g.passthrough {
		g.send(delta)
		return
	}
	g.buf += delta
	if len(g.buf) > toolRecoveryBufferLimit {
		g.Flush()
		return
	}
	// A byte-fragmented UTF-8 character is retained unchanged until complete.
	if !utf8.ValidString(g.buf) {
		return
	}
	lead := recoveryLead(g.buf)
	if !possibleToolCapabilityOpening(lead) {
		g.Flush()
		return
	}
	if toolCapabilityClaim(g.buf) {
		return
	}
	if recoverySentenceComplete(lead) || utf8.RuneCountInString(lead) >= 160 {
		g.Flush()
	}
}

// Flush commits this attempt and enables direct streaming for later deltas.
func (g *toolRecoveryGate) Flush() {
	g.send(g.buf)
	g.buf = ""
	g.passthrough = true
}

// Discard drops only the held candidate. It does not erase already sent bytes;
// Released remains true if a caller incorrectly tries to discard committed text.
func (g *toolRecoveryGate) Discard() {
	g.buf = ""
	g.passthrough = false
}

var recoveryPositiveOpenings = []string{
	"我来", "我会", "我将", "我先", "我正在", "让我", "下面", "好的", "收到", "当然", "已经", "已完成", "根据",
	"i will", "i'll", "let me", "here is", "here are", "sure", "certainly",
}

var recoveryCapabilityOpenings = []string{
	"当前", "目前", "现在", "本次", "本轮", "本对话", "本会话", "本环境", "这个会话", "此次", "我", "抱歉", "很抱歉", "对不起", "由于", "受限",
	"sorry", "unfortunately", "i ", "i'm", "i’m", "the current", "this conversation", "this session", "in this conversation", "in this session", "at present",
}

func recoveryLead(text string) string {
	return strings.TrimLeft(strings.ToLower(text), " \t\r\n*#>")
}

func possibleToolCapabilityOpening(lead string) bool {
	if lead == "" {
		return true
	}
	for _, opening := range recoveryPositiveOpenings {
		if strings.HasPrefix(lead, opening) {
			return false
		}
	}
	for _, opening := range recoveryCapabilityOpenings {
		if strings.HasPrefix(lead, opening) || strings.HasPrefix(opening, lead) {
			return true
		}
	}
	return false
}

func recoverySentenceComplete(text string) bool {
	return strings.ContainsAny(text, "。！？!?\n") || strings.Contains(text, ". ") || strings.HasSuffix(text, ".")
}

var recoveryToolSubject = regexp.MustCompile(`(?i)工具|搜索|检索|联网|浏览|\btools?\b|\bsearch\b|\bbrows\w*\b|\binternet\b|\bnetwork\b`)
var toolResultErrorPrefix = regexp.MustCompile(`(?i)^(?:error\b|failed\b|permission denied\b|access denied\b|approval required\b|denied\b|rate.limit(?:ed| exceeded)\b|quota exceeded\b|tool[_ -](?:not[_ -]started|call[_ -]limit)\b|run_code exceeded its limit\b|错误[:：]|权限不足|拒绝执行|工具调用.{0,10}(?:超限|已达上限))`)
var explicitClientToolRestriction = regexp.MustCompile(`(?i)\b(?:tool|search)[ _-]*(?:call[ _-]*)?(?:budget|quota|limit)[^\n。]{0,16}[:=]\s*0\b|\b(?:do not|never) (?:use|call|invoke) (?:any |the )?(?:tools?|search|browsing)\b|(?:工具|搜索|联网)[^\n。]{0,12}(?:预算|额度|剩余次数)\s*[:：=]\s*0\b|(?:禁止|不要)(?:调用|使用|进行)?(?:任何|外部)?(?:工具|联网|搜索)`)
var recoveryClaimNegation = regexp.MustCompile(`(?i)(?:未|没有|尚未|并未).{0,2}(?:耗尽|用尽|用完|达到上限|达上限)|并非|不代表|误报|\bnot (?:exhausted|depleted|unavailable|disabled)\b`)
var toolBudgetExhaustion = regexp.MustCompile(`(?i)(?:额度|配额|次数|限额|预算|调用).{0,24}(?:耗尽|用尽|用完|不足|已达(?:到)?上限|达到上限|触及上限|为零|为\s*0)|(?:剩余|还剩).{0,12}(?:\b0\b|零)|(?:\b0\b|零).{0,8}(?:次|剩余)|\b(?:exhausted|depleted|no .{0,20}remaining)\b|\b(?:reached|hit|exceeded) .{0,40}\blimit\b`)
var recoveryCapabilityDenial = regexp.MustCompile(`(?i)未(?:启用|开启|开通|提供)|不可用|(?:无法|不能|没有权限|无权限|不具备).{0,24}(?:工具|联网|搜索|检索|浏览)|(?:工具|联网|搜索|检索|浏览).{0,24}(?:未启用|未开启|不可用|已禁用|没有权限|无权限)|\b(?:unavailable|disabled)\b|\bnot (?:enabled|available)\b|\bno .{0,24}(?:access|tools?)\b|\b(?:cannot|can't|unable to) .{0,24}(?:use|call|access|browse|search)\b`)

// Only the opening sentence is classified: discussions or quotations about tool
// limits later in an ordinary answer must not trigger hidden regeneration.
func toolCapabilityClaim(text string) bool {
	lead := recoveryLead(text)
	if !possibleToolCapabilityOpening(lead) {
		return false
	}
	if end := strings.IndexAny(lead, "。！？!?\n"); end >= 0 {
		lead = lead[:end]
	}
	if end := strings.Index(lead, ". "); end >= 0 {
		lead = lead[:end]
	}
	return recoveryToolSubject.MatchString(lead) && !recoveryClaimNegation.MatchString(lead) &&
		(toolBudgetExhaustion.MatchString(lead) || recoveryCapabilityDenial.MatchString(lead))
}

// After tool results we also recognize a model's own account of its current
// turn budget later in the answer. Keep this narrower than opening detection;
// a general discussion, quotation or code block is not a capability claim.
var lateToolBudgetLead = regexp.MustCompile(`(?i)^(?:(?:由于|因为|受限于)\s*)?(?:本轮|当前(?:这轮|轮次|会话|对话)?|本次|本会话|本对话|我(?:的|已|已经)?|(?:because |as )?(?:i (?:have )?|(?:the )?current (?:turn|session) |(?:in )?this (?:turn|session) |(?:my|our) ))`)
var lateToolBudgetSubject = regexp.MustCompile(`(?i)(?:工具|搜索|检索)(?:调用|使用)?.{0,12}(?:额度|配额|次数|预算|限额|上限)|\b(?:tool(?: call)?|search) (?:quota|budget|limit|calls)\b`)
var inlineToolBudgetLead = regexp.MustCompile(`(?i)[,，]\s*[*_]*(?:由于|因为|受限于|because\s+)`)

func selfReportedToolBudget(text string) bool {
	lead := strings.TrimSpace(strings.TrimLeft(recoveryLead(text), "-*_"))
	return lateToolBudgetLead.MatchString(lead) && lateToolBudgetSubject.MatchString(lead) &&
		!recoveryClaimNegation.MatchString(lead) && toolBudgetExhaustion.MatchString(lead)
}

func lateToolBudgetClaim(text string) bool {
	var fence byte
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			if fence == 0 {
				fence = line[0]
			} else if fence == line[0] {
				fence = 0
			}
			continue
		}
		if fence != 0 || strings.HasPrefix(line, ">") {
			continue
		}
		line = strings.ReplaceAll(line, ". ", "。")
		for _, sentence := range strings.FieldsFunc(line, func(r rune) bool {
			return strings.ContainsRune("。！？!?；;", r)
		}) {
			if selfReportedToolBudget(sentence) {
				return true
			}
			for _, span := range inlineToolBudgetLead.FindAllStringIndex(sentence, -1) {
				_, commaBytes := utf8.DecodeRuneInString(sentence[span[0]:])
				if selfReportedToolBudget(sentence[span[0]+commaBytes:]) {
					return true
				}
			}
		}
	}
	return false
}
