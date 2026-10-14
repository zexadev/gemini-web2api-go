package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/google/uuid"
)

// Gemini 服务端认的模型 id，来自 batchexecute?rpcids=otAQ7b 返回的权威清单。
const (
	hexFlash36   = "fbb127bbb056c959" // 3.6 Flash
	hexFlashLite = "cf41b0e0dd7d53e5" // 3.5 Flash-Lite
	hexPro31     = "9d8ca3786ebdfbea" // 3.1 Pro
	// 3.7 Flash：按账号灰度放出。hex 是 otAQ7b 里 3.7 条目的**主 hex**（第一个元素），
	// 两个独立已灰度账号的清单里都是它、且有 3.7 号的用户实测发它回报 "3.7 Flash"
	// （issue #4 / PR #11）。注意别用 compat 列表里那个 797f3d0293f288ad —— 那是
	// "当前 Flash" 泛指针，老批次号发它拿到的是 3.6，会冒充 3.7。老批次号发这个主 hex
	// 会干净降级成 3.5 Flash-Lite（跟 3.1 Pro 一样），所以 gate 成要 cookie。
	hexFlash38 = "56fdd199312815e2" // 3.8 Flash（原记 3.7；Google 把这个 hex 原地升成 3.8，HAR 响应帧显示名已是 "3.8 Flash"）
	// 4 Argon（内部代号 Argon4p0，定位「复杂任务」）：hex 来自登录态账号的 otAQ7b
	// 清单捕获（issue #35），已实测：带 cookie 发它，服务端自报 "4 Argon"。
	// 匿名清单里这个条目**不带 hex**（brownfield_default_policy 里只有一个空 hex
	// 的能力条目），所以匿名发它拿不到 Argon，gate 成要 cookie。
	hexArgon4 = "95a4c0abcba24364" // 4 Argon
)

// innerSlots 是 payload 里 inner 数组的长度。浏览器发 97-98 槽，我们原来只开 80，
// 于是 inner[80]（扩展思考）连位置都没有、想填也填不进去。
//
// 加长本身是安全的：曾经把它从 80 加到 102 来试 inner[79] 会不会复活，结论是不会
// —— 模型选择完全由 x-goog-ext-525001261-jspb header 决定，长度不影响。
const innerSlots = 97

// inner[80] / 模型 header 下标 15 的取值：思考档位。
//
// **只在登录态生效**：匿名请求带上它服务端静默忽略，回报的仍是普通模型、思考链 0
// 字符（对齐登录态抓包参数复测 3 组，8 次全灭）。所以带这个的模型跟 3.1 Pro 一样，
// 没 cookie 时不暴露。
//
// 网页 UI（Argon 的「思考等级」子菜单，issue #35 捕获的模型清单元数据）是三档，
// 线值 1=低点（快速高效）、4=中等（深度适中）、2=高点（超级详尽）。1 和 2 是老值
// （扩展思考抓包验证过）；4 曾只见于清单元数据，现已实测：发 4 服务端自报
// "4 Argon Medium"、发 2 自报 "4 Argon High"，三档全部真实生效。只有高点吐思考链
// （低点/Medium 的 reasoning 长度为 0，思考在服务端内部完成）。
const (
	thinkingNormal   = 1 // 低点：所有模型的默认档
	thinkingExtended = 2 // 高点：网页 UI 的「扩展思考」，上游自报 "4 Argon High"
	thinkingMedium   = 4 // 中等：上游自报 "4 Argon Medium"，已实测生效
)

// ModelConfig holds the server-side model id plus the legacy MODE_CATEGORY value.
type ModelConfig struct {
	// HexID 走 x-goog-ext-525001261-jspb header，是服务端唯一认的模型开关。
	// 实测：不发这个 header 时 inner[79] 取 1..6 全部落到 3.5 Flash-Lite；
	// header 写 3.6 而 inner[79] 写 6 时拿到的是 3.6 —— header 压过 inner[79]。
	HexID string
	Mode  int
	Desc  string
	// ThinkLevel 是请求里显式带的思考档位（线值见 thinkingNormal 等常量）；
	// 0 表示不指定，按老行为发普通档（线值 1）。以前是个 Thinking bool，
	// Argon 把网页的思考等级从两态开成三档后才改成存线值。
	ThinkLevel int
	// Tool 非 0 时填 inner[49]，让服务端换后端模型生成媒体产物（14=生图 Nano Banana
	// / 21=音乐 Lyria）。产物不在 StreamGenerate 响应里，要再走 hNvQHb 拿下载链、
	// 用下载 host 认的 cookie 子集下回原始字节，见 media.go。跟登录态绑定：匿名请求
	// 这个会被静默降级成一句「Are you signed in?」文本。
	Tool int
}

// thinkWire 返回 inner[80] / 模型 header 下标 15 实际要发的线值。
func (mc ModelConfig) thinkWire() int {
	if mc.ThinkLevel == 0 {
		return thinkingNormal
	}
	return mc.ThinkLevel
}

// 只暴露服务端清单（batchexecute?rpcids=otAQ7b）里真实存在的模型。
// 旧的 gemini-3.5-flash / -thinking / -thinking-lite / gemini-auto /
// gemini-flash-lite 别名已移除：它们在服务端没有对应条目，留着只会让人
// 以为有五种不同的模型可选。
var Models = map[string]ModelConfig{
	"gemini-3.6-flash":      {HexID: hexFlash36, Mode: 1, Desc: "Latest all-around model"},
	"gemini-3.5-flash-lite": {HexID: hexFlashLite, Mode: 6, Desc: "Fastest, lightweight"},
	"gemini-3.1-pro":        {HexID: hexPro31, Mode: 3, Desc: "Most capable; needs a signed-in cookie (downgraded to Flash-Lite without one)"},
	// 3.8 Flash：Google 把原 3.7 那个 hex 原地升级了（同 hex，服务端显示名从 3.7 变 3.8）。
	// 实测是**付费号专属**（不是灰度）：只有付费 Gemini 账号有，免费号降级成 3.5 Flash-Lite。
	// gemini-3.7-flash 保留为别名（同 hex）。
	"gemini-3.8-flash": {HexID: hexFlash38, Mode: 1, Desc: "3.8 Flash; needs a signed-in PAID Google account (free accounts get downgraded to 3.5 Flash-Lite)"},
	"gemini-3.7-flash": {HexID: hexFlash38, Mode: 1, Desc: "alias of gemini-3.8-flash (same hex; Google renamed 3.7→3.8)"},

	// 4 Argon（issue #35）：定位「复杂任务」的新旗舰。Mode=8 取自服务端模型清单
	// brownfield_default_policy 条目的档位字段——同字段里 Flash-Lite=6 / Flash=1 /
	// Pro=3 与既有实测值逐个吻合，所以 8 可信（现已实测：服务端自报 "4 Argon"）。
	// 要登录态：匿名清单里这个条目不带 hex。网页 UI 给 Argon 配了三档思考等级
	// （线值 1=低 / 4=中 / 2=高），三档都已实测生效，服务端分别自报
	// "4 Argon" / "4 Argon Medium" / "4 Argon High"；只有高点吐思考链。
	"gemini-4-argon":                 {HexID: hexArgon4, Mode: 8, Desc: "4 Argon (complex tasks); needs a signed-in cookie (not granted anonymously)"},
	"gemini-4-argon-thinking-medium": {HexID: hexArgon4, Mode: 8, ThinkLevel: thinkingMedium, Desc: "4 Argon at the web UI's medium thinking level (reported upstream as \"4 Argon Medium\"); needs a signed-in cookie. Emits no reasoning chain"},

	// 扩展思考版。inner[80]=2 跟模型 hex 正交，都能开；但只在登录态生效，
	// 所以跟 3.1 Pro 一样在没 cookie 时不暴露。
	"gemini-3.6-flash-thinking":      {HexID: hexFlash36, Mode: 1, ThinkLevel: thinkingExtended, Desc: "3.6 Flash with extended thinking; needs a signed-in cookie"},
	"gemini-3.5-flash-lite-thinking": {HexID: hexFlashLite, Mode: 6, ThinkLevel: thinkingExtended, Desc: "3.5 Flash-Lite with extended thinking; needs a signed-in cookie"},
	"gemini-3.1-pro-thinking":        {HexID: hexPro31, Mode: 3, ThinkLevel: thinkingExtended, Desc: "3.1 Pro with extended thinking; needs a signed-in cookie"},
	"gemini-3.8-flash-thinking":      {HexID: hexFlash38, Mode: 1, ThinkLevel: thinkingExtended, Desc: "3.8 Flash with extended thinking; needs a signed-in cookie"},
	"gemini-3.7-flash-thinking":      {HexID: hexFlash38, Mode: 1, ThinkLevel: thinkingExtended, Desc: "alias of gemini-3.8-flash-thinking"},
	"gemini-4-argon-thinking":        {HexID: hexArgon4, Mode: 8, ThinkLevel: thinkingExtended, Desc: "4 Argon at the web UI's high thinking level (reported upstream as \"4 Argon High\"); needs a signed-in cookie"},

	// 媒体生成。inner[49] 一填，服务端换后端模型出图/出乐；产物走 hNvQHb + 下载 host
	// 取回，以 base64 data URL 塞进 content 返回。都要登录态，没 cookie 时不暴露。
	"gemini-image": {HexID: hexFlash36, Mode: 1, Tool: toolImage, Desc: "Image generation (Nano Banana); returns a base64 data URL; needs a signed-in cookie"},
	"gemini-music": {HexID: hexFlash36, Mode: 1, Tool: toolMusic, Desc: "Music generation (Lyria, ~30s); returns a base64 data URL; needs a signed-in cookie"},
	"gemini-video": {HexID: hexFlash36, Mode: 1, Tool: toolVideo, Desc: "Video generation (Veo, async); returns a base64 data URL; needs a signed-in cookie (usually a paid account)"},
	// 画布：生成 immersive 交互 HTML 文档，内联返回（不是二进制、不用下载）。要登录态。
	"gemini-canvas": {HexID: hexFlash36, Mode: 1, Tool: toolCanvas, Desc: "Canvas: generates an interactive HTML document (returned inline as a ```html block); needs a signed-in cookie"},
}

