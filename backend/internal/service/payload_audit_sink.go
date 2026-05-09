package service

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// PayloadAuditRepository is the interface the sink uses to persist events.
// The wire layer provides an adapter that converts service.PayloadAuditEvent →
// repository.PayloadAuditEvent and delegates to repository.PayloadAuditRepo.BatchInsert.
type PayloadAuditRepository interface {
	BatchInsert(ctx context.Context, events []*PayloadAuditEvent) error
}

// SinkConfig controls the behaviour of PayloadAuditSink.
type SinkConfig struct {
	WorkerCount   int   // number of consumer goroutines (default 4)
	QueueSize     int   // channel capacity — count-based bound (default 32768)
	QueueMaxBytes int64 // total byte budget across all queued events (default 1 GiB)
	BatchSize     int   // max events per batch INSERT (default 100)
	BatchFlushMs  int   // max ms before a partial batch is flushed (default 200)
}

// SinkMetrics exposes live counters for observability.
type SinkMetrics struct {
	Accepted       atomic.Int64
	DropQueueFull  atomic.Int64
	DropByteBudget atomic.Int64
	BatchInserted  atomic.Int64
	BatchFailed    atomic.Int64
	DropOnShutdown atomic.Int64
	WorkerPanic    atomic.Int64
	QueueDepth     atomic.Int64
	QueueBytesUsed atomic.Int64
}

// SinkStats is a point-in-time snapshot of SinkMetrics.
type SinkStats struct {
	Accepted       int64
	DropQueueFull  int64
	DropByteBudget int64
	BatchInserted  int64
	BatchFailed    int64
	WorkerPanic    int64
	QueueDepth     int64
	QueueBytesUsed int64
	DropOnShutdown int64
}

// PayloadAuditSink is a non-blocking, double-bounded (count + bytes) async queue
// backed by a worker pool that batches events to the database.
type PayloadAuditSink struct {
	repo     PayloadAuditRepository
	cfg      SinkConfig
	queue    chan *PayloadAuditEvent
	byteUsed atomic.Int64
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
	metrics  SinkMetrics
	started  atomic.Bool
}

// eventSize returns an approximate in-memory cost of a single event.
func eventSize(e *PayloadAuditEvent) int64 {
	if e == nil {
		return 0
	}
	return int64(len(e.InputBody) + len(e.OutputBody) +
		len(e.InputExcerpt) + len(e.OutputExcerpt) + 256)
}

