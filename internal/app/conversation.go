package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/google/uuid"
)

// convState 是一路 Gemini 原生会话的状态。整条会话绑定同一个 cookie / 出口，中途不换
// —— 换了 Gemini 就当成另一个会话，续接的 cid 失效。
type convState struct {
	cid, rid, rcid, tok26 string
	turn                  int    // 会话内轮次索引，填 inner[17]，逐轮 +1
	cookie                string // 会话绑定 cookie：登录号的 cookie，或匿名 /app 拿的 session
	sapisid               string // 登录态算 SAPISIDHASH 用；匿名为空
	isLogin               bool
	accountID             int64
	authuser              int // 登录号的账号序号（/u/N）；整条会话不变，匿名为 0
	proxyID               int64
	proxyURL              string
	updated               time.Time
}

const (
	// convTTL 跟导出 cookie ~2 小时寿命对齐，过期的会话状态没有复用价值。
	convTTL = 90 * time.Minute
	convMax = 4000
)

var (
	convMu    sync.Mutex
	convStore = map[string]*convState{}
)

// convTake atomically consumes the longest matching proper prefix. A state is
// never left under an old fingerprint after Google advances it: concurrent
// duplicates and forks start fresh instead of mutating the same upstream turn.
func convTake(keys []string) (*convState, int) {
	convMu.Lock()
	defer convMu.Unlock()
	for i := len(keys) - 2; i >= 0; i-- {
		st, ok := convStore[keys[i]]
		if !ok {
			continue
		}
		delete(convStore, keys[i])
		if time.Since(st.updated) > convTTL {
			continue
		}
		copy := *st
		return &copy, i + 1
	}
	return nil, 0
}

// convPut 存一路会话，超量就按最旧的淘汰一批。
func convPut(key string, st *convState) {
	if key == "" {
		return
	}
	convMu.Lock()
	defer convMu.Unlock()
	snapshot := *st
	snapshot.updated = time.Now()
	convStore[key] = &snapshot
	if len(convStore) > convMax {
		// 简单淘汰：删掉过期的；还超就删最旧的一小批。
		var oldestK string
		var oldestT time.Time
		first := true
		for k, v := range convStore {
			if time.Since(v.updated) > convTTL {
				delete(convStore, k)
				continue
			}
			if first || v.updated.Before(oldestT) {
				oldestK, oldestT, first = k, v.updated, false
			}
		}
		if len(convStore) > convMax && oldestK != "" {
			delete(convStore, oldestK)
		}
	}
}

// canonicalMessages retains the semantic message structure, including tools.
// JSON escaping makes embedded control characters unambiguous.
func canonicalMessages(messages []map[string]interface{}) string {
	b, _ := json.Marshal(normalizeConversationMessages(messages))
	return string(b)
}

func hashStr(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

var rcidRe = regexp.MustCompile(`"(rc_[A-Za-z0-9_-]{6,})"`)

// parseConvIDs 从 StreamGenerate 响应里取续接要用的四样：cid=[1][0]、rid=[1][1]、
// tok26=帧[26]、rcid=正文里的 rc_xxx（登录态续接第 3 位要用它，匿名为空）。
func parseConvIDs(raw string) (cid, rid, rcid, tok26 string) {
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
			var f []interface{}
			if json.Unmarshal([]byte(payload), &f) != nil {
				continue
			}
			if len(f) > 1 {
				if meta, ok := f[1].([]interface{}); ok && len(meta) >= 2 {
					if s, ok := meta[0].(string); ok && s != "" {
						cid = s
					}
					if s, ok := meta[1].(string); ok && s != "" {
						rid = s
					}
				}
			}
			if len(f) > 26 {
				if s, ok := f[26].(string); ok && s != "" {
					tok26 = s
				}
			}
		}
	}
	if m := rcidRe.FindStringSubmatch(raw); m != nil {
		rcid = m[1]
	}
	return
}