// hasCookie 表示 cookie 池里有没有可用账号。决定 3.1 Pro 是否出现在模型列表里。
func hasCookie() bool {
	_, enabled := accountCount()
	return enabled > 0
}

// modelNeedsLogin 判断一个模型是否必须登录态才真正生效。匿名请求这批会被服务端
// 静默降级：3.1 Pro / 3.8 Flash → 3.5 Flash-Lite；4 Argon 的匿名清单条目干脆不带
// hex；思考链消失；媒体工具变成一句 "Are you signed in?" 文本。是「没 cookie 时
// 排除哪些模型」和「#20 匿名优先要不要占一个 cookie 账号」共用的单一判据。
//
// 按 HexID + ThinkLevel + Tool 判，跟旧的按模型名（3.1-pro/3.8-flash/3.7-flash）判
// 逐模型核对等价：那三个正名的 HexID 就是 hexPro31 / hexFlash38，其余登录模型都被
// ThinkLevel 或 Tool 覆盖。
func modelNeedsLogin(mc ModelConfig) bool {
	return mc.HexID == hexPro31 || mc.HexID == hexFlash38 || mc.HexID == hexArgon4 || mc.ThinkLevel > 0 || mc.Tool > 0
}

// anonFirstEligible 判断「匿名优先」开关下这次请求能不能走匿名（不占 cookie 账号）：
// 开关开着 + 没带附件 + 模型不需要登录态。附件（图/视频）在对话里引用必须登录
// （匿名会被上游回 1100），所以带附件时一律挑号。开关关着永远返回 false（保持旧行为：
// 池里有号就用号）。
func anonFirstEligible(mc ModelConfig, hasAttachment bool) bool {
	return rtCfg().AnonFirst && !hasAttachment && !modelNeedsLogin(mc)
}

// temporaryChatEligible 判断这次请求该不该走网页端原生「临时对话」（inner[45]=1，#34）。
//
// 与 multi_turn 冲突：临时对话不进会话列表，而 multi_turn 要按历史认出续接、靠服务端
// 留着这串对话——两个一起开，续接那半就没了。所以 multi_turn 开着时一律不发 inner[45]，
// 由 multi_turn 决定会话怎么留。调用点还会再要求有登录 cookie：匿名本来就不落账号，
// 发 inner[45] 是没验过的无谓偏离。
func temporaryChatEligible() bool {
	return rtCfg().TemporaryChat && !rtCfg().MultiTurn
}

// availableModels 返回当前配置下值得暴露的模型。
//
// 没配 cookie 时排除 3.1 Pro：实测匿名请求它会被静默降级成 3.5 Flash-Lite，
// 客户端还以为自己用上了 Pro。与其让它"成功"，不如直接不提供、让选型时就报错。
//
// 配了有效 cookie 时它是真能用的：连打 6 次服务端回报的都是 "3.1 Pro" 本身，
// 且每次都带思考链（118-152 字符，普通 3.6 Flash 为 0）。早前记录的「免费号
// 登录也只能拿到 3.6 Flash 扩展」是在缺 XSRF token 的条件下测的，那时候带
// cookie 的请求根本发不出去（见 xsrf.go）。
func availableModels() map[string]ModelConfig {
	if hasCookie() {
		return Models
	}
	out := make(map[string]ModelConfig, len(Models))
	for k, v := range Models {
		if modelNeedsLogin(v) {
			continue
		}
		out[k] = v
	}
	return out
}

// resolveModel maps a model name to its config.
//
// "name@think=N" 后缀会被剥掉并忽略。旧版本把它写进 inner[17] 当思考深度，
// 那是误读：抓包显示 inner[17] 是会话内的轮次索引（首轮 [[0]]，带会话 id 的
// 第二轮 [[1]]，逐轮递增），跟思考深度无关。我们每次都开新会话，该值恒为 0。
// 后缀不报错只忽略，避免打断已经配了这个写法的客户端。
func resolveModel(modelName string) (string, ModelConfig, error) {
	if idx := strings.Index(modelName, "@think="); idx >= 0 {
		modelName = modelName[:idx]
	}
	mc, ok := availableModels()[modelName]
	if !ok {
		if full, exists := Models[modelName]; exists && !hasCookie() {
			downgrade := "are silently downgraded to 3.5 Flash-Lite"
			if full.Tool > 0 {
				downgrade = "are silently downgraded to a plain \"Are you signed in?\" text reply"
			}
			return "", ModelConfig{}, fmt.Errorf(
				"%s is unavailable without a Google account cookie: anonymous requests for it "+
					"%s. Add a cookie in the admin panel (Cookie pool) or via --cookie-file to enable it",
				modelName, downgrade)
		}
		return "", ModelConfig{}, fmt.Errorf("unknown model: %s", modelName)
	}
	return modelName, mc, nil
}

