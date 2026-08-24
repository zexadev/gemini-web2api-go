package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// randHex returns n random hex chars. Equivalent to Python's
// `uuid.uuid4().hex[:n]` — produces a pure hex string of length n.
//
// Note: Go's `uuid.NewString()` returns the dashed 8-4-4-4-12 format,
// so naive slicing like `uuid.NewString()[:12]` would yield 11 hex + 1 dash.
// We sidestep that by drawing fresh random bytes.
func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// messagesToPrompt converts OpenAI messages[] (+ optional tools[]) to a single
// prompt string for Gemini. Tool schemas are embedded as a system instruction
// telling the model to emit ```tool_call``` blocks.
//
// toolChoice 是 OpenAI 的 tool_choice 字段（"none"/"auto"/"required" 或
// {"type":"function","function":{"name":...}}）。上游没有协议层的工具调用，
// 只能把约束写进指令。"required" 尤其必要：实测 Gemini 对自己能回答的问题
// （查天气之类）会直接作答而不调工具，不强制就拿不到 tool_call。
// 返回 (拼好的 prompt, 最新那条用户消息)。
//
// 超长检查不在这里做 —— 挂了 cookie 时超长会转成文本附件，而那要等挑完账号和
// 出口才知道能不能做，所以判断放在 streamGenerate 里。最新那条消息单独返回，
// 转附件时用来内联，好让模型不必去文件里找问题。
func messagesToPrompt(messages []map[string]interface{}, tools []map[string]interface{},
	toolChoice interface{}) (string, string) {
	return buildPrompt(messages, tools, toolChoice), latestUserMessage(messages)
}

// latestUserMessage 取最后一条 user 消息的正文，没有则返回空串。
func latestUserMessage(messages []map[string]interface{}) string {
	for i := len(messages) - 1; i >= 0; i-- {
		role := getStr(messages[i], "role")
		if role == "user" || role == "" {
			if c := strings.TrimSpace(contentToString(messages[i]["content"])); c != "" {
				return c
			}
		}
	}
	return ""
}

