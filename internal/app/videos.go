package app

// /v1/videos —— OpenAI（Sora）形状的异步视频生成端点。
//
// gemini 视频要几十秒到几分钟，同步阻塞一个 HTTP 请求不现实，所以照 OpenAI Sora 那套走异步：
//   POST /v1/videos               建任务，立即返回 {id, status:"queued"}
//   GET  /v1/videos/{id}          轮询状态 queued|in_progress|completed|failed
//   GET  /v1/videos/{id}/content  完成后下 MP4
// 底层复用 callGemini（跟 /v1/chat/completions 里 model=gemini-video 同一条链），
// 只是把「阻塞等结果」挪到后台 goroutine，前台立即返 id。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type videoJob struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Model     string `json:"model"`
	Status    string `json:"status"` // queued | in_progress | completed | failed
	CreatedAt int64  `json:"created_at"`
	Prompt    string `json:"prompt,omitempty"`
	Size      string `json:"size,omitempty"` // "1280x720" (16:9) or "720x1280" (9:16)
	Error     string `json:"error,omitempty"`
	// Extension chain: RemixedFrom is the job this one extends (OpenAI Sora field name);
	// Seconds is the duration of this job's MP4 when known.
	RemixedFrom string  `json:"remixed_from_video_id,omitempty"`
	Account     string  `json:"account,omitempty"` // pool account that made it (label)
	Seconds     float64 `json:"seconds,omitempty"`

	mp4  []byte // 完成后的视频字节，不进 JSON
	mime string

	// Where the video lives upstream, needed to extend it (same account + conversation).
	accountID      int64
	cid, rid, rcid string
	turn           int // this job's turn index in the conversation (first = 0)
	// 生成时 temporary_chat 和 auto_delete_conversation 都关着，会话才留在账号里、才能续写。
	convKept bool
}

var (
	videoJobs   = map[string]*videoJob{}
	videoJobsMu sync.Mutex
)

func putVideoJob(j *videoJob) {
	videoJobsMu.Lock()
	// 顺手清掉 2 小时前的旧任务，别让内存里越堆越多。
	cutoff := time.Now().Unix() - 2*3600
	for id, old := range videoJobs {
		if old.CreatedAt < cutoff {
			delete(videoJobs, id)
		}
	}
	videoJobs[j.ID] = j
	videoJobsMu.Unlock()
}

// videoJobView 在锁里拷一份任务给响应序列化用。任务一建好后台 goroutine 就在改它
// （Status/Error/Seconds…，都在 videoJobsMu 里改），直接把指针交给 writeJSON 等于不加锁读。
func videoJobView(j *videoJob) videoJob {
	videoJobsMu.Lock()
	defer videoJobsMu.Unlock()
	return *j
}

func getVideoJob(id string) *videoJob {
	videoJobsMu.Lock()
	defer videoJobsMu.Unlock()
	return videoJobs[id]
}

