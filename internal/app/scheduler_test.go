package app

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestScheduler_EnqueueAndRelease(t *testing.T) {
	sched := &Scheduler{
		slotState: SlotIdle,
		queue:     make([]*queuedItem, 0),
	}
	sched.cond = syncNewCond(&sched.mu)
	go sched.loop()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Enqueue first request
	err := sched.Enqueue(ctx, "req-1", "proj-a", "gemini-3.6-flash", PriorityP2Standard, 100, true, "chat.completions", nil)
	if err != nil {
		t.Fatalf("unexpected error enqueuing req-1: %v", err)
	}

	active, queued, slot := sched.Snapshot()
	if len(active) != 1 || active[0].ID != "req-1" {
		t.Errorf("expected req-1 to be active, got: %+v", active)
	}
	if len(queued) != 0 {
		t.Errorf("expected empty queue, got %d items", len(queued))
	}
	if slot.State != SlotBusy {
		t.Errorf("expected slot state BUSY, got %v", slot.State)
	}

	// Release with 1 second cooldown
	sched.Release("req-1", 1)

	_, _, slotAfter := sched.Snapshot()
	if slotAfter.State != SlotCoolingDown {
		t.Errorf("expected slot state COOLING_DOWN, got %v", slotAfter.State)
	}

	// Wait for cooldown to elapse
	time.Sleep(1500 * time.Millisecond)

	_, _, slotIdle := sched.Snapshot()
	if slotIdle.State != SlotIdle {
		t.Errorf("expected slot state IDLE after cooldown, got %v", slotIdle.State)
	}
}

func TestScheduler_CancellationPurgesQueue(t *testing.T) {
	sched := &Scheduler{
		slotState: SlotBusy, // Busy so new requests must wait in queue
		queue:     make([]*queuedItem, 0),
	}
	sched.cond = syncNewCond(&sched.mu)
	go sched.loop()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		err := sched.Enqueue(ctx, "req-cancel", "proj-b", "gemini-4-neon-thinking", PriorityP1Blocker, 200, false, "chat.completions", nil)
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	_, queued, _ := sched.Snapshot()
	if len(queued) != 1 || queued[0].ID != "req-cancel" {
		t.Fatalf("expected req-cancel in queue, got: %+v", queued)
	}

	// Client cancels!
	cancel()

	err := <-done
	if err == nil {
		t.Errorf("expected error on context cancellation, got nil")
	}

	// Queue should now be empty
	_, queuedAfter, _ := sched.Snapshot()
	if len(queuedAfter) != 0 {
		t.Errorf("expected queue to be purged immediately, got %d items", len(queuedAfter))
	}
}

func syncNewCond(l sync.Locker) *sync.Cond {
	return sync.NewCond(l)
}

func TestScheduler_SequentialQueueWithPacing(t *testing.T) {
	sched := &Scheduler{
		slotState: SlotIdle,
		queue:     make([]*queuedItem, 0),
	}
	sched.cond = syncNewCond(&sched.mu)
	go sched.loop()

	order := make([]string, 0)
	var mu sync.Mutex

	ctx := context.Background()

	// Enqueue req-A
	errA := sched.Enqueue(ctx, "req-A", "proj-1", "gemini-3.6-flash", PriorityP2Standard, 100, false, "chat.completions", nil)
	if errA != nil {
		t.Fatalf("unexpected errA: %v", errA)
	}
	mu.Lock()
	order = append(order, "A-start")
	mu.Unlock()

	// While req-A is running, enqueue req-B
	doneB := make(chan struct{})
	go func() {
		errB := sched.Enqueue(ctx, "req-B", "proj-2", "gemini-3.6-flash", PriorityP2Standard, 100, false, "chat.completions", nil)
		if errB != nil {
			t.Errorf("unexpected errB: %v", errB)
		}
		mu.Lock()
		order = append(order, "B-start")
		mu.Unlock()
		close(doneB)
	}()

	time.Sleep(50 * time.Millisecond)

	// Verify req-B is queued
	_, queued, slot := sched.Snapshot()
	if slot.State != SlotBusy || slot.ActiveID != "req-A" {
		t.Errorf("expected slot to be BUSY with req-A, got: %+v", slot)
	}
	if len(queued) != 1 || queued[0].ID != "req-B" {
		t.Errorf("expected req-B in queue, got: %+v", queued)
	}

	// Complete req-A with 1s pacing
	sched.Release("req-A", 1)

	// Wait for req-B to be dispatched after cooldown
	<-doneB

	mu.Lock()
	if len(order) != 2 || order[0] != "A-start" || order[1] != "B-start" {
		t.Errorf("expected A then B execution, got: %v", order)
	}
	mu.Unlock()

	sched.Release("req-B", 1)
}

