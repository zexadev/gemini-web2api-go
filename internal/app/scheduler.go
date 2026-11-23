package app

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"time"
)

type Priority int

const (
	PriorityP0Human Priority = iota
	PriorityP1Blocker
	PriorityP2Standard
	PriorityP3Background
)

type SlotState string

const (
	SlotIdle        SlotState = "IDLE"
	SlotBusy        SlotState = "BUSY"
	SlotCoolingDown SlotState = "COOLING_DOWN"
)

// ActiveRequestView is a safe read-only snapshot for Admin UI / API.
type ActiveRequestView struct {
	ID            string    `json:"id"`
	ProjectID     string    `json:"project_id"`
	Model         string    `json:"model"`
	UpstreamModel string    `json:"upstream_model"`
	AccountLabel  string    `json:"account_label"`
	ProxyName     string    `json:"proxy_name"`
	StartedAt     time.Time `json:"started_at"`
	ElapsedSec    int       `json:"elapsed_sec"`
	PromptTokens  int       `json:"prompt_tokens"`
	OutputTokens  int       `json:"output_tokens"`
	Stream        bool      `json:"stream"`
	Endpoint      string    `json:"endpoint"`
}

// QueuedRequestView is a safe read-only snapshot for Admin UI / API.
type QueuedRequestView struct {
	ID           string    `json:"id"`
	ProjectID    string    `json:"project_id"`
	Model        string    `json:"model"`
	Priority     int       `json:"priority"`
	Position     int       `json:"position"`
	EnqueuedAt   time.Time `json:"enqueued_at"`
	WaitSec      int       `json:"wait_sec"`
	ETASec       int       `json:"eta_sec"`
	PromptTokens int       `json:"prompt_tokens"`
	Stream       bool      `json:"stream"`
	Endpoint     string    `json:"endpoint"`
}

// SlotStatusView reports the current slot state.
type SlotStatusView struct {
	State                SlotState `json:"state"`
	ActiveID             string    `json:"active_id,omitempty"`
	CooldownRemainingSec int       `json:"cooldown_remaining_sec"`
	QueueDepth           int       `json:"queue_depth"`
}

type queuedItem struct {
	id           string
	projectID    string
	model        string
	priority     Priority
	enqueuedAt   time.Time
	promptTokens int
	stream       bool
	endpoint     string
	readyCh      chan struct{}
	cancel       context.CancelFunc
}

type activeItem struct {
	id            string
	projectID     string
	model         string
	upstreamModel string
	accountLabel  string
	proxyName     string
	startedAt     time.Time
	promptTokens  int
	outputTokens  int
	stream        bool
	endpoint      string
	cancel        context.CancelFunc
}

// Scheduler serializes incoming requests to enforce strict concurrency = 1
// and automatic inter-request pacing cooldowns, eliminating 429s.
type Scheduler struct {
	mu            sync.Mutex
	slotState     SlotState
	cooldownUntil time.Time
	active        *activeItem
	queue         []*queuedItem
	cond          *sync.Cond
}

var globalScheduler *Scheduler
var schedulerOnce sync.Once

func startScheduler() {
	_ = GetScheduler()

	// Restore background maintenance and account token rotation loops
	go func() {
		// On boot: catch up any aggregations missed while service was down.
		aggregateHourlyCatchup()
		aggregateDailyCatchup()
		retentionSweep()

		hourly := time.NewTicker(5 * time.Minute)
		daily := time.NewTicker(1 * time.Hour)
		retention := time.NewTicker(6 * time.Hour)
		proxyReload := time.NewTicker(60 * time.Second)
		rotate := time.NewTimer(firstRotateDelay)
		defer hourly.Stop()
		defer daily.Stop()
		defer retention.Stop()
		defer proxyReload.Stop()
		defer rotate.Stop()

		for {
			select {
			case <-hourly.C:
				aggregateHourlyCatchup()
			case <-daily.C:
				aggregateDailyCatchup()
			case <-retention.C:
				retentionSweep()
			case <-proxyReload.C:
				loadProxies()
			case <-rotate.C:
				rotate.Reset(rotateAllAccounts())
			}
		}
	}()
}

func GetScheduler() *Scheduler {
	schedulerOnce.Do(func() {
		s := &Scheduler{
			slotState: SlotIdle,
			queue:     make([]*queuedItem, 0),
		}
		s.cond = sync.NewCond(&s.mu)
		go s.loop()
		globalScheduler = s
	})
	return globalScheduler
}