// handleCreateVideo 处理 POST /v1/videos。
func handleCreateVideo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": map[string]string{"message": "method not allowed"}})
		return
	}
	var req map[string]any
	var fileRef *pendingUpload
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		// OpenAI SDK shape: videos.create(prompt=..., input_reference=<file>) is multipart.
		if err := r.ParseMultipartForm(maxImageBytes + 1<<20); err != nil {
			writeJSON(w, 400, map[string]any{"error": map[string]string{"message": "bad multipart body: " + err.Error()}})
			return
		}
		req = map[string]any{}
		for k, v := range r.MultipartForm.Value {
			if len(v) > 0 {
				req[k] = v[0]
			}
		}
		if f, hdr, err := r.FormFile("input_reference"); err == nil {
			data, rerr := io.ReadAll(io.LimitReader(f, maxImageBytes+1))
			f.Close()
			mime := hdr.Header.Get("Content-Type")
			if mime == "" || mime == "application/octet-stream" {
				mime = http.DetectContentType(data)
			}
			up, uerr := newMediaUpload(data, mime, 1)
			if rerr != nil || uerr != nil {
				writeJSON(w, 400, map[string]any{"error": map[string]string{"message": fmt.Sprintf("input_reference: %v%v", rerr, uerr), "type": "invalid_request_error"}})
				return
			}
			fileRef = &up
		}
	} else if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"message": "bad json body"}})
		return
	}
	prompt, _ := req["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"message": "missing prompt", "type": "invalid_request_error"}})
		return
	}
	model, _ := req["model"].(string)
	if model == "" {
		model = "gemini-video"
	}
	// 校验模型确实是视频模型（查基础配置，不用 resolveModel —— 它无 cookie 时对登录态模型
	// 直接返「需要 cookie」错，那个错该留给 job 里报，别在这一步糊成「不是视频模型」）。
	if base, ok := Models[model]; !ok || base.Tool != toolVideo {
		writeJSON(w, 400, map[string]any{"error": map[string]string{
			"message": "model must be a video model (gemini-video)", "type": "invalid_request_error"}})
		return
	}
	// Image→video: OpenAI Sora's `input_reference` (start frame). Accepts a data: URL,
	// an http(s) URL, or {"image_url": "..."} / {"url": "..."}. Materialized now so a
	// bad image fails the POST (400) instead of a queued job.
	var images []pendingUpload
	if fileRef != nil {
		images = append(images, *fileRef)
	} else if ref := inputReferenceSrc(req["input_reference"]); ref != "" {
		img, err := materializeImage(ref, "", 1)
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": map[string]string{"message": "input_reference: " + err.Error(), "type": "invalid_request_error"}})
			return
		}
		images = append(images, img)
	}
	size, err := videoSize(req)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"message": err.Error(), "type": "invalid_request_error"}})
		return
	}
	// Every account is out of video quota: say so now, with when to come back, instead of
	// queueing a job that can only fail.
	if exhausted, reset := videoPoolExhausted(); exhausted {
		w.Header().Set("Retry-After", fmt.Sprint(retryAfterSeconds(reset)))
		writeJSON(w, 429, map[string]any{"error": map[string]any{
			"message": videoPoolExhaustedError(reset), "type": "rate_limit_error", "code": errVideoQuota,
			"retry_after": retryAfterSeconds(reset)}})
		return
	}
	writeJSON(w, 200, videoJobView(startVideoJob(model, prompt, size, images)))
}

// startVideoJob 建一个视频任务并在后台跑，立即返回（status=queued）。
func startVideoJob(model, prompt, size string, images []pendingUpload) *videoJob {
	j := &videoJob{
		Size:      size,
		ID:        "video_" + randHex(16),
		Object:    "video",
		Model:     model,
		Status:    "queued",
		CreatedAt: time.Now().Unix(),
		Prompt:    prompt,
	}
	putVideoJob(j)
	go runVideoJob(j, images)
	return j
}

// videoSize maps OpenAI's `size` ("1280x720"/"720x1280") or an `aspect_ratio`
// ("16:9"/"9:16") onto the two shapes Gemini web can produce. Default 16:9.
func videoSize(req map[string]any) (string, error) {
	s, _ := req["size"].(string)
	if s == "" {
		s, _ = req["aspect_ratio"].(string)
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "16:9", "landscape", "1280x720", "1920x1080":
		return "1280x720", nil
	case "9:16", "portrait", "720x1280", "1080x1920":
		return "720x1280", nil
	}
	return "", fmt.Errorf("unsupported size %q: use 1280x720 (16:9) or 720x1280 (9:16)", s)
}

// inputReferenceSrc pulls an image source out of the input_reference field.
func inputReferenceSrc(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case map[string]any:
		for _, k := range []string{"image_url", "url", "data"} {
			if s, ok := x[k].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
			if m, ok := x[k].(map[string]any); ok {
				if s, ok := m["url"].(string); ok {
					return strings.TrimSpace(s)
				}
			}
		}
	}
	return ""
}

// runVideoJob 后台跑视频生成，结果写回 job。
func runVideoJob(j *videoJob, images []pendingUpload) {
	runVideoJobWith(j, images, 0, nil)
}