// NewPayloadAuditSink creates a new sink. Call Start to launch workers.
func NewPayloadAuditSink(repo PayloadAuditRepository, cfg SinkConfig) *PayloadAuditSink {
	if cfg.WorkerCount <= 0 {
		cfg.WorkerCount = 4
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 32768
	}
	if cfg.QueueMaxBytes <= 0 {
		cfg.QueueMaxBytes = 1 << 30 // 1 GiB
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.BatchFlushMs <= 0 {
		cfg.BatchFlushMs = 200
	}
	return &PayloadAuditSink{
		repo:   repo,
		cfg:    cfg,
		queue:  make(chan *PayloadAuditEvent, cfg.QueueSize),
		stopCh: make(chan struct{}),
	}
}

// TryEnqueue attempts a non-blocking enqueue. Returns false (and increments a
// drop counter) when the queue is full or the byte budget is exceeded.
func (s *PayloadAuditSink) TryEnqueue(evt *PayloadAuditEvent) bool {
	if evt == nil {
		return false
	}
	sz := eventSize(evt)
	if s.byteUsed.Add(sz) > s.cfg.QueueMaxBytes {
		s.byteUsed.Add(-sz)
		s.metrics.DropByteBudget.Add(1)
		return false
	}
	select {
	case s.queue <- evt:
		s.metrics.Accepted.Add(1)
		s.metrics.QueueDepth.Add(1)
		s.metrics.QueueBytesUsed.Store(s.byteUsed.Load())
		return true
	default:
		s.byteUsed.Add(-sz)
		s.metrics.DropQueueFull.Add(1)
		return false
	}
}

// Start launches the worker pool. Idempotent.
func (s *PayloadAuditSink) Start(ctx context.Context) {
	if !s.started.CompareAndSwap(false, true) {
		return
	}
	for i := 0; i < s.cfg.WorkerCount; i++ {
		s.wg.Add(1)
		go s.workerLoop(ctx, i)
	}
}

func (s *PayloadAuditSink) workerLoop(ctx context.Context, id int) {
	defer s.wg.Done()

	batch := make([]*PayloadAuditEvent, 0, s.cfg.BatchSize)

	defer func() {
		if r := recover(); r != nil {
			s.metrics.WorkerPanic.Add(1)
			slog.Error("payload_audit.worker_panic", "id", id, "panic", r, "in_flight_batch", len(batch))
			// release in-flight batch bytes + queue depth
			for _, e := range batch {
				s.byteUsed.Add(-eventSize(e))
				s.metrics.QueueDepth.Add(-1)
			}
			// restart unless shutting down
			select {
			case <-s.stopCh:
				return
			default:
				s.wg.Add(1)
				go s.workerLoop(ctx, id)
			}
		}
	}()
	flushInterval := time.Duration(s.cfg.BatchFlushMs) * time.Millisecond
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.flushBatch(ctx, batch); err != nil {
			if ctx.Err() != nil {
				// parent ctx cancelled (shutdown) — last-ditch attempt with background ctx
				cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err2 := s.repo.BatchInsert(cctx, batch); err2 != nil {
					slog.Warn("payload_audit.batch_failed_on_shutdown", "n", len(batch), "err", err2)
					s.metrics.BatchFailed.Add(int64(len(batch)))
				} else {
					s.metrics.BatchInserted.Add(int64(len(batch)))
				}
				cancel()
			} else {
				// retry once
				time.Sleep(100 * time.Millisecond)
				if err2 := s.flushBatch(ctx, batch); err2 != nil {
					slog.Warn("payload_audit.batch_failed", "n", len(batch), "err", err2)
					s.metrics.BatchFailed.Add(int64(len(batch)))
				}
			}
		}
		for _, e := range batch {
			s.byteUsed.Add(-eventSize(e))
			s.metrics.QueueDepth.Add(-1)
		}
		s.metrics.QueueBytesUsed.Store(s.byteUsed.Load())
		batch = batch[:0]
	}

	for {
		select {
		case evt := <-s.queue:
			batch = append(batch, evt)
			if len(batch) >= s.cfg.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-s.stopCh:
			// drain remaining then flush
			for {
				select {
				case evt := <-s.queue:
					batch = append(batch, evt)
					if len(batch) >= s.cfg.BatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

func (s *PayloadAuditSink) flushBatch(ctx context.Context, batch []*PayloadAuditEvent) error {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.repo.BatchInsert(cctx, batch); err != nil {
		return err
	}
	s.metrics.BatchInserted.Add(int64(len(batch)))
	return nil
}

// Stop signals workers to drain and flush, then waits up to deadline.
// Returns any events that could not be flushed before the deadline.
func (s *PayloadAuditSink) Stop(_ context.Context, deadline time.Duration) (remaining []*PayloadAuditEvent) {
	if !s.started.Load() {
		return nil
	}
	s.stopOnce.Do(func() { close(s.stopCh) })
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-time.After(deadline):
	}
	// force drain channel residue
	for {
		select {
		case evt := <-s.queue:
			remaining = append(remaining, evt)
			s.metrics.DropOnShutdown.Add(1)
		default:
			return remaining
		}
	}
}

// Stats returns a point-in-time snapshot of sink metrics.
func (s *PayloadAuditSink) Stats() SinkStats {
	return SinkStats{
		Accepted:       s.metrics.Accepted.Load(),
		DropQueueFull:  s.metrics.DropQueueFull.Load(),
		DropByteBudget: s.metrics.DropByteBudget.Load(),
		BatchInserted:  s.metrics.BatchInserted.Load(),
		BatchFailed:    s.metrics.BatchFailed.Load(),
		WorkerPanic:    s.metrics.WorkerPanic.Load(),
		QueueDepth:     s.metrics.QueueDepth.Load(),
		QueueBytesUsed: s.metrics.QueueBytesUsed.Load(),
		DropOnShutdown: s.metrics.DropOnShutdown.Load(),
	}
}
