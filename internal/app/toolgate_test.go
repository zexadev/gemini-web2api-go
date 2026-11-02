package app

import (
	"encoding/json"
	"strings"
	"testing"
)

// feed 按给定的切法把整段文本喂进闸门，返回实际发给客户端的内容。
// 切法要能任意变化：上游帧边界落在哪儿完全不由我们决定，闸门的结果不能因此不同。
func feed(t *testing.T, chunks []string) string {
	t.Helper()
	var got strings.Builder
	g := newToolFenceGate(func(s string) { got.WriteString(s) })
	for _, c := range chunks {
		g.Push(c)
	}
	if got.String() != g.Sent() {
		t.Fatalf("Sent() 和实际发出的对不上: %q vs %q", g.Sent(), got.String())
	}
	return got.String()
}

// 逐字节喂是最狠的切法：每个字符都是一个新帧，半截围栏必然出现在帧边界上。
func chars(s string) []string {
	var out []string
	for _, r := range s {
		out = append(out, string(r))
	}
	return out
}

func TestToolFenceGate(t *testing.T) {
	cases := []struct {
		name string
		full string
		want string
	}{
		{"没有围栏就原样发", "你好，今天天气不错。", "你好，今天天气不错。"},
		{
			"围栏前后的正文都要发，围栏本身扣住",
			"让我查一下。\n```tool_call\n{\"name\": \"get_weather\", \"arguments\": {}}\n```\n查完了。",
			"让我查一下。\n\n查完了。",
		},
		{
			"整段都是围栏则一个字都不发",
			"```tool_call\n{\"name\": \"f\", \"arguments\": {}}\n```",
			"",
		},
		{
			"连着两个围栏",
			"a\n```tool_call\n{\"name\":\"f\"}\n```\nb\n```tool_call\n{\"name\":\"g\"}\n```\nc",
			"a\n\nb\n\nc",
		},
		{
			// 普通代码块不是 tool_call，不能扣住
			"普通代码块照常发",
			"看这段：\n```python\nprint(1)\n```\n就这样。",
			"看这段：\n```python\nprint(1)\n```\n就这样。",
		},
		{
			// 参数的字符串里带 ```：闭合围栏是 JSON 结束之后那个，不是第一个
			"参数里带代码块",
			"写文件。\n```tool_call\n" + fencedArgsCall + "\n```\n写好了。",
			"写文件。\n\n写好了。",
		},
		{
			// JSON 写坏了：退回第一个 ``` 当闭合
			"围栏里 JSON 写坏了",
			"a\n```tool_call\n{\"name\": f}\n```\nb",
			"a\n\nb",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 三种切法结果必须一致
			if got := feed(t, []string{c.full}); got != c.want {
				t.Errorf("整段喂: 得到 %q，期望 %q", got, c.want)
			}
			if got := feed(t, chars(c.full)); got != c.want {
				t.Errorf("逐字符喂: 得到 %q，期望 %q", got, c.want)
			}
			mid := len(c.full) / 2
			for mid < len(c.full) && !isRuneStart(c.full[mid]) {
				mid++
			}
			if got := feed(t, []string{c.full[:mid], c.full[mid:]}); got != c.want {
				t.Errorf("对半喂: 得到 %q，期望 %q", got, c.want)
			}
		})
	}
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// 闸门发出去的内容 + parseToolCalls 的最终文本，必须能拼回同一份正文 ——
// 这是流式和非流式结果一致的判据。
func TestToolFenceGateMatchesParse(t *testing.T) {
	full := "让我查一下。\n```tool_call\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"London\"}}\n```\n查完了。"

	streamed := feed(t, chars(full))
	final, calls := parseToolCalls(full)

	if len(calls) != 1 || calls[0].Function.Name != "get_weather" {
		t.Fatalf("工具调用没解析出来: %+v", calls)
	}
	// 流式发出的可能比最终文本多首尾空白（parseToolCalls 收尾会 TrimSpace）。
	if strings.TrimSpace(streamed) != final {
		t.Errorf("流式发出 %q，最终文本 %q", strings.TrimSpace(streamed), final)
	}
}

// fencedArgsCall 是一次写文件调用，content 里有两个 markdown 代码块（实测模型就这么吐：
// ``` 原样出现在 JSON 字符串里，换行和引号正常转义）。
const fencedArgsCall = `{"name": "write_file", "arguments": {"path": "README.md", "content": "# Foo\n\n` +
	"```" + `bash\npip install foo\n` + "```" + `\n\n` + "```" + `python\nprint(\"hi\")\n` + "```" + `\n"}}`

// 参数里的 ``` 不能把调用截断：要解析出完整参数，正文里不能漏出半截 JSON，
// 流式发出的和最终文本要一致。
func TestToolCallWithFencedArgs(t *testing.T) {
	full := "写文件。\n```tool_call\n" + fencedArgsCall + "\n```\n写好了。"
	final, calls := parseToolCalls(full)
	if len(calls) != 1 || calls[0].Function.Name != "write_file" {
		t.Fatalf("工具调用没解析出来: %+v，正文 %q", calls, final)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("参数不是合法 JSON: %v", err)
	}
	if want := "# Foo\n\n```bash\npip install foo\n```\n\n```python\nprint(\"hi\")\n```\n"; args["content"] != want {
		t.Errorf("content 得到 %q，期望 %q", args["content"], want)
	}
	if final != "写文件。\n\n写好了。" {
		t.Errorf("最终正文 %q", final)
	}
	if streamed := feed(t, chars(full)); strings.TrimSpace(streamed) != final {
		t.Errorf("流式发出 %q，最终文本 %q", streamed, final)
	}
}

// 未闭合的围栏：闸门扣住不发，收尾时由 remainingOf 补回去，客户端不会丢内容。
func TestToolFenceGateUnclosed(t *testing.T) {
	full := "开始了。\n```tool_call\n{\"name\": \"f\""
	streamed := feed(t, chars(full))
	if streamed != "开始了。\n" {
		t.Fatalf("扣住的部分不对: %q", streamed)
	}
	// parseToolCalls 对未闭合围栏不做处理，原样留在文本里
	final, calls := parseToolCalls(full)
	if len(calls) != 0 {
		t.Fatalf("未闭合围栏不该解析出工具调用: %+v", calls)
	}
	if rest := remainingOf(final, streamed); rest == "" {
		t.Error("收尾补发算出来是空的，扣住的内容会丢")
	}
}

func TestPartialPrefixLen(t *testing.T) {
	m := toolFenceOpen // "```tool_call"
	for _, c := range []struct {
		s    string
		want int
	}{
		{"abc", 0},
		{"abc`", 1},
		{"abc``", 2},
		{"abc```", 3},
		{"abc```tool_c", 9},
		{"", 0},
		{"中文`", 1}, // 切点必须落在 ASCII 上，不能劈开多字节字符
		{"中文", 0},
	} {
		if got := partialPrefixLen(c.s, m); got != c.want {
			t.Errorf("partialPrefixLen(%q) = %d，期望 %d", c.s, got, c.want)
		}
	}
}
