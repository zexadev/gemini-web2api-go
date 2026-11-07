package app

import (
	"encoding/json"
	"strings"
	"testing"
)

const uiSample = `Intro paragraph.

<Sequence>
  <Step subtitle="Tune first" title="Hold the Guitar">
    Sit up straight.
    * keep the thumb behind the neck
  </Step>
  <Step title="Learn Chords">
    Start with **Em** and **Am**.
  </Step>
</Sequence>

<Carousel>
  <Video component_placeholder_id="youtube_video_0" count="3" query="beginner guitar" source="youtube"/>
</Carousel>

<TemplateWidget header_title="Gear" templateName="checklist" component_placeholder_id="im_1">
` + "```json" + `
{"items":[{"id":"1","label":"Tuner","sublines":["clip-on"],"checked":false},{"id":"2","label":"Picks","checked":true}]}
` + "```" + `
</TemplateWidget>

<FollowUp label="Want a 7-day plan?" query="Give me a 7-day plan."/>

<ElicitationsGroup message="Where would you like to start?">
  <Elicitation label="Pick a guitar" query="Help me pick a guitar."/>
  <Elicitation label="Read tabs &amp; charts" query="How do I read tabs?"/>
</ElicitationsGroup>`

const uiWant = `Intro paragraph.

**1. Hold the Guitar** — Tune first

Sit up straight.
* keep the thumb behind the neck

**2. Learn Chords**

Start with **Em** and **Am**.

- [Lesson One](http://www.youtube.com/watch?v=a) — Andy
- [Lesson Two](http://www.youtube.com/watch?v=b)

**Gear**

- [ ] Tuner — clip-on
- [x] Picks

Want a 7-day plan?

Where would you like to start?
- Pick a guitar
- Read tabs & charts`

var uiSampleVideos = [][]uiVideo{{
	{Title: "Lesson One", URL: "http://www.youtube.com/watch?v=a", Channel: "Andy"},
	{Title: "Lesson Two", URL: "http://www.youtube.com/watch?v=b"},
}}

func TestRenderUIMarkup(t *testing.T) {
	if got := renderUIMarkup(uiSample, uiSampleVideos, true); got != uiWant {
		t.Fatalf("渲染结果不对:\n%s", got)
	}
}

// 流式逐字喂进去，每一步的输出都必须是最终输出的前缀（deltaTracker 才能正确 diff），
// 且最后一步等于最终输出。
func TestRenderUIMarkupStreamingPrefix(t *testing.T) {
	final := renderUIMarkup(uiSample, uiSampleVideos, true)
	for i := 1; i <= len(uiSample); i++ {
		got := renderUIMarkup(uiSample[:i], uiSampleVideos, false)
		if !strings.HasPrefix(final, got) {
			t.Fatalf("前 %d 字节的输出不是最终输出的前缀:\n%q", i, got)
		}
		if strings.Contains(got, "<") {
			t.Fatalf("前 %d 字节的输出漏出了标签:\n%q", i, got)
		}
	}
	if got := renderUIMarkup(uiSample, uiSampleVideos, false); got != final {
		t.Fatalf("完整输入的流式输出跟最终输出不一致:\n%q", got)
	}
}

// 代码围栏里的标签、未知标签、不在行首的标签都不动。
func TestRenderUIMarkupLeavesOtherTagsAlone(t *testing.T) {
	in := "Here is JSX:\n```jsx\n<Carousel>\n  <Video src=\"a.mp4\"/>\n</Carousel>\n```\n<MyWidget title=\"x\"/>\ntext <FollowUp label=\"a\"/>"
	if got := renderUIMarkup(in, nil, true); got != in {
		t.Fatalf("不该改动:\n%s", got)
	}
}

// 结构残缺的块（上游中途重启生成会把半截标签和新回答混在一起）原样保留，不吞掉中间的正文。
func TestRenderUIMarkupMalformedKept(t *testing.T) {
	in := "<Carousel>\n  <Video component_placeholder_id=\"youtube_video_0\" query=\"half\nNew answer starts here.\n</Carousel>"
	if got := renderUIMarkup(in, nil, true); got != in {
		t.Fatalf("残缺块应原样保留:\n%s", got)
	}
	unclosed := "a\n<Sequence>\n  <Step title=\"x\">\n    body"
	if got := renderUIMarkup(unclosed, nil, true); got != unclosed {
		t.Fatalf("没闭合的块应原样保留:\n%s", got)
	}
	if got := renderUIMarkup(unclosed, nil, false); got != "a" {
		t.Fatalf("流式下没闭合的块应扣住:\n%q", got)
	}
}

// 视频对不上数据时整块丢掉，不留空行堆。
func TestRenderUIMarkupCarouselWithoutVideos(t *testing.T) {
	in := "before\n\n<Carousel>\n  <Video component_placeholder_id=\"youtube_video_0\"/>\n</Carousel>\n\nafter"
	if got := renderUIMarkup(in, nil, true); got != "before\n\nafter" {
		t.Fatalf("got %q", got)
	}
}

func TestVideosInLine(t *testing.T) {
	video := []interface{}{"Title A", "id", "http://www.youtube.com/watch?v=a", "Chan", nil}
	cand := make([]interface{}, 13)
	cand[0] = "rc_1"
	cand[1] = []interface{}{"text"}
	cand[12] = []interface{}{nil, nil, nil, nil, []interface{}{
		[]interface{}{nil, nil, nil, nil, []interface{}{[]interface{}{video}}},
	}}
	inner := make([]interface{}, 5)
	inner[4] = []interface{}{cand}
	innerJSON, _ := json.Marshal(inner)
	line, _ := json.Marshal([]interface{}{[]interface{}{"wrb.fr", nil, string(innerJSON)}})
	got := videosInLine(string(line))
	if len(got) != 1 || len(got[0]) != 1 || got[0][0] != (uiVideo{"Title A", "http://www.youtube.com/watch?v=a", "Chan"}) {
		t.Fatalf("got %+v", got)
	}
}
