package app

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// MCP 的媒体工具：生图 / 音乐 / 视频，底层跟 /v1/chat/completions 里的 gemini-image /
// gemini-music、/v1/videos 是同一条链。
//
// 产物用 MCP 原生的 image / audio 内容类型返回（base64 放在 data 字段，带 mimeType），
// 不塞进 text：支持的客户端把它当附件渲染，不会把几十万字的 base64 当文本喂进上下文。
// 不做「短链」那种不带鉴权的公开 URL —— 那要额外的存储、过期、对外地址配置，
// 而原生内容类型已经解决了「文本里塞 base64 卡死客户端」这个问题。

func generateImageToolDef() map[string]interface{} {
	return map[string]interface{}{
		"name": "generate_image",
		"description": "Generate an image with Google Gemini (Nano Banana). Optionally pass " +
			"reference images to edit or restyle. The image is attached to the result; " +
			"don't repeat or re-describe it.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"prompt": map[string]interface{}{
					"type":        "string",
					"description": "What to draw, or how to change the reference images.",
				},
				"image_urls": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "Optional reference images: data: URLs or http(s) links.",
				},
			},
			"required": []interface{}{"prompt"},
		},
	}
}

func generateMusicToolDef() map[string]interface{} {
	return map[string]interface{}{
		"name": "generate_music",
		"description": "Generate a ~30 second music clip with Google Gemini (Lyria). " +
			"The audio is attached to the result.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"prompt": map[string]interface{}{
					"type":        "string",
					"description": "Style, mood, instruments, tempo, and so on.",
				},
			},
			"required": []interface{}{"prompt"},
		},
	}
}

// mcpGenerateMedia 用媒体模型生成一次，返回 MCP 工具结果。
func mcpGenerateMedia(model, prompt string, refs []string, key *APIKey) (map[string]interface{}, error) {
	_, mc, err := resolveModelForKey(model, key)
	if err != nil {
		return nil, err
	}
	var images []pendingUpload
	for i, src := range refs {
		img, err := materializeImage(src, "", i+1)
		if err != nil {
			return nil, err
		}
		images = append(images, img)
	}
	_, _, res, err := callGemini(prompt, prompt, mc, nil, images, nil, nil)
	if err != nil {
		return nil, err
	}
	var content []interface{}
	for _, a := range res.Artifacts {
		content = append(content, mcpMediaContent(a))
	}
	if len(content) == 0 {
		return nil, fmt.Errorf("no media in the response")
	}
	// 给模型一句话交代：产物已经在结果里了，免得它在正文里再复述 / 再贴一遍。
	note := fmt.Sprintf("Generated %d file(s); they are attached to this result.", len(content))
	return map[string]interface{}{"content": append([]interface{}{
		map[string]interface{}{"type": "text", "text": note}}, content...)}, nil
}

// mcpMediaContent 按 mime 选 MCP 内容类型：image/* → image，audio/* → audio，
// 其余（如 video/mp4）没有对应类型，用 embedded resource 的 blob 带上。
// mime 来自下载时上游的 Content-Type，不一定跟工具对得上（音乐不保证是 audio/*），
// 所以按 mime 选，而不是按工具。
func mcpMediaContent(a MediaArtifact) map[string]interface{} {
	data := base64.StdEncoding.EncodeToString(a.Data)
	switch {
	case strings.HasPrefix(a.Mime, "image/"):
		return map[string]interface{}{"type": "image", "data": data, "mimeType": a.Mime}
	case strings.HasPrefix(a.Mime, "audio/"):
		return map[string]interface{}{"type": "audio", "data": data, "mimeType": a.Mime}
	}
	return map[string]interface{}{"type": "resource", "resource": map[string]interface{}{
		"uri": "gemini://artifact/" + randHex(8), "mimeType": a.Mime, "blob": data}}
}

// mcpStrings 取 JSON 数组参数里的字符串项，空串跳过。
func mcpStrings(v interface{}) []string {
	arr, _ := v.([]interface{})
	var out []string
	for _, it := range arr {
		if s, ok := it.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

func createVideoTaskToolDef() map[string]interface{} {
	return map[string]interface{}{
		"name": "create_video_task",
		"description": "Start generating a video with Google Gemini (Veo). Takes one to several " +
			"minutes; returns a task id to poll with get_video_status. Needs a paid account.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"prompt": map[string]interface{}{
					"type":        "string",
					"description": "What happens in the video.",
				},
				"size": map[string]interface{}{
					"type":        "string",
					"enum":        []interface{}{"1280x720", "720x1280"},
					"description": "1280x720 (16:9, default) or 720x1280 (9:16).",
				},
				"input_reference": map[string]interface{}{
					"type":        "string",
					"description": "Optional start frame: a data: URL or http(s) link to an image.",
				},
			},
			"required": []interface{}{"prompt"},
		},
	}
}

func getVideoStatusToolDef() map[string]interface{} {
	return map[string]interface{}{
		"name": "get_video_status",
		"description": "Check a video task from create_video_task. When completed, set " +
			"include_video to get the MP4 attached (several MB).",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]interface{}{
					"type":        "string",
					"description": "The task id returned by create_video_task.",
				},
				"include_video": map[string]interface{}{
					"type":        "boolean",
					"description": "Attach the MP4 when the task is completed.",
				},
			},
			"required": []interface{}{"id"},
		},
	}
}

// mcpCreateVideo 跟 POST /v1/videos 同一套校验和任务：尺寸、首帧、全池额度用完时直接报错。
func mcpCreateVideo(args map[string]interface{}, key *APIKey) (map[string]interface{}, error) {
	prompt := strings.TrimSpace(getStr(args, "prompt"))
	if prompt == "" {
		return nil, fmt.Errorf("prompt 不能为空")
	}
	if base, ok := Models["gemini-video"]; !ok {
		return nil, fmt.Errorf("video model unavailable")
	} else if err := keyCanUseModel(key, base); err != nil {
		return nil, err
	}
	size, err := videoSize(map[string]any{"size": getStr(args, "size")})
	if err != nil {
		return nil, err
	}
	var images []pendingUpload
	if ref := strings.TrimSpace(getStr(args, "input_reference")); ref != "" {
		img, err := materializeImage(ref, "", 1)
		if err != nil {
			return nil, fmt.Errorf("input_reference: %w", err)
		}
		images = append(images, img)
	}
	if exhausted, reset := videoPoolExhausted(); exhausted {
		return nil, fmt.Errorf("%s", videoPoolExhaustedError(reset))
	}
	return mcpVideoJobResult(startVideoJob("gemini-video", prompt, size, images, key), false), nil
}

// mcpVideoJobResult 把任务状态写成文本 JSON（跟 GET /v1/videos/{id} 同形状）；
// 完成且 withVideo 时附上 MP4（embedded resource）。
func mcpVideoJobResult(j *videoJob, withVideo bool) map[string]interface{} {
	videoJobsMu.Lock()
	state, _ := json.Marshal(j)
	status, mp4, mime := j.Status, j.mp4, j.mime
	videoJobsMu.Unlock()
	content := []interface{}{map[string]interface{}{"type": "text", "text": string(state)}}
	if withVideo && status == "completed" && len(mp4) > 0 {
		content = append(content, mcpMediaContent(MediaArtifact{Mime: mime, Data: mp4}))
	}
	return map[string]interface{}{"content": content}
}