func buildPrompt(messages []map[string]interface{}, tools []map[string]interface{},
	toolChoice interface{}) string {
	var parts []string

	toolsInjected := false
	mode, forced := parseToolChoice(toolChoice)
	if len(tools) > 0 && mode != "none" {
		var defs []map[string]interface{}
		for _, tool := range tools {
			fn := tool
			if t, ok := tool["type"].(string); ok && t == "function" {
				if f, ok := tool["function"].(map[string]interface{}); ok {
					fn = f
				}
			}
			name := getStr(fn, "name")
			if forced != "" && name != forced {
				continue // 指定了函数名，其余不进 prompt
			}
			defs = append(defs, map[string]interface{}{
				"name":        name,
				"description": getStr(fn, "description"),
				"parameters":  fn["parameters"],
			})
		}
		if len(defs) > 0 {
			defsJSON, _ := json.MarshalIndent(defs, "", "  ")
			rule := "Only use tool_call blocks when needed."
			switch {
			case forced != "":
				rule = fmt.Sprintf(
					"You MUST call the tool %q. Reply with the tool_call block and nothing "+
						"else — do not answer the question yourself, even if you know the answer.",
					forced)
			case mode == "required":
				rule = "You MUST call one of the tools above. Reply with the tool_call block " +
					"and nothing else — do not answer the question yourself, even if you " +
					"know the answer."
			}
			parts = append(parts, fmt.Sprintf(
				"[System instruction]: You have access to tools. "+
					"To call a tool, respond with:\n"+
					"```tool_call\n{\"name\": \"func_name\", \"arguments\": {...}}\n```\n"+
					"%s\n\n"+
					"Available tools:\n%s", rule, string(defsJSON),
			))
			toolsInjected = true
		}
	}

	for _, msg := range messages {
		role := getStr(msg, "role")
		content := contentToString(msg["content"])

		switch role {
		case "system":
			parts = append(parts, "[System instruction]: "+content)
		case "assistant":
			if tcs, ok := msg["tool_calls"].([]interface{}); ok && len(tcs) > 0 {
				var tcStrs []string
				for _, tc := range tcs {
					tcMap, ok := tc.(map[string]interface{})
					if !ok {
						continue
					}
					fn, _ := tcMap["function"].(map[string]interface{})
					name := getStr(fn, "name")
					args := getStr(fn, "arguments")
					if args == "" {
						args = "{}"
					}
					tcStrs = append(tcStrs, fmt.Sprintf(
						"```tool_call\n{\"name\": \"%s\", \"arguments\": %s}\n```",
						name, args,
					))
				}
				parts = append(parts, "[Assistant]: "+content+"\n"+strings.Join(tcStrs, "\n"))
			} else {
				parts = append(parts, "[Assistant]: "+content)
			}
		case "tool":
			parts = append(parts, formatToolResult(getStr(msg, "name"), content))
		default:
			if content != "" {
				parts = append(parts, content)
			}
		}
	}

	// 工具格式指令放在最前，但 agentic 客户端（Codex/rikkahub 等）的开发者提示动辄
	// 40KB，还自带「emit function calls」这种原生工具框架的措辞，压在中间把顶部那条
	// 格式指令冲没了 —— 模型于是要么答「我没有工具」要么答非所问（用户报的「已读乱回」）。
	// 判据：拿 Codex 真实 42KB prompt 原样重放，模型说「无法查看本地文件系统」、不吐围栏；
	// 同一份末尾追加提醒后立刻吐 ```tool_call```。所以末尾再锚一次格式、明确「没有别的
	// 工具通道」，压过客户端的原生框架。
	// 两个进一步判据（弱模型 anon 3.6 Flash、多命令任务、各 4 次）：
	//  ① 只放格式指令、不带示例：0/4 命中，全部退化成写 ```powershell 代码块给用户看；
	//  ② 带一个「用户问 X → 正确回复是这个 tool_call 块」的具体示例：3-4/4 命中。
	// 所以必须给行为化的 few-shot。示例用中性工具名（run_command），实测模型仍用真实
	// 工具名（shell_command）0/4 抄假名 —— 示例教的是「动作」不是名字，故不必按客户端
	// 动态生成 schema。
	if toolsInjected {
		parts = append(parts,
			"[System instruction — highest priority]: To run a command or read a file you MUST "+
				"output a ```tool_call``` block — this is the ONLY execution channel. A "+
				"```powershell/```bash/```cmd/```python code block is NOT executed, it is only shown "+
				"to the user; never use one to run anything, and never claim you lack tools.\n"+
				"Act, do not explain: when the request needs a tool, your reply is ONE ```tool_call``` "+
				"block and nothing else — do not list the commands you 'would' run, do not output more "+
				"than one tool_call, do not add prose. If several steps are needed, do the FIRST one "+
				"now; you will be called again with its result to continue.\n"+
				"Know when to STOP: once a tool result shows a step succeeded (for example it exited "+
				"with code 0, even with no printed output), that step is DONE — do not run it again and "+
				"do not retry the same thing a different way. When the whole request is accomplished, "+
				"stop calling tools and reply with a short final answer instead of another tool_call.\n"+
				"Example of a correct turn — user asks \"list the files here and tell me the size of "+
				"README.md\", you reply with exactly one block and nothing else:\n"+
				"```tool_call\n{\"name\": \"run_command\", \"arguments\": {\"command\": \"ls -la\"}}\n```\n"+
				"(In your block use one of the real tool names listed above with its own arguments.) "+
				"When the request needs a tool, emit the tool_call block now.")
	}

	var nonEmpty []string
	for _, p := range parts {
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	return strings.Join(nonEmpty, "\n\n")
}

func contentToString(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		var bits []string
		for _, c := range v {
			cm, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			t := getStr(cm, "type")
			if t == "text" || t == "input_text" {
				bits = append(bits, getStr(cm, "text"))
			}
		}
		return strings.Join(bits, " ")
	default:
		return ""
	}
}

func getStr(m map[string]interface{}, k string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[k].(string); ok {
		return s
	}
	return ""
}

// exitCodeRe 从工具结果里抠退出码。Codex 的 shell 结果是一段带
// "Process exited with code N" 的包装文本。
var exitCodeRe = regexp.MustCompile(`(?i)exit(?:ed with|\s*code)?\s*(?:code\s*)?(\d+)`)

