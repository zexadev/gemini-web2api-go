package app

import (
	"encoding/json"
	"testing"
)

// 图片信息块，结构取自 hNvQHb 历史抓包（令牌、链接截短）。
// 图片信息块，结构取自 2026-11 实测的生成响应（[3] 生成参数截短、令牌截短）。
const testImageBlock = `[[null,null,null,[null,1,"watermarked_img_1.jpg","https://lh3.googleusercontent.com/gg-dl/AAA",null,"$TOKEN",null,null,null,[1786696709,587113269],null,"image/jpeg",null,null,null,[1376,768,832695]]],["http://googleusercontent.com/image_generation_content/0_480"],null,[20,null,"imagen_default.complaintflow"],null,null,null,null,"im_4c4a32ef65b677b1",null,null,[true]]`

func testFrame(t *testing.T, payload string) string {
	t.Helper()
	b, err := json.Marshal([]interface{}{[]interface{}{"wrb.fr", nil, payload}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFindImageGenBlocks(t *testing.T) {
	// 同一张图在两帧里重发，取后一帧；另有一张不同的图。
	early := `[null,[[` + testImageBlock + `]]]`
	other := `[[null,null,null,[null,1,"b.png","u",null,"$T2"]],["http://googleusercontent.com/image_generation_content/0_481"],null,[1]]`
	late := `[null,[[` + testImageBlock + `,` + other + `]]]`
	raw := ")]}'\n" + testFrame(t, early) + "\n" + testFrame(t, late)
	got := findImageGenBlocks(raw)
	if len(got) != 2 {
		t.Fatalf("got %d blocks, want 2", len(got))
	}
	if imageGenBlockKey(got[0]) != imageGenPlaceholderPrefix+"0_480" || imageGenBlockKey(got[1]) != imageGenPlaceholderPrefix+"0_481" {
		t.Fatalf("wrong order: %v / %v", imageGenBlockKey(got[0]), imageGenBlockKey(got[1]))
	}
	// 没有令牌的不算信息块。
	noTok := testFrame(t, `[[null,["http://googleusercontent.com/image_generation_content/0_1"],null,[1]]]`)
	if n := len(findImageGenBlocks(noTok)); n != 0 {
		t.Fatalf("block without token should be ignored, got %d", n)
	}
}

func TestBuildUpscaleParams(t *testing.T) {
	var block []interface{}
	if err := json.Unmarshal([]byte(testImageBlock), &block); err != nil {
		t.Fatal(err)
	}
	got, err := buildUpscaleParams(block, "r_1", "rc_2", "c_3")
	if err != nil {
		t.Fatal(err)
	}
	// 形状照浏览器抓包：令牌放在 [0][0][3][5]、占位链后面跟 0、前 9 项其余原样（含 im_ 编号）。
	want := `[[[null,null,null,[null,null,null,null,null,"$TOKEN"]],["http://googleusercontent.com/image_generation_content/0_480",0],null,` +
		`[20,null,"imagen_default.complaintflow"],null,null,null,null,"im_4c4a32ef65b677b1"],` +
		`["r_1","rc_2","c_3"],1,0,1]`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestUpscaleDownloadURL(t *testing.T) {
	cases := map[string]string{
		"https://lh3.googleusercontent.com/gg-dl/AHiVA1lk":    "https://lh3.googleusercontent.com/gg-dl/AHiVA1lk=d-I?alr=yes",
		"https://lh3.googleusercontent.com/gg-dl/AHiVA1lk=s0": "https://lh3.googleusercontent.com/gg-dl/AHiVA1lk=d-I?alr=yes",
	}
	for in, want := range cases {
		if got := upscaleDownloadURL(in); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}