// StreamResult holds raw body + per-request proxy + timing info.
type StreamResult struct {
	// Emitted 是流式模式下已经通过 onDelta 发出去的文本；非流式为空。
	Emitted string
	Raw     string
	// Reasoning 是模型的思考链（只有 3.1 Pro 会产出）。
	// 上游每次都发，我们以前只取正文、把它扔了。
	Reasoning string
	// EmittedReasoning 是流式下已经通过 onReasoning 发出去的思考链；非流式为空。
	EmittedReasoning string
	// UpstreamModel 是服务端在响应帧 [42] 里自报的模型显示名。
	// 跟请求的模型未必一致：gemini-3.1-pro 匿名时被静默降级成 3.5 Flash-Lite，
	// 只看请求名根本发现不了，所以这个字段要一直记着。
	UpstreamModel string
	ProxyID       int64
	ProxyName     string
	// 用了 cookie 池里的哪个账号，0 = 匿名。失败的请求也要带上——
	// 排查"加了 cookie 就大面积失败"时，最需要知道的正是失败那条用的哪个号。
	AccountID    int64
	AccountLabel string
	TTFBMs       int64
	TotalMs      int64
	// Artifacts 是媒体模型（生图/音乐）取回的产物原始字节，非媒体模型为空。
	Artifacts []MediaArtifact
	// MediaErr 记媒体产物取回失败的原因：生成本身 200 了、但走 hNvQHb / 下载那步挂了。
	// 调用方据此报错，而不是返回一个只有文字没有图的「半成功」。
	MediaErr string
}

// RateLimitError 表示所有 IP slot 都达到了限流上限。
// HTTP handler 看到这个错时返回 429 给客户端。
type RateLimitError struct {
	Reason  string // "concurrent" / "rpm" / "rph"
	ProxyID int64  // 0 = 直连 slot 满
}

func (e *RateLimitError) Error() string {
	if e.ProxyID == 0 {
		return "direct IP slot full: " + e.Reason + " limit reached (configure proxies to scale)"
	}
	return "all proxy slots full: " + e.Reason + " limit reached"
}

// acquireSlot 选一个有容量的 slot 给本次请求用。
// 优先级：代理池里有容量的代理 → 直连。
// 全满返回 *RateLimitError。
//
// 调用方拿到 (proxy, ok=true) 必须配 deferred releaseSlot()。
func acquireSlot(preferProxyID int64) (Proxy, bool, error) {
	// 1. 先试代理池（如果配了）
	proxyMu.RLock()
	hasProxies := len(proxyCache) > 0
	proxyMu.RUnlock()

	if hasProxies {
		if p, ok := pickProxyPreferring(preferProxyID); ok {
			return p, true, nil
		}
		// 代理池里有代理但一个都用不上（限流满 / 全禁用 / 全熔断且没过冷却）。
		// 默认**不退回直连** —— 配了代理池就意味着不想让上游看到本机 IP，
		// 悄悄直连等于把这个前提废掉，而且日志上只是几条普通请求，很难发现。
		// 要可用性优先于隐藏 IP 的部署可以打开 fallback_direct。
		if !rtCfg().FallbackDirect {
			return Proxy{}, false, &RateLimitError{Reason: "rph", ProxyID: -1}
		}
		if ok, reason := trySlotAcquire(0); ok {
			logf("[proxy] 代理池无可用出口，本次退回直连（fallback_direct 已开）")
			return Proxy{}, true, nil
		} else {
			return Proxy{}, false, &RateLimitError{Reason: reason, ProxyID: 0}
		}
	}

	// 2. 没配代理池 → 用直连 slot（id=0）
	if ok, reason := trySlotAcquire(0); ok {
		return Proxy{}, true, nil // ProxyID=0 表示直连
	} else {
		return Proxy{}, false, &RateLimitError{Reason: reason, ProxyID: 0}
	}
}

// 一个请求最多试几个 cookie。池子大时挨个试到底会让失败请求拖很久，
// 而连试 3 个都不行基本说明是池子整体的问题，不是撞上个别坏号。
const maxCookieTries = 3

// releaseSlot 释放占用。proxyID=0 表示直连。
func releaseSlot(proxyID int64) {
	slotRelease(proxyID)
}

// deltaTracker 把上游的累积帧转成增量。
//
// 上游每帧带的是**到目前为止的全文**，不是新增部分，所以要跟已发出的做前缀
// 比对。帧之间偶尔不满足前缀关系（模型改写、或 clean 掉的 artifact 落在边界
// 上），这时宁可跳过也不能发——发了就等于把重复内容推给客户端，而已发出的
// 内容收不回来。漏掉的部分由调用方在结束时用 remainingText 补齐。
type deltaTracker struct{ emitted string }

// Push 吃进一帧的累积全文，返回相对上一次的增量；没有新增或无法安全 diff 时返回 ""。
func (d *deltaTracker) Push(fullText string) string {
	cleaned := cleanGeminiText(fullText)
	if len(cleaned) <= len(d.emitted) || !strings.HasPrefix(cleaned, d.emitted) {
		return ""
	}
	delta := cleaned[len(d.emitted):]
	d.emitted = cleaned
	return delta
}

// streamGenerate POSTs to Gemini's StreamGenerate endpoint and returns raw body
// plus proxy/timing telemetry for the metrics layer.
// The 80-slot inner array is verbatim from the Python reference.
// onDelta 非 nil 时开启真流式：上游每写一帧就解析一次，跟已发出的内容做前缀
// diff，把新增部分立刻回调出去。上游每帧带的是累积全文而不是增量，diff 必须
// 自己做。一旦已经吐过内容就不再重试——重试会让客户端收到重复文本。
func streamGenerate(prompt, latest string, mc ModelConfig,
	onDelta, onReasoning func(string)) (*StreamResult, error) {
	return streamGenerateWithFiles(prompt, latest, mc, nil, onDelta, onReasoning)
}

// fileRef 是一个已上传附件的引用。
type fileRef struct {
	Ref  string // 上传返回的路径，形如 /contrib_service/ttl_1d/…
	Name string // 展示给模型看的文件名
	Kind int    // 附件类型：1=图片，3=文本/普通文件
	Mime string // 内容类型，服务端按它决定怎么解析附件
}