// formatToolResult 把工具结果渲染进 prompt，关键是让弱模型认得出「成功」。
//
// 为什么要重写：agentic 客户端（Codex）的成功结果长这样——
//
//	Chunk ID: 0cdbf0\nWall time: 0.07s\nProcess exited with code 0\nOriginal token count: 0\nOutput:\n
//
// 写文件/设值这类命令没有 stdout，"Output:" 后面是空的。弱模型（anon 3.6 Flash）
// 把「无输出」当成「没干成」，于是换个写法一遍遍重试——实测「写 hello 到 a.txt」
// 一个任务里试了 26 种命令、90s 不收敛，文件其实第一次就写对了。exit 0 就明摆在
// 结果里，但被 Chunk ID / Wall time / token count 这些噪音埋了，且没告诉它「无输出
// 是正常的」。这里把它压成一行清爽的成功/失败信号，并显式说明无输出正常、别重跑。
//
// 只加终止指令（提醒里那句 "Know when to STOP"）实测没用（26→27 命令），要配合
// 这个清爽信号一起才压得住循环。
func formatToolResult(name, raw string) string {
	label := "Tool result"
	if name != "" {
		label = "Tool result for " + name
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Sprintf("[%s]: ✅ 完成，无输出（正常，动作已生效，不要重复执行）。", label)
	}
	// Codex 风格包装：末尾 "Output:" 后是真正的 stdout。
	out := trimmed
	if i := strings.LastIndex(trimmed, "Output:"); i >= 0 {
		out = strings.TrimSpace(trimmed[i+len("Output:"):])
	}
	if m := exitCodeRe.FindStringSubmatch(trimmed); m != nil {
		if m[1] == "0" {
			if out == "" {
				return fmt.Sprintf("[%s]: ✅ 命令成功（exit 0），无文本输出——这对写文件/设值类命令是正常的，动作已完成，不要再跑同一条命令。", label)
			}
			return fmt.Sprintf("[%s]: ✅ 命令成功（exit 0）。输出：\n%s", label, out)
		}
		return fmt.Sprintf("[%s]: ❌ 命令失败（exit %s）。输出：\n%s", label, m[1], out)
	}
	return fmt.Sprintf("[%s]: %s", label, raw)
}

const (
	toolFenceOpen  = "```tool_call"
	toolFenceClose = "```"
)

// toolFenceGate 让带 tools 的请求也能真流式。
//
// 问题：上游没有协议层的工具调用，我们让模型吐 ```tool_call``` 围栏，而围栏要
// 完整文本才能解析。边出边转发会把围栏原文推给客户端 —— 客户端看到的是一段
// markdown 代码块，不是 tool_calls。所以这条路以前退化成收完再发。
//
// 解法：只发**确定不属于围栏**的部分。围栏内的全部扣住，最后由 parseToolCalls
// 统一转成 tool_calls。关键是尾巴上可能压着半个开围栏（比如只到两个反引号，
// 或者到 tool_c 就断了），那部分也得扣住等下一帧 —— 否则先发出去，下一帧才发现
// 它是围栏的开头，而已发出的内容收不回来。
//
// Sent() 是**实际发给客户端**的文本，跟 deltaTracker 的 emitted 不是一回事
// （后者含围栏原文）。收尾补发尾巴时必须拿这个比，否则前缀对不上，尾巴会丢。
type toolFenceGate struct {
	emit    func(string)
	buf     string // 还没判定完的尾巴
	sent    strings.Builder
	inFence bool
}

func newToolFenceGate(emit func(string)) *toolFenceGate {
	return &toolFenceGate{emit: emit}
}

// Sent 返回到目前为止实际发给客户端的全部文本。
func (g *toolFenceGate) Sent() string {
	if g == nil {
		return ""
	}
	return g.sent.String()
}

func (g *toolFenceGate) send(s string) {
	if s == "" {
		return
	}
	g.sent.WriteString(s)
	g.emit(s)
}

// Push 吃进一段增量文本，把确定不在围栏里的部分立刻发出去。
func (g *toolFenceGate) Push(delta string) {
	g.buf += delta
	for {
		if g.inFence {
			j := strings.Index(g.buf, toolFenceClose)
			if j < 0 {
				return // 围栏还没闭合，整段扣住
			}
			g.buf = g.buf[j+len(toolFenceClose):]
			g.inFence = false
			continue
		}
		if i := strings.Index(g.buf, toolFenceOpen); i >= 0 {
			g.send(g.buf[:i])
			g.buf = g.buf[i+len(toolFenceOpen):]
			g.inFence = true
			continue
		}
		keep := partialPrefixLen(g.buf, toolFenceOpen)
		g.send(g.buf[:len(g.buf)-keep])
		g.buf = g.buf[len(g.buf)-keep:]
		return
	}
}

// partialPrefixLen 返回 s 的末尾有多少字节是 marker 的前缀（不含完整匹配）。
//
// marker 全是 ASCII，所以匹配到的后缀必然也全是 ASCII，切点不会落在多字节
// 字符中间 —— UTF-8 的续字节 >=0x80，永远不等于 marker 里的任何字节。
func partialPrefixLen(s, marker string) int {
	max := len(marker) - 1
	if len(s) < max {
		max = len(s)
	}
	for k := max; k > 0; k-- {
		if strings.HasPrefix(marker, s[len(s)-k:]) {
			return k
		}
	}
	return 0
}

