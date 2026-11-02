package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/google/uuid"
)

// 新协议 processSession（Google WebChannel），开关 web_channel。
//
// 后端没换，换的是传输层：上行发的还是 StreamGenerate 那个 inner 数组，下行帧里装的
// 还是老响应那套数组（[null,[cid,rid],null,null,[[rc,[正文]…]]…]）。所以这里把下行帧
// 包成老格式的 wrb.fr 行交给调用方，正文、思考链、模型名、降级判断、媒体取回都原样复用。
//
// 流程（host 是 geminiweb-pa.clients6.google.com）：
//  1. 握手 POST count=0：拿 gsessionid（响应头）和 SID（响应体）。鉴权在这一步绑到会话上：
//     $httpHeaders 里的 API key、模型头、SAPISIDHASH，加上这条请求本身带的 cookie。
//  2. 下行 GET 长轮询：帧是「长度\n JSON」首尾相接。服务端约 60 秒空闲就关，要带 AID 重连。
//  3. 上行 POST：ofs=0 发 inner（正文槽留空）；下行回了确认后隔 wcPromptGap 发 ofs=1 正文；
//     看到「生成完毕」标记后发 ofs=2 收尾，服务端发完剩余帧（标题等）再给结束帧
//     [null,null,null,[]]。
//
// 实测判据（2026-11-02，登录号逐项对照）：
//   - ofs=2 紧跟正文发：有会话 id 和标题、没有正文——它是收尾信号，必须等生成完毕。
//   - 握手 $httpHeaders 带 SAPISIDHASH 而握手请求不带 cookie：status 16「缺凭证」。
//     所以授权头只在有 cookie 时加，cookie 每一步都带。
//   - 不声明 X-WebChannel-Content-Type: application/json+protobuf：400「Root element must be
//     a message」，服务端按 proto3 JSON 解析，JSPB 数组被拒。
//   - GET 不带凭证、不带 SAPISIDHASH 都不影响结果。
//   - ofs=0 之后立即发正文：多数只回会话 id 和 rid、不生成，见 wcPromptGap。
//
// 覆盖范围：单轮这条路（streamGenerateOnce），实测文本（3.6 Flash / 3.1 Pro / Flash-Lite，
// 流式和非流式）、看图、生图、匿名都通。多轮续接（conversation.go）仍走老端点。
// 已知限制：扩展思考没切成功——模型头字段 16 照发，回报仍是「3.1 Pro」而不是「3.1 Pro High」。
// 音乐、视频没测。
const wcEndpoint = "https://geminiweb-pa.clients6.google.com/v1/processSession"

// wcAckWait 是等 ofs=0 确认的上限，到点没等到也照发正文。
const wcAckWait = 3 * time.Second

// wcPromptGap 是收到 ofs=0 确认之后、发正文之前再等的时间。
//
// 确认只说明收到了，不说明会话准备好了：确认后立即发正文，同一个号连续实测约 6 次只成 1 次，
// 其余只回会话 id 和 rid、不生成，或回一句通用失败文案；隔 500ms 连续 5 次全成。浏览器在用户
// 打字时就先发 ofs=0，正文通常隔几秒才到，所以这个间隔本来就在。
const wcPromptGap = 500 * time.Millisecond

// wcRequest 是一次新协议请求需要的全部输入。
type wcRequest struct {
	Inner    []interface{} // ofs=0 发的 inner，正文槽留空
	Prompt   string        // ofs=1 发的正文
	HexID    string
	Mode     int
	Think    int
	Cookie   string
	SAPISID  string
	AuthUser int
}

// wcStatusError 是服务端在下行帧里给的错误（{"__sm__":{"status":…}}）。
type wcStatusError struct {
	Code int // google.rpc.Code（16=UNAUTHENTICATED）或 HTTP 状态码（400）
	Msg  string
}

func (e *wcStatusError) Error() string {
	return fmt.Sprintf("processSession status %d: %s", e.Code, e.Msg)
}

