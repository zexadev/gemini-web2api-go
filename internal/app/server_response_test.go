package app

import (
	"testing"
	"time"
)

func TestResponsesConversationPromptFitsOversizedFreshAndContinuation(t *testing.T) {
	oldCfg := rtCfg()
	defer func() {
		rtMu.Lock()
		rtVal = oldCfg
		rtMu.Unlock()
	}()
	rtMu.Lock()
	rtVal.MaxPromptBytes = 10
	rtMu.Unlock()

	messages := []map[string]interface{}{
		{"role": "user", "content": "first"},
		{"role": "assistant", "content": "answer"},
		{"role": "user", "content": "second"},
	}
	prompt := "this prompt is over the configured byte budget"
	if responsesConversationPromptFits(messages, prompt) {
		t.Fatal("oversized fresh conversation should use the attachment fallback")
	}

	parent := convParentKey(messages)
	convPut(parent, &convState{updated: time.Now()})
	defer convDelete(parent)
	if !responsesConversationPromptFits(messages, prompt) {
		t.Fatal("oversized continuation should use the existing Gemini conversation")
	}
}