// getAnonSession 匿名 GET /app 拿一份 session cookie（NID/COMPASS 等），给匿名多轮当会话
// 载体。续接请求必须每轮带上它，否则被服务端当新会话（实测不带 0/4 通、带 4/4 通）。
// 必须走跟正式请求同一出口。
func getAnonSession(proxyURL string) (string, error) {
	req := func() (*http.Response, io.ReadCloser, []string, error) {
		if proxyURL != "" {
			r, err := http.NewRequest("GET", "https://gemini.google.com/app", nil)
			if err != nil {
				return nil, nil, nil, err
			}
			applyChromeHeaders(r)
			for k, v := range appPageHeaders() {
				r.Header.Set(k, v)
			}
			resp, err := getStdlibClient(proxyURL).Do(r)
			if err != nil {
				return nil, nil, nil, err
			}
			return resp, resp.Body, resp.Header.Values("Set-Cookie"), nil
		}
		r, err := fhttp.NewRequest("GET", "https://gemini.google.com/app", nil)
		if err != nil {
			return nil, nil, nil, err
		}
		applyBrowserFHeaders(r)
		for k, v := range appPageHeaders() {
			r.Header.Set(k, v)
		}
		resp, err := getTLSClient().Do(r)
		if err != nil {
			return nil, nil, nil, err
		}
		return nil, resp.Body, resp.Header.Values("Set-Cookie"), nil
	}
	_, body, setCookies, err := req()
	if err != nil {
		return "", err
	}
	if body != nil {
		io.Copy(io.Discard, body)
		body.Close()
	}
	var pairs []string
	seen := map[string]bool{}
	for _, sc := range setCookies {
		nv := strings.SplitN(sc, ";", 2)[0]
		name := strings.TrimSpace(strings.SplitN(nv, "=", 2)[0])
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		pairs = append(pairs, strings.TrimSpace(nv))
	}
	if len(pairs) == 0 {
		return "", fmt.Errorf("匿名 /app 没返回 Set-Cookie")
	}
	return strings.Join(pairs, "; "), nil
}

