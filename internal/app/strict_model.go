package app

import (
	"fmt"
	"regexp"
	"strings"
)

// strict_model（#41）：额度耗尽时上游会把请求**静默降级**成更弱的模型，却照回 200，
// 客户端拿到的是一个"成功"的降级回答。对人来说只是答得差一点，对 agent 是灾难 ——
// 它会据此宣称任务完成（issue #41 原话：「产生严重幻觉并破坏任务」）。
//
// 这个文件只做一件事：把「请求的模型」和「服务端自报的模型」比一下，对不上就报出来。
// 开关与 HTTP 响应在 server.go，判定逻辑保持纯函数以便单测。
//
// 判据来自响应帧 [42] 的显示名（extractUpstreamModel）。**不能拿响应体里的 model 字段
// 判断**：那是我们回给客户端的、填的是请求值（server.go 的 writeJSON），也就是说
// 响应体本身在撒谎，真相只在服务端自报的那个名字里。

// strictModelExpect 是判据表：模型 id → 「真的用上时，服务端在响应帧 [42] 里自报的
// 显示名中必定包含的片段」。判定前两边都会过 normalizeModelName。
//
// 为什么单独一张表、不塞进 Models：Models 是协议层事实（hex / mode / tool 槽位），
// 这张表只服务 strict_model 一个开关；Google 改名时改这里就够了，不必动模型定义。
//
// 几个刻意的选择：
//   - **留空 = 不参与判定**。生图/生视频/画布这类请求服务端不一定自报显示名，
//     猜错会让用户开始无视这个开关 —— 比漏判更糟。
//   - 思考档与普通档**共用片段**：实测开思考时服务端回报 "4 Argon High"，
//     与普通档同一个 hex，显示名里不带 thinking。
//   - 3.7 / 3.8 **互为容错**：Google 把 3.7 原地升级成 3.8（同 hex、显示名变了），
//     两个都列上，改名期间不会误报。
var strictModelExpect = map[string][]string{
	"gemini-4-argon":          {"4 argon"},
	"gemini-4-argon-thinking": {"4 argon"},

	"gemini-3.8-flash":          {"3.8 flash"},
	"gemini-3.7-flash":          {"3.8 flash", "3.7 flash"},
	"gemini-3.8-flash-thinking": {"3.8 flash"},
	"gemini-3.7-flash-thinking": {"3.8 flash", "3.7 flash"},

	"gemini-3.6-flash":          {"3.6 flash"},
	"gemini-3.6-flash-thinking": {"3.6 flash"},

	"gemini-3.1-pro":          {"3.1 pro"},
	"gemini-3.1-pro-thinking": {"3.1 pro"},

	"gemini-3.5-flash-lite":          {"3.5 flash lite"},
	"gemini-3.5-flash-lite-thinking": {"3.5 flash lite"},
}

// strictModelNormalizeRe 把非字母/数字/点的字符（连字符、空格、全角符号…）折叠掉，
// 于是 "3.5 Flash-Lite" 与 "3.5 flash lite"、"4 Argon High" 与 "4 argon" 都能对上。
var strictModelNormalizeRe = regexp.MustCompile(`[^a-z0-9.]+`)

// normalizeModelName 把模型名折叠成可比形式："3.5 Flash-Lite" → "3.5 flash lite"。
func normalizeModelName(s string) string {
	return strings.TrimSpace(strictModelNormalizeRe.ReplaceAllString(strings.ToLower(s), " "))
}

// ModelDowngraded 判断这次请求有没有被静默降级。
//
// 返回的第二个值是判据（期望出现的显示名片段），让调用方能说清"期望什么"。
// upstream 为空（服务端没自报、或抽取正则没命中）时**不判定** —— "看不见"和
// "没降级"是两回事，把前者当成后者会把这道防线变成摆设。
func ModelDowngraded(model, upstream string) (bool, string) {
	up := strings.TrimSpace(upstream)
	if up == "" {
		return false, ""
	}
	want := strictModelExpect[model]
	if len(want) == 0 {
		return false, ""
	}
	hay := normalizeModelName(up)
	for _, name := range want {
		if strings.Contains(hay, normalizeModelName(name)) {
			return false, ""
		}
	}
	return true, want[0]
}

// StrictModelError 是"上游静默降级"这件事本身，做成可识别的错误类型。
//
// 为什么需要类型：底层读取（readBody）在**帧里第一次看到模型名**时就能判定，
// 此时必须立刻中断本次请求 —— 不能把它当成"上游故障"去走重试，否则会把同样的
// 降级再等一遍（实测每次要多等 45-55 秒）。
type StrictModelError struct {
	Requested string // 客户端要的模型
	Actual    string // 上游自报实际用的
	Expected  string // 判据（期望出现的显示名片段）
}

func (e *StrictModelError) Error() string {
	return fmt.Sprintf(
		"strict_model：上游把 %s 静默降级成了 %s（期望 %s）。通常是该账号额度已耗尽，"+
			"换一个还有额度的账号，或关掉 strict_model。",
		e.Requested, e.Actual, e.Expected)
}

// strictModelAbort 在读到上游自报的模型名时做**提前**判定。
// 返回非 nil 表示要立刻中断本次请求：实测把"发现降级"从整段生成完（45-55 秒）
// 提前到首帧到达（3-4 秒），顺带省下那段白生成的用量。
func strictModelAbort(model, upstream string) error {
	if !rtCfg().StrictModel {
		return nil
	}
	bad, want := ModelDowngraded(model, upstream)
	if !bad {
		return nil
	}
	return &StrictModelError{Requested: model, Actual: upstream, Expected: want}
}

// strictModelHook 给底层读取用的钩子；开关关闭时返回 nil（零开销，逐行扫描里
// 连一次正则都不会跑）。
func strictModelHook(model string) func(string) error {
	if !rtCfg().StrictModel {
		return nil
	}
	return func(upstream string) error { return strictModelAbort(model, upstream) }
}

// strictModelViolation 在响应读完之后再兜一次底（空串 = 放行）。
// 提前判定没命中时（模型名出现在很晚的帧里 / 被行边界切开）由它负责。
func strictModelViolation(model string, res *StreamResult) string {
	if res == nil {
		return ""
	}
	if err := strictModelAbort(model, res.UpstreamModel); err != nil {
		return err.Error()
	}
	return ""
}