// loop runs as a background manager popping queued requests when the slot is IDLE.
func (s *Scheduler) loop() {
	for {
		s.mu.Lock()
		for {
			now := time.Now()
			// If cooling down, check if cooldown elapsed
			if s.slotState == SlotCoolingDown {
				if now.Before(s.cooldownUntil) {
					// Wait remaining cooldown time
					rem := s.cooldownUntil.Sub(now)
					s.mu.Unlock()
					time.Sleep(rem)
					s.mu.Lock()
					continue
				}
				// Cooldown finished -> transition to IDLE
				s.slotState = SlotIdle
				logf("[scheduler] Slot pacing cooldown completed; slot is now IDLE")
			}

			// If slot is IDLE and there are queued items, pop the head!
			if s.slotState == SlotIdle && len(s.queue) > 0 {
				item := s.queue[0]
				s.queue = s.queue[1:]
				s.slotState = SlotBusy
				s.active = &activeItem{
					id:           item.id,
					projectID:    item.projectID,
					model:        item.model,
					startedAt:    time.Now(),
					promptTokens: item.promptTokens,
					stream:       item.stream,
					endpoint:     item.endpoint,
					cancel:       item.cancel,
				}
				close(item.readyCh)
				logf("[scheduler] Dispatched queued request %s (model: %s)", item.id, item.model)
				break
			}

			// Otherwise wait for state change or new item
			s.cond.Wait()
		}
		s.mu.Unlock()
	}
}

// EnqueueParams encapsulates arguments for enqueuing a request into the scheduler.
type EnqueueParams struct {
	Ctx          context.Context
	ID           string
	ProjectID    string
	Model        string
	Priority     Priority
	PromptTokens int
	Stream       bool
	Endpoint     string
	OnQueuedTick func(pos, depth, etaSec int)
}

// Enqueue waits in the scheduler queue until the slot is acquired or ctx is canceled.
// onQueuedTick is called periodically while waiting (e.g. to emit SSE keepalive comments).
func (s *Scheduler) Enqueue(
	ctx context.Context,
	id, projectID, model string,
	priority Priority,
	promptTokens int,
	stream bool,
	endpoint string,
	onQueuedTick func(pos, depth, etaSec int),
) error {
	return s.EnqueueWithParams(EnqueueParams{
		Ctx:          ctx,
		ID:           id,
		ProjectID:    projectID,
		Model:        model,
		Priority:     priority,
		PromptTokens: promptTokens,
		Stream:       stream,
		Endpoint:     endpoint,
		OnQueuedTick: onQueuedTick,
	})
}

// EnqueueWithParams waits in the scheduler queue using structured parameters.
func (s *Scheduler) EnqueueWithParams(p EnqueueParams) error {
	readyCh := make(chan struct{})

	ctx := p.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	itemCtx, itemCancel := context.WithCancel(ctx)
	acquired := false
	defer func() {
		if !acquired {
			itemCancel()
		}
	}()

	item := &queuedItem{
		id:           p.ID,
		projectID:    p.ProjectID,
		model:        p.Model,
		priority:     p.Priority,
		enqueuedAt:   time.Now(),
		promptTokens: p.PromptTokens,
		stream:       p.Stream,
		endpoint:     p.Endpoint,
		readyCh:      readyCh,
		cancel:       itemCancel,
	}

	cfg := rtCfg()
	queueEnabled := cfg.QueueEnabled
	maxDepth := cfg.QueueMaxDepth
	if maxDepth <= 0 {
		maxDepth = 20
	}

	s.mu.Lock()
	if !queueEnabled && (s.slotState != SlotIdle || len(s.queue) > 0) {
		s.mu.Unlock()
		return fmt.Errorf("queue_disabled: concurrent requests blocked (concurrency = 1)")
	}

	if len(s.queue) >= maxDepth {
		s.mu.Unlock()
		return fmt.Errorf("queue_full: server queue limit reached (%d jobs waiting)", maxDepth)
	}

	// Insert into queue maintaining priority order (higher priority first, FIFO within same priority)
	insertIdx := len(s.queue)
	for i, qItem := range s.queue {
		if item.priority < qItem.priority {
			insertIdx = i
			break
		}
	}
	if insertIdx == len(s.queue) {
		s.queue = append(s.queue, item)
	} else {
		s.queue = append(s.queue[:insertIdx], append([]*queuedItem{item}, s.queue[insertIdx:]...)...)
	}
	s.cond.Broadcast()
	s.mu.Unlock()

	logf("[scheduler] Request %s enqueued at position %d", p.ID, s.QueuePosition(p.ID))

	hbSec := cfg.QueueHeartbeatSec
	if hbSec <= 0 {
		hbSec = 4
	}
	ticker := time.NewTicker(time.Duration(hbSec) * time.Second)
	defer ticker.Stop()

	// Initial notification
	if p.OnQueuedTick != nil {
		pos, depth, eta := s.GetQueueStats(p.ID)
		if pos > 0 {
			p.OnQueuedTick(pos, depth, eta)
		}
	}

	for {
		select {
		case <-itemCtx.Done():
			// Client disconnected, canceled, or timed out!
			s.Remove(p.ID)
			logf("[scheduler] Request %s canceled while in queue, purged cleanly", p.ID)
			return itemCtx.Err()

		case <-ticker.C:
			if p.OnQueuedTick != nil {
				pos, depth, eta := s.GetQueueStats(p.ID)
				if pos > 0 {
					p.OnQueuedTick(pos, depth, eta)
				}
			}

		case <-readyCh:
			// Slot acquired! Ready to execute!
			acquired = true
			return nil
		}
	}
}

