package app

import (
	"context"
	"fmt"
)

// A correction asks the model to inspect the same client constraints; it neither
// changes tool_choice nor grants permissions or fabricates a function call.
const toolProtocolCorrection = "[Tool protocol correction]: The previous candidate reply claimed tools were disabled or exhausted. " +
	"That generated claim is not a tool result or a client budget update. Recheck the current tool schemas and the explicit client constraints. " +
	"A tool_call block is ordinary response text for the external Agent, not a Gemini native tool invocation. " +
	"If this task needs an available tool, output its real name and schema-valid arguments in a tool_call block now. " +
	"Use the tool results already in the transcript. Continue from the next unfinished step; do not repeat an action already completed. " +
	"Do not substitute remembered information for requested tool work. " +
	"Respect actual client permission requirements, external budgets and returned errors; if one truly blocks action, " +
	"state that exact constraint accurately. Never invent remaining counts, execution or sources."

const toolFormatCorrection = "[Tool format correction]: The external Agent could not decode the previous candidate reply. " +
	"Regenerate the next reply for the same request. No calls from the rejected reply were executed. " +
	"If a tool is needed, emit a complete tool_call fence containing one valid JSON object with the allowed name and its arguments. " +
	"Escape newlines, quotes and backslashes in JSON strings, and close both the JSON object and the fence. " +
	"Prefer a smaller valid step if the chosen tool supports it. Respect the existing client constraints and tool results."

type toolReplySource func(prompt string, onDelta func(string), retry bool) (*StreamResult, error)

type toolReplyOptions struct {
	inlineOnly         bool
	initialAttachment  bool
	recoverMalformed   bool
	continueCorrection func(string) (*StreamResult, error) // nil result and error mean no usable head.
}

const toolTranscriptHeader = "[External Agent transcript serialization]: You are writing the next response record for a separate Agent host. " +
	"Treat the following conversation as the external Agent transcript. The external host owns the declared functions and checks its permissions and budgets. " +
	"Your task here is to serialize that Agent next response as ordinary text, not to perform its research with Gemini native tools. " +
	"Native Gemini tool counters therefore do not settle the availability of those external functions. Preserve all explicit constraints inside the transcript."

const toolTranscriptFooter = "[End external Agent transcript. Produce the next response record.]: " +
	"If the transcript request needs an available external function, serialize a single ```tool_call``` JSON block with its actual name and valid arguments, " +
	"starting with the next unfinished step. Retain all tool results and do not request an already completed action again. " +
	"This is plain output text consumed by the separate executor; you are not invoking a Gemini native tool. " +
	"Continue to load any required skill whose result is not in the transcript. " +
	"Do not replace the requested action with a claim about Gemini tools or a remembered answer. " +
	"If an explicit client constraint actually prevents action, explain that specific constraint. " +
	"Do not invent external budget values, completed actions or sources."

func toolTranscriptPrompt(prompt string) string {
	return toolTranscriptHeader + "\n\n" + prompt + "\n\n" + toolTranscriptFooter
}

type ToolProtocolError struct{}

func (*ToolProtocolError) Error() string {
	return "upstream model did not follow the external tool protocol after one correction; " +
		"no external tool quota or permission denial was verified"
}