// streamGenerateWithFiles 同上，但可以带附件。
//
// 附件填 inner[0][3]，形状 [[[ref, 1], "文件名"], …]。附件只在登录态可用：
// 匿名能把文件传上去，但对话里一引用就被服务端回 1100。
func streamGenerateWithFiles(prompt, latest string, mc ModelConfig, pending []pendingUpload,
	onDelta, onReasoning func(string)) (*StreamResult, error) {
	var files []fileRef

	// 挑号 + 挑出口。粘绑的动机：一个号在几十个出口之间跳，在 Google 眼里就是账号
	// 共享的特征。两个策略分岔见 acquireExitForAccount：
	//
	//   account_first=false（默认，可用性优先）：绑定的出口满载就让这个号漂到别的出口。
	//   account_first=true（换号不换出口）：绑定的出口到了限流线就让这个号歇着、换下一个
	//   号走它自己绑的出口 —— Google 眼里永远「一号一个固定 IP」，不出现一个号多个 IP。
	//
	// 挑号排在占 slot 之前不违反「取 XSRF 必须走正式出口」：挑号只读库、不发请求，
	// 真正发请求的是下面的 getXSRF，它在拿到 slot 之后。
	// #20 匿名优先：不需要登录态能力时不占用 cookie 账号，走匿名省额度（见 anonFirstEligible）。
	// 带附件（图/视频）时不能走匿名——下面 uploadBytes 那段会因 cookieStr=="" 直接报错。
	//
	// cookie 取 XSRF 失败时换下一个号：account_first 下连它绑的出口一起换（每个号
	// 固有自己的出口），旧行为则沿用已占住的出口。
	var picked Proxy
	slotHeld := false
	releaseHeld := func() {
		if slotHeld {
			releaseSlot(picked.ID)
			slotHeld = false
		}
	}
	defer releaseHeld() // 兜底：任何 return 路径都不漏还 slot
	var cookieID int64
	var cookieLabel string
	attrib := func(err error) (*StreamResult, error) {
		return &StreamResult{
			AccountID: cookieID, AccountLabel: cookieLabel,
			ProxyID: picked.ID, ProxyName: picked.Name,
		}, err
	}

	accountFirst := rtCfg().AccountFirst
	anon := anonFirstEligible(mc, len(pending) > 0)
	cookieStr, sapisid, xsrfToken := "", "", ""
	var lastCookieErr error
	tried := map[int64]bool{}
	// 每个号只给一次「轮转后重试」的机会，避免在一个请求里反复打 accounts.google.com
	rotatedOnce := map[int64]bool{}
	sawAccount := false
	quotaSkipped := 0 // 因高级配额受限被绕开的号数
	expensive := 0    // 真发过 XSRF 网络请求的候选号数；满载换号是纯内存操作，不计入
	for {
		var acct *CookieAccount
		if !anon {
			// 配额感知路由：登录态模型的挑号绕开配额受限的号（Flash-Lite 等
			// 基础模型不受影响）。受限号继续服务只会产出静默降级的回答。
			skipSet, blockedN := quotaSkipSet(mc)
			quotaSkipped = blockedN
			skip := mergeSkip(tried, skipSet)
			acct, _ = pickCookieAccountExcept(skip)
			if acct == nil {
				break
			}
			tried[acct.ID] = true
			sawAccount = true
			// 归属先记上：这一轮失败了也留痕，面板上看得出是哪个号在坏
			cookieID, cookieLabel = acct.ID, accountDisplayName(acct)
			p, ok, needsBind, serr := acquireExitForAccount(acct, accountFirst)
			if !ok {
				if serr != nil {
					return attrib(serr)
				}
				// 绑定的出口到线了（account_first）：没发过网络请求，换下一个号很便宜
				continue
			}
			if expensive >= maxCookieTries {
				// 已经对 maxCookieTries 个号做过 XSRF 都失败，再换号大概率同样失败
				releaseHeld()
				break
			}
			expensive++
			picked, slotHeld = p, true
			if needsBind {
				bindAccountProxy(acct.ID, picked.ID)
				acct.ProxyID = picked.ID
			}
		} else {
			p, ok, aerr := acquireSlot(0)
			if !ok {
				return attrib(aerr)
			}
			picked, slotHeld = p, true
			break
		}

		proxyURL := picked.URL
		// 取 XSRF token。一个 cookie 失效不该让整个请求失败：当前号取不到就换下一个，
		// 最多试 maxCookieTries 个。不这么做的话，池子里 2 个号坏 1 个就会让大约一半
		// 请求挂掉，而每次失败看起来都像上游的问题，用户只会觉得"成功率莫名很低"。
		cookieID, cookieLabel = acct.ID, accountDisplayName(acct)
		tok, err := getXSRF(acct.Cookie, proxyURL)
		if err == nil {
			cookieStr, sapisid, xsrfToken = acct.Cookie, extractSAPISID(acct.Cookie), tok
			break
		}
		// 取不到 SNlM0e 基本等于这个 cookie 已失效（页面把我们当匿名用户了）。
		// 换号之前先给它一次机会：强制轮转一次再重取。
		//
		// 轮转会换发 __Secure-1PSIDTS（约 30 分钟过期的那张票）并合并 *SIDCC。
		// 只是票过期、持久身份还在的号，这一步能救回来；救不回来再换号。
		// 只试一次，且只在这一轮。
		if !rotatedOnce[acct.ID] {
			rotatedOnce[acct.ID] = true
			if _, _, rerr := rotateAccount(*acct); rerr == nil {
				if fresh := accountByID(acct.ID); fresh != nil {
					if tok2, err2 := getXSRF(fresh.Cookie, proxyURL); err2 == nil {
						logf("[cookie] 账号 #%d 轮转后恢复可用", acct.ID)
						cookieStr, sapisid, xsrfToken = fresh.Cookie, extractSAPISID(fresh.Cookie), tok2
						if fresh.ProxyID != picked.ID {
							bindAccountProxy(fresh.ID, picked.ID)
						}
						break
					}
				}
			}
		}
		// 救不回来：换下一个号。account_first 下连出口一起换（下一个号用它自己绑的
		// 出口），slot 当场还回去。
		//
		// 定罪要分清责任方：「页面能打开但没有登录态 / 被重定向到登录页」才算 cookie
		// 的错；网络层失败（出口抖动、CDN 偶发 h2 帧、超时）记到**出口的熔断健康度**
		// 上、不动 cookie —— 否则一个坏出口一次抖动就把钉在它上面的号标红，account_first
		// 下号换不掉出口，会反复背锅（markCookieByStatus 只认 401/403，传 0 即不记）。
		msg := err.Error()
		if strings.Contains(msg, "no SNlM0e") || strings.Contains(msg, "HTTP 302") {
			markCookieByStatus(acct.ID, 401, msg)
		} else if picked.ID > 0 {
			recordProxyResult(picked.ID, false, msg)
		}
		lastCookieErr = err
		logf("[cookie] 账号 #%d 不可用，换下一个：%v", acct.ID, err)
		releaseHeld()
	}

	// 所有账号的高级配额都在受限期（配额感知路由）：明确报错而不是匿名服务——
	// 匿名发登录态模型必然被静默降级，客户端拿到的是「看起来成功」的劣化回答。
	if !slotHeld && sawAccount == false && quotaSkipped > 0 {
		return attrib(fmt.Errorf(
			"全部 %d 个账号的高级配额都在受限期（滚动窗口，最迟 %v 后自动恢复）；"+
				"本次请求已取消，避免拿到静默降级的 Flash-Lite 回答", quotaSkipped, quotaBlockDur))
	}

	// 所有 enabled 账号绑定的出口都在限流线上（account_first）：要么按 fallback_direct
	// 退直连，要么明确 429 —— 绝不静默漂移，也不静默降级成匿名（那会让登录态模型
	// 被静默降级，客户端完全看不出来）。
	if !slotHeld {
		if sawAccount {
			if rtCfg().FallbackDirect && func() bool { ok, _ := trySlotAcquire(0); return ok }() {
				logf("[proxy] 所有账号绑定的出口都在限流线上，本次退回直连（fallback_direct 已开）")
				picked, slotHeld = Proxy{}, true
			} else {
				return attrib(&RateLimitError{Reason: "rph", ProxyID: -1})
			}
		} else {
			// 池里一个号都没有：跟旧行为一致，走匿名（无 cookie）
			p, ok, aerr := acquireSlot(0)
			if !ok {
				return attrib(aerr)
			}
			picked, slotHeld = p, true
		}
	}

	// picked.URL 为空 = 直连 slot。代理只有代理池一个入口，没有别的兜底出口了
	// （原来那个「静态代理」字段已并进池子，见 seedProxiesFromConfig）。
	proxyURL := picked.URL
	pickedOK := picked.ID > 0 // 是否真用了代理池里的代理

	// endpoint 要等出口定下来才能拼：currentBL 可能顺手踢一次后台抓取，
	// 那个抓取必须跟正式请求走同一个出口，否则配了代理池也会从本机 IP 漏一次。
	reqid := time.Now().Unix() % 1000000
	endpoint := fmt.Sprintf(
		"https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate?bl=%s&hl=en&_reqid=%d&rt=c",
		currentBL(proxyURL), reqid,
	)
	if lastCookieErr != nil && cookieStr == "" {
		if !rtCfg().FallbackAnon {
			// 默认报错而不是降级：cookie 失效后上游不会拒绝，只是把你当匿名用户，
			// 纯文本请求照样 200 —— 于是 3.1 Pro 被静默降级成 3.5 Flash-Lite、
			// 思考链消失，客户端完全看不出来。宁可明确失败也不给假的成功。
			return attrib(fmt.Errorf("cookie 池里 %d 个账号都不可用（最后一个：%w）；"+
				"到面板「Cookie 池」用「检测」按钮逐个排查，或打开 fallback_anon 降级匿名",
				len(tried), lastCookieErr))
		}
		logf("[cookie] 试过的 %d 个账号都不可用，本次降级匿名（能力会退化到匿名档）", len(tried))
		cookieID, cookieLabel = 0, ""
	}
	// 图片附件：上传要 cookie，而且必须走跟正式请求同一个出口，所以排在这里。
	if len(pending) > 0 {
		if cookieStr == "" {
			return attrib(fmt.Errorf("image input needs a Google account cookie: " +
				"anonymous uploads succeed but referencing them in a conversation is " +
				"rejected upstream. Add a cookie in the admin panel (Cookie pool)"))
		}
		nImg, nVid := 0, 0
		for _, u := range pending {
			ref, uerr := uploadBytes(cookieStr, proxyURL, u.Data, u.Name)
			if uerr != nil {
				return attrib(fmt.Errorf("上传附件 %s 失败: %w", u.Name, uerr))
			}
			files = append(files, fileRef{Ref: ref, Name: u.Name, Kind: u.Kind, Mime: u.Mime})
			if u.Kind == 2 {
				nVid++
			} else {
				nImg++
			}
		}
		logf("[vision] 上传了 %d 张图 / %d 个视频", nImg, nVid)
	}

	// prompt 超长时转成文本附件。要等挑完号和出口才能做：上传要 cookie，
	// 而且必须走跟正式请求同一个出口。
	budget := rtCfg().MaxPromptBytes
	if p, f, used, ferr := prepareContextFile(prompt, latest, budget, cookieStr, proxyURL); ferr != nil {
		return attrib(ferr)
	} else if used {
		prompt = p
		files = append(files, f...)
	}
	if budget > 0 && len(prompt) > budget {
		return attrib(&PromptTooLongError{
			Bytes: len(prompt), Budget: budget, HasCookie: cookieStr != "",
		})
	}

	inner := make([]interface{}, innerSlots)
	if len(files) > 0 {
		// 形状逐字取自浏览器抓包：
		//   [[[路径, 类型, null, mime], "文件名", null×6, [0]], …]
		// 类型位是 1=图片 / 3=文本文件 —— 拿 1 传文本文件等于告诉服务端"这是张图"。
		refs := make([]interface{}, 0, len(files))
		for _, f := range files {
			kind := f.Kind
			if kind == 0 {
				kind = 3
			}
			mime := f.Mime
			if mime == "" {
				mime = "text/plain"
			}
			refs = append(refs, []interface{}{
				[]interface{}{f.Ref, kind, nil, mime}, f.Name,
				nil, nil, nil, nil, nil, nil,
				[]interface{}{0},
			})
		}
		inner[0] = []interface{}{prompt, 0, nil, refs, nil, nil, 0}
	} else {
		inner[0] = []interface{}{prompt, 0, nil, nil, nil, nil, 0}
	}
	inner[1] = []interface{}{"en"}
	inner[2] = []interface{}{"", "", "", nil, nil, nil, nil, nil, nil, ""}
	inner[6] = []interface{}{0}
	inner[7] = 1
	inner[10] = 1
	inner[11] = 0
	// 会话内轮次索引；我们每次都是新会话，恒为 0。
	inner[17] = []interface{}{[]interface{}{0}}
	inner[18] = 0
	inner[27] = 1
	inner[30] = []interface{}{4}
	// 抓包里浏览器三种场景（有 cookie / 无 cookie / 扩展思考）全是 [1]。
	// 我们原来写 [2]，是早期抄来的值、协议层已被证伪。含义仍未知，
	// 匿名两个值都能通，但没有理由继续偏离浏览器。
	inner[41] = []interface{}{1}
	// 临时对话（#34）：inner[45]=1 让这条不进账号的会话列表、不入训练。只在有登录
	// cookie 时发——匿名本就不落账号，发了也没意义且没验过。multi_turn 开着时
	// temporaryChatEligible 直接为假（两者冲突，见那里）。
	if temporaryChatEligible() && cookieStr != "" {
		inner[45] = 1
	}
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
	// 媒体工具开关。填了服务端就换后端模型出图/出乐（响应里带产物引用，字节要另取）。
	if mc.Tool > 0 {
		inner[49] = mc.Tool
	}
	// 视频还要在 inner[55] 指定画幅比例：[[16]]=16:9，[[17]]=9:16（抓包）。
	if mc.Tool == toolVideo {
		inner[55] = []interface{}{[]interface{}{16}}
	}

	innerJSON, err := json.Marshal(inner)
	if err != nil {
		return nil, err
	}
	outer := []interface{}{nil, string(innerJSON)}
	outerJSON, err := json.Marshal(outer)
	if err != nil {
		return nil, err
	}

	// 带 cookie 时必须多发一个表单字段 at（XSRF token），否则上游直接 400。
	// 匿名请求不需要，getXSRF 对空 cookie 返回空串。见 xsrf.go。
	buildBody := func(at string) string {
		form := url.Values{}
		form.Set("f.req", string(outerJSON))
		if at != "" {
			form.Set("at", at)
		}
		return form.Encode()
	}

	body := buildBody(xsrfToken)

	// 抓包里 header 下标 16 和 inner[59] 是两个不同的 uuid，各生成各的。
	modelHeader := buildModelHeader(mc.HexID, mc.Mode, mc.thinkWire(), uuid.NewString())
	sessionHeader := fmt.Sprintf(`["%s",1]`, reqUUID)

	geminiHeaders := buildGeminiHeaders(cookieStr, sapisid, mc.HexID)
	geminiHeaders["x-goog-ext-525001261-jspb"] = modelHeader
	geminiHeaders["x-goog-ext-525005358-jspb"] = sessionHeader
	var lastErr error
	// 最后一次拿到的 HTTP 状态码，0 表示网络层就失败了没拿到响应。
	// cookie 健康度只认 401/403，别的状态不算 cookie 的错，见 markCookieByStatus。
	lastStatus := 0
	xsrfRetried := false // XSRF 自愈只做一次，避免死循环
	t0 := time.Now()

	tracker := &deltaTracker{}
	rtracker := &deltaTracker{}
	var lineCB func(string)
	if onDelta != nil || onReasoning != nil {
		lineCB = func(line string) {
			// 思考链先推完才轮到正文，所以先处理它，客户端拿到的顺序才对。
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

	for attempt := 0; attempt < rtCfg().RetryAttempts; attempt++ {
		statusCode, raw, ttfb, setCookie, err := doGeminiRequest(endpoint, body, geminiHeaders, proxyURL, lineCB)
		if len(setCookie) > 0 && cookieID > 0 {
			if merged := mergeSetCookie(cookieStr, setCookie); merged != cookieStr {
				cookieStr = merged
				updateAccountCookie(cookieID, merged)
			}
		}
		if err != nil {
			lastErr = err
			if pickedOK {
				recordProxyResult(picked.ID, false, err.Error())
			}
			// 已经往客户端吐过内容就不能重试，否则会重复（思考链也算吐过）。
			if tracker.emitted != "" || rtracker.emitted != "" {
				break
			}
			if attempt < rtCfg().RetryAttempts-1 {
				logf("retry %d/%d: %v", attempt+1, rtCfg().RetryAttempts, err)
				time.Sleep(time.Duration(rtCfg().RetryDelaySec) * time.Second)
			}
			continue
		}
		if statusCode != 200 {
			// token 过期时上游回 400 + xsrf。作废缓存重取一次再打，
			// 这种自愈不算进重试预算，否则一次过期就吃掉全部重试。
			if statusCode == 400 && isXSRFError(string(raw)) && cookieStr != "" && !xsrfRetried {
				xsrfRetried = true
				invalidateXSRF(cookieStr)
				if tok, e := getXSRF(cookieStr, proxyURL); e == nil {
					body = buildBody(tok)
					geminiHeaders = buildGeminiHeaders(cookieStr, sapisid, mc.HexID)
					geminiHeaders["x-goog-ext-525001261-jspb"] = modelHeader
					geminiHeaders["x-goog-ext-525005358-jspb"] = sessionHeader
					attempt--
					continue
				}
			}
			lastErr = fmt.Errorf("upstream HTTP %d: %s", statusCode, truncate(string(raw), 200))
			lastStatus = statusCode
			if pickedOK {
				recordProxyResult(picked.ID, false, lastErr.Error())
			}
			if tracker.emitted != "" || rtracker.emitted != "" {
				break
			}
			if attempt < rtCfg().RetryAttempts-1 {
				time.Sleep(time.Duration(rtCfg().RetryDelaySec) * time.Second)
			}
			continue
		}
		// HTTP 200 但一个内容帧都没有 —— 上游的瞬时拒绝（响应里那个 1155）。
		// 它不是限流：干净 IP 间隔 1s 连打 15 次全过、同 IP 并发 10 共 18 次全过、
		// 打了 60+ 次的 IP 之后照样成功，没有可预测阈值，同样的请求有时成功有时失败。
		// 重发一次通常就好，所以必须纳入重试——不然一次抖动就变成客户端可见的 502。
		//
		// 判据是**有没有内容帧**，不是 BardErrorInfo：正常响应的结束帧里也带错误码
		// （1096 = 会话未持久化），拿它判错会把每个正常响应都判成失败。
		if !hasContentFrame(string(raw)) {
			lastErr = fmt.Errorf("upstream returned no content frame (raw %d bytes)", len(raw))
			if pickedOK {
				// 记进代理健康度：1155 跟出口质量强相关（干净出口 60+ 次 0 发生，
				// 脏出口一天约 9 次），连续踩中说明这个出口该歇了。
				recordProxyResult(picked.ID, false, lastErr.Error())
			}
			if tracker.emitted != "" || rtracker.emitted != "" {
				break
			}
			if attempt < rtCfg().RetryAttempts-1 {
				logf("retry %d/%d: 空响应（无内容帧，%d 字节）", attempt+1, rtCfg().RetryAttempts, len(raw))
				time.Sleep(time.Duration(rtCfg().RetryDelaySec) * time.Second)
			}
			continue
		}
		if pickedOK {
			recordProxyResult(picked.ID, true, "")
		}
		markCookieByStatus(cookieID, 200, "")
		result := &StreamResult{
			Emitted:          tracker.emitted,
			EmittedReasoning: rtracker.emitted,
			Raw:              string(raw),
			Reasoning:        extractReasoning(string(raw)),
			UpstreamModel:    extractUpstreamModel(string(raw)),
			ProxyID:          picked.ID,
			ProxyName:        picked.Name,
			AccountID:        cookieID,
			AccountLabel:     cookieLabel,
			TTFBMs:           ttfb,
			TotalMs:          time.Since(t0).Milliseconds(),
		}
		// 配额感知路由：登录态模型的响应被自报成了别家模型 = 静默降级。
		// 给该号打受限标记，还没吐过字就换号重试；已吐字（真流式）或没有可换的号
		// 时按降级结果如实返回——upstream_model 字段让客户端看得见。
		if downgraded(mc, result) {
			markQuotaBlocked(cookieID)
			if tracker.emitted == "" && rtracker.emitted == "" &&
				enabledAccountCount() > quotaBlockedCount() {
				logf("[quota] 账号 #%d 被静默降级（上游自报 %s），换号重试",
					cookieID, result.UpstreamModel)
				return streamGenerateWithFiles(prompt, latest, mc, pending, onDelta, onReasoning)
			}
			logf("[quota] 账号 #%d 被静默降级（上游自报 %s），无可换的号或已吐字，按降级结果返回",
				cookieID, result.UpstreamModel)
		}

		// 媒体模型：生成的产物字节不在这条响应里，要用同一套 cookie / 出口再走一遍
		// hNvQHb + 下载 host 取回。取不到就记 MediaErr，让上层报错而不是返回半成品。
		if mc.Tool == toolImage || mc.Tool == toolMusic || mc.Tool == toolVideo {
			mime := "image/png"
			switch mc.Tool {
			case toolMusic:
				mime = "audio/mpeg"
			case toolVideo:
				mime = "video/mp4"
			}
			arts, aerr := fetchMediaArtifacts(
				mc.Tool, string(raw), extractConversationID(string(raw)),
				cookieStr, sapisid, xsrfToken, proxyURL, mime)
			if aerr != nil {
				logf("[media] 取回产物失败: %v", aerr)
				result.MediaErr = aerr.Error()
			} else {
				result.Artifacts = arts
				logf("[media] 取回 %d 份产物", len(arts))
			}
		}
		// #19 自动删会话：出完结果把 gemini.google.com 上留下的这条会话删掉，避免
		// 用户账号里堆一堆。只登录态能删（要 XSRF），异步 best-effort，不影响响应。
		//
		// 临时对话（#34）生效时跳过：那条压根没进会话列表，删它是多发一次 RPC 删空气。
		// 这正是临时对话比 create-then-delete 省的那一次出站请求。
		if rtCfg().AutoDeleteConversation && !temporaryChatEligible() && cookieStr != "" && xsrfToken != "" {
			if cid := extractConversationID(string(raw)); cid != "" {
				go deleteConversation(cid, cookieStr, sapisid, xsrfToken, proxyURL)
			}
		}
		return result, nil
	}
	if lastErr != nil {
		markCookieByStatus(cookieID, lastStatus, lastErr.Error())
	}
	return attrib(lastErr)
}

// upstreamModelRe 匹配响应帧里服务端自报的模型显示名（帧的 [42] 位）。
// 形如 ...,"fbb127bbb056c959",null,null,"3.6 Flash",true,...
var upstreamModelRe = regexp.MustCompile(`\\"[0-9a-f]{16}\\",null,null,\\"([^"\\]{1,40})\\"`)

// extractUpstreamModel 取服务端实际使用的模型名，取不到返回空串。
func extractUpstreamModel(raw string) string {
	m := upstreamModelRe.FindAllStringSubmatch(raw, -1)
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1][1]
}

// buildModelHeader 拼 x-goog-ext-525001261-jspb，形状逐槽取自抓包：
//
//	[1,null,null,null,"<hex>",null,null,0,[4,5,6,8],null,null,1,null,null,<mode>,<think>,"<uuid>"]
//	下标                4                8                          14      15       16
//
// 下标 14 跟 inner[79] 同值、下标 15 跟 inner[80] 同值 —— 模型和思考模式在 header 和
// payload 里各存一份。**服务端认的是 header**：只填 inner[80]=2 而 header 留最小形式，
// 三个模型实测思考链全是 0 字符；这跟模型选择本身"header 压过 inner[79]"是同一个规律。
//
// 下标 16 是**另一个** uuid，跟 inner[59] 不是一个值 —— 跟 inner[59] 同值的是
// x-goog-ext-525005358-jspb。两份抓包都是这个规律，别图省事复用同一个。
//
// uuid 留空时退回最小形式（匿名路径不需要这些槽位，少发一截更省事）。
func buildModelHeader(hexID string, mode, think int, uuid string) string {
	if uuid == "" {
		return fmt.Sprintf(`[1,null,null,null,"%s"]`, hexID)
	}
	return fmt.Sprintf(
		`[1,null,null,null,"%s",null,null,0,[4,5,6,8],null,null,1,null,null,%d,%d,"%s"]`,
		hexID, mode, think, uuid)
}

// buildGeminiHeaders 准备 StreamGenerate 必需的应用层 header。
// hexID 决定服务端用哪个模型；留空则服务端一律回落到 3.5 Flash-Lite。
func buildGeminiHeaders(cookieStr, sapisid, hexID string) map[string]string {
	h := map[string]string{
		"Accept":          "*/*",
		"Accept-Language": "en-US,en;q=0.9",
		"Content-Type":    "application/x-www-form-urlencoded;charset=UTF-8",
		"Origin":          "https://gemini.google.com",
		"Referer":         "https://gemini.google.com/app",
		"X-Same-Domain":   "1",
		"X-Goog-AuthUser": "0",
		// 这两个浏览器每次都发，值是固定的。
		"x-goog-ext-73010989-jspb": "[0]",
		"x-goog-ext-73010990-jspb": "[0,0,0]",
	}
	if hexID != "" {
		h["x-goog-ext-525001261-jspb"] = buildModelHeader(hexID, 0, 0, "")
	}
	if cookieStr != "" {
		h["Cookie"] = cookieStr
	}
	if sapisid != "" {
		h["Authorization"] = makeSAPISIDHash(sapisid)
	}
	return h
}

// doGeminiRequest 发一次请求到 endpoint。proxyURL 非空走 stdlib（支持 socks5/http），
// 空走 tls-client（chrome146 真指纹）。返回 (HTTP status, body bytes, err)。
// 返回值多了 setCookie：服务端几乎每个响应都在刷新 SIDCC / __Secure-1PSIDCC /
// __Secure-3PSIDCC，浏览器收下再带回去。一直发旧值的客户端会被判定为过期会话，
// 实测号活一两小时就失效 —— 所以这些必须收下来并写回账号。
func doGeminiRequest(endpoint, body string, headers map[string]string, proxyURL string,
	onLine func(string)) (int, []byte, int64, []string, error) {
	sendAt := time.Now()
	if proxyURL != "" {
		// 走 stdlib 的 http.ProxyURL，已知能过 socks5/socks5h。
		req, err := http.NewRequest("POST", endpoint, strings.NewReader(body))
		if err != nil {
			return 0, nil, 0, nil, err
		}
		applyChromeHeaders(req)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		client := getStdlibClient(proxyURL)
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, 0, nil, err
		}
		defer resp.Body.Close()
		raw, ttfb, err := readBody(resp.Body, onLine, sendAt)
		if err != nil {
			return resp.StatusCode, nil, ttfb, resp.Header.Values("Set-Cookie"), err
		}
		return resp.StatusCode, raw, ttfb, resp.Header.Values("Set-Cookie"), nil
	}

	// 直连 → tls-client，保留 chrome146 TLS/HTTP2 真指纹
	req, err := fhttp.NewRequest("POST", endpoint, strings.NewReader(body))
	if err != nil {
		return 0, nil, 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := getTLSClient()
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, 0, nil, err
	}
	defer resp.Body.Close()
	raw, ttfb, err := readBody(resp.Body, onLine, sendAt)
	if err != nil {
		return resp.StatusCode, nil, ttfb, resp.Header.Values("Set-Cookie"), err
	}
	return resp.StatusCode, raw, ttfb, resp.Header.Values("Set-Cookie"), nil
}