// httpStatus 把下行错误映射成 HTTP 状态，给上层重试和 cookie 健康度用。
// 只有 16（缺凭证/凭证无效）算 cookie 的错；7（没认出 API key）是我们的问题，不能记到号头上。
func (e *wcStatusError) httpStatus() int {
	switch e.Code {
	case 16:
		return 401
	case 400:
		return 400
	}
	return 502
}

// --- API key ---

// 页面 WIZ 里的 d2zJAe。跟登录态无关（匿名页和登录页是同一个），按出口缓存不必要，全局一份。
var wcKeyRe = regexp.MustCompile(`"d2zJAe":"(AIza[0-9A-Za-z_-]{35})"`)

const wcKeyTTL = 6 * time.Hour

var (
	wcKeyMu sync.Mutex
	wcKey   string
	wcKeyAt time.Time
)

func webChannelAPIKey(proxyURL string) (string, error) {
	wcKeyMu.Lock()
	defer wcKeyMu.Unlock()
	if wcKey != "" && time.Since(wcKeyAt) < wcKeyTTL {
		return wcKey, nil
	}
	body, err := fetchAppPage("", 0, proxyURL)
	if err != nil {
		if wcKey != "" {
			return wcKey, nil // 抓页失败先用旧的，key 很少变
		}
		return "", fmt.Errorf("取 processSession API key: %w", err)
	}
	m := wcKeyRe.FindSubmatch(body)
	if m == nil {
		if wcKey != "" {
			return wcKey, nil
		}
		return "", errors.New("取 processSession API key: 页面里没有 d2zJAe")
	}
	wcKey, wcKeyAt = string(m[1]), time.Now()
	return wcKey, nil
}

// --- 请求头 ---

// wcModelHeaderBin 是 x-goog-ext-525001261-jspb 的 proto 版：JSPB 数组下标 n ↔ proto 字段 n+1。
// 字段 1=1、5=hex、15=mode、16=think。跟老协议一样，服务端选模型认这个头不认 payload。
func wcModelHeaderBin(hexID string, mode, think int) string {
	var b []byte
	b = append(b, 0x08, 0x01)
	if hexID != "" {
		b = append(b, 0x2a)
		b = appendVarint(b, uint64(len(hexID)))
		b = append(b, hexID...)
	}
	if mode > 0 {
		b = append(b, 0x78) // 15<<3|0
		b = appendVarint(b, uint64(mode))
	}
	if think > 0 {
		b = append(b, 0x80, 0x01) // 16<<3|0
		b = appendVarint(b, uint64(think))
	}
	return base64.StdEncoding.EncodeToString(b)
}

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// wcHTTPHeaders 是握手 URL 里 $httpHeaders 的值（"K:V\r\n" 拼接）。
// 授权头只在有 cookie 时加：带授权头不带 cookie 服务端判「缺凭证」（见文件头）。
// 新协议的 host 带不了 /u/N 路径，选号靠 X-Goog-AuthUser（浏览器抓包如此）。
func wcHTTPHeaders(apiKey string, r wcRequest) string {
	var b strings.Builder
	add := func(k, v string) { b.WriteString(k + ":" + v + "\r\n") }
	add("X-Goog-Api-Key", apiKey)
	add("x-goog-ext-525001261-bin", wcModelHeaderBin(r.HexID, r.Mode, r.Think))
	add("X-WebChannel-Content-Type", "application/json+protobuf")
	if r.Cookie != "" && r.SAPISID != "" {
		add("Authorization", makeSAPISIDHash(r.SAPISID))
		add("X-Goog-AuthUser", strconv.Itoa(r.AuthUser))
	}
	return b.String()
}

func wcZX() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 12)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

// --- HTTP ---

type wcResp struct {
	status    int
	setCookie []string
	sessionID string
	body      io.ReadCloser
}

