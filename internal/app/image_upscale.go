package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 生成图放大（#62）。
//
// 网页上点下载并不是直接下生成响应里那张图，而是先发 batchexecute rpc c8o8Fe
// （前端名 DownloadGeneratedImage），服务端回一条新的 lh3 链，再下那条。实测（2026-11-02）：
//
//   - Ultra 号，出图模型 Nano Banana 2.1、信息块 [3][0]=23：下回 upsampled_img_<id>.jpg，
//     1376×768 → 2752×1536（宽高各 2 倍）。
//   - 普通号，Nano Banana 2、[3][0]=20：下回的是原始文件，尺寸不变，但比 =s0 那张
//     （服务端重新压缩过）大 4-6 倍、画质更好。
//
// [3][0] 是图片本身的类型，前端原样回传；改成 23 发过去不会放大。
//
// 请求参数是信息块改造后回传，见 buildUpscaleParams。

// imageGenPlaceholderPrefix 是图片信息块 [1][0] 的占位链前缀，用它认出信息块。
const imageGenPlaceholderPrefix = "http://googleusercontent.com/image_generation_content/"

// findImageGenBlocks 从响应里找出每张生成图的信息块（同一张图在流式里多帧重发，按占位链
// 去重、取最后一次出现的）。顺序按占位链首次出现的顺序。
func findImageGenBlocks(raw string) [][]interface{} {
	var order []string
	byKey := map[string][]interface{}{}
	var walk func(interface{})
	walk = func(o interface{}) {
		arr, ok := o.([]interface{})
		if !ok {
			return
		}
		if key := imageGenBlockKey(arr); key != "" {
			if _, seen := byKey[key]; !seen {
				order = append(order, key)
			}
			byKey[key] = arr
		}
		for _, x := range arr {
			walk(x)
		}
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[[") {
			continue
		}
		var rows []interface{}
		if json.Unmarshal([]byte(line), &rows) != nil {
			continue
		}
		for _, it := range rows {
			row, ok := it.([]interface{})
			if !ok || len(row) < 3 {
				continue
			}
			payload, ok := row[2].(string)
			if !ok {
				continue
			}
			var inner interface{}
			if json.Unmarshal([]byte(payload), &inner) == nil {
				walk(inner)
			}
		}
	}
	out := make([][]interface{}, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	return out
}

// imageGenBlockKey 判断 arr 是不是图片信息块：[1][0] 是占位链、[0][3][5] 是令牌、[3] 是数组。
// 是就返回占位链，否则空串。
func imageGenBlockKey(arr []interface{}) string {
	if len(arr) < 4 {
		return ""
	}
	ph, _ := jsAt(arr[1], 0).(string)
	if !strings.HasPrefix(ph, imageGenPlaceholderPrefix) {
		return ""
	}
	if tok, _ := jsAt(jsAt(arr[0], 3), 5).(string); tok == "" {
		return ""
	}
	if _, ok := arr[3].([]interface{}); !ok {
		return ""
	}
	return ph
}

// jsAt 安全取 JSON 数组下标，越界或不是数组返回 nil。
func jsAt(v interface{}, i int) interface{} {
	if l, ok := v.([]interface{}); ok && i >= 0 && i < len(l) {
		return l[i]
	}
	return nil
}

// buildUpscaleParams 用一张图的信息块拼 c8o8Fe 的参数（JSON 字符串）。
func buildUpscaleParams(block []interface{}, rid, rcid, cid string) (string, error) {
	if imageGenBlockKey(block) == "" {
		return "", fmt.Errorf("不是图片信息块")
	}
	tok := jsAt(jsAt(block[0], 3), 5).(string)
	// 前 9 项原样回传（[3] 是生成参数、[8] 是图片编号 im_…，一轮多张图时靠它区分），
	// [0] 只留令牌、[1] 占位链后补 0。浏览器还会在 [9] 和 id 组末尾带一个前端本地算的
	// 字符串哈希，实测不带结果一样，不发。
	n := len(block)
	if n > 9 {
		n = 9
	}
	img := append([]interface{}{}, block[:n]...)
	img[0] = []interface{}{nil, nil, nil, []interface{}{nil, nil, nil, nil, nil, tok}}
	img[1] = []interface{}{jsAt(block[1], 0), 0}
	ids := []interface{}{rid, rcid, cid}
	// 末尾三项：请求来源、一个布尔、尺寸档（1=推荐 2=最大），照抄浏览器默认的「推荐」。
	b, err := json.Marshal([]interface{}{img, ids, 1, 0, 1})
	return string(b), err
}

// upscaleDownloadURL 把 c8o8Fe 回的 gg-dl 链改成浏览器下载用的形式（链尾 =d-I、query alr=yes）。
func upscaleDownloadURL(u string) string {
	if i := strings.LastIndexByte(u, '/'); i >= 0 {
		if j := strings.IndexByte(u[i+1:], '='); j >= 0 {
			u = u[:i+1+j]
		}
	}
	return u + "=d-I?alr=yes"
}

// downloadUpscaled 下载放大图。带 alr=yes 的链不回 302，而是回 200 text/plain、正文是下一跳
// 地址（浏览器抓包：lh3 → work.fife → lh3，前两跳都是这样），照着正文往下跳直到拿到图。
func downloadUpscaled(u, cookie, proxyURL, defaultMime string, authuser int) (string, []byte, error) {
	for hop := 0; hop < 5; hop++ {
		mime, data, err := downloadBytes(u, cookie, proxyURL, defaultMime, authuser)
		if err != nil {
			return "", nil, err
		}
		next := strings.TrimSpace(string(data))
		if len(data) < 4096 && strings.HasPrefix(next, "https://") && !strings.ContainsAny(next, " \n<") {
			u = next
			continue
		}
		return mime, data, nil
	}
	return "", nil, fmt.Errorf("放大图下载跳转次数过多")
}

// requestUpscale 发一次 c8o8Fe，返回放大图的下载链。
func requestUpscale(params, cid, cookie, sapisid, xsrf, proxyURL string, authuser int) (string, error) {
	freq, _ := json.Marshal([]interface{}{[]interface{}{[]interface{}{"c8o8Fe", params, nil, "generic"}}})
	form := url.Values{}
	form.Set("f.req", string(freq))
	form.Set("at", xsrf)
	sourcePath := "/app/" + strings.TrimPrefix(cid, "c_")
	if authuser > 0 {
		sourcePath = fmt.Sprintf("/u/%d%s", authuser, sourcePath)
	}
	endpoint := fmt.Sprintf(
		"%s/_/BardChatUi/data/batchexecute?rpcids=c8o8Fe&source-path=%s&bl=%s&hl=en&_reqid=%d&rt=c",
		geminiBase(authuser), url.QueryEscape(sourcePath), currentBL(proxyURL), time.Now().UnixNano()%1000000)
	headers := buildGeminiHeaders(cookie, sapisid, "")
	setAuthUserHeaders(headers, authuser)
	delete(headers, "x-goog-ext-525001261-jspb")
	status, _, body, err := uploadPost(endpoint, headers, []byte(form.Encode()), proxyURL)
	if err != nil {
		return "", err
	}
	if status != 200 {
		return "", fmt.Errorf("c8o8Fe HTTP %d: %s", status, truncate(string(body), 160))
	}
	// 回的链两种都见过：浏览器抓包里是 gg-dl/，纯 HTTP 实测是 gg/。
	urls := walkFramesForURLs(string(body), func(s string) bool {
		return strings.Contains(s, "lh3.googleusercontent.com/gg-dl/") ||
			strings.Contains(s, "lh3.googleusercontent.com/gg/")
	})
	if len(urls) == 0 {
		return "", fmt.Errorf("c8o8Fe 响应里没有下载链: %s", truncate(string(body), 160))
	}
	return urls[0], nil
}

// fetchUpscaledImages 对响应里每张生成图请求放大并下载。任何一张失败就整体返回错误，
// 由调用方退回原来的下载方式（不重新生成）。
func fetchUpscaledImages(raw, cid, rid, cookie, sapisid, xsrf, proxyURL, defaultMime string, authuser int) ([]MediaArtifact, error) {
	if cid == "" || rid == "" || xsrf == "" {
		return nil, fmt.Errorf("缺会话 id / 回复 id / XSRF，无法请求放大")
	}
	blocks := findImageGenBlocks(raw)
	_, _, rcid, _ := parseConvIDs(raw)
	if rcid == "" {
		rcid = rcidInTurnRe.FindString(raw)
	}
	if len(blocks) == 0 {
		// 流式响应里没带信息块时，从会话历史里找。
		if body, err := pollHistoryRaw(cid, cookie, sapisid, xsrf, proxyURL, authuser); err == nil {
			if scoped, ok := historyTurn(body, rid); ok {
				body = scoped
			}
			blocks = findImageGenBlocks(body)
			if rcid == "" {
				rcid = rcidInTurnRe.FindString(body)
			}
		}
	}
	if len(blocks) == 0 {
		return nil, fmt.Errorf("响应里没有图片信息块")
	}
	if rcid == "" {
		return nil, fmt.Errorf("响应里没有回复候选 id（rc_）")
	}
	var arts []MediaArtifact
	for _, b := range blocks {
		params, err := buildUpscaleParams(b, rid, rcid, cid)
		if err != nil {
			return nil, err
		}
		dl, err := requestUpscale(params, cid, cookie, sapisid, xsrf, proxyURL, authuser)
		if err != nil {
			return nil, err
		}
		mime, data, err := downloadUpscaled(upscaleDownloadURL(dl), cookie, proxyURL, defaultMime, authuser)
		if err != nil {
			return nil, err
		}
		// downloadBytes 遇到 HTML 错误页会把类型填成默认值，所以按字节头判断。
		if sniff := http.DetectContentType(data); !strings.HasPrefix(sniff, "image/") {
			return nil, fmt.Errorf("放大图下载回来不是图片（%s，%d 字节）", sniff, len(data))
		} else if !strings.HasPrefix(mime, "image/") {
			mime = sniff
		}
		if !artifactSeen(arts, data) {
			arts = append(arts, MediaArtifact{Mime: mime, Data: data})
		}
	}
	return arts, nil
}