// readBody 读完整个响应体并原样返回；onLine 非 nil 时每读到一行就回调一次，
// 让上层能在上游还没写完时就往客户端转发。
//
// 始终走逐行扫描（而不是 onLine==nil 时图省事用 io.ReadAll），因为要拿到
// **第一行到达的时刻**当 TTFB。用 ReadAll 的话读完才返回，测出来的"首字节
// 耗时"实际是完整耗时，跟总耗时永远一样。
//
// start 必须是**请求发出前**的时刻，由调用方传入。放在本函数里取 time.Now()
// 是不对的：那时 client.Do 已经返回、响应头甚至部分 body 都到了，测出来恒为 0。
func readBody(r io.Reader, onLine func(string), start time.Time) ([]byte, int64, error) {
	var buf bytes.Buffer
	sc := bufio.NewScanner(io.TeeReader(r, &buf))
	// 单帧可能很大（实测见过 40 万字节的响应），默认 64KB 上限不够。
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var ttfb int64 = -1
	for sc.Scan() {
		if ttfb < 0 {
			ttfb = time.Since(start).Milliseconds()
		}
		if onLine != nil {
			onLine(sc.Text())
		}
	}
	if ttfb < 0 {
		ttfb = time.Since(start).Milliseconds()
	}
	if err := sc.Err(); err != nil {
		return buf.Bytes(), ttfb, err
	}
	return buf.Bytes(), ttfb, nil
}

