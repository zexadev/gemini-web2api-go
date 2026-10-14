package app

import (
	"encoding/json"
	"fmt"
	"sync"
)

// RuntimeConfig 是面板里能随时改、改完立刻生效的那部分配置。
//
// 部署期配置（监听地址、DB 路径、admin token、API key、cookie 文件）**不在这里**：
// 那些改了本来就要重启进程，放 docker-compose.yml 的 environment / command 更合适，
// 也避免把凭证放进一个网页表单。这里只放调优参数——为了改个超时重启一次服务太蠢。
//
// 取值优先级：面板改过的（存 kv 表） > config.json / CLI flag > 内置默认。
//
// 加字段必读：面板保存走整体反序列化（admin.go 的 Decode(&RuntimeConfig)），而前端
// saveRtCfg 只按 admin_ui 的 RT_GROUPS 拼 PUT body。新加的字段必须同步进 RT_GROUPS，
// 否则它不在 body 里、每次点「保存并生效」都被解成零值冲掉（multi_turn 就这么被静默
// 重置过，连 config.json 里设的都白设，见 #27）。
type RuntimeConfig struct {
	RetryAttempts   int    `json:"retry_attempts"`
	RetryDelaySec   int    `json:"retry_delay_sec"`
	RequestTimeout  int    `json:"request_timeout_sec"`
	DefaultModel    string `json:"default_model"`
	PerIPConcurrent int    `json:"per_ip_concurrent"`
	PerIPRPM        int    `json:"per_ip_rpm"`
	PerIPRPH        int    `json:"per_ip_rph"`
	RetentionDays   int    `json:"retention_days"`
	LogRequests     bool   `json:"log_requests"`
	Impersonate     string `json:"impersonate"`
	GeminiBL        string `json:"gemini_bl"`
	// ProxyCooldownMin 是代理连续失败熔断后隔多久放回池子，单位分钟。
	// 0 = 不恢复（熔断即永久除名，要手动重置）。默认按实测的封禁恢复时长取 120。
	ProxyCooldownMin int `json:"proxy_cooldown_min"`
	// FallbackDirect 决定代理池一个出口都用不上时是退回直连还是直接 429。
	// 默认 false：配了代理池就意味着不想暴露本机 IP，悄悄直连会把这个前提废掉。
	FallbackDirect bool `json:"fallback_direct"`
	// FallbackAnon 决定 cookie 失效时是降级成匿名继续跑还是直接报错。
	// 默认 false：匿名档拿不到 3.1 Pro / 扩展思考 / 生图，降级了客户端也看不出来。
	FallbackAnon bool `json:"fallback_anon"`
	// GeminiBLAuto 决定是否定期从 /app 页面抓最新的 bl 版本号覆盖上面钉死的值。
	GeminiBLAuto bool `json:"gemini_bl_auto"`
	// MaxPromptBytes 是单次请求 prompt 的 UTF-8 字节上限，超了直接报错。
	// 单位是字节不是 token：实测上游的墙按字节走，跟语言无关，见 messages.go。
	// 0 = 关掉检查（原样发出，由上游从尾部静默截断）。
	MaxPromptBytes int `json:"max_prompt_bytes"`
	// MultiTurn 开启后走 Gemini 原生 conversation_id 服务端多轮：按历史前缀识别续接，
	// 命中就只发最新一句、历史留服务端，绕开单请求字节墙。匿名/登录都行，见 conversation.go。
	// 默认 false（保持现有"每轮拼全量 prompt"行为）。实测多轮不放大上下文窗口，只让长
	// 对话不撞单请求墙——对 Codex 这类长会话有用，对"喂长文档"没用。
	MultiTurn bool `json:"multi_turn"`
	// 出完结果自动删掉 gemini.google.com 上的这条会话（#19）。只登录态生效。默认 false。
	AutoDeleteConversation bool `json:"auto_delete_conversation"`
	// 匿名优先（#20）：不需要登录态能力的请求（纯文本、非思考、无工具、无图）走匿名、
	// 不占 cookie 账号，省账号额度；需要登录才挑号。默认 false，见 modelNeedsLogin。
	AnonFirst bool `json:"anon_first"`
	// 临时对话（#34）：塞 inner[45]=1，走网页端原生「临时对话」——不进会话列表、不入训练，
	// 取代 auto_delete_conversation（省掉删除 RPC）。只登录态有意义，与 multi_turn 冲突
	// （multi_turn 开着时不发）。默认 false，见 temporaryChatEligible。
	TemporaryChat bool `json:"temporary_chat"`
	// 提示词后缀：非空时追加到每次请求 prompt 的最末尾（末尾指令权重最大，与本文件
	// 工具围栏的实测结论一致）。用途：把网页端调教出来的「聊天伙伴」自我认知扳回任务
	// 执行引擎——Gemini 网页系模型在 agent 场景会冒出「我只是聊天助手/无法操作」之类
	// 的推诿。留空 = 不追加，保持纯透传。
	SystemPromptSuffix string `json:"system_prompt_suffix"`
	// 账号优先调度：绑定的出口到了限流线时**换账号不换出口**。开着时每个号永远只从
	// 自己绑定的那个 IP 出去（Google 眼里不存在「一个号多个 IP」的账号共享特征），
	// 线是本地的并发/RPM/RPH 三档——它们本身留足了离 Google 真正封禁的余量，在线上
	// 换号就是「封禁线之前主动切」。全部账号的绑定出口都在线上时明确 429（或按
	// fallback_direct 退直连）。默认 false = 旧行为：可用性优先，绑定的满了让这个号
	// 漂到别的出口。没绑定过的号分新出口时优先挑没被别的 enabled 账号占的（一号一出口），
	// 两种策略下都一样。见 acquireExitForAccount。
	AccountFirst bool `json:"account_first"`
}