// wcSend 发一条请求。跟 doGeminiRequest 同一套出口规则：配了代理走 stdlib，直连走 tls-client。
func wcSend(ctx context.Context, method, u, body, cookie, proxyURL string) (*wcResp, error) {
	headers := map[string]string{
		"Accept":          "*/*",
		"Accept-Language": "en-US,en;q=0.9",
		"Origin":          "https://gemini.google.com",
		"Referer":         "https://gemini.google.com/",
	}
	if method == "POST" {
		headers["Content-Type"] = "application/x-www-form-urlencoded"
	}
	if cookie != "" {
		headers["Cookie"] = cookie
	}
	var rd io.Reader
	if method == "POST" {
		rd = strings.NewReader(body)
	}
	if proxyURL != "" {
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return nil, err
		}
		applyChromeHeaders(req)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := getStdlibClient(proxyURL).Do(req)
		if err != nil {
			return nil, err
		}
		return &wcResp{resp.StatusCode, resp.Header.Values("Set-Cookie"),
			resp.Header.Get("X-HTTP-Session-Id"), resp.Body}, nil
	}
	req, err := fhttp.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := getTLSClient().Do(req)
	if err != nil {
		return nil, err
	}
	return &wcResp{resp.StatusCode, resp.Header.Values("Set-Cookie"),
		resp.Header.Get("X-HTTP-Session-Id"), resp.Body}, nil
}

// --- 帧 ---

// wcItem 是下行帧里的一项 [id, payload]。
type wcItem struct {
	ID      int64
	Payload json.RawMessage
}

// wcReadFrames 从 r 里逐帧读出 [id,payload] 项交给 fn，fn 返回 false 就停。
//
// 帧格式是「长度\n JSON」首尾相接、没有分隔符。长度按 JS 字符串长度计（UTF-16 码元），
// 跟 Go 的字节数对不上，所以不按长度切：长度当一个 JSON 数字读掉，后面紧跟的就是一个完整
// JSON 值，json.Decoder 能自己找到边界。
func wcReadFrames(r io.Reader, fn func(wcItem) bool) error {
	dec := json.NewDecoder(r)
	for {
		var n json.Number
		if err := dec.Decode(&n); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		var frame []json.RawMessage
		if err := dec.Decode(&frame); err != nil {
			return err
		}
		for _, it := range frame {
			var pair []json.RawMessage
			if json.Unmarshal(it, &pair) != nil || len(pair) < 2 {
				continue
			}
			id, _ := strconv.ParseInt(string(pair[0]), 10, 64)
			if !fn(wcItem{ID: id, Payload: pair[1]}) {
				return nil
			}
		}
	}
}

// wcEvent 是一项下行 payload 的含义。
type wcEvent struct {
	Line string // 包成老格式的 wrb.fr 行（有内容才有）
	Ack  bool   // 服务端对 ofs=0 的确认（消息 [[]]）
	Done bool   // 生成完毕标记
	End  bool   // 流结束（[[null,null,null,[]]] 或 ["close"]）
	Err  *wcStatusError
}

