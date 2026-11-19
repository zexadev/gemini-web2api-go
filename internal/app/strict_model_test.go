package app

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func setStrictModel(t *testing.T, on bool) {
	old := rtCfg()
	t.Cleanup(func() { rtMu.Lock(); rtVal = old; rtMu.Unlock() })
	rtMu.Lock()
	rtVal.StrictModel = on
	rtMu.Unlock()
}

// 结构取自 2026-10-24 实测：显示名和第一段正文在同一行（无 Argon 权限的号请求 Argon）。
const downgradedLine = `[["wrb.fr",null,"[null,[\"c_1\",\"r_1\"],null,null,[[\"rc_1\",[\"1+1等于2。\"]]],null,\"cf41b0e0dd7d53e5\",null,null,\"3.5 Flash-Lite\",true]"]]`

func TestStrictLineCheck(t *testing.T) {
	argon := Models["gemini-4-argon"]

	setStrictModel(t, false)
	if strictLineCheck(argon, func() int64 { return 7 }) != nil {
		t.Fatal("开关关着应返回 nil，不能给读取路径加任何开销")
	}

	setStrictModel(t, true)
	if strictLineCheck(Models["gemini-image"], func() int64 { return 7 }) != nil {
		t.Fatal("媒体模型没登记 Upstream，不该检查")
	}
	check := strictLineCheck(argon, func() int64 { return 7 })
	err := check(downgradedLine)
	var mde *ModelDowngradedError
	if !errors.As(err, &mde) || mde.Reported != "3.5 Flash-Lite" || mde.AccountID != 7 {
		t.Fatalf("降级行应报 ModelDowngradedError(3.5 Flash-Lite, #7)，得到 %v", err)
	}
	if err := strictLineCheck(Models["gemini-3.5-flash-lite"], func() int64 { return 7 })(downgradedLine); err != nil {
		t.Fatalf("请求的就是 Flash-Lite，不该报降级：%v", err)
	}
}

func addTestAccount(t *testing.T, label string) int64 {
	t.Helper()
	id, err := accountAdd(label, "SAPISID="+label+"; SID=x", "")
	if err != nil {
		t.Fatalf("插测试账号失败: %v", err)
	}
	t.Cleanup(func() { _ = accountDelete(id) })
	return id
}

func resetDowngrades(t *testing.T) {
	t.Cleanup(func() { downgradeMu.Lock(); downgradeUntil = map[downgradeKey]time.Time{}; downgradeMu.Unlock() })
}

func TestPickAccountForModelSkipsCooling(t *testing.T) {
	setStrictModel(t, true)
	rtMu.Lock()
	rtVal.StrictModelCooldownMin = 300
	rtMu.Unlock()
	resetDowngrades(t)
	a1 := addTestAccount(t, "acct1")
	a2 := addTestAccount(t, "acct2")
	argon, flash := Models["gemini-4-argon"], Models["gemini-3.6-flash"]

	markDowngraded(a1, argon)
	for i := 0; i < 4; i++ { // 轮转会换号，多挑几次确认 a1 一次都不出现
		a, _, err := pickAccountForModel(argon, nil)
		if err != nil || a == nil || a.ID != a2 {
			t.Fatalf("a1 在 Argon 上冷却中，应只挑到 a2，得到 %v %v", a, err)
		}
	}
	if _, skip, _ := pickAccountForModel(flash, nil); skip[a1] {
		t.Fatal("冷却只针对被降级的那一档，a1 在 3.6 Flash 上应照常可用")
	}

	markDowngraded(a2, argon)
	_, _, err := pickAccountForModel(argon, nil)
	var mde *ModelDowngradedError
	if !errors.As(err, &mde) || mde.Reported != "" || mde.RetryAfter < 299*time.Minute {
		t.Fatalf("全池冷却应报降级错误并带解冻时间（~300 分钟），得到 %v", err)
	}
}

// cooldown=0：不记冷却，但同一请求里换号仍靠 exclude 生效。
func TestCooldownZeroStillExcludesWithinRequest(t *testing.T) {
	setStrictModel(t, true)
	rtMu.Lock()
	rtVal.StrictModelCooldownMin = 0
	rtMu.Unlock()
	resetDowngrades(t)
	a1 := addTestAccount(t, "acct1")
	a2 := addTestAccount(t, "acct2")
	argon := Models["gemini-4-argon"]

	exclude := map[int64]bool{}
	err := &ModelDowngradedError{Requested: "4 Argon", Reported: "3.5 Flash-Lite", AccountID: a1}
	if !retryOnAnotherAccount(&StreamResult{}, err, argon, exclude) {
		t.Fatal("没吐过内容的降级应换号重来")
	}
	if cooling, _ := coolingAccounts(argon); len(cooling) != 0 {
		t.Fatalf("cooldown=0 不该记冷却：%v", cooling)
	}
	if a, _, _ := pickAccountForModel(argon, exclude); a == nil || a.ID != a2 {
		t.Fatalf("本次请求里 a1 已降级，应换到 a2，得到 %v", a)
	}
}

