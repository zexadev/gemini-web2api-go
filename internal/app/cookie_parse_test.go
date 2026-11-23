package app

import "testing"

// 面板的填空模板要能跟原始 cookie 串一样入池。
// 判据是"填进去什么，拼出来就是什么"——归一化错了的话池子里存的是一整段 JSON，
// SAPISID 提取和后续请求全错。
func TestNormalizeCookieForms(t *testing.T) {
	want := "SID=a; HSID=b; SSID=c; APISID=d; SAPISID=e; __Secure-1PSID=f; __Secure-1PSIDTS=g"

	cases := []struct{ name, in, want string }{
		{"原始串原样通过", want, want},
		{
			"填好的 JSON 模板",
			`{"SID":"a","HSID":"b","SSID":"c","APISID":"d","SAPISID":"e",
			  "__Secure-1PSID":"f","__Secure-1PSIDTS":"g"}`,
			want,
		},
		{
			// JSON 的键顺序不该影响结果，否则同一份 cookie 会因为粘贴顺序不同
			// 被当成两个不同账号，去重就失效了
			"模板字段顺序打乱",
			`{"SAPISID":"e","__Secure-1PSIDTS":"g","HSID":"b","SID":"a",
			  "__Secure-1PSID":"f","APISID":"d","SSID":"c"}`,
			want,
		},
		{
			"留空的项自动跳过",
			`{"SID":"a","HSID":"","SSID":"  ","SAPISID":"e"}`,
			"SID=a; SAPISID=e",
		},
		{
			// 用户从 DevTools 多复制几个进来不能丢，附在模板字段后面按名字排序
			"模板外的字段保留",
			`{"SID":"a","SAPISID":"e","NID":"z","AEC":"y"}`,
			"SID=a; SAPISID=e; AEC=y; NID=z",
		},
		{"旧单 cookie 的 JSON 形态", `{"cookie":"SID=a; SAPISID=e","sapisid":"e"}`, "SID=a; SAPISID=e"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := normalizeCookie(c.in, "test")
			if !ok {
				t.Fatal("解析失败")
			}
			if got != c.want {
				t.Errorf("得到 %q\n期望 %q", got, c.want)
			}
		})
	}
}

