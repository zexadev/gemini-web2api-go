package app

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// 扩展定时推 cookie：同会话更新、换会话按标签更新、都对不上才新建，账号序号分开算。
func TestAccountSync(t *testing.T) {
	resetSeedState(t)
	v1 := "SID=a; SAPISID=e; __Secure-1PSID=f; __Secure-1PSIDTS=t1; OSID=o"
	v2 := "SID=a; SAPISID=e; __Secure-1PSID=f; __Secure-1PSIDTS=t2; OSID=o"

	r, err := accountSync("Me@Gmail.com", 0, v1)
	if err != nil || r.Action != "added" {
		t.Fatalf("首次应新建：%+v %v", r, err)
	}
	id := r.ID

	if r, _ := accountSync("me@gmail.com", 0, v1); r.Action != "unchanged" || r.ID != id {
		t.Fatalf("内容没变应 unchanged：%+v", r)
	}
	// 同会话、1PSIDTS 换了：标签不同也照样按会话认出来
	if r, _ := accountSync("", 0, v2); r.Action != "updated" || r.ID != id {
		t.Fatalf("同会话应更新原行：%+v", r)
	}
	if a := accountByID(id); a.Cookie != v2 {
		t.Fatalf("没写进去：%q", a.Cookie)
	}

	// 重新登录：会话全换了，靠标签（不分大小写）认回同一行
	v3 := "SID=b; SAPISID=x; __Secure-1PSID=y; OSID=o"
	if r, _ := accountSync("ME@gmail.com", 0, v3); r.Action != "updated" || r.ID != id {
		t.Fatalf("同标签应更新原行：%+v", r)
	}

	// 同一串 cookie 的 /u/1 是另一个号，不能写进 /u/0 那行
	r1, _ := accountSync("me@gmail.com", 1, v3)
	if r1.Action != "added" || r1.ID == id || accountByID(r1.ID).AuthUser != 1 {
		t.Fatalf("/u/1 应单独新建：%+v", r1)
	}

	// 自动停用的号拿到新 cookie 恢复；手动停用的不动
	_, _ = getDB().Exec(`UPDATE accounts SET status='disabled', fail_count=3, last_error='x' WHERE id=?`, id)
	if r, _ := accountSync("", 0, v1); r.Action != "added" || r.ID == id {
		t.Fatalf("%+v", r)
	}
	// v1 的会话跟 v3 不同，但标签为空 → 新建，不该去碰 id
	if a := accountByID(id); a.Status != "disabled" {
		t.Fatalf("无标签、会话不同的推送不该动这一行")
	}
	if r, _ := accountSync("me@gmail.com", 0, v3); r.ID != id {
		t.Fatalf("%+v", r)
	}
	if a := accountByID(id); a.Status != "enabled" || a.FailCount != 0 || a.LastError != "" {
		t.Fatalf("自动停用的号应恢复：%+v", a)
	}
	_ = accountSetStatus(r1.ID, "disabled")
	_, _ = accountSync("me@gmail.com", 1, v2)
	if a := accountByID(r1.ID); a.Status != "disabled" {
		t.Fatalf("手动停用的号不该被重新启用")
	}

	if _, err := accountSync("x", 0, "SID=a; HSID=b"); err == nil {
		t.Fatal("没 SAPISID 应报错")
	}
	if _, err := accountSync("x", 12, v1); err == nil {
		t.Fatal("序号越界应报错")
	}
}

// 接口层：cookie 字段既可以是字符串，也可以直接是扩展导出的数组。
func TestHandleAdminCookieSync(t *testing.T) {
	resetSeedState(t)
	body := `{"label":"me@gmail.com","authuser":0,"cookie":[
	  {"domain":".google.com","name":"SAPISID","value":"e"},
	  {"domain":".google.com","name":"__Secure-1PSID","value":"f"},
	  {"domain":".youtube.com","name":"YSC","value":"y"}]}`
	w := httptest.NewRecorder()
	handleAdminCookieSync(w, httptest.NewRequest("POST", "/admin/api/cookies/sync", strings.NewReader(body)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"action":"added"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	accts := accountList()
	if len(accts) != 1 || accts[0].Cookie != "SAPISID=e; __Secure-1PSID=f" {
		t.Fatalf("%+v", accts)
	}

	w = httptest.NewRecorder()
	handleAdminCookieSync(w, httptest.NewRequest("POST", "/admin/api/cookies/sync",
		strings.NewReader(`{"label":"me@gmail.com","cookie":"SAPISID=e; __Secure-1PSID=f; NID=1"}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"action":"updated"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	handleAdminCookieSync(w, httptest.NewRequest("POST", "/admin/api/cookies/sync", strings.NewReader(`{"label":"x"}`)))
	if w.Code != 400 {
		t.Fatalf("空 cookie 应 400：%d", w.Code)
	}
}