// runVideoJobWith optionally pins the account and continues a conversation turn.
func runVideoJobWith(j *videoJob, images []pendingUpload, pin int64, cont *ConvRef) {
	videoJobsMu.Lock()
	j.Status = "in_progress"
	videoJobsMu.Unlock()

	_, mc, err := resolveModel(j.Model)
	if err != nil {
		finishVideoJob(j, nil, "", err.Error())
		return
	}
	if j.Size == "720x1280" {
		mc.VideoAspect = 17
	}
	mc.Cont = cont
	kept := !temporaryChatEligible() && !rtCfg().AutoDeleteConversation
	var res *StreamResult
	if pin > 0 {
		// An extension stays on the account and conversation of its parent; an exhausted
		// account is still remembered so new videos skip it.
		mc.PinAccountID = pin
		_, _, res, err = videoCall(j.Prompt, j.Prompt, mc, nil, images, nil, nil)
		if errors.Is(err, errVideoQuotaUsed) {
			markVideoExhausted(pin, videoQuotaReset(err))
		}
	} else {
		res, err = callOnVideoAccount(j, mc, images)
	}
	if res != nil {
		videoJobsMu.Lock()
		j.convKept = kept
		j.accountID = res.AccountID
		j.Account = res.AccountLabel
		if res.ConvCID != "" {
			j.cid, j.rid, j.rcid = res.ConvCID, res.ConvRID, res.ConvRCID
		}
		videoJobsMu.Unlock()
	}
	if err != nil {
		recordRequest("videos", j.Model, j.Prompt, "", res, 502, err.Error(), false)
		finishVideoJob(j, nil, "", err.Error())
		return
	}
	if res == nil || len(res.Artifacts) == 0 {
		recordRequest("videos", j.Model, j.Prompt, "", res, 502, "no video produced", false)
		finishVideoJob(j, nil, "", "no video produced")
		return
	}
	a := res.Artifacts[0]
	recordRequest("videos", j.Model, j.Prompt, "", res, 200, "", false)
	finishVideoJob(j, a.Data, a.Mime, "")
}

// videoCall is callGemini for video jobs; tests replace it.
var videoCall = callGemini

// callOnVideoAccount runs a new video on the least recently used account that is not in
// its quota cooldown. When that account turns out to be out of video quota (nothing is
// consumed) or not signed in, the same job moves on to the next account. With no account
// left it fails with the pool-exhausted error and the earliest reset.
func callOnVideoAccount(j *videoJob, mc ModelConfig, images []pendingUpload) (*StreamResult, error) {
	skip := videoCoolingAccounts()
	var res *StreamResult
	var err error
	for tries := len(accountList()); tries > 0; tries-- {
		acct, ok := pickCookieAccountExcept(skip)
		if !ok {
			break
		}
		skip[acct.ID] = true
		mc.PinAccountID = acct.ID
		_, _, res, err = videoCall(j.Prompt, j.Prompt, mc, nil, images, nil, nil)
		if err == nil {
			return res, nil
		}
		if errors.Is(err, errVideoQuotaUsed) {
			markVideoExhausted(acct.ID, videoQuotaReset(err))
			logf("[video] %s: 账号 #%d 额度用完，换下一个账号", j.ID, acct.ID)
			continue
		}
		var pinned *pinnedAccountError
		if errors.As(err, &pinned) {
			continue
		}
		return res, err
	}
	if exhausted, reset := videoPoolExhausted(); exhausted {
		return res, fmt.Errorf("%s", videoPoolExhaustedError(reset))
	}
	if err == nil {
		err = fmt.Errorf("no signed-in account is available for video")
	}
	return res, err
}

func finishVideoJob(j *videoJob, mp4 []byte, mime, errStr string) {
	videoJobsMu.Lock()
	defer videoJobsMu.Unlock()
	if errStr != "" {
		j.Status = "failed"
		j.Error = errStr
		return
	}
	j.mp4 = mp4
	j.mime = mime
	if j.mime == "" {
		j.mime = "video/mp4"
	}
	j.Seconds = mp4Duration(mp4)
	j.Status = "completed"
}

