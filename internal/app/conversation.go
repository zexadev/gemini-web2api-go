package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// convDelete 作废一路会话（续接失败时用，避免客户端重试还撞同一路死会话）。
func convDelete(key string) {
	if key == "" {
		return
	}
	convMu.Lock()
	delete(convStore, key)
	convMu.Unlock()
}

// convGet 取一路会话，顺带清掉过期的。
func convGet(key string) *convState {
	if key == "" {
		return nil
	}
	convMu.Lock()
	defer convMu.Unlock()
	st, ok := convStore[key]
	if !ok {
		return nil
	}
	if time.Since(st.updated) > convTTL {
		delete(convStore, key)
		return nil
	}
	return st
}

// convPut 存一路会话，超量就按最旧的淘汰一批。
func convPut(key string, st *convState) {
	if key == "" {
		return
	}
	convMu.Lock()
	defer convMu.Unlock()
	st.updated = time.Now()
	convStore[key] = st
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

// canonicalMessages 把消息列表压成 role+text 的稳定串，用来算会话指纹。
// 只取 role 和文本内容 —— 客户端每轮重发同样的历史，压出来的串一致才能识别续接。
func canonicalMessages(messages []map[string]interface{}) string {
	var b strings.Builder
	for _, m := range messages {
		b.WriteString(getStr(m, "role"))
		b.WriteByte(0x1f)
		b.WriteString(contentToString(m["content"]))
		b.WriteByte(0x1e)
	}
	return b.String()
}

func hashStr(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// convParentKey 是"这轮之前的历史"的指纹：除最后一条消息外的全部。
// 命中 store 说明这是某路已知会话的延续，只要发最后一条新消息即可。
func convParentKey(messages []map[string]interface{}) string {
	if len(messages) < 2 {
		return ""
	}
	return hashStr(canonicalMessages(messages[:len(messages)-1]))
}

// convChildKey 是"这轮之后的历史"的指纹：历史 + 本轮模型回复。
// 下一轮请求的 parentKey 会正好等于它（客户端把我们的回复原样带回来），从而续上。
func convChildKey(messages []map[string]interface{}, responseText string) string {
	full := append(append([]map[string]interface{}{}, messages...),
		map[string]interface{}{"role": "assistant", "content": responseText})
	return hashStr(canonicalMessages(full))
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
		r.Header.Set("User-Agent", webUA)
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

// webUA 是匿名 GET /app 的 User-Agent（tls-client 路径要手动带）。
const webUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

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
		if tok, err := getXSRF(conv.cookie, proxyURL); err == nil {
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
	inner[80] = mc.thinkWire()
	inner[91] = 0
	inner[96] = 0
	if mc.ThinkLevel > 0 {
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
	geminiHeaders["x-goog-ext-525001261-jspb"] = buildModelHeader(mc.HexID, mc.Mode, mc.thinkWire(), uuid.NewString())
	geminiHeaders["x-goog-ext-525005358-jspb"] = fmt.Sprintf(`["%s",1]`, reqUUID)

	reqid := time.Now().UnixNano() % 1000000
	endpoint := fmt.Sprintf(
		"https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate?bl=%s&hl=en&_reqid=%d&rt=c",
		currentBL(proxyURL), reqid)

	tracker := &deltaTracker{}
	rtracker := &deltaTracker{}
	var lineCB func(string)
	if onDelta != nil || onReasoning != nil {
		lineCB = func(line string) {
			if onReasoning != nil {
				if r := reasoningInLine(line); r != "" {
					if d := rtracker.Push(r); d != "" {
						onReasoning(d)
					}
				}
			}
			if onDelta == nil {
				return
			}
			for _, t := range textsInLine(line) {
				if d := tracker.Push(t); d != "" {
					onDelta(d)
				}
			}
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
		if err != nil || statusCode != 200 || !hasContentFrame(string(raw)) {
			if err == nil && statusCode == 200 {
				lastErr = fmt.Errorf("续接无内容帧（会话可能已失效，raw %d 字节）", len(raw))
			} else if err != nil {
				lastErr = err
			} else {
				lastErr = fmt.Errorf("upstream HTTP %d: %s", statusCode, truncate(string(raw), 160))
			}
			if pickedOK {
				recordProxyResult(p.ID, false, lastErr.Error())
			}
			if tracker.emitted != "" || rtracker.emitted != "" {
				break
			}
			if attempt < rtCfg().RetryAttempts-1 {
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
//   - 命中已知会话 → 只发最后一条新消息（user 或 tool 结果），历史留服务端（绕开字节墙）。
//   - 没命中 → 新建会话，首轮发全量拼接的历史（含 tools 指令，把当前上下文交给服务端）。
//
// 支持 tools（续接里也解析 ```tool_call``` 围栏）；图片走原单轮路径（callGemini）。
func callGeminiConv(messages []map[string]interface{}, mc ModelConfig,
	tools []map[string]interface{}, toolChoice interface{},
	onDelta, onReasoning func(string)) (string, []ToolCall, *StreamResult, error) {
	parentKey := convParentKey(messages)
	conv := convGet(parentKey)
	fresh := conv == nil

	var prompt string
	var res *StreamResult
	var convErr error
	if fresh {
		// 挑号 + 死 cookie 换号：Google 对无效 cookie **不报错、只当匿名用户**，
		// 拿死号建会话 = 整条会话被静默降级（登录态模型变 Flash-Lite、思考链消失、
		// 工具围栏失灵——agent 场景里表现就是「4 Argon 突然变成聊天模式」）。
		// 所以首轮必须先证明 cookie 活着：XSRF 阶段判死就换下一个号，最多
		// maxCookieTries 个。#20 匿名优先：模型不需要登录态时不占号（多轮已被
		// gate 限成无图无工具，附件恒无，所以第二个参数传 false）。必须在调用前
		// 短路：pickCookieAccount 会更新 last_used_at，调了不用等于白占一轮。
		var lastCookieErr string
		tried := map[int64]bool{}
		emitted := false
		servedDowngraded := false
		wrapDelta := func(d string) {
			emitted = true
			if onDelta != nil {
				onDelta(d)
			}
		}
		skipSet, blockedN := quotaSkipSet(mc)
		for attempt := 0; attempt < maxCookieTries; attempt++ {
			var a *CookieAccount
			skip := mergeSkip(tried, skipSet)
			a, _ = pickCookieAccountExcept(skip)
			if a == nil {
				if attempt == 0 && blockedN > 0 {
					// 全部账号的高级配额都在受限期：明确报错而不是匿名服务——
					// 匿名发登录态模型必然被静默降级，agent 拿到的是「看起来成功」
					// 的劣化回答。
					return "", nil, nil, fmt.Errorf(
						"全部 %d 个账号的高级配额都在受限期（滚动窗口，最迟 %v 后自动恢复）；"+
							"本次请求已取消，避免拿到静默降级的 Flash-Lite 回答", blockedN, quotaBlockDur)
				}
				break
			}
			tried[a.ID] = true
			conv = &convState{}
			conv.cookie = a.Cookie
			conv.sapisid = extractSAPISID(a.Cookie)
			conv.isLogin = true
			conv.accountID = a.ID
			conv.proxyID = a.ProxyID
			// 首轮带上 tools 指令：模型要靠它知道怎么吐 tool_call 围栏，续接轮不再重发
			//（指令已在服务端首轮历史里）。
			prompt, _ = messagesToPrompt(messages, tools, toolChoice)
			logf("[conv] 新会话，账号 #%d，首轮发 %d 字节（tools=%d）", a.ID, len(prompt), len(tools))
			res, convErr = streamGenerateConv(prompt, mc, conv, wrapDelta, onReasoning)
			if convErr == nil {
				// 配额感知路由：登录态模型被自报成别家模型 = 静默降级。还没吐过字
				// 就换下一个号重建会话；已吐字只能如实返回（upstream_model 可见）。
				if downgraded(mc, res) {
					markQuotaBlocked(a.ID)
					if emitted {
						logf("[quota] 账号 #%d 被静默降级（上游自报 %s），已吐字，按降级结果返回",
							a.ID, res.UpstreamModel)
						servedDowngraded = true
						break
					}
					logf("[quota] 账号 #%d 被静默降级（上游自报 %s），换下一个号建会话",
						a.ID, res.UpstreamModel)
					continue
				}
				break
			}
			msg := convErr.Error()
			if !strings.Contains(msg, "no SNlM0e") && !strings.Contains(msg, "HTTP 302") {
				break // 不是 cookie 的错（出口/限流等），换号救不了，按原错误返回
			}
			if emitted {
				break // 已经往客户端吐过字，换号重发会重复，只能如实报错
			}
			markCookieByStatus(a.ID, 401, msg)
			lastCookieErr = msg
			logf("[conv] 账号 #%d cookie 已死，换下一个建会话：%v", a.ID, msg)
		}
		if servedDowngraded {
			// 所有候选号都被降级过：按最后一次的降级结果返回（upstream_model 可见）。
			// 时机上是正确的降级兜底——明确失败 vs 劣化成功之间，这里选劣化成功，
			// 因为多轮会话的上下文已经在服务端，客户端重试也改变不了配额现实。
			logf("[quota] %d 个候选账号均被静默降级，按最后一次降级结果返回", len(tried))
		}
		if convErr != nil {
			// 最后一个号死于 cookie 之外的错误：如实返回（res 可能为 nil）
			return "", nil, res, convErr
		}
		if res == nil {
			if lastCookieErr != "" {
				return "", nil, nil, fmt.Errorf("cookie 池里 %d 个账号都不可用（最后一个：%v）；"+
					"到面板「Cookie 池」重新导一个登录态", len(tried), lastCookieErr)
			}
			// 池子里一个号都没有：走匿名（首轮就地拿 session cookie）
			conv = &convState{}
			prompt, _ = messagesToPrompt(messages, tools, toolChoice)
			logf("[conv] 新会话，匿名，首轮发 %d 字节（tools=%d）", len(prompt), len(tools))
			res, convErr = streamGenerateConv(prompt, mc, conv, onDelta, onReasoning)
			if convErr != nil {
				return "", nil, res, convErr
			}
		}
	} else {
		prompt = formatNewTurn(messages[len(messages)-1])
		logf("[conv] 续接命中 turn=%d，只发 %d 字节（历史留服务端）", conv.turn, len(prompt))
		res, convErr = streamGenerateConv(prompt, mc, conv, onDelta, onReasoning)
		if convErr != nil {
			// 续接失败：这路会话作废，客户端重试时会当新会话全量重发。
			convDelete(parentKey)
			return "", nil, res, convErr
		}
	}
	text := extractResponseText(res.Raw)
	var toolCalls []ToolCall
	if len(tools) > 0 {
		text, toolCalls = parseToolCalls(text)
	}
	if text == "" && len(toolCalls) == 0 {
		if !fresh {
			convDelete(parentKey)
		}
		return "", nil, res, fmt.Errorf("upstream returned no content frame (raw %d bytes)", len(res.Raw))
	}
	// 存续接状态：客户端下一轮把这段回复原样带回来时，parentKey 会命中这里。
	// 存的是解析掉围栏后的正文 —— 客户端把 assistant 消息带回来时 content 也是这段。
	convPut(convChildKey(messages, text), conv)
	return text, toolCalls, res, nil
}

// formatNewTurn 把续接要发的那条新消息格式化，跟 messagesToPrompt 里各角色的写法对齐，
// 好让服务端历史里的模型认得出这是工具结果还是用户新话。
func formatNewTurn(msg map[string]interface{}) string {
	content := contentToString(msg["content"])
	if getStr(msg, "role") == "tool" {
		return fmt.Sprintf("[Tool result for %s]: %s", getStr(msg, "name"), content)
	}
	return content
}
