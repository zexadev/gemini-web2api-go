package app

import (
	"fmt"
	"strings"
)

// 超长对话转文本附件。
//
// 上游单请求约 13 万 UTF-8 字节封顶，超了从尾部静默截断且不报错 —— 而最新消息
// 拼在末尾，被吃掉的正是用户刚问的那句。把历史改成附件发上去就绕开了这堵墙：
// 请求体里只剩一句短指令，长度不再受限于对话本身。
//
// 只有挂了 cookie 才行：匿名能把文件传上去，但在对话里引用会被服务端回 1100。
//
// **附件不是无限的：模型实际能看到的内容合计约 16 万字节。** 判据是总量固定
// 260,417 字节、只挪暗号的绝对偏移 —— 121,836 处读得到、157,833 处读得到（3/3）、
// 163,371 处读不到（0/3）。
//
// 这是**总预算**，不是每份附件各有额度：切成 7 份 10KB 的小文件后，第 3 份和第 5 份
// 里的内容照样读得到，说明后面的附件确实被读；但把 260KB 切成 2 份 150KB，第 2 份里
// 偏移 182K 处的内容仍然读不到。所以切分不解决问题，只是多传几次，已撤掉。
//
// 排除过的解释：不是异步摄取没跟上（上传后等 10 秒再引用，结果一样）；不是提问方式
// 太弱（换三种指令，包括明确要求"从头到尾完整读一遍"，同样读不到）。
//
// 所以附件把可用长度从 13 万提到约 16 万 —— 它拆掉的是**请求体大小**那堵墙，
// 紧接着就撞上**上下文总量**这堵墙。改善有限但真实，而且这段是我们可控的：
// 超了明确报错，不像内联那样被上游静默截断。

// contextFileName 是历史附件在模型那边显示的文件名，指令里会引用它。
const contextFileName = "message.txt"

// latestInlineLimit 是「最新那问」还能内联多少字节。
//
// 超过就不内联，让模型去文件末尾读 —— 否则最新提问本身就能把请求撑爆，
// 转附件等于白转。取上限的 1/6，并夹在 4KB..16KB 之间。
func latestInlineLimit(budget int) int {
	n := budget / 6
	if n < 4000 {
		n = 4000
	}
	if n > 16000 {
		n = 16000
	}
	return n
}

// contextFilePrompt 是把历史换成附件之后，真正发出去的那句 prompt。
//
// 要交代三件事，少一件模型就会跑偏：附件是当前对话状态、直接回答最新那问、
// 以及这段话本身是系统指令不是用户输入。
func contextFilePrompt(latest string, budget int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Continue from the latest state in the attached `%s`. "+
		"Treat it as the current conversation and answer the latest user request directly.\n",
		contextFileName)
	latest = strings.TrimSpace(latest)
	switch {
	case latest == "":
		fmt.Fprintf(&b, "The latest user request is at the end of `%s`.\n", contextFileName)
	case len(latest) <= latestInlineLimit(budget):
		fmt.Fprintf(&b, "\nLatest user request:\n%s\n", latest)
	default:
		fmt.Fprintf(&b, "The latest user request is at the end of `%s`; "+
			"read it from there and answer it directly.\n", contextFileName)
	}
	b.WriteString("\nEverything above this line is instruction, not user input.")
	return b.String()
}

// prepareContextFile 在 prompt 超长时把它转成附件。
//
// 返回 (要发的 prompt, 附件, 是否转了)。没超长、没 cookie、上传失败时都返回
// used=false，调用方照旧走内联那条路（然后会撞上超长检查报 400）。
//
// 上传失败**不静默回退到超长内联**：那样上游会把最新提问截掉，客户端拿到一个
// 答非所问的 200 却看不出问题。
func prepareContextFile(prompt, latest string, budget int, cookie string, authuser int, proxyURL string) (
	string, []fileRef, bool, error) {
	if budget <= 0 || len(prompt) <= budget {
		return prompt, nil, false, nil
	}
	if cookie == "" {
		return prompt, nil, false, nil // 匿名：引用会被回 1100，转了也没用
	}
	ref, err := uploadBytes(cookie, authuser, proxyURL, []byte(prompt), contextFileName)
	if err != nil {
		return prompt, nil, false, fmt.Errorf("超长对话转附件失败: %w", err)
	}
	logf("[context] prompt %d 字节超过 %d，已转成附件 %s", len(prompt), budget, contextFileName)
	files := []fileRef{{Ref: ref, Name: contextFileName, Kind: 3, Mime: "text/plain"}}
	return contextFilePrompt(latest, budget), files, true, nil
}