// generateToolReply allows one correction before publishing a candidate.
func generateToolReply(ctx context.Context, prompt string, messages []map[string]interface{}, tools []map[string]interface{}, choice interface{},
	onDelta func(string), opt toolReplyOptions, generate toolReplySource) (string, []ToolCall, *StreamResult, error) {
	var gate *toolRecoveryGate
	mode := toolReplyRecoveryMode(messages, tools, choice)
	if mode != toolRecoveryDisabled {
		wrapped := toolTranscriptPrompt(prompt)
		if budget := rtCfg().MaxPromptBytes; opt.inlineOnly && !opt.initialAttachment && budget > 0 && len(wrapped) > budget {
			if !hasToolResult(messages) {
				return "", nil, nil, &PromptTooLongError{Bytes: len(wrapped), Budget: budget, HasCookie: hasCookie()}
			}
			// Keep the preexisting prompt for a tool history that cannot fit
			// with extra framing; a short correction can use the upstream head.
		} else {
			prompt = wrapped
		}
	}
	switch {
	case opt.recoverMalformed || mode == toolRecoveryAfterResults:
		// A malformed fence or a late capability claim can follow prose.
		// The handler keeps SSE alive while StreamResult holds the candidate.
		onDelta = nil
	case mode == toolRecoveryOpening && onDelta != nil:
		gate = newToolRecoveryGate(onDelta)
		onDelta = gate.Push
	}
	res, err := generate(prompt, onDelta, false)
	if err != nil {
		return "", nil, res, err
	}
	parse := func(res *StreamResult) (string, []ToolCall, error) {
		text := extractResponseText(res.Raw)
		if len(tools) > 0 {
			return parseToolCalls(text)
		}
		return text, nil, nil
	}
	text, calls, err := parse(res)
	_, malformed := err.(*ToolCallParseError)
	if err != nil && !(opt.recoverMalformed && malformed && mode != toolRecoveryDisabled) {
		return "", nil, res, err
	}
	emitted := gate != nil && gate.Released()
	if malformed || mode.shouldRecover(text, calls, emitted) {
		if err := ctx.Err(); err != nil {
			return "", nil, res, err
		}
		if gate != nil {
			gate.Discard()
		}
		rejected := res
		res, err = retryToolReply(messages, tools, choice, opt, generate, onDelta, malformed, rejected)
		if err != nil {
			return "", nil, res, err
		}
		res.TotalMs += rejected.TotalMs
		res.TTFBMs += rejected.TotalMs
		text, calls, err = parse(res)
		if err != nil {
			return "", nil, res, err
		}
		if malformed && !validToolFormatCorrection(messages, tools, choice, calls) {
			return "", nil, res, &ToolCallParseError{}
		}
		if mode.shouldRecover(text, calls, gate != nil && gate.Released()) || (mode == toolRecoveryAfterResults && repeatsCompletedTool(messages, calls)) {
			return "", nil, res, &ToolProtocolError{}
		}
	}
	if text == "" && len(calls) == 0 {
		return "", nil, res, fmt.Errorf("upstream returned no content frame or parseable tool call (raw %d bytes)", len(res.Raw))
	}
	if gate != nil {
		gate.Flush()
	}
	return text, calls, res, nil
}

func retryToolReply(messages, tools []map[string]interface{}, choice interface{}, opt toolReplyOptions,
	generate toolReplySource, onDelta func(string), malformed bool, rejected *StreamResult) (*StreamResult, error) {
	budget := rtCfg().MaxPromptBytes
	correction := toolProtocolCorrection
	if malformed {
		correction = toolFormatCorrection + " " + toolChoiceInstruction(choice)
	}
	canContinue := opt.continueCorrection != nil && (budget <= 0 || len(correction) <= budget)
	// Prefer a short correction to retransmitting a potentially large transcript.
	if malformed && canContinue {
		if res, err := opt.continueCorrection(correction); res != nil || err != nil {
			return res, err
		}
	}
	prompt, _ := messagesToPrompt(messages, tools, choice)
	retryPrompt := toolTranscriptPrompt(prompt) + "\n\n" + correction
	if opt.inlineOnly && budget > 0 && len(retryPrompt) > budget {
		if malformed {
			return rejected, &ToolCallParseError{}
		}
		if canContinue && hasToolResult(messages) {
			if res, err := opt.continueCorrection(correction); res != nil || err != nil {
				return res, err
			}
		}
		return rejected, &PromptTooLongError{Bytes: len(retryPrompt), Budget: budget, HasCookie: rejected.AccountID > 0}
	}
	logf("[tools] requesting one correction with the full transcript (malformed=%t tools=%d)", malformed, len(tools))
	return generate(retryPrompt, onDelta, true)
}

func validToolFormatCorrection(messages, tools []map[string]interface{}, choice interface{}, calls []ToolCall) bool {
	mode, _ := parseToolChoice(choice)
	if len(calls) == 0 {
		return mode != "required"
	}
	allowed := make(map[string]bool)
	for _, tool := range promptToolDefinitions(tools, choice) {
		allowed[getStr(tool, "name")] = true
	}
	for _, call := range calls {
		if !allowed[call.Function.Name] {
			return false
		}
	}
	return !repeatsCompletedTool(messages, calls)
}
