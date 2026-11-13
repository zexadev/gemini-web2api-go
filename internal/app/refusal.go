package app

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
)

// The single opt-in switch is deliberately link-only: wording, language,
// quotation markers and reply length do not affect detection. Require the
// actual policy URL, not a similarly named host or a different path.
var refusalPolicyLink = regexp.MustCompile(`(?i)https?://gemini\.google/policy-guidelines/?(?:[?#][^\s)<>"'“”‘’\x60\]}]*)?(?:$|[\s)<>"'“”‘’\x60\]}]|[.,;:，。；：](?:$|[\s)<>"'“”‘’\x60\]}]))`)

const geminiRefusalCode = "upstream_refusal"

type GeminiRefusalError struct{}

func (*GeminiRefusalError) Error() string {
	return "upstream_refusal: Gemini response contains its official policy URL"
}

func isGeminiRefusal(text string) bool {
	return refusalPolicyLink.MatchString(text)
}

func isGeminiRefusalError(err error) bool {
	var refusal *GeminiRefusalError
	return errors.As(err, &refusal)
}

func writeGeminiRefusalError(w http.ResponseWriter) {
	// 422 identifies an unusable upstream response. Never include the prompt or
	// refusal in the error, and leave retry/model routing to the API client.
	writeJSON(w, http.StatusUnprocessableEntity, map[string]interface{}{
		"error": map[string]string{
			"message": (&GeminiRefusalError{}).Error(),
			"type":    "upstream_response_error",
			"code":    geminiRefusalCode,
		},
	})
}

// Hold ordinary chat output until the final reply is checked, so a refusal
// cannot escape in an early content/reasoning delta. Media behavior is unchanged.
type checkedChatOutput struct {
	onText, onReasoning func(string)
	text, reasoning     strings.Builder
}

func newCheckedChatOutput(enabled bool, mc ModelConfig, onText, onReasoning func(string)) *checkedChatOutput {
	if !enabled || mc.Tool != 0 || (onText == nil && onReasoning == nil) {
		return nil
	}
	return &checkedChatOutput{onText: onText, onReasoning: onReasoning}
}

func (c *checkedChatOutput) accept() {
	if c == nil {
		return
	}
	if c.onReasoning != nil && c.reasoning.Len() > 0 {
		c.onReasoning(c.reasoning.String())
	}
	if c.onText != nil && c.text.Len() > 0 {
		c.onText(c.text.String())
	}
}

func rejectGeminiRefusal(text string, res *StreamResult) error {
	if !isGeminiRefusal(text) {
		return nil
	}
	if res != nil {
		res.Emitted = ""
		res.EmittedReasoning = ""
	}
	return &GeminiRefusalError{}
}
