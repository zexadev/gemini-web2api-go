package app

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
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

var (
	_, cgnatNet, _ = net.ParseCIDR("100.64.0.0/10")
	_, zeroNet, _  = net.ParseCIDR("0.0.0.0/8")

	directImageTransport = &http.Transport{
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			dialer := &net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}
			if ip := net.ParseIP(host); ip != nil {
				if isBlockedIP(ip) {
					return nil, fmt.Errorf("dial target IP %s is blocked", ip.String())
				}
				return dialer.DialContext(ctx, network, addr)
			}
			ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
			if err != nil {
				return nil, fmt.Errorf("lookup ip %q: %w", host, err)
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("no IP found for %q", host)
			}
			for _, ip := range ips {
				if isBlockedIP(ip) {
					return nil, fmt.Errorf("resolved IP %s for host %q is blocked", ip.String(), host)
				}
			}
			var lastErr error
			for _, ip := range ips {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				target := net.JoinHostPort(ip.String(), port)
				conn, err := dialer.DialContext(ctx, network, target)
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, fmt.Errorf("failed to dial %q: %w", host, lastErr)
		},
	}
)

// isBlockedIP checks if an IP is in a private, loopback, link-local, multicast,
// CGNAT (Tailscale), or unspecified range to guard against SSRF.
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if cgnatNet != nil && (cgnatNet.Contains(ip) || (ip.To4() != nil && cgnatNet.Contains(ip.To4()))) {
		return true
	}
	if zeroNet != nil && (zeroNet.Contains(ip) || (ip.To4() != nil && zeroNet.Contains(ip.To4()))) {
		return true
	}
	return false
}

// validateExternalURL checks that a URL is a valid http/https URL and does not target
// localhost, loopback, private networks, link-local, or cloud metadata endpoints (SSRF guard).
// If no proxy is configured, it fails closed if net.LookupIP fails.
func validateExternalURL(rawURL string, proxyURL ...string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported scheme %q (only http and https allowed)", u.Scheme)
	}
	hostname := u.Hostname()
	if hostname == "" {
		return fmt.Errorf("missing host in URL")
	}
	lowerHost := strings.TrimSuffix(strings.ToLower(hostname), ".")
	if lowerHost == "localhost" || strings.HasSuffix(lowerHost, ".localhost") ||
		lowerHost == "169.254.169.254" || lowerHost == "metadata.google.internal" ||
		lowerHost == "metadata" {
		return fmt.Errorf("access to local/metadata host %q is forbidden", hostname)
	}
	if ip := net.ParseIP(hostname); ip != nil {
		if isBlockedIP(ip) {
			return fmt.Errorf("access to blocked IP %s is forbidden", ip.String())
		}
	} else {
		hasProxy := len(proxyURL) > 0 && strings.TrimSpace(proxyURL[0]) != ""
		ips, err := net.LookupIP(hostname)
		if err != nil {
			if !hasProxy {
				return fmt.Errorf("failed to resolve host %q: %w", hostname, err)
			}
		} else {
			if len(ips) == 0 && !hasProxy {
				return fmt.Errorf("no IP addresses found for host %q", hostname)
			}
			for _, ip := range ips {
				if isBlockedIP(ip) {
					return fmt.Errorf("host %q resolves to blocked IP %s", hostname, ip.String())
				}
			}
		}
	}
	return nil
}

// fetchImage 下载远程图片。走跟正式请求同一个出口：图从别的 IP 拉、对话从这个 IP 发，
// 除了慢一点没别的好处，还多暴露一个出口。
func fetchImage(src, proxyURL string, idx int) (pendingUpload, error) {
	if err := validateExternalURL(src, proxyURL); err != nil {
		return pendingUpload{}, fmt.Errorf("image %d: %w", idx, err)
	}

	timeoutSec := 30
	if rtCfg().RequestTimeout > 0 {
		timeoutSec = rtCfg().RequestTimeout
	}
	clientTimeout := time.Duration(timeoutSec) * time.Second

	checkRedirect := func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("stopped after 5 redirects")
		}
		if err := validateExternalURL(req.URL.String(), proxyURL); err != nil {
			return fmt.Errorf("redirect target blocked: %w", err)
		}
		return nil
	}

	var client *http.Client
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return pendingUpload{}, fmt.Errorf("image %d: invalid proxy URL: %w", idx, err)
		}
		tr := &http.Transport{
			Proxy:                 http.ProxyURL(u),
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
		client = &http.Client{
			Timeout:       clientTimeout,
			Transport:     tr,
			CheckRedirect: checkRedirect,
		}
	} else {
		client = &http.Client{
			Timeout:       clientTimeout,
			Transport:     directImageTransport,
			CheckRedirect: checkRedirect,
		}
	}

	req, err := http.NewRequest("GET", src, nil)
	if err != nil {
		return pendingUpload{}, err
	}
	applyChromeHeaders(req)
	resp, err := client.Do(req)
	if err != nil {
		return pendingUpload{}, fmt.Errorf("image %d: fetch failed: %w", idx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return pendingUpload{}, fmt.Errorf("image %d: fetch returned HTTP %d", idx, resp.StatusCode)
	}
	ctype := resp.Header.Get("Content-Type")
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return pendingUpload{}, err
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
