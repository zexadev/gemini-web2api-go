package app

import (
	"encoding/json"
	"html"
	"regexp"
	"strconv"
	"strings"
)

// 网页端的「生成式 UI」组件标记。
//
// 2026-11 起上游在正文里直接写组件标签，网页前端把它们渲染成卡片：
//
//	<Sequence><Step title=… subtitle=…>正文</Step>…</Sequence>          步骤卡片
//	<Carousel><Video component_placeholder_id="youtube_video_N" …/></Carousel>  视频轮播
//	<ElicitationsGroup message=…><Elicitation label=… query=…/>…</ElicitationsGroup>  追问建议
//	<FollowUp label=… query=…/>                                         单条追问
//	<TemplateWidget header_title=… templateName="checklist">```json …```</TemplateWidget>  清单
//
// 匿名和登录态都会出，模型头下标 8 的旗标怎么填都关不掉，只能在这边转成 Markdown，
// 否则客户端看到的是一堆裸标签。视频的标题/链接不在标签里，在同一帧的
// inner[4][0][12][4][N][4][0]（N 对应占位 id 的后缀），闭合标签出现的那一帧里就有。
//
// 只认行首的已知标签、跳过代码围栏里的内容：用户让模型写 HTML/JSX 时别误伤。

// uiVideo 是视频轮播里的一条。
type uiVideo struct {
	Title, URL, Channel string
}

var uiBlockTags = map[string]bool{
	"Sequence": true, "Carousel": true, "ElicitationsGroup": true,
	"FollowUp": true, "TemplateWidget": true,
}

var (
	uiOpenRe  = regexp.MustCompile(`^<([A-Z][A-Za-z]*)(?:[\s/>]|$)`)
	uiAttrRe  = regexp.MustCompile(`([A-Za-z_][\w-]*)="([^"]*)"`)
	uiVideoRe = regexp.MustCompile(`^youtube_video_(\d+)$`)
)

// videosInLine 从一帧里取视频轮播数据，下标对应 youtube_video_N 的 N。
func videosInLine(line string) [][]uiVideo {
	inner := innerOfLine(line)
	cand := jsonIndex(jsonIndex(inner, 4), 0)
	groups, _ := jsonIndex(jsonIndex(cand, 12), 4).([]interface{})
	var out [][]uiVideo
	for _, g := range groups {
		var vs []uiVideo
		items, _ := jsonIndex(jsonIndex(g, 4), 0).([]interface{})
		for _, it := range items {
			title, _ := jsonIndex(it, 0).(string)
			url, _ := jsonIndex(it, 2).(string)
			channel, _ := jsonIndex(it, 3).(string)
			if title != "" && url != "" {
				vs = append(vs, uiVideo{Title: title, URL: url, Channel: channel})
			}
		}
		out = append(out, vs)
	}
	return out
}

// innerOfLine 解出 wrb.fr 帧里的 inner 数组，不是内容帧返回 nil。
func innerOfLine(line string) []interface{} {
	if !strings.Contains(line, `"wrb.fr"`) {
		return nil
	}
	var arr []interface{}
	if json.Unmarshal([]byte(line), &arr) != nil || len(arr) == 0 {
		return nil
	}
	s, _ := jsonIndex(arr[0], 2).(string)
	var inner []interface{}
	if s == "" || json.Unmarshal([]byte(s), &inner) != nil {
		return nil
	}
	return inner
}

func jsonIndex(v interface{}, i int) interface{} {
	a, ok := v.([]interface{})
	if !ok || i < 0 || i >= len(a) {
		return nil
	}
	return a[i]
}

