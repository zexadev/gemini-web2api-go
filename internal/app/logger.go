package app

import (
	"regexp"
	"strings"
	"sync"
	"time"
)

// LogEntry represents a single structured log line.
type LogEntry struct {
	ID        int64     `json:"id"`
	Timestamp time.Time `json:"ts"`
	TimeStr   string    `json:"time"`
	Level     string    `json:"level"`  // INFO, WARN, ERROR
	Source    string    `json:"source"` // scheduler, cookie, rotate, bl, context, conv, media, vision, db, config, general
	Message   string    `json:"message"`
	Raw       string    `json:"raw"`
}

// LogRingBuffer stores the last N log entries in memory safely.
type LogRingBuffer struct {
	mu       sync.RWMutex
	capacity int
	entries  []LogEntry
	nextID   int64
}

// Global log buffer with a default capacity of 1000 lines.
var globalLogBuffer = newLogRingBuffer(1000)

func newLogRingBuffer(capacity int) *LogRingBuffer {
	if capacity <= 0 {
		capacity = 1000
	}
	return &LogRingBuffer{
		capacity: capacity,
		entries:  make([]LogEntry, 0, capacity),
		nextID:   1,
	}
}

var tagRegexp = regexp.MustCompile(`^\[([a-zA-Z0-9_\-]+)\]\s*(.*)$`)

// 状态码只认独立的数字：消息里常有字节数、账号编号（"发 1500 字节"、"账号 #401"），
// 按子串匹配会把它们判成错误。前后不能紧挨数字，也不能是 # 后面的编号。
var (
	logHTTPErrRe = regexp.MustCompile(`(^|[^0-9#])(401|403|5\d\d)([^0-9]|$)`)
	log429Re     = regexp.MustCompile(`(^|[^0-9#])429([^0-9]|$)`)
)

// parseLogMeta classifies source and level from the message string.
func parseLogMeta(msg string) (source, level, cleanMsg string) {
	cleanMsg = strings.TrimSpace(msg)
	source = "general"
	level = "INFO"

	// Extract [tag] if present at the start of cleanMsg
	if m := tagRegexp.FindStringSubmatch(cleanMsg); len(m) == 3 {
		source = strings.ToLower(m[1])
		cleanMsg = strings.TrimSpace(m[2])
	}

	lower := strings.ToLower(cleanMsg)
	// Determine level
	if strings.Contains(lower, "failed") ||
		strings.Contains(lower, "failure") ||
		strings.Contains(lower, "error") ||
		strings.Contains(lower, "fail") ||
		strings.Contains(lower, "失败") ||
		strings.Contains(lower, "被风控") ||
		logHTTPErrRe.MatchString(cleanMsg) ||
		strings.Contains(lower, "panic") ||
		strings.Contains(lower, "拒绝") {
		level = "ERROR"
	} else if strings.Contains(lower, "warn") ||
		log429Re.MatchString(cleanMsg) ||
		strings.Contains(lower, "限流") ||
		strings.Contains(lower, "retry") ||
		strings.Contains(lower, "重试") ||
		strings.Contains(lower, "跳过") ||
		strings.Contains(lower, "cooldown") {
		level = "WARN"
	}

	return source, level, cleanMsg
}

// Add appends a new entry to the circular buffer.
func (b *LogRingBuffer) Add(now time.Time, msg, raw string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	source, level, cleanMsg := parseLogMeta(msg)

	entry := LogEntry{
		ID:        b.nextID,
		Timestamp: now,
		TimeStr:   now.Format("15:04:05"),
		Level:     level,
		Source:    source,
		Message:   cleanMsg,
		Raw:       raw,
	}
	b.nextID++

	if len(b.entries) >= b.capacity {
		// Shift by slicing off oldest item
		b.entries = append(b.entries[1:], entry)
	} else {
		b.entries = append(b.entries, entry)
	}
}

// LogFilter defines filtering options for querying logs.
type LogFilter struct {
	Tail    int
	SinceID int64
	Source  string
	Level   string
	Query   string
}

// Query returns entries matching the given filter.
func (b *LogRingBuffer) Query(f LogFilter) []LogEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()

	tail := f.Tail
	if tail <= 0 {
		tail = 100
	}
	if tail > b.capacity {
		tail = b.capacity
	}

	sourceFilter := strings.ToLower(strings.TrimSpace(f.Source))
	levelFilter := strings.ToUpper(strings.TrimSpace(f.Level))
	qFilter := strings.ToLower(strings.TrimSpace(f.Query))

	var matched []LogEntry
	for _, entry := range b.entries {
		if f.SinceID > 0 && entry.ID <= f.SinceID {
			continue
		}
		if sourceFilter != "" && !strings.EqualFold(entry.Source, sourceFilter) {
			continue
		}
		if levelFilter != "" && entry.Level != levelFilter {
			continue
		}
		if qFilter != "" {
			if !strings.Contains(strings.ToLower(entry.Message), qFilter) &&
				!strings.Contains(strings.ToLower(entry.Raw), qFilter) &&
				!strings.Contains(strings.ToLower(entry.Source), qFilter) {
				continue
			}
		}
		matched = append(matched, entry)
	}

	if len(matched) > tail {
		matched = matched[len(matched)-tail:]
	}

	return matched
}

// Clear resets the log buffer.
func (b *LogRingBuffer) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries = make([]LogEntry, 0, b.capacity)
}
