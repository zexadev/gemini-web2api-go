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

type toolReplySource func(prompt string, onDelta func(string), retry bool) (*StreamResult, error)

type toolReplyOptions struct {
	inlineOnly         bool
	continueCorrection func(string) (*StreamResult, error)
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

// generateToolReply repairs at most one unexposed capability disclaimer. After
// tool results the claim can occur anywhere, so publish only the selected reply.
func generateToolReply(ctx context.Context, prompt string, messages []map[string]interface{}, tools []map[string]interface{}, choice interface{},
	onDelta func(string), opt toolReplyOptions, generate toolReplySource) (string, []ToolCall, *StreamResult, error) {
	var gate *toolRecoveryGate
	mode := toolReplyRecoveryMode(messages, tools, choice)
	if mode != toolRecoveryDisabled {
		wrapped := toolTranscriptPrompt(prompt)
		if budget := rtCfg().MaxPromptBytes; opt.inlineOnly && budget > 0 && len(wrapped) > budget {
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
	case mode == toolRecoveryAfterResults:
		// StreamResult already retains the full candidate. Keep SSE alive at
		// the handler, but do not commit text that may need correction.
		onDelta = nil
	case mode == toolRecoveryOpening && onDelta != nil:
		gate = newToolRecoveryGate(onDelta)
		onDelta = gate.Push
	}
	res, err := generate(prompt, onDelta, false)
	if err != nil {
		return "", nil, res, err
	}
	parse := func(res *StreamResult) (string, []ToolCall) {
		text := extractResponseText(res.Raw)
		if len(tools) > 0 {
			return parseToolCalls(text)
		}
		return text, nil
	}
	text, calls := parse(res)
	emitted := gate != nil && gate.Released()
	if mode.shouldRecover(text, calls, emitted) {
		if err := ctx.Err(); err != nil {
			return "", nil, res, err
		}
		// Use full original history in a new upstream conversation. The hidden
		// candidate and this adapter-only correction are not client history.
		retryPrompt, _ := messagesToPrompt(messages, tools, choice)
		retryPrompt = toolTranscriptPrompt(retryPrompt) + "\n\n" + toolProtocolCorrection
		continueUpstream := false
		if budget := rtCfg().MaxPromptBytes; opt.inlineOnly && budget > 0 && len(retryPrompt) > budget {
			continueUpstream = hasToolResult(messages) && opt.continueCorrection != nil && len(toolProtocolCorrection) <= budget
			if !continueUpstream {
				return "", nil, res, &PromptTooLongError{Bytes: len(retryPrompt), Budget: budget, HasCookie: res.AccountID > 0}
			}
		}
		logf("[tools] recovering an unexposed capability disclaimer once (tools=%d)", len(tools))
		if gate != nil {
			gate.Discard()
		}
		firstMs := res.TotalMs
		if continueUpstream {
			res, err = opt.continueCorrection(toolProtocolCorrection)
		} else {
			res, err = generate(retryPrompt, onDelta, true)
		}
		if res != nil {
			res.TotalMs += firstMs
			res.TTFBMs += firstMs
		}
		if err != nil {
			return "", nil, res, err
		}
		text, calls = parse(res)
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
