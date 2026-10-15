package app

import (
	"strings"
	"testing"
)

// quotaFrame 把一段载荷包成一行 batchexecute 帧。
//
// 解析只认以 `[[` 开头的行（见 quotaPayloads），`)]}'` 防嗅探行和长度行会被跳过，
// 所以测试里可以只喂帧行 + 噪声行。
// 载荷里不能出现双引号，否则这层手拼的 JSON 会坏——本文件用的载荷都不带引号。
func quotaFrame(payload string) string {
	return `[["wrb.fr","jSf9Qc","` + payload + `",null,null,null,"generic"]]`
}

// quotaBody 拼一份「像真实响应那样」的分帧文本。
func quotaBody(lines ...string) string {
	return ")]}'\n\n1000\n" + strings.Join(lines, "\n") + "\n12\n" + `[["di",42]]` + "\n"
}

// 真实形态：外层数组里有一个 bucket，bucket 里两档额度。
// [limit, fraction, period, [[resetEpoch, nanos]]]：period 1 = 5 小时，2 = 每周。
const quotaSamplePayload = `[[[100,0.63,1,[[1792050755,0]]],[100,0.13,2,[[1792486355,0]]]]]`

func TestParseQuotaBodySample(t *testing.T) {
	five, weekly, err := parseQuotaBody(quotaBody(quotaFrame(quotaSamplePayload)))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if five == nil {
		t.Fatal("5 小时额度缺失")
	}
	if five.UsedPercent != 63 {
		t.Errorf("5 小时已用百分比应为 63，实际 %d", five.UsedPercent)
	}
	if five.ResetAt != 1792050755 {
		t.Errorf("5 小时重置时间不对: %d", five.ResetAt)
	}
	if weekly == nil {
		t.Fatal("每周额度缺失")
	}
	if weekly.UsedPercent != 13 {
		t.Errorf("每周已用百分比应为 13，实际 %d", weekly.UsedPercent)
	}
	if weekly.ResetAt != 1792486355 {
		t.Errorf("每周重置时间不对: %d", weekly.ResetAt)
	}
}

// 上游只回一档（或将来加了新档位）时，不能整段解析失败。
func TestParseQuotaBodySingleBucket(t *testing.T) {
	payload := `[[[100,0.4,1,[[1792050755,0]]]]]`
	five, weekly, err := parseQuotaBody(quotaBody(quotaFrame(payload)))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if five == nil || five.UsedPercent != 40 {
		t.Fatalf("5 小时额度解析不对: %+v", five)
	}
	if weekly != nil {
		t.Errorf("只回一档时 weekly 应为 nil，实际 %+v", weekly)
	}
}

// 前面几帧形状不对时，要继续往后找，而不是直接判失败。
func TestParseQuotaBodySkipsUnrelatedFrames(t *testing.T) {
	raw := quotaBody(
		`[["wrb.fr","jSf9Qc","[[1,2,3]]",null,null,null,"generic"]]`,
		quotaFrame(`[[[100,0.63,1,[[1792050755,0]]]]]`),
	)
	five, _, err := parseQuotaBody(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if five == nil || five.UsedPercent != 63 {
		t.Fatalf("应跳过无关帧后找到额度，实际 %+v", five)
	}
}

func TestParseQuotaBodyRejectsBadPayloads(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"已用比例超出上界", `[[[100,1.6,1,[[1792050755,0]]]]]`},
		{"重置时间是占位值", `[[[100,0.5,1,[[1,0]]]]]`},
		{"重置时间是空数组", `[[[100,0.5,1,[]]]]`},
		{"额度桶元素不是数组", `[[1,2,3]]`},
		{"认不出的档位编号", `[[[100,0.5,3,[[1792050755,0]]]]]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := parseQuotaBody(quotaBody(quotaFrame(c.payload))); err == nil {
				t.Fatalf("应报错，实际解析成功（payload=%s）", c.payload)
			}
		})
	}
}

func TestParseQuotaBodyRejectsGarbage(t *testing.T) {
	for _, raw := range []string{"", "not json at all", ")]}'\n\n7\n", `[["wrb.fr"]]`} {
		if _, _, err := parseQuotaBody(raw); err == nil {
			t.Errorf("垃圾输入应报错: %q", raw)
		}
	}
}

func TestParseAccountID(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false},      // 不传 = 自动选号
		{"   ", 0, false},   // 只有空白
		{"7", 7, false},     //
		{" 42 ", 42, false}, // 两端空白
		{"0", 0, true},      // 0 不是合法账号 id
		{"-3", 0, true},     // 负数
		{"abc", 0, true},    //
		{"1.5", 0, true},    // 小数
		{"1;drop", 0, true}, // 别把脏东西塞进 SQL
	}
	for _, c := range cases {
		got, err := parseAccountID(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseAccountID(%q) 应报错，实际返回 %d", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseAccountID(%q) 报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseAccountID(%q) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

// 停用/冷却中的号也要能按 id 查到额度 —— 这正是加 ?account_id=N 的目的：
// 主号 5 小时额度见底时，要能一眼看到别的号还剩多少。
func TestQuotaAccountByIDFindsDisabledAccount(t *testing.T) {
	id, err := accountAdd("quota-test", "SAPISID=quotatest; SID=qt", "")
	if err != nil {
		t.Fatalf("插测试账号失败: %v", err)
	}
	t.Cleanup(func() { _ = accountDelete(id) })

	if err := accountSetStatus(id, "disabled"); err != nil {
		t.Fatalf("停用测试账号失败: %v", err)
	}

	got, ok := quotaAccountByID(id)
	if !ok || got == nil {
		t.Fatal("停用中的账号也应该能按 id 查到（多号池要看冷却号还剩多少额度）")
	}
	if got.ID != id {
		t.Fatalf("查到的账号不对：期望 #%d，实际 #%d", id, got.ID)
	}

	if _, ok := quotaAccountByID(id + 1000000); ok {
		t.Error("不存在的 id 不该查到")
	}
}