// renderUIMarkup 把正文里的组件标签转成 Markdown。
//
// final=false 用于流式：没闭合的组件、以及末尾一行可能是组件开头的半截，都从那里截断不输出，
// 等后续帧补全再渲染。这样每次的输出都是最终输出的前缀，deltaTracker 的前缀 diff 照常成立。
func renderUIMarkup(text string, videos [][]uiVideo, final bool) string {
	if !strings.Contains(text, "<") {
		return text
	}
	lines := strings.Split(text, "\n")
	var out []string
	inFence := false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			out = append(out, line)
			continue
		}
		if inFence {
			out = append(out, line)
			continue
		}
		last := i == len(lines)-1
		if !final && last && uiMaybeOpening(trimmed) {
			break // 末尾半行，可能是组件开头，等下一帧
		}
		m := uiOpenRe.FindStringSubmatch(trimmed)
		if m == nil || !uiBlockTags[m[1]] {
			out = append(out, line)
			continue
		}
		name := m[1]
		end := -1
		if strings.HasSuffix(trimmed, "/>") {
			end = i
		} else {
			for j := i + 1; j < len(lines); j++ {
				if strings.TrimSpace(lines[j]) == "</"+name+">" {
					end = j
					break
				}
			}
		}
		if end < 0 {
			if final {
				out = append(out, line) // 没闭合就原样留下
				continue
			}
			break
		}
		body := lines[i+1 : max(end, i+1)]
		if !uiBlockWellFormed(name, trimmed, body) {
			// 结构不对（上游中途重启生成时会把半截标签和新回答混在一起）：原样留下，不吞内容
			out = append(out, lines[i:end+1]...)
			i = end
			continue
		}
		rendered := renderUIBlock(name, uiAttrs(trimmed), body, videos)
		if rendered == "" {
			// 整块丢弃时顺带吞掉后面一个空行，免得留下连续空行
			if end+1 < len(lines) && strings.TrimSpace(lines[end+1]) == "" && len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
				end++
			}
		} else {
			out = append(out, rendered)
		}
		i = end
	}
	return strings.Join(out, "\n")
}

// uiMaybeOpening 判断一行（可能是半截）会不会是已知组件的开头。
func uiMaybeOpening(trimmed string) bool {
	if !strings.HasPrefix(trimmed, "<") {
		return false
	}
	name := trimmed[1:]
	if k := strings.IndexAny(name, " \t/>"); k >= 0 {
		return uiBlockTags[name[:k]]
	}
	// "<" "<Seq" 这类名字还没写完的
	for tag := range uiBlockTags {
		if strings.HasPrefix(tag, name) {
			return true
		}
	}
	return false
}

func uiAttrs(tagLine string) map[string]string {
	attrs := map[string]string{}
	for _, m := range uiAttrRe.FindAllStringSubmatch(tagLine, -1) {
		attrs[m[1]] = html.UnescapeString(m[2])
	}
	return attrs
}

// uiBlockWellFormed 校验组件结构：开标签写在一行里；轮播和追问组每行都是完整的子标签；
// 步骤组在 <Step>…</Step> 之外只能是空行。不合格的块不渲染。
func uiBlockWellFormed(name, open string, body []string) bool {
	if !strings.HasSuffix(open, ">") {
		return false
	}
	child := map[string]string{"Carousel": "<Video", "ElicitationsGroup": "<Elicitation"}[name]
	switch name {
	case "Carousel", "ElicitationsGroup":
		for _, l := range body {
			t := strings.TrimSpace(l)
			if t != "" && !(strings.HasPrefix(t, child) && strings.HasSuffix(t, "/>")) {
				return false
			}
		}
	case "Sequence":
		inStep := false
		for _, l := range body {
			t := strings.TrimSpace(l)
			switch {
			case inStep && t == "</Step>":
				inStep = false
			case inStep:
			case strings.HasPrefix(t, "<Step") && strings.HasSuffix(t, "/>"):
			case strings.HasPrefix(t, "<Step") && strings.HasSuffix(t, ">"):
				inStep = true
			case t != "":
				return false
			}
		}
		return !inStep
	}
	return true
}

func renderUIBlock(name string, attrs map[string]string, body []string, videos [][]uiVideo) string {
	switch name {
	case "Sequence":
		return renderUISequence(body)
	case "Carousel":
		return renderUICarousel(body, videos)
	case "ElicitationsGroup":
		var b strings.Builder
		b.WriteString(attrs["message"])
		for _, l := range body {
			a := uiAttrs(l)
			if a["label"] != "" {
				b.WriteString("\n- " + a["label"])
			}
		}
		return strings.TrimSpace(b.String())
	case "FollowUp":
		return attrs["label"]
	case "TemplateWidget":
		return renderUITemplate(attrs, body)
	}
	return ""
}

// renderUISequence 把 <Step> 渲染成带序号的粗体小标题 + 正文。
func renderUISequence(body []string) string {
	var parts []string
	n := 0
	for i := 0; i < len(body); i++ {
		t := strings.TrimSpace(body[i])
		if !strings.HasPrefix(t, "<Step") {
			continue
		}
		a := uiAttrs(t)
		var content []string
		if !strings.HasSuffix(t, "/>") {
			for i++; i < len(body) && strings.TrimSpace(body[i]) != "</Step>"; i++ {
				content = append(content, body[i])
			}
		}
		n++
		head := "**" + strconv.Itoa(n) + ". " + a["title"] + "**"
		if a["subtitle"] != "" {
			head += " — " + a["subtitle"]
		}
		if c := strings.TrimSpace(dedent(content)); c != "" {
			head += "\n\n" + c
		}
		parts = append(parts, head)
	}
	return strings.Join(parts, "\n\n")
}

