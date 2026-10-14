package app

import (
	"strings"
	"testing"
)

// system_prompt_suffix 必须锚在 prompt 最末尾（末尾指令权重最大），且纯空白不追加。
func TestSystemPromptSuffix(t *testing.T) {
	rtMu.Lock()
	saved := rtVal.SystemPromptSuffix
	rtVal.SystemPromptSuffix = "You are the execution engine of an agent system."
	rtMu.Unlock()
	t.Cleanup(func() {
		rtMu.Lock()
		rtVal.SystemPromptSuffix = saved
		rtMu.Unlock()
	})

	msgs := []map[string]interface{}{{"role": "user", "content": "hi"}}
	got := buildPrompt(msgs, nil, nil)
	want := "[System instruction]: You are the execution engine of an agent system."
	if !strings.HasSuffix(got, want) {
		t.Errorf("后缀应锚在 prompt 末尾, tail=%q", got[max(0, len(got)-100):])
	}

	rtMu.Lock()
	rtVal.SystemPromptSuffix = "   "
	rtMu.Unlock()
	if strings.Contains(buildPrompt(msgs, nil, nil), "execution engine") {
		t.Error("纯空白后缀不应追加")
	}
}
