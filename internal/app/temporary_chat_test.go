package app

import "testing"

// temporaryChatEligible 的不变量：temporary_chat 开着就走临时对话；Responses
// previous_response_id 续接状态由本进程维护，因此它可以和服务端多轮并用。
func TestTemporaryChatEligible(t *testing.T) {
	old := rtCfg()
	defer func() { rtMu.Lock(); rtVal = old; rtMu.Unlock() }()

	cases := []struct {
		temp, multi, want bool
	}{
		{false, false, false}, // 都关
		{true, false, true},   // 只开临时 → 生效
		{true, true, true},    // 临时+多轮 → 仍隐藏会话
		{false, true, false},  // 只开多轮
	}
	for _, c := range cases {
		rtMu.Lock()
		rtVal.TemporaryChat = c.temp
		rtVal.MultiTurn = c.multi
		rtMu.Unlock()
		if got := temporaryChatEligible(); got != c.want {
			t.Errorf("temp=%v multi=%v => %v, 期望 %v", c.temp, c.multi, got, c.want)
		}
	}
}