// reasoningInLine 从单个 wrb.fr 行里取出思考链。
//
// 位置是 inner[4][0][37][0][0]，比正文（inner[4][0][1][0]）深两层。
// 只有 3.1 Pro 会产出，3.6 Flash / Flash-Lite 恒为空。
//
// 时序（实测一次过河谜题）：思考链在头 5 帧里累积到 660 字符，此时正文还是空；
// 从第 6 帧起正文开始增长而思考链冻结不再变。两者都是**累积全文**不是增量，
// 所以流式侧可以直接复用 deltaTracker 的前缀 diff。
func reasoningInLine(line string) string {
	if !strings.Contains(line, `"wrb.fr"`) || len(line) < 200 {
		return ""
	}
	var arr []interface{}
	if err := json.Unmarshal([]byte(line), &arr); err != nil || len(arr) == 0 {
		return ""
	}
	first, ok := arr[0].([]interface{})
	if !ok || len(first) < 3 {
		return ""
	}
	innerStr, ok := first[2].(string)
	if !ok || len(innerStr) < 50 {
		return ""
	}
	var inner []interface{}
	if err := json.Unmarshal([]byte(innerStr), &inner); err != nil || len(inner) <= 4 {
		return ""
	}
	parts, ok := inner[4].([]interface{})
	if !ok || len(parts) == 0 {
		return ""
	}
	p0, ok := parts[0].([]interface{})
	if !ok || len(p0) <= 37 {
		return ""
	}
	lvl1, ok := p0[37].([]interface{})
	if !ok || len(lvl1) == 0 {
		return ""
	}
	lvl2, ok := lvl1[0].([]interface{})
	if !ok || len(lvl2) == 0 {
		return ""
	}
	s, _ := lvl2[0].(string)
	return s
}