// wcClassify 解析一项下行 payload。
//
// payload 是控制帧（["noop"] / ["close"]）、错误（{"__sm__":…}），或者一个只装一条消息的
// 数组 [msg]。内容消息形状：[null,[seq,null×5,[[flag,[rid],null,[null,null,null,X]]]]]，
// X 就是老响应的 inner 数组；flag=1 表示生成完毕。结束消息是 [null,null,null,[]]。
func wcClassify(p json.RawMessage) wcEvent {
	s := strings.TrimSpace(string(p))
	switch s {
	case `["noop"]`:
		return wcEvent{}
	case `["close"]`:
		return wcEvent{End: true}
	}
	if strings.HasPrefix(s, "{") {
		if e := wcParseStatus(p); e != nil {
			return wcEvent{Err: e}
		}
		return wcEvent{}
	}
	var msgs []json.RawMessage
	if json.Unmarshal(p, &msgs) != nil || len(msgs) == 0 {
		return wcEvent{}
	}
	msg := strings.TrimSpace(string(msgs[0]))
	switch msg {
	case `[null,null,null,[]]`:
		return wcEvent{End: true}
	case `[[]]`:
		return wcEvent{Ack: true}
	}
	var top []json.RawMessage
	if json.Unmarshal([]byte(msg), &top) != nil || len(top) < 2 {
		return wcEvent{}
	}
	var body []json.RawMessage
	if json.Unmarshal(top[1], &body) != nil || len(body) < 7 {
		return wcEvent{}
	}
	var cands [][]json.RawMessage
	if json.Unmarshal(body[6], &cands) != nil || len(cands) == 0 || len(cands[0]) < 4 {
		return wcEvent{}
	}
	ev := wcEvent{Done: strings.TrimSpace(string(cands[0][0])) == "1"}
	var wrap []json.RawMessage
	if json.Unmarshal(cands[0][3], &wrap) != nil || len(wrap) < 4 {
		return ev
	}
	inner := strings.TrimSpace(string(wrap[3]))
	if inner == "" || inner == "null" {
		return ev
	}
	line, _ := json.Marshal([]interface{}{[]interface{}{"wrb.fr", nil, inner}})
	ev.Line = string(line)
	return ev
}

// wcParseStatus 解析 {"__sm__":{"status":…}}。实测两种形状：
//
//	[[[16,"Request is missing required authentication credential…"]]]
//	[[{"error":{"code":400,"message":"Invalid JSON payload…"}}]]
func wcParseStatus(p json.RawMessage) *wcStatusError {
	var sm struct {
		SM struct {
			Status [][]json.RawMessage `json:"status"`
		} `json:"__sm__"`
	}
	if json.Unmarshal(p, &sm) != nil || len(sm.SM.Status) == 0 || len(sm.SM.Status[0]) == 0 {
		return nil
	}
	first := sm.SM.Status[0][0]
	var tuple []json.RawMessage
	if json.Unmarshal(first, &tuple) == nil && len(tuple) >= 1 {
		e := &wcStatusError{}
		_ = json.Unmarshal(tuple[0], &e.Code)
		if len(tuple) >= 2 {
			_ = json.Unmarshal(tuple[1], &e.Msg)
		}
		return e
	}
	var obj struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(first, &obj) == nil && obj.Error.Code != 0 {
		return &wcStatusError{Code: obj.Error.Code, Msg: obj.Error.Message}
	}
	return &wcStatusError{Code: 2, Msg: truncate(string(p), 200)}
}

// --- 主流程 ---