// Release completes the active slot and enters the pacing cooldown.
func (s *Scheduler) Release(id string, pacingSec int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.active != nil && s.active.id == id {
		if s.active.cancel != nil {
			s.active.cancel()
		}
		s.active = nil
	}

	if pacingSec < 0 {
		pacingSec = rtCfg().PacingDelaySec
	}
	var jitter time.Duration
	if pacingSec >= 5 {
		jitter = time.Duration(rand.IntN(4000)) * time.Millisecond
	}
	cooldownDuration := time.Duration(pacingSec)*time.Second + jitter

	if cooldownDuration <= 0 {
		s.slotState = SlotIdle
		logf("[scheduler] Request %s finished; pacing cooldown disabled, slot is IDLE", id)
	} else {
		s.slotState = SlotCoolingDown
		s.cooldownUntil = time.Now().Add(cooldownDuration)
		logf("[scheduler] Request %s finished; entering pacing cooldown for %.1fs", id, cooldownDuration.Seconds())
	}

	s.cond.Broadcast()
}

// UpdateActive updates dynamic fields of the active request (e.g. output tokens, model).
func (s *Scheduler) UpdateActive(id string, upstreamModel string, outTokens int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil && s.active.id == id {
		if upstreamModel != "" {
			s.active.upstreamModel = upstreamModel
		}
		if outTokens > 0 {
			s.active.outputTokens = outTokens
		}
	}
}

// Remove removes an item from the queue by ID if it hasn't started yet and cancels its wait.
func (s *Scheduler) Remove(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, it := range s.queue {
		if it.id == id {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			if it.cancel != nil {
				it.cancel()
			}
			s.cond.Broadcast()
			return true
		}
	}
	return false
}

// CancelActiveOrQueued cancels either an active running request or a queued request.
func (s *Scheduler) CancelActiveOrQueued(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check queued
	for i, it := range s.queue {
		if it.id == id {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			if it.cancel != nil {
				it.cancel()
			}
			s.cond.Broadcast()
			return true
		}
	}

	// Check active
	if s.active != nil && s.active.id == id {
		if s.active.cancel != nil {
			s.active.cancel()
		}
		return true
	}

	return false
}

func (s *Scheduler) QueuePosition(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, it := range s.queue {
		if it.id == id {
			return i + 1
		}
	}
	return 0
}

func (s *Scheduler) GetQueueStats(id string) (pos int, depth int, etaSec int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	depth = len(s.queue)
	for i, it := range s.queue {
		if it.id == id {
			pos = i + 1
			break
		}
	}
	if pos == 0 {
		return 0, depth, 0
	}

	// Estimate: remaining cooldown + active job estimate (e.g. 90s) + (pos-1)*110s
	now := time.Now()
	remCooldown := 0
	if s.slotState == SlotCoolingDown && s.cooldownUntil.After(now) {
		remCooldown = int(s.cooldownUntil.Sub(now).Seconds())
	}
	activeEst := 0
	if s.slotState == SlotBusy {
		activeEst = 90
	}
	etaSec = remCooldown + activeEst + (pos-1)*110
	return pos, depth, etaSec
}