func TestRetryOnAnotherAccountGuards(t *testing.T) {
	setStrictModel(t, true)
	resetDowngrades(t)
	argon := Models["gemini-4-argon"]
	if retryOnAnotherAccount(&StreamResult{}, &ModelDowngradedError{Reported: "x"}, argon, map[int64]bool{}) {
		t.Error("匿名降级没号可换")
	}
	if retryOnAnotherAccount(&StreamResult{Emitted: "部分"}, &ModelDowngradedError{Reported: "x", AccountID: 9}, argon, map[int64]bool{}) {
		t.Error("已经吐过内容的不能重来，客户端会收到两遍")
	}
	if retryOnAnotherAccount(&StreamResult{}, errors.New("网络错误"), argon, map[int64]bool{}) {
		t.Error("非降级错误不归这里管")
	}
}

// 回调报错必须立刻停读：后面的行不能再回调，否则正文会在报错之前漏给客户端。
func TestReadBodyStopsOnCallbackError(t *testing.T) {
	stop := errors.New("stop")
	var seen []string
	_, _, err := readBody(strings.NewReader("a\nb\nc\n"), func(line string) error {
		seen = append(seen, line)
		if line == "b" {
			return stop
		}
		return nil
	}, time.Now())
	if err != stop {
		t.Fatalf("应原样返回回调的错误，得到 %v", err)
	}
	if strings.Join(seen, ",") != "a,b" {
		t.Fatalf("报错后还在回调：%v", seen)
	}
}

func TestModelDowngraded(t *testing.T) {
	cases := []struct {
		model, reported string
		want            bool
	}{
		{"gemini-3.1-pro", "3.1 Pro", false},
		{"gemini-3.1-pro-thinking", "3.1 Pro High", false},
		{"gemini-3.6-flash-thinking", "3.6 Flash 扩展", false},
		{"gemini-4-argon-thinking", "4 Argon High", false},
		// 2026-11-16 实测：mode 8 回报 4 Carbon（上游改名），新旧两个名字互认
		{"gemini-4-argon", "4 Carbon", false},
		{"gemini-4-carbon", "4 Carbon", false},
		{"gemini-4-carbon", "4 Argon", false},
		{"gemini-4-argon-thinking", "4 Carbon High", false},
		{"gemini-4-carbon", "3.5 Flash-Lite", true},
		{"gemini-4-carbon", "4 Hydrogen", true},
		// 2026-11-19 实测：mode 3 回报 4 Neon（接替 3.1 Pro），新旧两个名字互认
		{"gemini-3.1-pro", "4 Neon", false},
		{"gemini-3.1-pro-thinking", "4 Neon High", false},
		{"gemini-4-neon", "4 Neon", false},
		{"gemini-4-neon-thinking", "4 Neon High", false},
		{"gemini-4-neon", "3.5 Flash-Lite", true},
		{"gemini-4-neon", "4 Hydrogen", true},
		{"gemini-3.7-flash", "3.8 Flash", false},
		{"gemini-4-hydrogen", "4 Hydrogen", false},
		{"gemini-4-hydrogen-thinking", "4 Hydrogen High", false},
		{"gemini-4-hydrogen-thinking", "4 Hydrogen Thinking High", false},
		// The old Flash names must accept the upstream rename without disabling strict_model.
		{"gemini-3.8-flash", "4 Hydrogen", false},
		{"gemini-3.7-flash", "4 Hydrogen", false},
		{"gemini-3.8-flash-thinking", "4 Hydrogen High", false},
		{"gemini-3.7-flash-thinking", "4 Hydrogen High", false},
		{"gemini-3.8-flash-thinking", "3.8 Flash High", false},
		// 2026-11-17 实测：登录号（免费号也是）发 mode 1 回报 4 Hydrogen，3.6 Flash 已不在清单
		{"gemini-3.6-flash", "4 Hydrogen", false},
		{"gemini-3.6-flash-thinking", "4 Hydrogen High", false},
		{"gemini-3.6-flash", "3.5 Flash-Lite", true},
		// An explicit Hydrogen request must still reject older models and other tiers.
		{"gemini-4-hydrogen", "3.8 Flash", true},
		{"gemini-4-hydrogen", "3.5 Flash-Lite", true},
		{"gemini-4-hydrogen-thinking", "4 Argon High", true},
		{"gemini-4-hydrogen", "4 Hydrogenium", true},
		// 2026-10-24 实测：无 Argon 权限的号请求 Argon，自报 3.5 Flash-Lite
		{"gemini-4-argon", "3.5 Flash-Lite", true},
		// 同日实测：免费号请求 3.8，自报 3.6 Flash
		{"gemini-3.8-flash", "3.6 Flash", true},
		{"gemini-3.1-pro", "3.5 Flash-Lite", true},
		// 前缀必须断在词边界：Flash 不能把 Flash-Lite 认成自己
		{"gemini-3.5-flash-lite", "3.5 Flash", true},
		// 看不见不判
		{"gemini-3.1-pro", "", false},
		{"gemini-image", "Nano Banana 2", false},
	}
	for _, c := range cases {
		mc, ok := Models[c.model]
		if !ok {
			t.Fatalf("Models 里没有 %s", c.model)
		}
		if got := modelDowngraded(mc, c.reported); got != c.want {
			t.Errorf("%s 自报 %q: downgraded=%v want %v", c.model, c.reported, got, c.want)
		}
	}
}

// 除了媒体工具，每个模型都得登记 Upstream，不然 strict_model 对它静默失效。
func TestEveryChatModelHasUpstream(t *testing.T) {
	for name, mc := range Models {
		if mc.Tool == 0 && mc.Upstream == "" {
			t.Errorf("%s 没有 Upstream", name)
		}
	}
}
