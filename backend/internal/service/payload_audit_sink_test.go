package service

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func smallEvent() *PayloadAuditEvent {
	return &PayloadAuditEvent{
		RequestID:    "req-1",
		InputBody:    "hello",
		OutputBody:   "world",
		InputExcerpt: "h",
		CreatedAt:    time.Now(),
	}
}

func bigEvent(bodySize int) *PayloadAuditEvent {
	return &PayloadAuditEvent{
		RequestID:  "req-big",
		InputBody:  strings.Repeat("x", bodySize),
		OutputBody: strings.Repeat("y", bodySize),
		CreatedAt:  time.Now(),
	}
}

// fakeRepo records batch calls.
type fakeRepo struct {
	mu         sync.Mutex
	batches    []int // sizes of each BatchInsert call
	totalCount int
}

func (r *fakeRepo) BatchInsert(_ context.Context, events []*PayloadAuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batches = append(r.batches, len(events))
	r.totalCount += len(events)
	return nil
}

func (r *fakeRepo) BatchCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.batches)
}

func (r *fakeRepo) LastBatchSize() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.batches) == 0 {
		return 0
	}
	return r.batches[len(r.batches)-1]
}

func (r *fakeRepo) TotalInserted() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.totalCount
}

// alwaysPanicRepo always panics on BatchInsert.
type alwaysPanicRepo struct{}

func (r *alwaysPanicRepo) BatchInsert(_ context.Context, _ []*PayloadAuditEvent) error {
	panic("always panic")
}

// panickyRepo panics on BatchInsert until call count reaches successAt.
type panickyRepo struct {
	calls      atomic.Int64
	successAt  int64
	successCnt atomic.Int64
}

func (r *panickyRepo) BatchInsert(_ context.Context, events []*PayloadAuditEvent) error {
	n := r.calls.Add(1)
	if n < r.successAt {
		panic("intentional test panic")
	}
	r.successCnt.Add(int64(len(events)))
	return nil
}

func (r *panickyRepo) SuccessCount() int64 { return r.successCnt.Load() }