// 浏览器扩展（Cookie-Editor / EditThisCookie）和 Playwright 导出的 JSON 要能直接入池。
// 判据：只留浏览器真会发给 gemini.google.com 的项，同名时更具体的 domain 胜出，顺序照导出。
func TestNormalizeCookieExportArrays(t *testing.T) {
	arr := `[
	  {"domain":".google.com","name":"SID","value":"a","path":"/","secure":false},
	  {"domain":".google.com","name":"SAPISID","value":"e","httpOnly":false},
	  {"domain":"accounts.google.com","name":"OSID","value":"wrong"},
	  {"domain":"gemini.google.com","hostOnly":true,"name":"OSID","value":"right"},
	  {"domain":".youtube.com","name":"VISITOR_INFO1_LIVE","value":"yt"},
	  {"domain":".google.com","name":"__Secure-1PSID","value":"f"},
	  {"domain":".google.com","name":"EMPTY","value":""},
	  {"name":"NID","value":"z"}
	]`
	want := "SID=a; SAPISID=e; OSID=right; __Secure-1PSID=f; NID=z"

	cases := []struct{ name, in string }{
		{"扩展导出的数组", arr},
		{"Playwright storageState", `{"cookies":` + arr + `,"origins":[]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := normalizeCookie(c.in, "test")
			if !ok {
				t.Fatal("解析失败")
			}
			if got != want {
				t.Errorf("得到 %q\n期望 %q", got, want)
			}
		})
	}

	// .google.com 的 OSID 在前、gemini.google.com 的在后：后者更具体，仍应胜出，但位置不变
	got, ok := normalizeCookie(`[{"domain":".google.com","name":"OSID","value":"a"},
		{"domain":".google.com","name":"SID","value":"s"},
		{"domain":"gemini.google.com","name":"OSID","value":"b"}]`, "test")
	if !ok || got != "OSID=b; SID=s" {
		t.Errorf("同名覆盖不对：%q", got)
	}

	for _, bad := range []string{`[]`, `[{"domain":".youtube.com","name":"X","value":"y"}]`, `[1,2]`} {
		if _, ok := normalizeCookie(bad, "test"); ok {
			t.Errorf("%s 不该解析成功", bad)
		}
	}
}

// 空模板必须被拦下来。字段名 "SAPISID" 在空模板里也是存在的，
// 所以判据只能是"有没有非空值"，不能是"字符串里出没出现过 SAPISID"。
func TestAccountAddRejectsBlankTemplate(t *testing.T) {
	resetSeedState(t)
	blank := `{"SID":"","HSID":"","SSID":"","APISID":"","SAPISID":"","__Secure-1PSID":"","__Secure-1PSIDTS":""}`
	if _, err := accountAdd("空模板", blank, ""); err == nil {
		t.Fatal("空模板被放进池子了")
	}
	if n := len(accountList()); n != 0 {
		t.Errorf("池子里多了 %d 条", n)
	}
}

// 填好的模板要真能入池，且存进去的是拼好的裸串不是 JSON。
func TestAccountAddAcceptsTemplate(t *testing.T) {
	resetSeedState(t)
	filled := `{"SID":"a","HSID":"b","SSID":"c","APISID":"d","SAPISID":"e","__Secure-1PSID":"f","__Secure-1PSIDTS":""}`
	if _, err := accountAdd("模板", filled, ""); err != nil {
		t.Fatal(err)
	}
	list := accountList()
	if len(list) != 1 {
		t.Fatalf("应有 1 条，实际 %d", len(list))
	}
	want := "SID=a; HSID=b; SSID=c; APISID=d; SAPISID=e; __Secure-1PSID=f"
	if list[0].Cookie != want {
		t.Errorf("存进去的是 %q\n期望 %q", list[0].Cookie, want)
	}
	if extractSAPISID(list[0].Cookie) != "e" {
		t.Error("入池后取不出 SAPISID")
	}
}

// 从 DevTools 复制出来的串不一定带空格。按 "; " 切的话整段会被当成一个键，
// 于是 SAPISID 取不到值、不发 Authorization 头、请求被上游当匿名处理，
// 而用户看到的是"成功了"——又一个静默降级。
func TestCookiePairsWithoutSpaces(t *testing.T) {
	for _, in := range []string{
		"SID=a; SAPISID=b; HSID=c",
		"SID=a;SAPISID=b;HSID=c",
		"  SID=a ;  SAPISID=b ;HSID=c  ",
		"SID=a;\nSAPISID=b;\nHSID=c",
	} {
		if got := extractSAPISID(in); got != "b" {
			t.Errorf("extractSAPISID(%q) = %q，期望 \"b\"", in, got)
		}
		if n := len(cookieNames(in)); n != 3 {
			t.Errorf("cookieNames(%q) 数出 %d 项，期望 3", in, n)
		}
	}
	// 值里带 '='（base64 补位）只能在第一个等号处切
	if got := extractSAPISID("SAPISID=ab==; SID=x"); got != "ab==" {
		t.Errorf("值里的等号被切坏了: %q", got)
	}
}

// 没空格的串以前能通过 accountAdd 的字符串包含判断，存进去却是废的。
func TestAccountAddAcceptsNoSpaceCookie(t *testing.T) {
	resetSeedState(t)
	if _, err := accountAdd("无空格", "SID=a;SAPISID=b;__Secure-1PSID=c", ""); err != nil {
		t.Fatalf("没空格的串应该能入池: %v", err)
	}
	if extractSAPISID(accountList()[0].Cookie) != "b" {
		t.Error("入池后取不出 SAPISID")
	}
}