func TestScheduler_ConfigurableOptions(t *testing.T) {
	sched := &Scheduler{
		slotState: SlotIdle,
		queue:     make([]*queuedItem, 0),
	}
	sched.cond = sync.NewCond(&sched.mu)
	go sched.loop()

	ctx := context.Background()

	// 1. Acquire slot with a dummy job
	err := sched.Enqueue(ctx, "req-1", "p1", "model", PriorityP2Standard, 10, false, "chat", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	// 2. Set rtVal with QueueMaxDepth = 2
	rtMu.Lock()
	oldVal := rtVal
	rtVal.QueueEnabled = true
	rtVal.QueueMaxDepth = 2
	rtVal.PacingDelaySec = 1
	rtMu.Unlock()
	defer func() {
		rtMu.Lock()
		rtVal = oldVal
		rtMu.Unlock()
	}()

	// Enqueue 2 items (filling depth)
	go func() { _ = sched.Enqueue(ctx, "q-1", "p1", "model", PriorityP2Standard, 10, false, "chat", nil) }()
	go func() { _ = sched.Enqueue(ctx, "q-2", "p1", "model", PriorityP2Standard, 10, false, "chat", nil) }()
	time.Sleep(50 * time.Millisecond)

	// 3rd item should hit queue_full
	err3 := sched.Enqueue(ctx, "q-3", "p1", "model", PriorityP2Standard, 10, false, "chat", nil)
	if err3 == nil || !strings.Contains(err3.Error(), "queue_full") {
		t.Errorf("expected queue_full error, got: %v", err3)
	}

	// Clean up
	sched.Remove("q-1")
	sched.Remove("q-2")
	sched.Release("req-1", 0)
}

func TestScheduler_PriorityOrdering(t *testing.T) {
	sched := &Scheduler{
		slotState: SlotIdle,
		queue:     make([]*queuedItem, 0),
	}
	sched.cond = syncNewCond(&sched.mu)
	go sched.loop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Occupy slot with active job
	go func() { _ = sched.Enqueue(ctx, "req-active", "p1", "model", PriorityP2Standard, 10, false, "chat", nil) }()
	time.Sleep(20 * time.Millisecond)

	// Enqueue background low priority job first
	go func() { _ = sched.Enqueue(ctx, "req-low", "p1", "model", PriorityP3Background, 10, false, "chat", nil) }()
	time.Sleep(20 * time.Millisecond)

	// Enqueue human high priority job next
	go func() { _ = sched.Enqueue(ctx, "req-high", "p1", "model", PriorityP0Human, 10, false, "chat", nil) }()
	time.Sleep(20 * time.Millisecond)

	// High priority job should jump ahead of low priority job in the queue
	posHigh := sched.QueuePosition("req-high")
	posLow := sched.QueuePosition("req-low")

	if posHigh != 1 {
		t.Errorf("expected req-high to be at position 1, got %d", posHigh)
	}
	if posLow != 2 {
		t.Errorf("expected req-low to be at position 2, got %d", posLow)
	}

	sched.Remove("req-high")
	sched.Remove("req-low")
	sched.Release("req-active", 0)
}