// renderUICarousel 把视频占位换成链接列表；数据对不上就整块丢掉，不留空标签。
func renderUICarousel(body []string, videos [][]uiVideo) string {
	var items []string
	for _, l := range body {
		m := uiVideoRe.FindStringSubmatch(uiAttrs(l)["component_placeholder_id"])
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		if n >= len(videos) {
			continue
		}
		for _, v := range videos[n] {
			item := "- [" + v.Title + "](" + v.URL + ")"
			if v.Channel != "" {
				item += " — " + v.Channel
			}
			items = append(items, item)
		}
	}
	return strings.Join(items, "\n")
}

// renderUITemplate：清单模板的 JSON 转成任务列表，别的模板保留标题和原内容。
func renderUITemplate(attrs map[string]string, body []string) string {
	head := ""
	if attrs["header_title"] != "" {
		head = "**" + attrs["header_title"] + "**\n\n"
	}
	raw := strings.TrimSpace(dedent(body))
	if attrs["templateName"] == "checklist" {
		js := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(raw, "```json"), "```"), "```")
		var data struct {
			Items []struct {
				Label    string   `json:"label"`
				Sublines []string `json:"sublines"`
				Checked  bool     `json:"checked"`
			} `json:"items"`
		}
		if json.Unmarshal([]byte(js), &data) == nil && len(data.Items) > 0 {
			var b strings.Builder
			for _, it := range data.Items {
				box := "[ ]"
				if it.Checked {
					box = "[x]"
				}
				b.WriteString("- " + box + " " + it.Label)
				if len(it.Sublines) > 0 {
					b.WriteString(" — " + strings.Join(it.Sublines, "; "))
				}
				b.WriteString("\n")
			}
			return head + strings.TrimRight(b.String(), "\n")
		}
	}
	return strings.TrimSpace(head + raw)
}

// dedent 去掉各行共同的前导空白。
func dedent(lines []string) string {
	common := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " \t"))
		if common < 0 || n < common {
			common = n
		}
	}
	if common <= 0 {
		return strings.Join(lines, "\n")
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		if len(l) >= common {
			out[i] = l[common:]
		} else {
			out[i] = strings.TrimLeft(l, " \t")
		}
	}
	return strings.Join(out, "\n")
}

// renderedTextsInLine 是 textsInLine 加组件渲染、去生图占位链。视频数据只在正文里有轮播时才去解析。
func renderedTextsInLine(line string, final bool) []string {
	ts := textsInLine(line)
	var videos [][]uiVideo
	for i, t := range ts {
		if videos == nil && strings.Contains(t, "<Carousel") {
			videos = videosInLine(line)
		}
		ts[i] = stripImagePlaceholders(renderUIMarkup(t, videos, final), final)
	}
	return ts
}

// 生图回复的正文里带一行 http://googleusercontent.com/image_generation_content/0_N，
// 是网页前端放图片的占位，打不开。真图我们另行下载附在末尾，这行对客户端只是噪音。
var imagePlaceholderRe = regexp.MustCompile(regexp.QuoteMeta(imageGenPlaceholderPrefix) + `\d+(?:_\d+)?[ \t]*\n?`)

// stripImagePlaceholders 去掉正文里的生图占位链，位置不限。
// final=false 时末尾若是占位链的半截前缀先扣住，免得流式把半条链吐出去。
func stripImagePlaceholders(text string, final bool) string {
	if !final {
		// 占位链一直延伸到末尾时还可能在长（0_ → 0_480），整段扣住
		if i := strings.LastIndex(text, imageGenPlaceholderPrefix); i >= 0 &&
			strings.Trim(text[i+len(imageGenPlaceholderPrefix):], "0123456789_") == "" {
			text = text[:i]
		}
	}
	if strings.Contains(text, imageGenPlaceholderPrefix) {
		text = imagePlaceholderRe.ReplaceAllString(text, "")
	}
	if !final {
		tail := text[strings.LastIndexAny(text, " \t\n")+1:]
		if tail != "" && strings.HasPrefix(imageGenPlaceholderPrefix, tail) {
			text = text[:len(text)-len(tail)]
		}
	}
	return text
}