// extractReasoning 取整个响应里最长的那段思考链。
// 取最长而不是最后一个：思考链冻结后，后续帧该位置可能缺失或被截短。
//
// 必须跟 extractResponseText 一样过 cleanGeminiText：流式侧 deltaTracker 推的是
// 清洗过的文本，这里不清洗的话两者前缀对不上，收尾补发时会把整段思考链重发一遍
// （实测块顺序变成 R→C→R）。
func extractReasoning(raw string) string {
	best := ""
	for _, line := range strings.Split(raw, "\n") {
		if r := reasoningInLine(line); len(r) > len(best) {
			best = r
		}
	}
	return cleanGeminiText(best)
}

// textsInLine 从单个 wrb.fr 行里取出候选回复文本。
// 上游每帧带的是**累积全文**而不是增量，所以流式转发时要自己做前缀 diff。
func textsInLine(line string) []string {
	if !strings.Contains(line, `"wrb.fr"`) || len(line) < 200 {
		return nil
	}
	var arr []interface{}
	if err := json.Unmarshal([]byte(line), &arr); err != nil || len(arr) == 0 {
		return nil
	}
	first, ok := arr[0].([]interface{})
	if !ok || len(first) < 3 {
		return nil
	}
	innerStr, ok := first[2].(string)
	if !ok || len(innerStr) < 50 {
		return nil
	}
	var inner []interface{}
	if err := json.Unmarshal([]byte(innerStr), &inner); err != nil || len(inner) <= 4 {
		return nil
	}
	parts, ok := inner[4].([]interface{})
	if !ok {
		return nil
	}
	var texts []string
	for _, p := range parts {
		pl, ok := p.([]interface{})
		if !ok || len(pl) < 2 {
			continue
		}
		tl, ok := pl[1].([]interface{})
		if !ok {
			continue
		}
		for _, t := range tl {
			if s, ok := t.(string); ok && s != "" {
				texts = append(texts, s)
			}
		}
	}
	return texts
}

// hasContentFrame 判断响应里到底有没有内容帧。
//
// 故意不复用 extractResponseText：那个会先 cleanGeminiText 掉代码产物，一个纯代码
// 产物的回复在它眼里是空的，但那明明是上游正常出了内容。这里只问"有没有帧"，
// 判断的是链路成没成功，不是内容合不合用。
func hasContentFrame(raw string) bool {
	for _, line := range strings.Split(raw, "\n") {
		if len(textsInLine(line)) > 0 || reasoningInLine(line) != "" {
			return true
		}
	}
	return false
}

