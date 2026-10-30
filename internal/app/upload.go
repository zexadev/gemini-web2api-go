package app

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	fhttp "github.com/bogdanfinn/fhttp"
)

// 两步 resumable，对齐浏览器抓包。也存在单次 multipart 打 content-push.googleapis.com
// 那条路（同样能传成功），选两步只为跟浏览器一致。
const uploadHost = "https://push.clients6.google.com/upload/"

// uploadBytes 上传字节，返回引用路径（形如 /contrib_service/ttl_1d/…）。上传不带 mime。
//
// 匿名也能传成功，但传上去的文件在对话里引用会被回 1100，所以调用方要自己确保有 cookie。
// proxyURL 必须跟正式请求同一出口，否则在 Google 眼里是两个会话共用文件。
func uploadBytes(cookie string, authuser int, proxyURL string, data []byte, filename string) (string, error) {
	pushID, pctx, err := getUploadTokens(cookie, authuser, proxyURL)
	if err != nil {
		return "", fmt.Errorf("取上传页面参数失败: %w", err)
	}
	if pushID == "" {
		return "", fmt.Errorf("页面里没有 push_id，无法上传")
	}

	base := map[string]string{
		"Origin":         "https://gemini.google.com",
		"Referer":        "https://gemini.google.com/",
		"X-Tenant-Id":    "bard-storage",
		"Push-ID":        pushID,
		"Accept":         "*/*",
		"Sec-Fetch-Site": "same-site",
		"Sec-Fetch-Mode": "cors",
		"Sec-Fetch-Dest": "empty",
	}
	if pctx != "" {
		base["X-Client-Pctx"] = pctx
	}
	if cookie != "" {
		base["Cookie"] = cookie
	}
	with := func(extra map[string]string) map[string]string {
		h := make(map[string]string, len(base)+len(extra))
		for k, v := range base {
			h[k] = v
		}
		for k, v := range extra {
			h[k] = v
		}
		return h
	}

	// start 拿一次性上传 URL。body 是纯文本 "File name: xxx"——尽管 Content-Type
	// 写的是 urlencoded，别按它去猜 body 形态。
	startHeaders := with(map[string]string{
		"Content-Type":                        "application/x-www-form-urlencoded;charset=UTF-8",
		"X-Goog-Upload-Command":               "start",
		"X-Goog-Upload-Protocol":              "resumable",
		"X-Goog-Upload-Header-Content-Length": strconv.Itoa(len(data)),
	})
	status, respHead, body, err := uploadPost(uploadStartURL(authuser), startHeaders,
		[]byte("File name: "+sanitizeUploadName(filename)), proxyURL)
	if err != nil {
		return "", fmt.Errorf("上传 start 失败: %w", err)
	}
	if status != 200 {
		return "", fmt.Errorf("上传 start 返回 HTTP %d: %s", status, truncate(string(body), 160))
	}
	putURL := respHead["x-goog-upload-url"]
	if putURL == "" {
		return "", fmt.Errorf("上传 start 没返回 x-goog-upload-url")
	}

	// 发字节并 finalize
	upHeaders := with(map[string]string{
		"Content-Type":          "application/x-www-form-urlencoded;charset=utf-8",
		"X-Goog-Upload-Command": "upload, finalize",
		"X-Goog-Upload-Offset":  "0",
	})
	status, _, body, err = uploadPost(putURL, upHeaders, data, proxyURL)
	if err != nil {
		return "", fmt.Errorf("上传 finalize 失败: %w", err)
	}
	if status != 200 {
		return "", fmt.Errorf("上传 finalize 返回 HTTP %d: %s", status, truncate(string(body), 160))
	}
	// 响应体就是引用路径。不以 "/" 开头说明拿到的是错误页，填进 payload 服务端
	// 未必报错，但模型看到的附件是空的。
	ref := strings.TrimSpace(string(body))
	if !strings.HasPrefix(ref, "/") {
		return "", fmt.Errorf("上传返回的不是引用路径: %s", truncate(ref, 160))
	}
	return ref, nil
}

// sanitizeUploadName 去掉会破坏请求体的字符——文件名原样进 start 的 body。
func sanitizeUploadName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "upload.bin"
	}
	return strings.NewReplacer("\r", "", "\n", "").Replace(name)
}

// uploadPost 走跟主请求相同的出口发 POST，响应头一起带回（start 要取 x-goog-upload-url）。
func uploadPost(url string, headers map[string]string, body []byte, proxyURL string) (
	int, map[string]string, []byte, error) {
	if proxyURL != "" {
		req, err := http.NewRequest("POST", url, bytes.NewReader(body))
		if err != nil {
			return 0, nil, nil, err
		}
		applyChromeHeaders(req)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := getStdlibClient(proxyURL).Do(req)
		if err != nil {
			return 0, nil, nil, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		h := map[string]string{}
		for k := range resp.Header {
			h[strings.ToLower(k)] = resp.Header.Get(k)
		}
		return resp.StatusCode, h, b, err
	}
	req, err := fhttp.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := getTLSClient().Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	h := map[string]string{}
	for k := range resp.Header {
		h[strings.ToLower(k)] = resp.Header.Get(k)
	}
	return resp.StatusCode, h, b, err
}

// uploadStartURL 是上传 start 那一步的地址。多号登录时要带 ?authuser=N，文件才记在
// 第 N 个号名下；不带就记在默认号名下，第 N 个号在对话里引用它被回 1100。
// 上传 host 不在 gemini.google.com 上，/u/N 路径用不了；实测 X-Goog-AuthUser 头、
// 改 Referer 都不管用，只认这个 query。finalize 用的是 start 返回的地址，不用再带。
func uploadStartURL(authuser int) string {
	if authuser > 0 {
		return uploadHost + "?authuser=" + strconv.Itoa(authuser)
	}
	return uploadHost
}
