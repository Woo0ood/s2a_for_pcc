package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9" //nolint:depguard // payload audit owns redis interaction
)

const (
	redisKeyShutdownBuffer = "payload_audit:shutdown_buffer"
	drainChunkBytes        = 4 * 1024 * 1024 // 4 MB
	drainChunkCount        = 50
	recoverPopBatch        = 100
)

// PayloadAuditRedisBuffer is used only at shutdown / startup to persist
// remaining audit events that could not be flushed to the database in time.
// It is NOT on the hot path.
type PayloadAuditRedisBuffer struct {
	rdb *redis.Client
	key string
}

// NewPayloadAuditRedisBuffer creates a buffer backed by the given Redis client.
func NewPayloadAuditRedisBuffer(rdb *redis.Client) *PayloadAuditRedisBuffer {
	return &PayloadAuditRedisBuffer{rdb: rdb, key: redisKeyShutdownBuffer}
}

// PartialDrainError is returned when DrainBatch could not push all events
// before the deadline expired or a chunk failed.
type PartialDrainError struct {
	Drained int
	Total   int
	Cause   error
}

func (e *PartialDrainError) Error() string {
	return fmt.Sprintf("partial drain: %d/%d events drained: %v", e.Drained, e.Total, e.Cause)
}

func (e *PartialDrainError) Unwrap() error { return e.Cause }

// DrainBatch pushes events into a Redis list in chunks, respecting both a
// per-chunk byte limit and a per-chunk count limit.
//
// deadline caps the total wall-clock time; if exceeded the method returns a
// PartialDrainError with the number of events already drained.
// Individual chunk failures are logged and cause an immediate return (no retry).
func (b *PayloadAuditRedisBuffer) DrainBatch(ctx context.Context, events []*PayloadAuditEvent, deadline time.Duration) error {
	if len(events) == 0 {
		return nil
	}
	if b.rdb == nil {
		return errors.New("redis client not configured")
	}

	cctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	drained := 0
	chunk := make([]any, 0, drainChunkCount)
	chunkBytes := 0

	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		if cctx.Err() != nil {
			return cctx.Err()
		}
		if err := b.rdb.LPush(cctx, b.key, chunk...).Err(); err != nil {
			slog.Warn("payload_audit.redis_drain_chunk_fail", "n", len(chunk), "err", err)
			return err
		}
		drained += len(chunk)
		chunk = chunk[:0]
		chunkBytes = 0
		return nil
	}

	for _, e := range events {
		if cctx.Err() != nil {
			return &PartialDrainError{Drained: drained, Total: len(events), Cause: cctx.Err()}
		}
		data, err := json.Marshal(e)
		if err != nil {
			slog.Warn("payload_audit.redis_drain_marshal_fail", "err", err)
			continue // skip this event
		}
		if chunkBytes+len(data) > drainChunkBytes || len(chunk) >= drainChunkCount {
			if err := flush(); err != nil {
				return &PartialDrainError{Drained: drained, Total: len(events), Cause: err}
			}
		}
		chunk = append(chunk, data)
		chunkBytes += len(data)
	}
	if err := flush(); err != nil {
		return &PartialDrainError{Drained: drained, Total: len(events), Cause: err}
	}
	return nil
}

// Recover reads all events back from the Redis list (called once at startup).
// Returns nil, nil when the key does not exist or is empty.
// After all items are popped the key is explicitly deleted (belt-and-suspenders).
func (b *PayloadAuditRedisBuffer) Recover(ctx context.Context) ([]*PayloadAuditEvent, error) {
	if b.rdb == nil {
		return nil, errors.New("redis client not configured")
	}
	var out []*PayloadAuditEvent
	for {
		items, err := b.rdb.RPopCount(ctx, b.key, recoverPopBatch).Result()
		if errors.Is(err, redis.Nil) || len(items) == 0 {
			break
		}
		if err != nil {
			return out, err // partial recovery + error
		}
		for _, raw := range items {
			var e PayloadAuditEvent
			if err := json.Unmarshal([]byte(raw), &e); err != nil {
				slog.Warn("payload_audit.redis_recover_unmarshal_fail", "err", err)
				continue
			}
			out = append(out, &e)
		}
	}
	// Explicit delete — list should already be empty but delete as a safeguard.
	_ = b.rdb.Del(ctx, b.key).Err()
	return out, nil
}