// eventually polls pred until it returns true or timeout expires.
func eventually(t *testing.T, timeout time.Duration, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("eventually timed out")
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestSink_TryEnqueueRespectsCount(t *testing.T) {
	sink := NewPayloadAuditSink(&fakeRepo{}, SinkConfig{
		WorkerCount: 0, QueueSize: 2, QueueMaxBytes: 1 << 30,
	})
	// Don't Start — queue won't be consumed.
	if !sink.TryEnqueue(smallEvent()) {
		t.Fatal("1st enqueue should succeed")
	}
	if !sink.TryEnqueue(smallEvent()) {
		t.Fatal("2nd enqueue should succeed")
	}
	if sink.TryEnqueue(smallEvent()) {
		t.Fatal("3rd enqueue should be rejected (queue full)")
	}
	if sink.Stats().DropQueueFull != 1 {
		t.Fatalf("expected DropQueueFull=1, got %d", sink.Stats().DropQueueFull)
	}
}

func TestSink_TryEnqueueRespectsBytes(t *testing.T) {
	sink := NewPayloadAuditSink(&fakeRepo{}, SinkConfig{
		WorkerCount: 0, QueueSize: 1000, QueueMaxBytes: 2048,
	})
	big := bigEvent(800)
	if !sink.TryEnqueue(big) {
		t.Fatal("1st big enqueue should succeed")
	}
	// eventSize(big) ≈ 800+800+256 = 1856. Second would push to 3712 > 2048.
	if sink.TryEnqueue(bigEvent(800)) {
		t.Fatal("2nd big enqueue should be rejected (byte budget)")
	}
	if sink.Stats().DropByteBudget < 1 {
		t.Fatalf("expected DropByteBudget>=1, got %d", sink.Stats().DropByteBudget)
	}
}

func TestSink_BatcherFlushBySize(t *testing.T) {
	repo := &fakeRepo{}
	sink := NewPayloadAuditSink(repo, SinkConfig{
		WorkerCount:  1,
		QueueSize:    100,
		QueueMaxBytes: 1 << 30,
		BatchSize:    3,
		BatchFlushMs: 60_000, // large interval — avoid time-triggered flush
	})
	sink.Start(context.Background())
	defer sink.Stop(context.Background(), 2*time.Second)
	for i := 0; i < 3; i++ {
		sink.TryEnqueue(smallEvent())
	}
	eventually(t, 2*time.Second, func() bool {
		return repo.BatchCount() >= 1 && repo.LastBatchSize() == 3
	})
}

func TestSink_BatcherFlushByTime(t *testing.T) {
	repo := &fakeRepo{}
	sink := NewPayloadAuditSink(repo, SinkConfig{
		WorkerCount:  1,
		QueueSize:    100,
		QueueMaxBytes: 1 << 30,
		BatchSize:    100, // large — won't fill up
		BatchFlushMs: 200,
	})
	sink.Start(context.Background())
	defer sink.Stop(context.Background(), 2*time.Second)
	sink.TryEnqueue(smallEvent())
	eventually(t, 2*time.Second, func() bool {
		return repo.BatchCount() >= 1
	})
}

func TestSink_WorkerPanicSelfRecovers(t *testing.T) {
	repo := &panickyRepo{successAt: 3} // panics on calls 1 and 2, succeeds from call 3
	sink := NewPayloadAuditSink(repo, SinkConfig{
		WorkerCount:  1,
		QueueSize:    10,
		QueueMaxBytes: 1 << 30,
		BatchSize:    1,
		BatchFlushMs: 100,
	})
	sink.Start(context.Background())
	defer sink.Stop(context.Background(), 2*time.Second)
	for i := 0; i < 3; i++ {
		sink.TryEnqueue(smallEvent())
	}
	eventually(t, 5*time.Second, func() bool {
		return repo.SuccessCount() >= 1
	})
	if sink.Stats().WorkerPanic == 0 {
		t.Fatal("expected at least one worker panic recorded")
	}
}

func TestSink_StopDrainsQueue(t *testing.T) {
	repo := &fakeRepo{}
	sink := NewPayloadAuditSink(repo, SinkConfig{
		WorkerCount:  1,
		QueueSize:    100,
		QueueMaxBytes: 1 << 30,
		BatchSize:    100,
		BatchFlushMs: 60_000,
	})
	sink.Start(context.Background())
	for i := 0; i < 5; i++ {
		sink.TryEnqueue(smallEvent())
	}
	remaining := sink.Stop(context.Background(), 3*time.Second)
	if len(remaining) != 0 {
		t.Fatalf("expected 0 remaining, got %d", len(remaining))
	}
	if repo.TotalInserted() != 5 {
		t.Fatalf("expected 5 inserted, got %d", repo.TotalInserted())
	}
}

func TestSink_NilEventRejected(t *testing.T) {
	sink := NewPayloadAuditSink(&fakeRepo{}, SinkConfig{QueueSize: 10})
	if sink.TryEnqueue(nil) {
		t.Fatal("nil event should be rejected")
	}
}

func TestSink_PanicReleasesByteBudget(t *testing.T) {
	repo := &alwaysPanicRepo{}
	sink := NewPayloadAuditSink(repo, SinkConfig{
		WorkerCount:   1,
		QueueSize:     100,
		QueueMaxBytes: 1024,
		BatchSize:     1,
		BatchFlushMs:  60_000,
	})
	sink.Start(context.Background())
	defer sink.Stop(context.Background(), 2*time.Second)

	// big event occupying ~956 bytes of the 1024 budget
	big := bigEvent(350)
	if !sink.TryEnqueue(big) {
		t.Fatal("1st enqueue should succeed")
	}

	// wait for worker to process (and panic), releasing the byte budget
	eventually(t, 3*time.Second, func() bool {
		return sink.Stats().WorkerPanic >= 1
	})

	// byte budget should have been released; second enqueue must succeed
	if !sink.TryEnqueue(bigEvent(350)) {
		t.Fatalf("byte budget should have been released after panic, stats=%+v", sink.Stats())
	}
}

func TestSink_StopIsIdempotent(t *testing.T) {
	sink := NewPayloadAuditSink(&fakeRepo{}, SinkConfig{
		WorkerCount:   1,
		QueueSize:     10,
		QueueMaxBytes: 1 << 30,
	})
	sink.Start(context.Background())
	sink.Stop(context.Background(), time.Second)
	// second Stop must not panic
	sink.Stop(context.Background(), time.Second)
}