// streamGenerateConv 发一路会话的一轮。turn==0 是首轮（建会话），>0 是续接（只发新消息）。
// 整条会话绑定 conv.cookie / conv.proxyID，中途不换号不换出口。成功后回填 conv 的
// cid/rid/rcid/tok26 和 turn+1，供下一轮续接。
func streamGenerateConv(prompt string, mc ModelConfig, conv *convState,
	onDelta, onReasoning func(string)) (*StreamResult, error) {
	p, slotOK, slotErr := acquireSlot(conv.proxyID)
	if !slotOK {
		return &StreamResult{}, slotErr
	}
	defer releaseSlot(p.ID)
	proxyURL := p.URL
	pickedOK := p.ID > 0

	// 首轮定出口；续接沿用会话原出口（acquireSlot 已优先，但拿不到原出口时只能换，
	// 换了续接大概率失败，靠调用方回退到全量重发兜底）。
	if conv.turn == 0 {
		conv.proxyID = p.ID
		conv.proxyURL = proxyURL
		// 匿名首轮：没有登录 cookie，就地拿一份 session cookie 当会话载体。
		if conv.cookie == "" && !conv.isLogin {
			c, err := getAnonSession(proxyURL)
			if err != nil {
				return &StreamResult{ProxyID: p.ID, ProxyName: p.Name}, fmt.Errorf("建匿名会话失败: %w", err)
			}
			conv.cookie = c
		}
	}

	// 登录态每轮要带 at（XSRF）；匿名不要。
	xsrf := ""
	if conv.isLogin && conv.cookie != "" {
		if tok, err := getXSRF(conv.cookie, conv.authuser, proxyURL); err == nil {
			xsrf = tok
		} else {
			return &StreamResult{ProxyID: p.ID, ProxyName: p.Name, AccountID: conv.accountID},
				fmt.Errorf("取 XSRF 失败: %w", err)
		}
	}

	inner := make([]interface{}, innerSlots)
	inner[0] = []interface{}{prompt, 0, nil, nil, nil, nil, 0}
	inner[1] = []interface{}{"en"}
	if conv.turn == 0 {
		inner[2] = []interface{}{"", "", "", nil, nil, nil, nil, nil, nil, ""}
	} else {
		// 续接：[cid, rid, rcid(登录)/""(匿名), null×6, tok26]
		inner[2] = []interface{}{conv.cid, conv.rid, conv.rcid,
			nil, nil, nil, nil, nil, nil, conv.tok26}
	}
	inner[6] = []interface{}{0}
	inner[7] = 1
	inner[10] = 1
	inner[11] = 0
	inner[17] = []interface{}{[]interface{}{conv.turn}}
	inner[18] = 0
	inner[27] = 1
	inner[30] = []interface{}{4}
	inner[41] = []interface{}{1}
	inner[53] = 0
	reqUUID := uuid.NewString()
	inner[59] = reqUUID
	inner[61] = []interface{}{}
	inner[68] = 1
	inner[79] = mc.Mode
	tw := mc.thinkWire()
	inner[80] = tw
	inner[91] = 0
	inner[96] = 0
	if tw != thinkingNormal {
		inner[96] = 1
	}

	innerJSON, _ := json.Marshal(inner)
	outerJSON, _ := json.Marshal([]interface{}{nil, string(innerJSON)})

	buildBody := func(at string) string {
		form := url.Values{}
		form.Set("f.req", string(outerJSON))
		if at != "" {
			form.Set("at", at)
		}
		return form.Encode()
	}
	body := buildBody(xsrf)

	geminiHeaders := buildGeminiHeaders(conv.cookie, conv.sapisid, mc.HexID)
	setAuthUserHeaders(geminiHeaders, conv.authuser)
	geminiHeaders["x-goog-ext-525001261-jspb"] = buildModelHeader(mc.HexID, mc.Mode, mc.thinkWire(), uuid.NewString())
	geminiHeaders["x-goog-ext-525005358-jspb"] = fmt.Sprintf(`["%s",1]`, reqUUID)

	reqid := time.Now().UnixNano() % 1000000
	endpoint := fmt.Sprintf(
		"%s/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate?bl=%s&hl=en&_reqid=%d&rt=c",
		geminiBase(conv.authuser), currentBL(proxyURL), reqid)

	tracker := &deltaTracker{}
	rtracker := &deltaTracker{}
	strictCheck := strictLineCheck(mc, func() int64 { return conv.accountID })
	var lineCB func(string) error
	if onDelta != nil || onReasoning != nil || strictCheck != nil {
		lineCB = func(line string) error {
			if strictCheck != nil {
				if err := strictCheck(line); err != nil {
					return err
				}
			}
			// 出错话术在吐给客户端之前拦下，中断读取走重试（见 gemini.go 同一处）。
			if failureReplyInLine(line) {
				return errUpstreamFailureReply
			}
			if onReasoning != nil {
				if r := reasoningInLine(line); r != "" {
					if d := rtracker.Push(r); d != "" {
						onReasoning(d)
					}
				}
			}
			if onDelta == nil {
				return nil
			}
			for _, t := range renderedTextsInLine(line, false) {
				if d := tracker.Push(t); d != "" {
					onDelta(d)
				}
			}
			return nil
		}
	}

	t0 := time.Now()
	var lastErr error
	for attempt := 0; attempt < rtCfg().RetryAttempts; attempt++ {
		statusCode, raw, ttfb, setCookie, err := doGeminiRequest(endpoint, body, geminiHeaders, proxyURL, lineCB)
		if len(setCookie) > 0 && conv.cookie != "" {
			if merged := mergeSetCookie(conv.cookie, setCookie); merged != conv.cookie {
				conv.cookie = merged
			}
		}
		// 降级：同一个号重试还是降级，直接交给上层（见 gemini.go 同一处）。
		var mde *ModelDowngradedError
		if errors.As(err, &mde) {
			return &StreamResult{ProxyID: p.ID, ProxyName: p.Name, AccountID: conv.accountID,
				UpstreamModel: mde.Reported, TotalMs: time.Since(t0).Milliseconds(),
				Emitted: tracker.emitted, EmittedReasoning: rtracker.emitted}, err
		}
		if err != nil || statusCode != 200 || !hasContentFrame(string(raw)) {
			if err == nil && statusCode == 200 {
				lastErr = fmt.Errorf("续接无内容帧（会话可能已失效，raw %d 字节）", len(raw))
			} else if err != nil {
				lastErr = err
			} else {
				lastErr = fmt.Errorf("upstream HTTP %d: %s", statusCode, truncate(string(raw), 160))
			}
			// 出错话术跟出口无关，不记进代理健康度
			if pickedOK && !errors.Is(err, errUpstreamFailureReply) {
				recordProxyResult(p.ID, false, lastErr.Error())
			}
			if tracker.emitted != "" || rtracker.emitted != "" {
				break
			}
			if !retryAfterTimeout(err) {
				logf("上游超时，retry_on_timeout 关着，不再重试: %v", err)
				break
			}
			if attempt < rtCfg().RetryAttempts-1 {
				if errors.Is(err, errUpstreamFailureReply) {
					logf("retry %d/%d: %v", attempt+1, rtCfg().RetryAttempts, err)
				}
				time.Sleep(time.Duration(rtCfg().RetryDelaySec) * time.Second)
			}
			continue
		}
		// 非流式没有 lineCB，在这里补判出错话术。续接状态还没回填，重发仍接在上一轮后面。
		if hasFailureReply(string(raw)) {
			lastErr = errUpstreamFailureReply
			if tracker.emitted != "" || rtracker.emitted != "" {
				break
			}
			if attempt < rtCfg().RetryAttempts-1 {
				logf("retry %d/%d: %v", attempt+1, rtCfg().RetryAttempts, lastErr)
				time.Sleep(time.Duration(rtCfg().RetryDelaySec) * time.Second)
			}
			continue
		}
		// 成功：回填续接状态。
		cid, rid, rcid, tok26 := parseConvIDs(string(raw))
		if cid != "" {
			conv.cid = cid
		}
		if rid != "" {
			conv.rid = rid
		}
		if rcid != "" {
			conv.rcid = rcid
		}
		conv.tok26 = tok26 // 每轮更新（可能为空）
		conv.turn++
		if pickedOK {
			recordProxyResult(p.ID, true, "")
		}
		return &StreamResult{
			Emitted:          tracker.emitted,
			EmittedReasoning: rtracker.emitted,
			Raw:              string(raw),
			Reasoning:        extractReasoning(string(raw)),
			UpstreamModel:    extractUpstreamModel(string(raw)),
			ProxyID:          p.ID,
			ProxyName:        p.Name,
			AccountID:        conv.accountID,
			TTFBMs:           ttfb,
			TotalMs:          time.Since(t0).Milliseconds(),
		}, nil
	}
	return &StreamResult{ProxyID: p.ID, ProxyName: p.Name, AccountID: conv.accountID}, lastErr
}