// extractResponseText parses StreamGenerate's wrb.fr stream and returns the
// last non-empty text chunk (matches Python extract_response_text behavior).
func extractResponseText(raw string) string {
	var texts []string
	for _, line := range strings.Split(raw, "\n") {
		texts = append(texts, textsInLine(line)...)
	}
	for i := len(texts) - 1; i >= 0; i-- {
		if strings.TrimSpace(texts[i]) != "" {
			cleaned := cleanGeminiText(texts[i])
			if cleaned == "" {
				// 整条回复都是代码执行产物（问数学/算式时模型直接跑代码），清洗后被剥空。
				// 别返回空——那样 callGemini 会当"无内容帧"报 502，用户看到的是调用失败。
				// 回退：只把 ?code_reference/stdout 标记去掉、保留代码和结果，当普通代码块给。
				cleaned = strings.TrimSpace(codeMarkerRe.ReplaceAllString(texts[i], "```$1"))
			}
			return cleaned
		}
	}
	return ""
}

var codeArtifactRe = regexp.MustCompile("(?s)```(?:python|javascript|text)\\?code_(?:reference|stdout)&code_event_index=\\d+\\n.*?```\\n?")

// codeMarkerRe 只匹配代码产物的**开围栏标记**（```python?code_reference&code_event_index=N），
// 用于清洗后为空时的回退：把标记降级成普通 ```python，保留代码/结果不整条清空。
var codeMarkerRe = regexp.MustCompile("```(python|javascript|text)\\?code_(?:reference|stdout)&code_event_index=\\d+")

func cleanGeminiText(text string) string {
	return strings.TrimSpace(codeArtifactRe.ReplaceAllString(text, ""))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ProbeResult 是 admin 测试接口返回的连通性诊断结果。
type ProbeResult struct {
	OK           bool   `json:"ok"`
	Status       string `json:"status"` // "success" / "blocked_sorry" / "rate_limited" / "upstream_error" / "network_error"
	HTTPCode     int    `json:"http_code"`
	TotalMs      int64  `json:"total_ms"`
	ProxyID      int64  `json:"proxy_id"`
	ProxyName    string `json:"proxy_name"`
	UseDirect    bool   `json:"use_direct"`
	ResponseText string `json:"response_text"` // 截断到 200 字符
	UpstreamSnip string `json:"upstream_snip"` // 上游原始响应前 300 字符
	Diagnostic   string `json:"diagnostic"`    // 中文诊断说明
	Impersonate  string `json:"impersonate"`
}

// probeGemini 直接调 Gemini StreamGenerate（绕过限流），返回详细诊断。
// 不写 db、不消耗限流 slot。
func probeGemini(prompt, proxyURL string) ProbeResult {
	res := ProbeResult{Impersonate: rtCfg().Impersonate}

	inner := make([]interface{}, innerSlots)
	inner[0] = []interface{}{prompt, 0, nil, nil, nil, nil, 0}
	inner[1] = []interface{}{"en"}
	inner[2] = []interface{}{"", "", "", nil, nil, nil, nil, nil, nil, ""}
	inner[6] = []interface{}{0}
	inner[7] = 1
	inner[10] = 1
	inner[11] = 0
	inner[17] = []interface{}{[]interface{}{0}}
	inner[18] = 0
	inner[27] = 1
	inner[30] = []interface{}{4}
	// 抓包里浏览器三种场景（有 cookie / 无 cookie / 扩展思考）全是 [1]。
	// 我们原来写 [2]，是早期抄来的值、协议层已被证伪。含义仍未知，
	// 匿名两个值都能通，但没有理由继续偏离浏览器。
	inner[41] = []interface{}{1}
	inner[53] = 0
	inner[59] = uuid.NewString()
	inner[61] = []interface{}{}
	inner[68] = 1
	probeModel := Models["gemini-3.6-flash"]
	inner[79] = probeModel.Mode

	innerJSON, _ := json.Marshal(inner)
	outer := []interface{}{nil, string(innerJSON)}
	outerJSON, _ := json.Marshal(outer)
	form := url.Values{}
	form.Set("f.req", string(outerJSON))
	body := form.Encode()

	reqid := time.Now().Unix() % 1000000
	endpoint := fmt.Sprintf(
		"https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate?bl=%s&hl=en&_reqid=%d&rt=c",
		currentBL(proxyURL), reqid,
	)

	// probe 是旁路探测，不回写 cookie 健康度：它的失败原因跟 cookie 无关。
	// 但 at 必须带——否则挂了 cookie 之后连通性探测会一直报 400，假报故障。
	cookieStr, sapisid := loadCookie()
	if tok, e := getXSRF(cookieStr, proxyURL); e == nil && tok != "" {
		form.Set("at", tok)
		body = form.Encode()
	}
	headers := buildGeminiHeaders(cookieStr, sapisid, probeModel.HexID)

	// 复用主流程同款 client 选择规则:有代理走 stdlib，没代理走 tls-client。
	// 但 probe 需要看 302 的 Location header,所以这里直接发不用 doGeminiRequest。
	var statusCode int
	var raw []byte
	var locHeader string
	var err error

	if proxyURL != "" {
		req, e := http.NewRequest("POST", endpoint, strings.NewReader(body))
		if e != nil {
			res.Status = "network_error"
			res.Diagnostic = "构建请求失败: " + e.Error()
			return res
		}
		applyChromeHeaders(req)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		client := getStdlibClient(proxyURL)
		resp, e := client.Do(req)
		if e != nil {
			res.Status = "network_error"
			res.Diagnostic = "网络层错误（DNS/TCP/TLS 失败）: " + e.Error()
			return res
		}
		defer resp.Body.Close()
		statusCode = resp.StatusCode
		locHeader = resp.Header.Get("Location")
		raw, err = io.ReadAll(resp.Body)
	} else {
		req, e := fhttp.NewRequest("POST", endpoint, strings.NewReader(body))
		if e != nil {
			res.Status = "network_error"
			res.Diagnostic = "构建请求失败: " + e.Error()
			return res
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		client := getTLSClient()
		resp, e := client.Do(req)
		if e != nil {
			res.Status = "network_error"
			res.Diagnostic = "网络层错误（DNS/TCP/TLS 失败）: " + e.Error()
			return res
		}
		defer resp.Body.Close()
		statusCode = resp.StatusCode
		locHeader = resp.Header.Get("Location")
		raw, err = io.ReadAll(resp.Body)
	}
	_ = err
	res.HTTPCode = statusCode
	res.UpstreamSnip = truncate(string(raw), 300)

	switch {
	case statusCode == 302:
		res.Status = "blocked_sorry"
		res.Diagnostic = "IP 被 Google 风控（重定向到 sorry/index）。" +
			"通常 6-24 小时解除，或换 VPN/代理 IP 立即恢复。Location: " + truncate(locHeader, 200)
		return res
	case statusCode == 429:
		res.Status = "rate_limited"
		res.Diagnostic = "Google 直接返回 429 限流。同样是 IP 嫌疑，但风控分支不同（朴素 SDK 路径）。"
		return res
	case statusCode != 200:
		res.Status = "upstream_error"
		res.Diagnostic = fmt.Sprintf("上游返回非 200 (HTTP %d)，可能是协议变更或临时故障。", statusCode)
		return res
	}

	text := extractResponseText(string(raw))
	if text == "" {
		res.Status = "upstream_error"
		res.Diagnostic = "上游 200 但只回了结束帧、没有内容帧，通常是请求被服务端拒绝" +
			"（例如带了不被接受的会话 id 或工具开关），不是帧格式变更。"
		return res
	}

	res.OK = true
	res.Status = "success"
	res.ResponseText = truncate(text, 200)
	res.Diagnostic = "调用成功。延迟 / 内容见上面字段。"
	return res
}
