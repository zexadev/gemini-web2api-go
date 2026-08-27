package app

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"

	fhttp "github.com/bogdanfinn/fhttp"
)

// 读图 / 读视频：把客户端传来的图片或视频上传成附件，再在对话里引用。
//
// 只有登录态可用 —— 匿名能把文件传上去，但一引用就被服务端回 1100。
// 附件类型位：1=图片、2=视频、3=文本文件，跟上传和引用共用一套（见 gemini.go 文件元组）。
// 视频实测（抓包）：上传同一条 resumable、payload 文件元组第 2 位填 2、mime video/mp4，
// 模型能读出视频内容（回「这是一个包含图标动画的短片」）。

// 单张图的大小上限。上游没给明确数字，取一个既能覆盖正常截图、又不至于让一次
// 请求拖太久的值。超了直接报错而不是硬传 —— 传上去被拒的话错误信息更难懂。
const maxImageBytes = 12 * 1024 * 1024

// 视频体积上限。视频 base64 塞进 JSON 请求体，太大会让一次请求拖很久，取一个
// 既能覆盖常见短片、又不至于把请求挂死的值。超了直接报错。
const maxVideoBytes = 50 * 1024 * 1024

// pendingUpload 是还没上传的附件。真正上传要等挑完账号和出口，
// 所以从 handler 到 streamGenerate 之间先这样带着。
type pendingUpload struct {
	Data []byte
	Name string
	Mime string
	Kind int // 1=图片，2=视频，3=文本/普通文件
}

// collectImages 从 OpenAI 格式的 messages 里把图片抠出来。
//
// 认两种写法：content 数组里的 image_url（OpenAI Chat）和 input_image（Responses）。
// 图片来源支持 data URL 和 http(s) 链接，后者会下载下来再传 —— 直接把链接给上游
// 是不行的，它只认自己存储里的附件。
func collectImages(messages []map[string]interface{}, proxyURL string) ([]pendingUpload, error) {
	var out []pendingUpload
	for _, m := range messages {
		parts, ok := m["content"].([]interface{})
		if !ok {
			continue
		}
		for _, c := range parts {
			cm, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			var src string
			switch getStr(cm, "type") {
			case "image_url":
				if iu, ok := cm["image_url"].(map[string]interface{}); ok {
					src = getStr(iu, "url")
				} else {
					src = getStr(cm, "image_url")
				}
			case "input_image":
				src = firstNonEmpty(getStr(cm, "image_url"), getStr(cm, "url"), getStr(cm, "data"))
			case "video_url":
				if vu, ok := cm["video_url"].(map[string]interface{}); ok {
					src = getStr(vu, "url")
				} else {
					src = getStr(cm, "video_url")
				}
			case "input_video":
				src = firstNonEmpty(getStr(cm, "video_url"), getStr(cm, "url"), getStr(cm, "data"))
			default:
				continue
			}
			if strings.TrimSpace(src) == "" {
				continue
			}
			img, err := materializeImage(src, proxyURL, len(out)+1)
			if err != nil {
				return nil, err
			}
			out = append(out, img)
		}
	}
	return out, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// materializeImage 把一个图片来源变成待上传的字节。
func materializeImage(src, proxyURL string, idx int) (pendingUpload, error) {
	if strings.HasPrefix(src, "data:") {
		return decodeDataURL(src, idx)
	}
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		return fetchImage(src, proxyURL, idx)
	}
	return pendingUpload{}, fmt.Errorf("image %d: unsupported source (want a data: URL or http(s) link)", idx)
}

// decodeDataURL 解析 data:<mime>;base64,<数据>。
func decodeDataURL(src string, idx int) (pendingUpload, error) {
	comma := strings.Index(src, ",")
	if comma < 0 {
		return pendingUpload{}, fmt.Errorf("image %d: malformed data URL", idx)
	}
	meta, payload := src[5:comma], src[comma+1:]
	mime := "image/png"
	if i := strings.Index(meta, ";"); i > 0 {
		mime = meta[:i]
	} else if meta != "" {
		mime = meta
	}
	var data []byte
	var err error
	if strings.Contains(meta, "base64") {
		data, err = base64.StdEncoding.DecodeString(payload)
	} else {
		data = []byte(payload)
	}
	if err != nil {
		return pendingUpload{}, fmt.Errorf("image %d: bad base64: %w", idx, err)
	}
	return newMediaUpload(data, mime, idx)
}