// callGeminiConv 是多轮开启时的入口：按历史前缀识别续接。
//   - 命中已知历史前缀 → 发全部新增消息（user 或 tool 结果），历史留服务端。
//   - 没命中 → 新建会话，首轮发全量拼接的历史（含 tools 指令，把当前上下文交给服务端）。
//
// 支持 tools（续接里也解析 ```tool_call``` 围栏）；图片走原单轮路径（callGemini）。
func callGeminiConv(messages []map[string]interface{}, mc ModelConfig,
	tools []map[string]interface{}, toolChoice interface{},
	onDelta, onReasoning func(string), options ...conversationOptions) (string, []ToolCall, *StreamResult, error) {
	messages = normalizeConversationMessages(messages)
	var opt conversationOptions
	if len(options) > 0 {
		opt = options[0]
	}
	checked := newCheckedChatOutput(opt.checkRefusal, mc, onDelta, onReasoning)
	if checked != nil {
		if onDelta != nil {
			onDelta = func(s string) { checked.text.WriteString(s) }
		}
		if onReasoning != nil {
			onReasoning = func(s string) { checked.reasoning.WriteString(s) }
		}
	}
	scope := convScopeKey(mc, tools, toolChoice, opt.requestScope)
	conv, prefixLen := convTake(convPrefixKeys(messages, scope, opt.normalizeToolIDs))
	fresh := conv == nil

	var prompt string
	if fresh {
		// 首轮带上 tools 指令：模型要靠它知道怎么吐 tool_call 围栏，续接轮不再重发
		//（指令已在服务端首轮历史里）。
		prompt, _ = messagesToPrompt(messages, tools, toolChoice)
		logf("[conv] 新会话，首轮发 %d 字节（tools=%d）", len(prompt), len(tools))
	} else {
		prompt = buildPrompt(messages[prefixLen:], nil, nil)
		logf("[conv] 续接命中 turn=%d，只发 %d 字节（新增 %d 条消息，历史留服务端）", conv.turn, len(prompt), len(messages)-prefixLen)
	}

	// strict_model 换号只对新会话做：续接的会话绑死在原来的号上，降级了只记冷却、
	// 报错作废这路会话，客户端重试时当新会话全量重发，自然挑到别的号。
	exclude := map[int64]bool{}
	var res *StreamResult
	var err error
	for try := 0; try < maxCookieTries; try++ {
		if fresh {
			conv = &convState{}
			// 有 cookie 池就用登录号（能力更全），否则匿名（首轮就地拿 session cookie）。
			// #20 匿名优先：模型不需要登录态时不占号（多轮已被 gate 限成无图无工具，附件
			// 恒无，所以第二个参数传 false）。必须在调用前短路：挑号会更新
			// last_used_at，调了不用等于白占一轮。
			if !anonFirstEligible(mc, false) {
				a, _, perr := pickAccountForModel(mc, exclude)
				if perr != nil {
					var last *ModelDowngradedError
					if try > 0 && errors.As(err, &last) {
						// 换号时已经没号可挑：报上一个号真实的降级，补上解冻时间
						last.RetryAfter = perr.(*ModelDowngradedError).RetryAfter
						break
					}
					return "", nil, &StreamResult{}, perr
				}
				if a != nil {
					conv.cookie = a.Cookie
					conv.sapisid = extractSAPISID(a.Cookie)
					conv.isLogin = true
					conv.accountID = a.ID
					conv.authuser = a.AuthUser
					conv.proxyID = a.ProxyID
				}
			}
		}
		res, err = runConversationTurn(prompt, mc, conv, onDelta, onReasoning)
		if !retryOnAnotherAccount(res, err, mc, exclude) || !fresh {
			break
		}
	}
	if err != nil {
		// The old head was consumed before generation; an ambiguous failure must
		// not restore it because the upstream may already have advanced.
		return "", nil, res, err
	}
	text := extractResponseText(res.Raw)
	if opt.checkRefusal && mc.Tool == 0 {
		if err := rejectGeminiRefusal(text, res); err != nil {
			return "", nil, res, err
		}
	}
	var toolCalls []ToolCall
	if len(tools) > 0 {
		text, toolCalls = parseToolCalls(text)
	}
	if text == "" && len(toolCalls) == 0 {
		return "", nil, res, fmt.Errorf("upstream returned no content frame (raw %d bytes)", len(res.Raw))
	}
	// Save the exact logical assistant turn, including tool calls. Only the new
	// head is published; tool-only replies must remain distinguishable.
	keys := convPrefixKeys(conversationMessagesWithReply(messages, text, toolCalls), scope, opt.normalizeToolIDs)
	if len(keys) > 0 {
		convPut(keys[len(keys)-1], conv)
	}
	checked.accept()
	return text, toolCalls, res, nil
}

// runConversationTurn is the upstream seam used by continuation regression tests.
var runConversationTurn = streamGenerateConv