// Snapshot returns live state for Admin UI / API.
func (s *Scheduler) Snapshot() ([]ActiveRequestView, []QueuedRequestView, SlotStatusView) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	var activeViews []ActiveRequestView
	if s.active != nil {
		activeViews = append(activeViews, ActiveRequestView{
			ID:            s.active.id,
			ProjectID:     s.active.projectID,
			Model:         s.active.model,
			UpstreamModel: s.active.upstreamModel,
			AccountLabel:  s.active.accountLabel,
			ProxyName:     s.active.proxyName,
			StartedAt:     s.active.startedAt,
			ElapsedSec:    int(now.Sub(s.active.startedAt).Seconds()),
			PromptTokens:  s.active.promptTokens,
			OutputTokens:  s.active.outputTokens,
			Stream:        s.active.stream,
			Endpoint:      s.active.endpoint,
		})
	}

	var queuedViews []QueuedRequestView
	for i, it := range s.queue {
		pos := i + 1
		etaSec := (pos - 1) * 110
		if s.slotState == SlotBusy {
			etaSec += 90
		}
		if s.slotState == SlotCoolingDown && s.cooldownUntil.After(now) {
			etaSec += int(s.cooldownUntil.Sub(now).Seconds())
		}
		queuedViews = append(queuedViews, QueuedRequestView{
			ID:           it.id,
			ProjectID:    it.projectID,
			Model:        it.model,
			Priority:     int(it.priority),
			Position:     pos,
			EnqueuedAt:   it.enqueuedAt,
			WaitSec:      int(now.Sub(it.enqueuedAt).Seconds()),
			ETASec:       etaSec,
			PromptTokens: it.promptTokens,
			Stream:       it.stream,
			Endpoint:     it.endpoint,
		})
	}

	remCooldown := 0
	if s.slotState == SlotCoolingDown && s.cooldownUntil.After(now) {
		remCooldown = int(s.cooldownUntil.Sub(now).Seconds())
	}

	activeID := ""
	if s.active != nil {
		activeID = s.active.id
	}

	slotView := SlotStatusView{
		State:                s.slotState,
		ActiveID:             activeID,
		CooldownRemainingSec: remCooldown,
		QueueDepth:           len(s.queue),
	}

	return activeViews, queuedViews, slotView
}

// aggregateHourlyCatchup walks every hour bucket from the latest aggregated
// hour up to (now - 1h), so a long downtime catches up cleanly.
func aggregateHourlyCatchup() {
	var lastBucket int64
	_ = getDB().QueryRow(`SELECT COALESCE(MAX(bucket), 0) FROM stats_hourly`).Scan(&lastBucket)
	now := time.Now().Unix()
	currentHourStart := now - (now % 3600)
	target := currentHourStart - 3600 // most recent CLOSED hour
	if lastBucket == 0 {
		// First run — bootstrap from oldest request available.
		var minTS int64
		_ = getDB().QueryRow(`SELECT COALESCE(MIN(ts), 0) FROM requests`).Scan(&minTS)
		if minTS == 0 {
			return
		}
		lastBucket = minTS - (minTS % 3600) - 3600
	}
	for b := lastBucket + 3600; b <= target; b += 3600 {
		aggregateBucket(b, 3600, "stats_hourly")
	}
}

func aggregateDailyCatchup() {
	var lastBucket int64
	_ = getDB().QueryRow(`SELECT COALESCE(MAX(bucket), 0) FROM stats_daily`).Scan(&lastBucket)
	now := time.Now().Unix()
	currentDayStart := now - (now % 86400)
	target := currentDayStart - 86400
	if lastBucket == 0 {
		var minTS int64
		_ = getDB().QueryRow(`SELECT COALESCE(MIN(bucket), 0) FROM stats_hourly`).Scan(&minTS)
		if minTS == 0 {
			return
		}
		lastBucket = minTS - (minTS % 86400) - 86400
	}
	for b := lastBucket + 86400; b <= target; b += 86400 {
		aggregateBucket(b, 86400, "stats_daily")
	}
}

