package app

import (
	"encoding/json"
	"os"
	"sync"
)

type Config struct {
	Port           int    `json:"port"`
	Host           string `json:"host"`
	RetryAttempts  int    `json:"retry_attempts"`
	RetryDelaySec  int    `json:"retry_delay_sec"`
	RequestTimeout int    `json:"request_timeout_sec"`
	GeminiBL       string `json:"gemini_bl"`
	DefaultModel   string `json:"default_model"`
	LogRequests    bool   `json:"log_requests"`
	CookieFile     string `json:"cookie_file"`
	Proxy          string `json:"proxy"` // 只用于启动时播种代理池，见 seedProxiesFromConfig
	Impersonate    string `json:"impersonate"`
	DBPath         string `json:"db_path"`
	AdminToken     string `json:"admin_token"`
	AdminEnabled   bool   `json:"admin_enabled"`
	RetentionDays  int    `json:"retention_days"`

	// Per-IP rate limit (一个 slot = 直连/或一个代理),0 表示不限。
	// 默认值取实测区间 80-180 的下沿，详见 ratelimit.go 的说明。
	PerIPConcurrent int `json:"per_ip_concurrent"` // 瞬时并发上限
	PerIPRPM        int `json:"per_ip_rpm"`        // 每分钟请求上限
	PerIPRPH        int `json:"per_ip_rph"`        // 每小时请求上限

	// 代理连续失败熔断后隔多久放回池子（分钟），0 = 不恢复。
	ProxyCooldownMin int `json:"proxy_cooldown_min"`
	// 代理池无可用出口时是否退回直连。默认 false。
	FallbackDirect bool `json:"fallback_direct"`
	// cookie 失效时是否降级成匿名继续跑。默认 false。
	FallbackAnon bool `json:"fallback_anon"`
	// 是否自动从 /app 页面抓最新 bl 版本号。默认 true。
	GeminiBLAuto bool `json:"gemini_bl_auto"`
	// 单次请求 prompt 的 UTF-8 字节上限，0 = 不限。
	MaxPromptBytes int `json:"max_prompt_bytes"`
	// 是否走 Gemini 原生 conversation_id 服务端多轮。默认 false，见 conversation.go。
	MultiTurn bool `json:"multi_turn"`
}

var (
	cfg     Config
	cfgOnce sync.Once
)

func defaultConfig() Config {
	return Config{
		Port:           8083,
		Host:           "0.0.0.0",
		RetryAttempts:  3,
		RetryDelaySec:  2,
		RequestTimeout: 180,
		GeminiBL:       "boq_assistant-bard-web-server_20260525.09_p0",
		DefaultModel:   "gemini-3.6-flash",
		LogRequests:    true,
		CookieFile:     "",
		Proxy:          "",
		Impersonate:    "chrome_146",
		DBPath:         "./data/gemini.db",
		AdminToken:     "",
		AdminEnabled:   true,
		RetentionDays:  30,
		// 单出口实测 80-180 次不等（连接策略和出口质量决定），静态 IP 上 188。
		// RPH 取下沿 80 保守留量；按低速率跑的部署可以调高很多 ——
		// 10 次/分钟连打 800 次、跨 110 分钟一次没被拦。
		PerIPConcurrent: 5,
		PerIPRPM:        30,
		PerIPRPH:        80,
		// 实测被拦的出口 106-121 分钟自动恢复，冷却取 120 分钟。
		ProxyCooldownMin: 120,
		FallbackDirect:   false,
		FallbackAnon:     false,
		GeminiBLAuto:     true,
		// 实测上游的墙在约 13 万 UTF-8 字节：129,950 字节中英文各 3/3 过，
		// 135,990 字节各 1/3，141,920 字节各 1/3。取 128000 留一点余量。
		MaxPromptBytes: 128000,
		MultiTurn:      false,
	}
}

func loadConfig(path string) error {
	cfg = defaultConfig()
	if path == "" {
		for _, p := range []string{"./config.json", os.ExpandEnv("$HOME/.config/gemini-web2api/config.json")} {
			if _, err := os.Stat(p); err == nil {
				path = p
				break
			}
		}
	}
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &cfg)
}