// doWebChannelRequest 走新协议发一次请求，返回值跟 doGeminiRequest 对齐：
// (HTTP 状态, 老格式的 raw, TTFB 毫秒, Set-Cookie, err)。下行错误按 wcStatusError.httpStatus
// 映射成状态码，body 是错误文本，让上层的重试和 cookie 健康度逻辑原样生效。
func doWebChannelRequest(r wcRequest, proxyURL string, onLine func(string) error) (int, []byte, int64, []string, error) {
	sendAt := time.Now()
	apiKey, err := webChannelAPIKey(proxyURL)
	if err != nil {
		return 0, nil, 0, nil, err
	}
	timeout := time.Duration(rtCfg().RequestTimeout) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var setCookie []string
	var setCookieMu sync.Mutex
	keepCookies := func(sc []string) {
		setCookieMu.Lock()
		setCookie = append(setCookie, sc...)
		setCookieMu.Unlock()
	}

	// 1) 握手
	rid := int64(10000 + rand.Intn(80000))
	hsURL := fmt.Sprintf("%s?VER=8&RID=%d&CVER=22&X-HTTP-Session-Id=gsessionid&$httpHeaders=%s&zx=%s&t=1",
		wcEndpoint, rid, url.QueryEscape(wcHTTPHeaders(apiKey, r)), wcZX())
	rid++
	hs, err := wcSend(ctx, "POST", hsURL, "count=0", r.Cookie, proxyURL)
	if err != nil {
		return 0, nil, 0, nil, err
	}
	keepCookies(hs.setCookie)
	hsBody, _ := io.ReadAll(io.LimitReader(hs.body, 1<<20))
	hs.body.Close()
	if hs.status != 200 {
		return hs.status, hsBody, 0, setCookie, nil
	}
	var sid string
	_ = wcReadFrames(strings.NewReader(string(hsBody)), func(it wcItem) bool {
		var arr []interface{}
		if json.Unmarshal(it.Payload, &arr) == nil && len(arr) >= 2 && arr[0] == "c" {
			sid, _ = arr[1].(string)
			return false
		}
		return true
	})
	gsid := hs.sessionID
	if sid == "" || gsid == "" {
		return 502, []byte("processSession 握手没拿到会话: " + truncate(string(hsBody), 200)), 0, setCookie, nil
	}
	sess := fmt.Sprintf("gsessionid=%s&SID=%s", url.QueryEscape(gsid), url.QueryEscape(sid))

	// 2) 下行：单独 goroutine 读帧，服务端关连接就带 AID 重连，直到流结束或超时。
	var lastID atomic.Int64
	events := make(chan wcEvent, 64)
	getErr := make(chan error, 1)
	getReady := make(chan struct{})
	var readyOnce sync.Once
	go func() {
		defer close(events)
		for ctx.Err() == nil {
			gURL := fmt.Sprintf("%s?%s&VER=8&RID=rpc&AID=%d&CI=0&TYPE=xmlhttp&zx=%s&t=1",
				wcEndpoint, sess, lastID.Load(), wcZX())
			g, err := wcSend(ctx, "GET", gURL, "", r.Cookie, proxyURL)
			if err != nil {
				getErr <- err
				return
			}
			keepCookies(g.setCookie)
			if g.status != 200 {
				b, _ := io.ReadAll(io.LimitReader(g.body, 4096))
				g.body.Close()
				getErr <- fmt.Errorf("processSession 下行 HTTP %d: %s", g.status, truncate(string(b), 200))
				return
			}
			readyOnce.Do(func() { close(getReady) })
			ended := false
			rerr := wcReadFrames(g.body, func(it wcItem) bool {
				if it.ID > lastID.Load() {
					lastID.Store(it.ID)
				}
				ev := wcClassify(it.Payload)
				if ev.Line == "" && !ev.Done && !ev.End && !ev.Ack && ev.Err == nil {
					return true
				}
				select {
				case events <- ev:
				case <-ctx.Done():
					return false
				}
				if ev.End || ev.Err != nil {
					ended = true
					return false
				}
				return true
			})
			g.body.Close()
			if ended {
				return
			}
			if rerr != nil && ctx.Err() == nil {
				getErr <- rerr
				return
			}
		}
	}()

	// 3) 上行
	post := func(ofs int, data interface{}) error {
		j, err := json.Marshal(data)
		if err != nil {
			return err
		}
		n := atomic.AddInt64(&rid, 1) - 1
		pURL := fmt.Sprintf("%s?VER=8&%s&RID=%d&AID=%d&zx=%s&t=1", wcEndpoint, sess, n, lastID.Load(), wcZX())
		resp, err := wcSend(ctx, "POST", pURL,
			fmt.Sprintf("count=1&ofs=%d&req0___data__=%s", ofs, url.QueryEscape(string(j))), r.Cookie, proxyURL)
		if err != nil {
			return err
		}
		keepCookies(resp.setCookie)
		b, _ := io.ReadAll(io.LimitReader(resp.body, 4096))
		resp.body.Close()
		if resp.status != 200 {
			return fmt.Errorf("processSession 上行 ofs=%d HTTP %d: %s", ofs, resp.status, truncate(string(b), 200))
		}
		return nil
	}
	meta := []interface{}{1, r.HexID, "en", uuid.NewString()}
	ofs0 := []interface{}{[]interface{}{meta, 0, nil, nil, nil, nil, nil,
		[]interface{}{nil, nil, nil, nil, []interface{}{r.Inner, nil, nil, 1}}}}
	ofs1 := []interface{}{nil, []interface{}{1, nil, nil, nil, nil, nil, nil, nil, []interface{}{r.Prompt}}}
	ofs2 := []interface{}{nil, nil, nil, []interface{}{1}}
	// 先等下行建立：ofs=0 的确认要从下行收，下行没建立就等不到。
	select {
	case <-getReady:
	case e := <-getErr:
		return 0, nil, 0, setCookie, e
	case <-ctx.Done():
		return 0, nil, 0, setCookie, fmt.Errorf("processSession 下行没建立（%s）", timeout)
	}
	if err := post(0, ofs0); err != nil {
		return 0, nil, 0, setCookie, err
	}

	// 4) 收下行，直到结束帧。正文（ofs=1）要等下行回了对 ofs=0 的确认（[[]]）再发：
	// 抢在确认之前发，服务端只回会话 id 和 rid、不生成（实测时有时无，是时序竞争）。
	// 确认一直不来就按超时补发，免得卡死。
	var raw strings.Builder
	var ttfb int64 = -1
	closing := false
	promptSent := false
	sendPrompt := func() error {
		promptSent = true
		time.Sleep(wcPromptGap)
		return post(1, ofs1)
	}
	ackTimer := time.NewTimer(wcAckWait)
	defer ackTimer.Stop()
	for {
		select {
		case <-ackTimer.C:
			if !promptSent {
				if err := sendPrompt(); err != nil {
					return 0, nil, 0, setCookie, err
				}
			}
			continue
		case ev, ok := <-events:
			if !ok {
				select {
				case e := <-getErr:
					return 0, []byte(raw.String()), ttfbOr(ttfb, sendAt), setCookie, e
				default:
				}
				if ctx.Err() != nil {
					return 0, []byte(raw.String()), ttfbOr(ttfb, sendAt), setCookie,
						fmt.Errorf("processSession 超时（%s）", timeout)
				}
				return 200, []byte(raw.String()), ttfbOr(ttfb, sendAt), setCookie, nil
			}
			if ev.Err != nil {
				return ev.Err.httpStatus(), []byte(ev.Err.Error()), ttfbOr(ttfb, sendAt), setCookie, nil
			}
			if ev.Ack && !promptSent {
				if err := sendPrompt(); err != nil {
					return 0, nil, 0, setCookie, err
				}
			}
			if ev.Line != "" {
				if ttfb < 0 {
					ttfb = time.Since(sendAt).Milliseconds()
				}
				raw.WriteString(ev.Line)
				raw.WriteByte('\n')
				if onLine != nil {
					if err := onLine(ev.Line); err != nil {
						return 200, []byte(raw.String()), ttfb, setCookie, err
					}
				}
			}
			if ev.Done && !closing {
				// 收尾信号必须等生成完毕再发，早发会把正文掐掉（见文件头）。
				closing = true
				go func() { _ = post(2, ofs2) }()
			}
			if ev.End {
				return 200, []byte(raw.String()), ttfbOr(ttfb, sendAt), setCookie, nil
			}
		case <-ctx.Done():
			return 0, []byte(raw.String()), ttfbOr(ttfb, sendAt), setCookie,
				fmt.Errorf("processSession 超时（%s）", timeout)
		}
	}
}

func ttfbOr(ttfb int64, start time.Time) int64 {
	if ttfb >= 0 {
		return ttfb
	}
	return time.Since(start).Milliseconds()
}

// wcInnerWithoutPrompt 复制一份 inner，把正文槽清空：浏览器在 ofs=0 发空正文，正文走 ofs=1。
func wcInnerWithoutPrompt(inner []interface{}) []interface{} {
	out := append([]interface{}(nil), inner...)
	if first, ok := out[0].([]interface{}); ok && len(first) > 0 {
		f := append([]interface{}(nil), first...)
		f[0] = ""
		out[0] = f
	}
	return out
}