const runtimeConfigKey = "runtime_config"

var (
	rtMu  sync.RWMutex
	rtVal RuntimeConfig
)

// initRuntimeConfig 用启动配置做基线，再把面板改过的值盖上去。
// 必须在 DB 打开之后调用。
func initRuntimeConfig() {
	base := RuntimeConfig{
		RetryAttempts:   cfg.RetryAttempts,
		RetryDelaySec:   cfg.RetryDelaySec,
		RequestTimeout:  cfg.RequestTimeout,
		DefaultModel:    cfg.DefaultModel,
		PerIPConcurrent: cfg.PerIPConcurrent,
		PerIPRPM:        cfg.PerIPRPM,
		PerIPRPH:        cfg.PerIPRPH,
		RetentionDays:   cfg.RetentionDays,
		LogRequests:     cfg.LogRequests,
		Impersonate:     cfg.Impersonate,
		GeminiBL:        cfg.GeminiBL,

		ProxyCooldownMin: cfg.ProxyCooldownMin,
		FallbackDirect:   cfg.FallbackDirect,
		FallbackAnon:     cfg.FallbackAnon,
		GeminiBLAuto:     cfg.GeminiBLAuto,
		MaxPromptBytes:   cfg.MaxPromptBytes,
		MultiTurn:        cfg.MultiTurn,

		AutoDeleteConversation: cfg.AutoDeleteConversation,
		AnonFirst:              cfg.AnonFirst,
		TemporaryChat:          cfg.TemporaryChat,
		AccountFirst:           cfg.AccountFirst,
	}
	if raw := kvGet(runtimeConfigKey); raw != "" {
		saved := base
		if err := json.Unmarshal([]byte(raw), &saved); err == nil {
			if err := validateRuntimeConfig(saved); err == nil {
				base = saved
			} else {
				logf("[config] 忽略 kv 里不合法的运行时配置: %v", err)
			}
		}
	}
	rtMu.Lock()
	rtVal = base
	rtMu.Unlock()
}

// rtCfg 返回当前运行时配置的快照。
func rtCfg() RuntimeConfig {
	rtMu.RLock()
	defer rtMu.RUnlock()
	return rtVal
}

// validateRuntimeConfig 校验面板传来的值。
//
// 这些数字直接决定重试次数、超时和限流额度，是外部输入进敏感落点：
// 0 或负数会让限流器永远拒绝或永远放行，超大值能把单个请求挂死几小时。
// 上界给得比任何合理用法都宽，只挡明显离谱的输入。
func validateRuntimeConfig(c RuntimeConfig) error {
	type rangeCheck struct {
		name     string
		v        int
		min, max int
	}
	for _, r := range []rangeCheck{
		{"retry_attempts", c.RetryAttempts, 1, 10},
		{"retry_delay_sec", c.RetryDelaySec, 0, 60},
		{"request_timeout_sec", c.RequestTimeout, 5, 600},
		{"per_ip_concurrent", c.PerIPConcurrent, 0, 1000},
		{"per_ip_rpm", c.PerIPRPM, 0, 10000},
		{"per_ip_rph", c.PerIPRPH, 0, 100000},
		{"retention_days", c.RetentionDays, 1, 3650},
		{"proxy_cooldown_min", c.ProxyCooldownMin, 0, 10080},
		{"max_prompt_bytes", c.MaxPromptBytes, 0, 10000000},
	} {
		if r.v < r.min || r.v > r.max {
			return fmt.Errorf("%s=%d 超出允许范围 [%d, %d]", r.name, r.v, r.min, r.max)
		}
	}
	if len(c.SystemPromptSuffix) > 20000 {
		return fmt.Errorf("system_prompt_suffix 过长: %d 字节（上限 20000）", len(c.SystemPromptSuffix))
	}
	if _, _, err := resolveModel(c.DefaultModel); err != nil {
		return fmt.Errorf("default_model 不可用: %v", err)
	}
	if c.Impersonate == "" {
		return fmt.Errorf("impersonate 不能为空")
	}
	if c.GeminiBL == "" {
		return fmt.Errorf("gemini_bl 不能为空")
	}
	return nil
}

// saveRuntimeConfig 校验并持久化，成功后立刻生效。
func saveRuntimeConfig(next RuntimeConfig) error {
	if err := validateRuntimeConfig(next); err != nil {
		return err
	}
	b, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := kvSet(runtimeConfigKey, string(b)); err != nil {
		return err
	}
	rtMu.Lock()
	rtVal = next
	rtMu.Unlock()
	logf("[config] 运行时配置已更新")
	return nil
}