// fetchImage 下载远程图片。走跟正式请求同一个出口：图从别的 IP 拉、对话从这个 IP 发，
// 除了慢一点没别的好处，还多暴露一个出口。
func fetchImage(src, proxyURL string, idx int) (pendingUpload, error) {
	var body []byte
	var ctype string
	if proxyURL != "" {
		req, err := http.NewRequest("GET", src, nil)
		if err != nil {
			return pendingUpload{}, err
		}
		applyChromeHeaders(req)
		resp, err := getStdlibClient(proxyURL).Do(req)
		if err != nil {
			return pendingUpload{}, fmt.Errorf("image %d: fetch failed: %w", idx, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return pendingUpload{}, fmt.Errorf("image %d: fetch returned HTTP %d", idx, resp.StatusCode)
		}
		ctype = resp.Header.Get("Content-Type")
		body, err = io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
		if err != nil {
			return pendingUpload{}, err
		}
	} else {
		req, err := fhttp.NewRequest("GET", src, nil)
		if err != nil {
			return pendingUpload{}, err
		}
		resp, err := getTLSClient().Do(req)
		if err != nil {
			return pendingUpload{}, fmt.Errorf("image %d: fetch failed: %w", idx, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return pendingUpload{}, fmt.Errorf("image %d: fetch returned HTTP %d", idx, resp.StatusCode)
		}
		ctype = resp.Header.Get("Content-Type")
		body, err = io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
		if err != nil {
			return pendingUpload{}, err
		}
	}
	if i := strings.Index(ctype, ";"); i > 0 {
		ctype = ctype[:i]
	}
	if !strings.HasPrefix(ctype, "image/") && !strings.HasPrefix(ctype, "video/") {
		ctype = "image/png"
	}
	return newMediaUpload(body, ctype, idx)
}

// newMediaUpload 按 mime 决定这是图片还是视频：
//   - video/* → 附件类型位 2（跟抓包一致：文件元组 [路径,2,null,"video/mp4"]），上限 maxVideoBytes；
//   - 其余当图片 → 类型位 1，上限 maxImageBytes。
// 类型位 1/2 是服务端认媒体种类的开关，填错模型就按错的类型解析附件。
func newMediaUpload(data []byte, mime string, idx int) (pendingUpload, error) {
	if len(data) == 0 {
		return pendingUpload{}, fmt.Errorf("media %d: empty", idx)
	}
	if strings.HasPrefix(mime, "video/") {
		if len(data) > maxVideoBytes {
			return pendingUpload{}, fmt.Errorf("video %d: %d bytes exceeds the %d-byte limit",
				idx, len(data), maxVideoBytes)
		}
		return pendingUpload{
			Data: data, Mime: mime, Kind: 2,
			Name: fmt.Sprintf("video%d%s", idx, mediaExt(mime)),
		}, nil
	}
	if len(data) > maxImageBytes {
		return pendingUpload{}, fmt.Errorf("image %d: %d bytes exceeds the %d-byte limit",
			idx, len(data), maxImageBytes)
	}
	return pendingUpload{
		Data: data, Mime: mime, Kind: 1,
		Name: fmt.Sprintf("image%d%s", idx, mediaExt(mime)),
	}, nil
}

// mediaExt 按 mime 给个扩展名。文件名会显示给模型看，扩展名对不上容易让它误判。
func mediaExt(mime string) string {
	switch mime {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/heic":
		return ".heic"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	default:
		if strings.HasPrefix(mime, "video/") {
			return ".mp4"
		}
		return ".png"
	}
}
