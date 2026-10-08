package app

import "testing"

// cfb2h 是从上游页面 HTML 里抠出来的，属于外部输入直接进 URL。
// 形状校验挡住的是"页面结构变了、正则抓到别的东西"这种情况——抓错了就该继续用
// 配置里钉的值，而不是拿一个乱七八糟的字符串去拼请求。
func TestBLValueShape(t *testing.T) {
	good := []string{
		"boq_gemini-web-uiserver_20261007.01_p0",       // 2026-10 上游改名后的当前值
		"boq_assistant-bard-web-server_20260805.16_p0", // 改名前的旧值，仍接受
		"boq_assistant-bard-web-server_20260525.09_p0",
		"boq_assistant-bard-web-server_20260802.16_p12",
		"boq_some-future-service_20270101.01_p0", // 服务名再改也接受，只认总体形状
	}
	for _, s := range good {
		if !blValueRe.MatchString(s) {
			t.Errorf("应通过却被拒: %q", s)
		}
	}

	bad := []string{
		"",
		"boq_assistant-bard-web-server", // 缺版本段
		"boq_assistant-bard-web-server_2026080.16_p0",    // 日期 7 位
		"boq_assistant-bard-web-server_20260805.163_p0",  // 小版本 3 位
		"boq_assistant-bard-web-server_20260805.16_p",    // p 后面没数字
		"boq_assistant-bard-web-server_20260805.16_p0&x", // 尾巴带 URL 参数
		"gemini-web-uiserver_20261007.01_p0",             // 缺 boq_ 前缀
		"../../etc/passwd",
	}
	for _, s := range bad {
		if blValueRe.MatchString(s) {
			t.Errorf("应被拒却通过: %q", s)
		}
	}
}

// cfb2hRe 要能从真实页面那种一大坨内联 JSON 里精确取到值。
func TestCfb2hExtract(t *testing.T) {
	page := `...,"cfb2h":"boq_assistant-bard-web-server_20260805.16_p0","fJfDgd":"x",...`
	m := cfb2hRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("没匹配到 cfb2h")
	}
	if want := "boq_assistant-bard-web-server_20260805.16_p0"; m[1] != want {
		t.Errorf("取到 %q，期望 %q", m[1], want)
	}
}