// aggregateBucket reads raw requests in [start, start+span) and groups by
// (model, proxy_id), writing one row per group.
// Source for hourly = requests; for daily = stats_hourly (already aggregated).
func aggregateBucket(start, span int64, target string) {
	end := start + span

	// We pre-aggregate latencies in Go to compute p50/p95 (SQLite percentile is awkward).
	type key struct {
		model string
		proxy int64
	}
	type acc struct {
		count, success, fail   int
		totalMs, promptT, outT int64
		latencies              []int
	}
	groups := map[key]*acc{}

	var rows interface {
		Close() error
		Next() bool
		Scan(...interface{}) error
	}
	var err error

	if target == "stats_hourly" {
		// from raw requests
		r, e := getDB().Query(`SELECT model, COALESCE(proxy_id,0), status, total_ms, prompt_tokens, output_tokens
			FROM requests WHERE ts >= ? AND ts < ?`, start, end)
		err = e
		rows = r
	} else {
		// from hourly aggregates
		r, e := getDB().Query(`SELECT model, proxy_id, requests, successes, failures, total_ms, prompt_tokens, output_tokens, p50_ms, p95_ms
			FROM stats_hourly WHERE bucket >= ? AND bucket < ?`, start, end)
		err = e
		rows = r
	}
	if err != nil {
		logf("[agg] %s query failed: %v", target, err)
		return
	}
	defer rows.Close()

	if target == "stats_hourly" {
		for rows.Next() {
			var model string
			var proxyID int64
			var status int
			var totalMs, pt, ot int64
			if err := rows.Scan(&model, &proxyID, &status, &totalMs, &pt, &ot); err != nil {
				continue
			}
			k := key{model, proxyID}
			a := groups[k]
			if a == nil {
				a = &acc{}
				groups[k] = a
			}
			a.count++
			if status == 200 {
				a.success++
			} else {
				a.fail++
			}
			a.totalMs += totalMs
			a.promptT += pt
			a.outT += ot
			a.latencies = append(a.latencies, int(totalMs))
		}
	} else {
		// daily: roll up hourly rows. p50/p95 = avg of hourly p50/p95 (good enough).
		type dailyAcc struct {
			acc
			p50Sum, p95Sum, p50N, p95N int
		}
		dgroups := map[key]*dailyAcc{}
		for rows.Next() {
			var model string
			var proxyID int64
			var requests, successes, failures int
			var totalMs, pt, ot int64
			var p50, p95 int
			if err := rows.Scan(&model, &proxyID, &requests, &successes, &failures, &totalMs, &pt, &ot, &p50, &p95); err != nil {
				continue
			}
			k := key{model, proxyID}
			a := dgroups[k]
			if a == nil {
				a = &dailyAcc{}
				dgroups[k] = a
			}
			a.count += requests
			a.success += successes
			a.fail += failures
			a.totalMs += totalMs
			a.promptT += pt
			a.outT += ot
			if p50 > 0 {
				a.p50Sum += p50
				a.p50N++
			}
			if p95 > 0 {
				a.p95Sum += p95
				a.p95N++
			}
		}
		tx, err := getDB().Begin()
		if err != nil {
			return
		}
		for k, a := range dgroups {
			p50 := 0
			if a.p50N > 0 {
				p50 = a.p50Sum / a.p50N
			}
			p95 := 0
			if a.p95N > 0 {
				p95 = a.p95Sum / a.p95N
			}
			_, _ = tx.Exec(upsert("stats_daily",
				[]string{"bucket", "model", "proxy_id", "requests", "successes", "failures",
					"total_ms", "p50_ms", "p95_ms", "prompt_tokens", "output_tokens"},
				[]string{"bucket", "model", "proxy_id"}),
				start, k.model, k.proxy,
				a.count, a.success, a.fail, a.totalMs, p50, p95, a.promptT, a.outT)
		}
		_ = tx.Commit()
		return
	}

	// hourly bucket commit
	tx, err := getDB().Begin()
	if err != nil {
		return
	}
	for k, a := range groups {
		p50, p95 := percentiles(a.latencies)
		_, _ = tx.Exec(upsert("stats_hourly",
			[]string{"bucket", "model", "proxy_id", "requests", "successes", "failures",
				"total_ms", "p50_ms", "p95_ms", "prompt_tokens", "output_tokens"},
			[]string{"bucket", "model", "proxy_id"}),
			start, k.model, k.proxy,
			a.count, a.success, a.fail, a.totalMs, p50, p95, a.promptT, a.outT)
	}
	_ = tx.Commit()
}

func percentiles(latencies []int) (p50, p95 int) {
	if len(latencies) == 0 {
		return 0, 0
	}
	sort.Ints(latencies)
	p50 = latencies[len(latencies)*50/100]
	p95idx := len(latencies) * 95 / 100
	if p95idx >= len(latencies) {
		p95idx = len(latencies) - 1
	}
	p95 = latencies[p95idx]
	return
}

// retentionSweep deletes raw requests older than RetentionDays and expired sessions.
func retentionSweep() {
	if rtCfg().RetentionDays <= 0 {
		return
	}
	cutoff := time.Now().Unix() - int64(rtCfg().RetentionDays)*86400
	res, err := getDB().Exec(`DELETE FROM requests WHERE ts < ?`, cutoff)
	if err != nil {
		logf("[retention] failed: %v", err)
		return
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		logf("[retention] purged %d old request rows", n)
	}
	_, _ = getDB().Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())
}