// 放宽：围栏内不强求前后换行——模型有时吐 ```tool_call\n{...}\n```、有时
// ```tool_call {...}``` 甚至一行内。老正则死磕 \n…\n，格式一变就漏解析，漏了就把
// 整个 ```tool_call``` 块当正文发给客户端 = 用户看到的「已读乱回」。这里只认围栏、
// 内容交给 JSON 解析兜底（非围栏内容里不会有 ```，非贪婪停在第一个闭合围栏）。
var toolCallRe = regexp.MustCompile("(?s)```tool_call(.*?)```")

// parseToolCalls extracts ```tool_call``` blocks. Returns clean text + tool_calls.
func parseToolCalls(text string) (string, []ToolCall) {
	var toolCalls []ToolCall
	for _, match := range toolCallRe.FindAllStringSubmatch(text, -1) {
		if len(match) < 2 {
			continue
		}
		var data struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(match[1])), &data); err != nil {
			continue
		}
		argsJSON, _ := json.Marshal(data.Arguments)
		toolCalls = append(toolCalls, ToolCall{
			ID:   "call_" + randHex(8),
			Type: "function",
			Function: ToolCallFunction{
				Name:      data.Name,
				Arguments: string(argsJSON),
			},
		})
	}
	clean := toolCallRe.ReplaceAllString(text, "")
	return strings.TrimSpace(clean), toolCalls
}

// parseToolChoice 解析 OpenAI 的 tool_choice。
// 返回 (mode, forcedName)：mode ∈ {"auto","none","required"}；
// forcedName 非空表示客户端点名了某个函数。
func parseToolChoice(tc interface{}) (string, string) {
	switch v := tc.(type) {
	case string:
		switch v {
		case "none", "required", "auto":
			return v, ""
		}
	case map[string]interface{}:
		// {"type":"function","function":{"name":"..."}}
		if f, ok := v["function"].(map[string]interface{}); ok {
			if n := getStr(f, "name"); n != "" {
				return "required", n
			}
		}
	}
	return "auto", ""
}

// PromptTooLongError 表示 prompt 超过了单次请求能塞的上限。
//
// 为什么是报错而不是我们自己截：上游超限时**从尾部静默截断**且不报错，而最新
// 消息拼在末尾，所以被吃掉的正好是用户刚问的那句 —— 模型只看到前面的系统前言，
// 回一句通用开场白，既不答题也不调工具。
//
// 也不自己丢历史：那仍然是静默丢数据，只是换了个地方丢。客户端以为整段都发出去
// 了，模型却忘了东西，答案微妙地错而没人知道。报 context_length_exceeded 是
// OpenAI 兼容客户端认得的信号，agentic 客户端收到会自己压缩上下文再试 ——
// 它比我们盲丢最旧的几段聪明得多。
//
// **为什么按字节而不是按 token 判**：静态 IP 上把中英文对齐到同一字节数实测，
// 两者的墙落在完全相同的位置 —— 约 129,950 字节各 3/3 通过、135,990 字节各 1/3、
// 141,920 字节各 1/3；而同一批请求的 tiktoken 计数差了 1.9 倍（英文 24,273 对
// 中文 46,591）。按 token 设阈值的话，同一个数字对英文太松、对中文卡在真实容量的
// 三分之一左右。
//
// 顺带排除了"墙在传输层"：prompt 进 f.req 要先 JSON 再 urlencode，中文每个字节
// 变成 %XX（3 倍膨胀）、英文基本原样，两者的线上体积差近 3 倍却撞同一堵墙，
// 所以计的是 prompt 内容的字节数，不是请求体大小。
//
// 真要撑住长上下文得走另一条路：把内容转成文件附件。但那需要登录态（匿名能上传、
// 对话里引用会被服务端回 1100 拒绝），所以现在只能报错。
type PromptTooLongError struct {
	Bytes, Budget int
	HasCookie     bool // 有 cookie 却还超，说明附件那条路也没救回来
}

func (e *PromptTooLongError) Error() string {
	base := fmt.Sprintf(
		"prompt is %d bytes, over the %d-byte per-request limit of the Gemini web "+
			"protocol (the limit is on UTF-8 bytes, not tokens). The upstream silently "+
			"truncates from the end, which would drop your latest message and produce an "+
			"unrelated answer, so this request is rejected instead.",
		e.Bytes, e.Budget)
	if !e.HasCookie {
		// 没 cookie 时这不是死路：导一个进来就能走附件，长度限制基本就没了。
		// 不说这句的话用户只会以为"这项目撑不住长上下文"。
		return base + " Add a Google account cookie in the admin panel (Cookie pool) — " +
			"with one configured, oversized conversations are uploaded as a text " +
			"attachment instead and this limit largely goes away."
	}
	return base + " Shorten the conversation, the system prompt, or the tool definitions."
}
