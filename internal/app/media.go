package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

// inner[49] 媒体工具开关：填了服务端就换后端模型生成产物。
const (
	toolImage  = 14 // 生图 → Nano Banana
	toolMusic  = 21 // 音乐 → Lyria（约 30 秒）
	toolCanvas = 2  // 画布 → immersive HTML 文档（内联在响应里，不用另下载）
	toolVideo  = 11 // 视频 → Veo（异步：提交后 MUAZcd 轮询到完成，再 hNvQHb 拿下载链）
)

// extractCanvasDoc 从 canvas（inner[49]=2）响应里抠出生成的 HTML 文档。
//
// 跟图/乐不同：文档不用另外下载，直接内联在帧里——immersive 结构
// （inner[4][0][30]… 那条）里有个形如 "```html\n<!DOCTYPE html>…```" 的字符串。
// 流式时同一份文档在多帧里累积重发，取所有帧里含 DOCTYPE 的**最长**字符串 = 最终完整版。
func extractCanvasDoc(raw string) string {
	best := ""
	var walk func(interface{})
	walk = func(o interface{}) {
		switch v := o.(type) {
		case string:
			if len(v) > len(best) && strings.Contains(v, "DOCTYPE") {
				best = v
			}
		case []interface{}:
			for _, x := range v {
				walk(x)
			}
		case map[string]interface{}:
			for _, x := range v {
				walk(x)
			}
		}
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[[") {
			continue
		}
		var arr []interface{}
		if json.Unmarshal([]byte(line), &arr) != nil {
			continue
		}
		for _, it := range arr {
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
	return best
}

// downloadOPI 是产物下载 URL（contribution 那条）必带的 opi 参数。取自 /app 页面，
// 实测两个独立账号都是这个值，当全局常量用。图片走 lh3 CDN 那条不需要它。
const downloadOPI = "103135050"

// MediaArtifact 是一份生成好的媒体产物的原始字节。
type MediaArtifact struct {
	Mime string
	Data []byte
}

// downloadCookieNames 是产物下载 host 认的 cookie 白名单 —— 浏览器发给它的就这 18 项，
// 全是 .google.com 域作用域的。
//
// 关键：把 gemini/accounts 主机专属的 cookie（__Host-1PLSID / __Host-GAPS / LSID /
// OSID / OTZ / COMPASS / _ga* 等）一起塞过去，下载 host 的鉴权会直接 403、回一个 gzip
// 的空错误页。实测只发这 18 项才 200。这是 media 下载 403 的根因之一，跟 token 新鲜度 /
// TLS 指纹 / opi / 各种 header 都无关。
//
// 另一个根因是跨域重定向：图片链（lh3.googleusercontent.com/gg-dl/…）会 302 到
// work.fife.usercontent.google.com/rd-gg-dl/…，而 http 客户端默认不把 Cookie 头带到
// 新域，于是重定向目标拿不到 cookie 照样 403。靠 mediaGetFollow 每跳重发 cookie 解决。
var downloadCookieNames = map[string]bool{
	"HSID": true, "SSID": true, "APISID": true, "SAPISID": true,
	"__Secure-1PAPISID": true, "__Secure-3PAPISID": true,
	"SID": true, "__Secure-1PSID": true, "__Secure-3PSID": true,
	"GOOGLE_ABUSE_EXEMPTION": true, "NID": true,
	"__Secure-1PSIDTS": true, "__Secure-1PSIDRTS": true,
	"__Secure-3PSIDTS": true, "__Secure-3PSIDRTS": true,
	"SIDCC": true, "__Secure-1PSIDCC": true, "__Secure-3PSIDCC": true,
}

// filterDownloadCookies 只留下载 host 认的那 18 项，别的一律不发。
func filterDownloadCookies(cookie string) string {
	var kept []string
	for _, p := range strings.Split(cookie, ";") {
		p = strings.TrimSpace(p)
		i := strings.IndexByte(p, '=')
		if i <= 0 {
			continue
		}
		if downloadCookieNames[p[:i]] {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "; ")
}

// fetchMediaArtifacts 取回媒体产物字节。cookie / sapisid / xsrf / proxyURL 必须跟生成
// 那次同一套 —— 产物挂在这个会话/这个出口上，换出口/换号都取不到。
//
// 图片和音乐取回路径不同：图片走 lh3 CDN（链在 StreamGenerate 响应里，302 跟随），
// 音乐/视频走 contribution.usercontent.google.com/download（链在 hNvQHb 历史里，单次
// 200）。按 tool 分流。
//
// rid scopes video lookup to this request's turn: in a continued conversation
// (video extension) the history also holds earlier turns' videos, which must not be
// mistaken for this one. Returns the turn's response-candidate id (rcid) as well,
// which is needed to continue the conversation again.
func fetchMediaArtifacts(tool int, raw, cid, rid, cookie, sapisid, xsrf, proxyURL, defaultMime string, authuser int) ([]MediaArtifact, string, error) {
	if tool == toolImage {
		// 放大（#62）失败一律退回原来的下载，不重新生成。
		if rtCfg().ImageUpscale && xsrf != "" {
			arts, err := fetchUpscaledImages(raw, cid, rid, cookie, sapisid, xsrf, proxyURL, defaultMime, authuser)
			if err == nil && len(arts) > 0 {
				return arts, "", nil
			}
			logf("[upscale] 放大失败，改下原图: %v", err)
		}
		arts, err := fetchImageArtifacts(raw, cid, cookie, sapisid, xsrf, proxyURL, defaultMime, authuser)
		return arts, "", err
	}
	// 音乐几乎立刻就绪，视频要生成几十秒到几分钟，所以视频轮询给足预算。
	maxPolls, interval := 6, 2*time.Second
	if tool == toolVideo {
		maxPolls, interval = 45, 8*time.Second // 约 6 分钟
	}
	arts, rcid, err := fetchDownloadArtifacts(cid, rid, cookie, sapisid, xsrf, proxyURL, defaultMime, tool, maxPolls, interval, authuser)
	if err != nil {
		return arts, rcid, err
	}
	// 视频一次请求 hNvQHb 里会挂多份下载链。Measured: one is the bare MP4 and the
	// other is a protobuf envelope (prompt + the same MP4 + a timestamp string) that is
	// *larger*, so "keep the largest" handed clients an unplayable file. Prefer real
	// MP4 bytes; if only envelopes came back, unwrap the MP4 embedded in them.
	if tool == toolVideo && len(arts) > 0 {
		arts = []MediaArtifact{pickVideoArtifact(arts)}
	}
	return arts, rcid, nil
}

// errVideoQuota prefixes the error when an account's daily video limit is used up;
// clients can match it and resubmit (the pool's LRU rotation picks another account).
const errVideoQuota = "video_quota_exhausted"

// errVideoQuotaUsed 是「这个号视频额度用完」的错误。文案以 errVideoQuota 开头给客户端匹配；
// 程序里判断一律 errors.Is，不比文案，改措辞不会让换号悄悄失效。
var errVideoQuotaUsed = errors.New(errVideoQuota + ": this account's video generation limit is used up (resets later); resubmit to use another account")

// videoQuotaError 是带重置时间的 errVideoQuotaUsed（errors.Is 认得它）。ResetAt 取自
// 拒绝那一轮里 Google 自己写的时间戳（unix 秒），那一轮没写时为 0。
type videoQuotaError struct{ ResetAt int64 }

func (e *videoQuotaError) Error() string        { return errVideoQuotaUsed.Error() }
func (e *videoQuotaError) Is(target error) bool { return target == errVideoQuotaUsed }

// videoQuotaReset 取额度错误里的重置时间，取不到返回 0。
func videoQuotaReset(err error) int64 {
	var qe *videoQuotaError
	if errors.As(err, &qe) {
		return qe.ResetAt
	}
	return 0
}

// errVideoRefused prefixes the error when Gemini answers with text instead of
// starting a video (a refusal); resubmitting the same prompt won't help.
const errVideoRefused = "video_refused"

// isMP4 reports whether b starts with an ISO-BMFF box header ("....ftyp").
func isMP4(b []byte) bool {
	return len(b) >= 12 && string(b[4:8]) == "ftyp"
}

// pickVideoArtifact returns the largest artifact that is a bare MP4; failing that, the
// largest MP4 found inside a protobuf envelope; failing that, the largest artifact.
func pickVideoArtifact(arts []MediaArtifact) MediaArtifact {
	var best *MediaArtifact
	for i := range arts {
		a := arts[i]
		if !isMP4(a.Data) {
			if inner := mp4InProtobuf(a.Data, 0); inner != nil {
				a = MediaArtifact{Mime: "video/mp4", Data: inner}
			} else {
				continue
			}
		}
		if best == nil || len(a.Data) > len(best.Data) {
			best = &a
		}
	}
	if best != nil {
		best.Mime = "video/mp4"
		return *best
	}
	largest := arts[0]
	for _, a := range arts[1:] {
		if len(a.Data) > len(largest.Data) {
			largest = a
		}
	}
	return largest
}

// mp4InProtobuf walks protobuf length-delimited fields (recursively) and returns the
// first payload that is an MP4. Returns nil if b doesn't parse as protobuf.
func mp4InProtobuf(b []byte, depth int) []byte {
	if depth > 6 {
		return nil
	}
	varint := func(i int) (uint64, int, bool) {
		var v uint64
		for s := uint(0); i < len(b) && s < 64; s += 7 {
			c := b[i]
			i++
			v |= uint64(c&0x7f) << s
			if c < 0x80 {
				return v, i, true
			}
		}
		return 0, i, false
	}
	for i := 0; i < len(b); {
		key, next, ok := varint(i)
		if !ok {
			return nil
		}
		i = next
		switch key & 7 {
		case 0:
			if _, i, ok = varint(i); !ok {
				return nil
			}
		case 1:
			i += 8
		case 5:
			i += 4
		case 2:
			n, next, ok := varint(i)
			if !ok || next+int(n) > len(b) || int(n) < 0 {
				return nil
			}
			p := b[next : next+int(n)]
			i = next + int(n)
			if isMP4(p) {
				return p
			}
			if len(p) > 1024 {
				if r := mp4InProtobuf(p, depth+1); r != nil {
					return r
				}
			}
		default:
			return nil
		}
	}
	return nil
}

// fetchImageArtifacts 取回生成的图片。链在 StreamGenerate 响应里就有，抠不到再退回轮询
// hNvQHb。下载走 mediaGetFollow（跟随 302 并每跳重发 cookie）。
func fetchImageArtifacts(raw, cid, cookie, sapisid, xsrf, proxyURL, defaultMime string, authuser int) ([]MediaArtifact, error) {
	urls := collectImageURLs(raw)
	if len(urls) == 0 {
		for i := 0; i < 6; i++ {
			if body, err := pollHistoryRaw(cid, cookie, sapisid, xsrf, proxyURL, authuser); err == nil {
				if u := collectImageURLs(body); len(u) > 0 {
					urls = u
					break
				}
			}
			time.Sleep(2 * time.Second)
		}
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("响应里没有图片 CDN 链接")
	}
	var arts []MediaArtifact
	for _, u := range urls {
		mime, data, err := downloadBytes(imageFullResURL(u), cookie, proxyURL, defaultMime, authuser)
		if err != nil {
			return arts, err
		}
		// 同一张图在响应里可能以不同 token 出现（不同分辨率/缩略图），按字节去重，
		// 免得客户端拿到几张一模一样的图。
		if !artifactSeen(arts, data) {
			arts = append(arts, MediaArtifact{Mime: mime, Data: data})
		}
	}
	return arts, nil
}

// imageFullResURL 给图片 CDN 链加尺寸参数取原图。plain gg-dl 默认下来是 ~500px 的缩略图
// （issue #14：控制台里是 1365×768，链尾 =s1024-rj 改 =s2048-rj 更大）。googleusercontent
// 惯例是在链尾加 =sN 指定最大边、=s0 取原始尺寸。这里用 =s0 取全分辨率、且不改格式
// （-rj 会强制 jpeg，我们要保持 PNG）。
//
// 选项在最后一个路径段里、以 = 分隔；gg-dl token 是 base64url 不含 =，所以按最后一个
// 路径段里的 = 切，去掉已有选项再加 =s0。
func imageFullResURL(u string) string {
	i := strings.LastIndexByte(u, '/')
	if i < 0 {
		return u + "=s0"
	}
	seg := u[i+1:]
	if j := strings.IndexByte(seg, '='); j >= 0 {
		u = u[:i+1] + seg[:j]
	}
	return u + "=s0"
}

// fetchDownloadArtifacts 取回音乐/视频：轮询 hNvQHb 等到 response_data 下载链，再下。
// gg-dl（lh3）和 temp_data 那两种链是预览用的，只有 response_data 那条能下到真字节。
func fetchDownloadArtifacts(cid, rid, cookie, sapisid, xsrf, proxyURL, defaultMime string, tool int,
	maxPolls int, interval time.Duration, authuser int) ([]MediaArtifact, string, error) {
	if cid == "" {
		return nil, "", fmt.Errorf("没拿到会话 id，无法定位产物")
	}
	var dlURLs []string
	rcid := ""
	for i := 0; i < maxPolls; i++ {
		if body, err := pollHistoryRaw(cid, cookie, sapisid, xsrf, proxyURL, authuser); err == nil {
			if rid != "" && tool == toolVideo {
				// Only this request's turn; skip polls where it hasn't shown up yet.
				// Video only: the turn layout was verified on video histories, music wasn't.
				scoped, ok := historyTurn(body, rid)
				if !ok {
					time.Sleep(interval)
					continue
				}
				body = scoped
			}
			if m := rcidInTurnRe.FindString(body); m != "" {
				rcid = m
			}
			// Text-only refusal: Gemini answered (reply present) but never started the
			// video tool, e.g. "I'm having a hard time fulfilling your request." Nothing
			// will ever be generated, so fail now with Gemini's own words.
			if tool == toolVideo {
				if reply, ok := turnReplyText(body); ok && !strings.Contains(body, "video_generation") {
					return nil, rcid, fmt.Errorf("%s: Gemini declined to make this video: %q (rephrase the prompt; real-artist styles, copyrighted lyrics and violent wording are common triggers)", errVideoRefused, truncate(reply, 200))
				}
			}
			if picked := pickResponseDataURLs(collectDownloadURLs(body)); len(picked) > 0 {
				dlURLs = picked
				break
			}
			// 视频被内容政策拒时 hNvQHb 里是「I can't generate that video」，别干等到超时。
			if strings.Contains(body, "can't generate that video") {
				return nil, rcid, fmt.Errorf("视频被内容政策拒绝（换个 prompt 再试）")
			}
			// Daily video quota exhausted on this account: the reply is "I can create more
			// videos as soon as your limit resets" with an internal "out of video generation
			// quota" status. Nothing will ever arrive, so fail fast instead of polling ~6 min.
			// Nothing was consumed; the caller can resubmit (LRU picks another account).
			if strings.Contains(body, "out of video generation quota") || strings.Contains(body, "as soon as your limit resets") {
				// 重置时间就在这一轮里（classifyVideoTurns 的 ResetAt），不用另读历史。
				var reset int64
				for _, ev := range classifyVideoTurns(body) {
					if ev.Kind == "quota_refused" && ev.ResetAt > reset {
						reset = ev.ResetAt
					}
				}
				return nil, rcid, &videoQuotaError{ResetAt: reset}
			}
		}
		time.Sleep(interval)
	}
	if len(dlURLs) == 0 {
		return nil, rcid, fmt.Errorf("hNvQHb 里没等到可下载的产物链接（response_data）")
	}
	var arts []MediaArtifact
	for _, u := range dlURLs {
		// contribution 那条要补 filename / opi。
		if !strings.Contains(u, "opi=") {
			sep := "?"
			if strings.Contains(u, "?") {
				sep = "&"
			}
			u += sep + "filename=artifact&opi=" + downloadOPI
		}
		mime, data, err := downloadBytes(u, cookie, proxyURL, defaultMime, authuser)
		if err != nil {
			return arts, rcid, err
		}
		if !artifactSeen(arts, data) {
			arts = append(arts, MediaArtifact{Mime: mime, Data: data})
		}
	}
	return arts, rcid, nil
}

var rcidInTurnRe = regexp.MustCompile(`rc_[0-9a-f]{8,}`)

// historyTurn returns an hNvQHb-shaped body holding only the turn whose response id
// is rid (turn[0] = [cid, rid]), so URL/refusal scans can't see other turns.
func historyTurn(body, rid string) (string, bool) {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[[") {
			continue
		}
		var arr []interface{}
		if json.Unmarshal([]byte(line), &arr) != nil {
			continue
		}
		for _, it := range arr {
			row, ok := it.([]interface{})
			if !ok || len(row) < 3 || row[0] != "wrb.fr" {
				continue
			}
			payload, ok := row[2].(string)
			if !ok {
				continue
			}
			var p []interface{}
			if json.Unmarshal([]byte(payload), &p) != nil || len(p) == 0 {
				continue
			}
			turns, ok := p[0].([]interface{})
			if !ok {
				continue
			}
			for _, t := range turns {
				tl, ok := t.([]interface{})
				if !ok || len(tl) == 0 {
					continue
				}
				ids, ok := tl[0].([]interface{})
				if !ok || len(ids) < 2 || ids[1] != rid {
					continue
				}
				one, _ := json.Marshal([]interface{}{[]interface{}{t}})
				env, _ := json.Marshal([]interface{}{[]interface{}{"wrb.fr", "hNvQHb", string(one)}})
				return string(env), true
			}
		}
	}
	return "", false
}

// turnReplyText returns the model's reply text of the (first) turn in an hNvQHb
// body: turn[3][0][0][1][0]. ok=false while the reply isn't there yet.
func turnReplyText(body string) (string, bool) {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[[") {
			continue
		}
		var arr []interface{}
		if json.Unmarshal([]byte(line), &arr) != nil {
			continue
		}
		for _, it := range arr {
			row, ok := it.([]interface{})
			if !ok || len(row) < 3 || row[0] != "wrb.fr" {
				continue
			}
			payload, _ := row[2].(string)
			var p []interface{}
			if json.Unmarshal([]byte(payload), &p) != nil || len(p) == 0 {
				continue
			}
			at := func(v interface{}, i int) interface{} {
				if l, ok := v.([]interface{}); ok && i < len(l) {
					return l[i]
				}
				return nil
			}
			txt, ok := at(at(at(at(at(at(p[0], 0), 3), 0), 0), 1), 0).(string)
			if ok && strings.TrimSpace(txt) != "" {
				return txt, true
			}
		}
	}
	return "", false
}

// pollHistoryRaw 调一次 hNvQHb 取会话历史，返回原始响应体。
func pollHistoryRaw(cid, cookie, sapisid, xsrf, proxyURL string, authuser int) (string, error) {
	inner, _ := json.Marshal([]interface{}{cid, 10, nil, 1, []interface{}{0}, []interface{}{4}, nil, 1})
	freq, _ := json.Marshal([]interface{}{[]interface{}{[]interface{}{"hNvQHb", string(inner), nil, "generic"}}})
	form := url.Values{}
	form.Set("f.req", string(freq))
	if xsrf != "" {
		form.Set("at", xsrf)
	}
	reqid := time.Now().UnixNano() % 1000000
	endpoint := fmt.Sprintf(
		"%s/_/BardChatUi/data/batchexecute?rpcids=hNvQHb&bl=%s&hl=en&_reqid=%d&rt=c", geminiBase(authuser),
		currentBL(proxyURL), reqid)

	// batchexecute 不带模型 header，其余（cookie / SAPISIDHASH / x-same-domain）跟主请求同款。
	headers := buildGeminiHeaders(cookie, sapisid, "")
	setAuthUserHeaders(headers, authuser)
	delete(headers, "x-goog-ext-525001261-jspb")

	status, _, body, err := uploadPost(endpoint, headers, []byte(form.Encode()), proxyURL)
	if err != nil {
		return "", err
	}
	if status != 200 {
		return "", fmt.Errorf("hNvQHb HTTP %d", status)
	}
	return string(body), nil
}

// deleteConversation 删掉 gemini.google.com 上留下的一条会话（#19）。
//
// 协议逐字取自抓包：rpc GzXR5e，参数 ["<cid>"]，mode "generic"，带 at=XSRF。
//
//	f.req=[[["GzXR5e","[\"c_xxx\"]",null,"generic"]]]&at=<xsrf>
//
// 只登录态可用（匿名没有 XSRF、会话也没落到账号里）。best-effort：删失败只记日志，
// 不影响已经返给客户端的响应。
func deleteConversation(cid, cookie, sapisid, xsrf, proxyURL string, authuser int) {
	inner, _ := json.Marshal([]interface{}{cid})
	freq, _ := json.Marshal([]interface{}{[]interface{}{[]interface{}{"GzXR5e", string(inner), nil, "generic"}}})
	form := url.Values{}
	form.Set("f.req", string(freq))
	form.Set("at", xsrf)
	reqid := time.Now().UnixNano() % 1000000
	endpoint := fmt.Sprintf(
		"%s/_/BardChatUi/data/batchexecute?rpcids=GzXR5e&bl=%s&hl=en&_reqid=%d&rt=c", geminiBase(authuser),
		currentBL(proxyURL), reqid)
	headers := buildGeminiHeaders(cookie, sapisid, "")
	setAuthUserHeaders(headers, authuser)
	delete(headers, "x-goog-ext-525001261-jspb")
	status, _, body, err := uploadPost(endpoint, headers, []byte(form.Encode()), proxyURL)
	if err != nil {
		logf("[autodel] 删会话 %s 失败: %v", cid, err)
		return
	}
	if status != 200 {
		logf("[autodel] 删会话 %s 返回 HTTP %d: %s", cid, status, truncate(string(body), 120))
		return
	}
	logf("[autodel] 已删会话 %s", cid)
}

// walkFramesForURLs 递归遍历 batchexecute 信封里所有字符串，返回 want 命中的那些（去重保序）。
//
// 响应结构：每行 [["wrb.fr","<rpc>","<json 字符串>",…]，真正的数据埋在那个内层 json
// 字符串里，深度不定。必须走 JSON 解析而不是对 raw 直接正则 —— raw 是**双层转义** JSON，
// URL 里的斜杠/边界在 raw 里跟解出来的不一样，正则会多抓或少抓几个字符，拼出来的 URL
// 就废了（实测正则抠的比真 URL 长 2 个字符，下载直接 400）。
func walkFramesForURLs(raw string, want func(string) bool) []string {
	seen := map[string]bool{}
	var out []string
	var walk func(interface{})
	walk = func(o interface{}) {
		switch v := o.(type) {
		case string:
			if want(v) && !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		case []interface{}:
			for _, x := range v {
				walk(x)
			}
		case map[string]interface{}:
			for _, x := range v {
				walk(x)
			}
		}
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[[") {
			continue
		}
		var arr []interface{}
		if json.Unmarshal([]byte(line), &arr) != nil {
			continue
		}
		for _, it := range arr {
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
	return out
}

// collectDownloadURLs 从响应里挖出所有 contribution 下载链（音乐/视频用）。
func collectDownloadURLs(raw string) []string {
	return walkFramesForURLs(raw, func(s string) bool {
		return strings.Contains(s, "contribution.usercontent.google.com/download")
	})
}

// collectImageURLs 从响应里挖出所有生成图片的 CDN 链（gg-dl 来自首帧、gg 来自 hNvQHb）。
// 这条 plain 链直接 GET 就回真图 —— 不要加 rd- 前缀（抓包里那个 rd- 是**另一套 token**，
// 拿本链的 token 拼 rd- 会 400）。
func collectImageURLs(raw string) []string {
	return walkFramesForURLs(raw, func(s string) bool {
		return strings.Contains(s, "lh3.googleusercontent.com/gg-dl/") ||
			strings.Contains(s, "lh3.googleusercontent.com/gg/")
	})
}

// pickResponseDataURLs 从一堆下载链里挑真正能下的那种（c 参数解出来含 "response_data"）。
// 另外两种（temp_data 预览、gg-dl）下下来是 403，得排除。
func pickResponseDataURLs(urls []string) []string {
	var out []string
	for _, u := range urls {
		if downloadIsResponseData(u) {
			out = append(out, u)
		}
	}
	return out
}

// downloadIsResponseData 解 c 参数的 base64，看 protobuf 里有没有 "response_data" 标记。
// 不用裸字符串匹配 base64 片段：那个受对齐影响，换个前缀就漏。
func downloadIsResponseData(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	c := u.Query().Get("c")
	if c == "" {
		return false
	}
	dec, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(c, "="))
	if err != nil {
		dec, err = base64.StdEncoding.DecodeString(c)
		if err != nil {
			return false
		}
	}
	return strings.Contains(string(dec), "response_data")
}

// downloadBytes GET 一条产物链，跟随重定向并每跳重发 cookie 子集，返回 content-type
// 和原始字节。图片链（会 302）和 contribution 链（单次 200）都走这个。
func downloadBytes(rawURL, cookie, proxyURL, defaultMime string, authuser int) (string, []byte, error) {
	rawURL = withAuthUserQuery(rawURL, authuser)
	headers := map[string]string{
		"Cookie":  filterDownloadCookies(cookie),
		"Origin":  "https://gemini.google.com",
		"Referer": "https://gemini.google.com/",
		"Accept":  "image/avif,image/webp,image/apng,image/*,*/*;q=0.8",
	}
	status, respHead, body, err := mediaGetFollow(rawURL, headers, proxyURL)
	if err != nil {
		return "", nil, err
	}
	if status != 200 {
		return "", nil, fmt.Errorf("下载产物 HTTP %d（%d 字节）", status, len(body))
	}
	mime := respHead["content-type"]
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	if mime == "" || strings.HasPrefix(mime, "text/html") {
		mime = defaultMime
	}
	return mime, body, nil
}

// mediaGetFollow 手动跟随重定向（最多 6 跳），每跳都把同一套 header（含 Cookie）重发。
//
// 为什么不用客户端自带的跟随：图片链会跨域 302（lh3 → work.fife.usercontent.google.com），
// http 客户端出于安全默认不把 Cookie 头带到新域，于是重定向目标拿不到 cookie 就 403。
// 手动跟随、每跳重发 cookie 才能过。两个 client 本来也都配了「不自动跟随」。
func mediaGetFollow(rawURL string, headers map[string]string, proxyURL string) (int, map[string]string, []byte, error) {
	cur := rawURL
	for hop := 0; hop < 6; hop++ {
		status, respHead, body, err := mediaGetOnce(cur, headers, proxyURL)
		if err != nil {
			return status, respHead, body, err
		}
		if status >= 300 && status < 400 {
			loc := respHead["location"]
			if loc == "" {
				return status, respHead, body, nil
			}
			next, err := resolveRef(cur, loc)
			if err != nil {
				return status, respHead, body, err
			}
			cur = next
			continue
		}
		return status, respHead, body, nil
	}
	return 0, nil, nil, fmt.Errorf("下载重定向次数过多")
}

// resolveRef 按当前 URL 解析 Location（多数是绝对地址，也兼容相对）。
func resolveRef(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return b.ResolveReference(r).String(), nil
}

// mediaGetOnce 发一次 GET，不跟随重定向，响应头一起带回（要取 content-type / location）。
// 有代理走 stdlib，没代理走 tls-client，跟 doGeminiRequest / uploadPost 一个规矩。
func mediaGetOnce(rawURL string, headers map[string]string, proxyURL string) (int, map[string]string, []byte, error) {
	if proxyURL != "" {
		req, err := http.NewRequest("GET", rawURL, nil)
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
		return resp.StatusCode, flattenHeaders(resp.Header), b, err
	}
	req, err := fhttp.NewRequest("GET", rawURL, nil)
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
	return resp.StatusCode, flattenFHeaders(resp.Header), b, err
}

func flattenHeaders(h http.Header) map[string]string {
	m := map[string]string{}
	for k := range h {
		m[strings.ToLower(k)] = h.Get(k)
	}
	return m
}

func flattenFHeaders(h fhttp.Header) map[string]string {
	m := map[string]string{}
	for k := range h {
		m[strings.ToLower(k)] = h.Get(k)
	}
	return m
}

// extractConversationID 从 StreamGenerate 响应里取会话 id（帧的 [1][0]）。
// 取回音乐/视频产物要用它去 hNvQHb 拿这次生成的历史。
func extractConversationID(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[[") {
			continue
		}
		var arr []interface{}
		if json.Unmarshal([]byte(line), &arr) != nil {
			continue
		}
		for _, it := range arr {
			row, ok := it.([]interface{})
			if !ok || len(row) < 3 || row[0] != "wrb.fr" {
				continue
			}
			payload, ok := row[2].(string)
			if !ok {
				continue
			}
			var inner []interface{}
			if json.Unmarshal([]byte(payload), &inner) != nil || len(inner) < 2 {
				continue
			}
			meta, ok := inner[1].([]interface{})
			if ok && len(meta) > 0 {
				if cid, ok := meta[0].(string); ok && cid != "" {
					return cid
				}
			}
		}
	}
	return ""
}

// artifactSeen 判断这份字节是不是已经收过（按内容比，收掉同图不同 token 的重复）。
func artifactSeen(arts []MediaArtifact, data []byte) bool {
	for _, a := range arts {
		if bytes.Equal(a.Data, data) {
			return true
		}
	}
	return false
}

// appendArtifactMarkdown 把产物字节转成 base64 data URL 追加到正文后面。
// 图片用 markdown 图片语法（多数聊天 UI 能直接渲染），其余（音频）用链接语法。
func appendArtifactMarkdown(text string, arts []MediaArtifact) string {
	var b strings.Builder
	b.WriteString(text)
	for _, a := range arts {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		dataURL := "data:" + a.Mime + ";base64," + base64.StdEncoding.EncodeToString(a.Data)
		if strings.HasPrefix(a.Mime, "image/") {
			b.WriteString("![image](" + dataURL + ")")
		} else {
			b.WriteString("[audio](" + dataURL + ")")
		}
	}
	return b.String()
}

// dataURLRe 匹配一整条 base64 data URL。
var dataURLRe = regexp.MustCompile(`data:([-\w.+/]+);base64,[A-Za-z0-9+/=]+`)

// stripDataURLs 把 base64 data URL 从文本里抠掉，只留个短占位。
// 算 token / 记长度时用 —— 一张图 base64 上百万字符，按它计费等于让用户为看不见的
// 二进制买单，下游 newapi 是按 output token 收钱的。产物本身照常在 content 里返回。
func stripDataURLs(text string) string {
	return dataURLRe.ReplaceAllString(text, "data:$1;base64,<omitted>")
}

// withAuthUserQuery 给产物下载链加 ?authuser=N（N>0 时）。
//
// 多号登录时第 N 个号生成的图，原样下载会在跳转链末端（lh3）回 403；第一跳带上
// authuser=N 就是 200（实测同一条链：不带 403、带 200；加 X-Goog-AuthUser 头或改
// Referer 都还是 403）。默认号不用带，链接保持原样。
func withAuthUserQuery(rawURL string, authuser int) string {
	if authuser <= 0 {
		return rawURL
	}
	// 直接追加，不走 url.Values 重新编码：contribution 链的 c= 参数是长 token，
	// 重编码会改动原有部分。
	sep := "?"
	if strings.Contains(rawURL, "?") {
		sep = "&"
	}
	return rawURL + sep + "authuser=" + strconv.Itoa(authuser)
}