// handleExtendVideo 处理 POST /v1/videos/{id}/extend: continue a finished video in its
// own Gemini conversation (like replying "continue…" to the video in the web app).
// Runs on the same account that made the parent; that account's quota is used.
func handleExtendVideo(w http.ResponseWriter, r *http.Request, parentID string) {
	var req map[string]any
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"message": "bad json body"}})
		return
	}
	prompt, _ := req["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"message": "missing prompt (describe what happens next)", "type": "invalid_request_error"}})
		return
	}
	// Optional image (same shapes as input_reference) attached to the continuation turn,
	// e.g. a reference the prompt asks the next part to match.
	var images []pendingUpload
	if ref := inputReferenceSrc(req["input_reference"]); ref != "" {
		img, err := materializeImage(ref, "", 1)
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": map[string]string{"message": "input_reference: " + err.Error(), "type": "invalid_request_error"}})
			return
		}
		images = append(images, img)
	}
	p := getVideoJob(parentID)
	if p == nil {
		writeJSON(w, 404, map[string]any{"error": map[string]string{"message": "video job not found (jobs are kept 2 h in memory)", "type": "not_found"}})
		return
	}
	videoJobsMu.Lock()
	status, acct, cid, rid, rcid, turn, model, size := p.Status, p.accountID, p.cid, p.rid, p.rcid, p.turn, p.Model, p.Size
	kept := p.convKept
	videoJobsMu.Unlock()
	if status != "completed" {
		writeJSON(w, 409, map[string]any{"error": map[string]string{"message": "parent video is " + status + "; only completed videos can be extended", "type": "invalid_request_error"}})
		return
	}
	// 用户开的开关优先：临时对话不留会话、自动删会话出完就删，两种情况父会话都不在了，
	// 发出去只会在一条不存在的会话上白扣一次额度。
	if !kept {
		writeJSON(w, 409, map[string]any{"error": map[string]string{"message": "parent video's conversation was not kept upstream " +
			"(temporary_chat or auto_delete_conversation was on when it was made); extending needs both off", "type": "invalid_request_error"}})
		return
	}
	if acct == 0 || cid == "" || rid == "" || rcid == "" {
		writeJSON(w, 409, map[string]any{"error": map[string]string{"message": "parent video has no conversation reference to continue", "type": "invalid_request_error"}})
		return
	}
	j := &videoJob{
		ID:          "video_" + randHex(16),
		Object:      "video",
		Model:       model,
		Status:      "queued",
		CreatedAt:   time.Now().Unix(),
		Prompt:      prompt,
		Size:        size,
		RemixedFrom: parentID,
		turn:        turn + 1,
	}
	putVideoJob(j)
	go runVideoJobWith(j, images, acct, &ConvRef{CID: cid, RID: rid, RCID: rcid, Turn: turn + 1})
	writeJSON(w, 200, videoJobView(j))
}

// mp4Duration reads mvhd (movie header) duration; 0 if not found.
func mp4Duration(b []byte) float64 {
	i := strings.Index(string(b[:min(len(b), 1<<20)]), "mvhd")
	if i < 0 || i+24 > len(b) {
		return 0
	}
	box := b[i+4:]
	be32 := func(x []byte) uint64 { return uint64(x[0])<<24 | uint64(x[1])<<16 | uint64(x[2])<<8 | uint64(x[3]) }
	if box[0] == 1 { // version 1: 64-bit times
		if len(box) < 32 {
			return 0
		}
		ts := be32(box[20:24])
		dur := be32(box[24:28])<<32 | be32(box[28:32])
		if ts == 0 {
			return 0
		}
		return float64(dur) / float64(ts)
	}
	ts, dur := be32(box[12:16]), be32(box[16:20])
	if ts == 0 {
		return 0
	}
	return float64(dur) / float64(ts)
}

// handleVideoItem 处理 GET /v1/videos/{id} 和 /v1/videos/{id}/content。
func handleVideoItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/videos/")
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	if r.Method == http.MethodPost && len(parts) == 2 && (parts[1] == "extend" || parts[1] == "remix") {
		handleExtendVideo(w, r, id)
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]any{"error": map[string]string{"message": "method not allowed"}})
		return
	}
	j := getVideoJob(id)
	if j == nil {
		writeJSON(w, 404, map[string]any{"error": map[string]string{"message": "video job not found", "type": "not_found"}})
		return
	}
	if len(parts) == 2 && parts[1] == "content" {
		videoJobsMu.Lock()
		status, mp4, mime := j.Status, j.mp4, j.mime
		videoJobsMu.Unlock()
		if status != "completed" || len(mp4) == 0 {
			writeJSON(w, 404, map[string]any{"error": map[string]string{
				"message": "video not ready (status=" + status + ")", "type": "not_found"}})
			return
		}
		w.Header().Set("Content-Type", mime)
		w.WriteHeader(200)
		_, _ = w.Write(mp4)
		return
	}
	writeJSON(w, 200, videoJobView(j))
}
