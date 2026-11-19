package app

import "testing"

// request_timeout_sec 上限 1200（#83）：长思考 + 长输出的 Carbon 请求能跑过 600 秒，
// 总超时不会被持续输出重置。卡住的请求另有 stream_idle_timeout_sec 兜底。
func TestRequestTimeoutBounds(t *testing.T) {
	saved := cfg
	t.Cleanup(func() { cfg = saved; initRuntimeConfig() })
	cfg = defaultConfig()
	cfg.DBPath = saved.DBPath // 别让 kvGet 去开真实的 ./data 库
	initRuntimeConfig()
	for _, tc := range []struct {
		v  int
		ok bool
	}{{4, false}, {5, true}, {600, true}, {1200, true}, {1201, false}} {
		c := rtCfg()
		c.RequestTimeout = tc.v
		if err := validateRuntimeConfig(c); (err == nil) != tc.ok {
			t.Errorf("request_timeout_sec=%d: err=%v, want ok=%v", tc.v, err, tc.ok)
		}
	}
}
