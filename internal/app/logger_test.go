package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLogRingBuffer_AddAndCapacity(t *testing.T) {
	buf := newLogRingBuffer(5)

	for i := 1; i <= 10; i++ {
		now := time.Now()
		buf.Add(now, "[scheduler] Test message", "[12:00:00] [scheduler] Test message")
	}

	entries := buf.Query(LogFilter{Tail: 10})
	if len(entries) != 5 {
		t.Fatalf("expected 5 entries (capacity limit), got %d", len(entries))
	}
	// Last entry should have ID 10
	if entries[4].ID != 10 {
		t.Errorf("expected last entry ID 10, got %d", entries[4].ID)
	}
}

func TestLogRingBuffer_MetaParsing(t *testing.T) {
	tests := []struct {
		input       string
		wantSource  string
		wantLevel   string
		wantMessage string
	}{
		{
			input:       "[scheduler] Request chatcmpl-123 finished; entering pacing cooldown for 20.0s",
			wantSource:  "scheduler",
			wantLevel:   "WARN", // because of cooldown
			wantMessage: "Request chatcmpl-123 finished; entering pacing cooldown for 20.0s",
		},
		{
			input:       "[cookie] 账号 #6 刷新 1PSIDTS 失败: network error",
			wantSource:  "cookie",
			wantLevel:   "ERROR", // because of 失败/error
			wantMessage: "账号 #6 刷新 1PSIDTS 失败: network error",
		},
		{
			input:       "[rotate] 账号 #6 SIDCC 保活被限流（RotateCookies 429），10m0s 内不再请求",
			wantSource:  "rotate",
			wantLevel:   "WARN", // because of 429/限流
			wantMessage: "账号 #6 SIDCC 保活被限流（RotateCookies 429），10m0s 内不再请求",
		},
		{
			input:       "[context] prompt 112540 字节超过 20000，已转成附件 message.txt",
			wantSource:  "context",
			wantLevel:   "INFO",
			wantMessage: "prompt 112540 字节超过 20000，已转成附件 message.txt",
		},
		{
			input:       "[conv] 新会话，首轮发 1500 字节（tools=0）",
			wantSource:  "conv",
			wantLevel:   "INFO", // 字节数里的 500 不是状态码
			wantMessage: "新会话，首轮发 1500 字节（tools=0）",
		},
		{
			input:       "[cookie] 账号 #401 已加入",
			wantSource:  "cookie",
			wantLevel:   "INFO", // #401 是账号编号
			wantMessage: "账号 #401 已加入",
		},
		{
			input:       "[autodel] 删会话 c_1 返回 HTTP 502: bad gateway",
			wantSource:  "autodel",
			wantLevel:   "ERROR",
			wantMessage: "删会话 c_1 返回 HTTP 502: bad gateway",
		},
		{
			input:       "Ordinary server message without tag",
			wantSource:  "general",
			wantLevel:   "INFO",
			wantMessage: "Ordinary server message without tag",
		},
	}

	for _, tc := range tests {
		s, l, m := parseLogMeta(tc.input)
		if s != tc.wantSource {
			t.Errorf("input %q: got source %q, want %q", tc.input, s, tc.wantSource)
		}
		if l != tc.wantLevel {
			t.Errorf("input %q: got level %q, want %q", tc.input, l, tc.wantLevel)
		}
		if m != tc.wantMessage {
			t.Errorf("input %q: got message %q, want %q", tc.input, m, tc.wantMessage)
		}
	}
}

func TestLogRingBuffer_Filtering(t *testing.T) {
	buf := newLogRingBuffer(100)
	now := time.Now()

	buf.Add(now, "[scheduler] Request enqueued", "[12:00:01] [scheduler] Request enqueued")
	buf.Add(now, "[rotate] 账号 #6 刷新了 SIDCC", "[12:00:02] [rotate] 账号 #6 刷新了 SIDCC")
	buf.Add(now, "[cookie] 账号 #6 失败: 401 unauthorized", "[12:00:03] [cookie] 账号 #6 失败: 401 unauthorized")

	// Filter by source
	schedEntries := buf.Query(LogFilter{Source: "scheduler"})
	if len(schedEntries) != 1 || schedEntries[0].Source != "scheduler" {
		t.Fatalf("expected 1 scheduler entry, got %d", len(schedEntries))
	}

	// Filter by level
	errEntries := buf.Query(LogFilter{Level: "ERROR"})
	if len(errEntries) != 1 || errEntries[0].Level != "ERROR" {
		t.Fatalf("expected 1 ERROR entry, got %d", len(errEntries))
	}

	// Filter by Query
	rotateEntries := buf.Query(LogFilter{Query: "sidcc"})
	if len(rotateEntries) != 1 {
		t.Fatalf("expected 1 entry matching 'sidcc', got %d", len(rotateEntries))
	}

	// Filter by SinceID
	sinceEntries := buf.Query(LogFilter{SinceID: 2})
	if len(sinceEntries) != 1 || sinceEntries[0].ID != 3 {
		t.Fatalf("expected entry with ID 3, got %v", sinceEntries)
	}
}

func TestHandleAdminLogs_JSONAndText(t *testing.T) {
	globalLogBuffer.Clear()
	logf("[scheduler] Dispatched task-99")
	logf("[cookie] Refresh failed with 502")

	// 1. JSON test
	reqJSON := httptest.NewRequest("GET", "/admin/api/logs?tail=10", nil)
	wJSON := httptest.NewRecorder()
	handleAdminLogs(wJSON, reqJSON)

	if wJSON.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wJSON.Code)
	}

	var res struct {
		Total int        `json:"total"`
		Items []LogEntry `json:"items"`
	}
	if err := json.Unmarshal(wJSON.Body.Bytes(), &res); err != nil {
		t.Fatalf("failed to decode json: %v", err)
	}
	if res.Total < 2 {
		t.Fatalf("expected at least 2 items, got %d", res.Total)
	}

	// 2. Plain text test
	reqText := httptest.NewRequest("GET", "/admin/api/logs?format=text&tail=10", nil)
	wText := httptest.NewRecorder()
	handleAdminLogs(wText, reqText)

	if wText.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wText.Code)
	}
	bodyStr := wText.Body.String()
	if !strings.Contains(bodyStr, "[scheduler] Dispatched task-99") {
		t.Errorf("expected plain text to contain scheduler log, got:\n%s", bodyStr)
	}
	if !strings.Contains(bodyStr, "[cookie] Refresh failed with 502") {
		t.Errorf("expected plain text to contain cookie log, got:\n%s", bodyStr)
	}
}

// tail 超过容量时给满容量，而不是悄悄退回默认的 100。
func TestLogRingBuffer_TailClampsToCapacity(t *testing.T) {
	buf := newLogRingBuffer(300)
	for i := 0; i < 300; i++ {
		buf.Add(time.Now(), "[x] m", "[00:00:00] [x] m")
	}
	if n := len(buf.Query(LogFilter{Tail: 5000})); n != 300 {
		t.Fatalf("tail=5000 应返回全部 300 条，得到 %d", n)
	}
	if n := len(buf.Query(LogFilter{})); n != 100 {
		t.Fatalf("不传 tail 默认 100 条，得到 %d", n)
	}
}
