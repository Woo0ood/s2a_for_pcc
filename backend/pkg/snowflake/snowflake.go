// Package snowflake generates 64-bit monotonic IDs.
// Layout: 1 sign | 41 ts-ms-since-2024 | 10 worker | 12 seq
package snowflake

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	epochMs       int64 = 1704067200000 // 2024-01-01 UTC in ms
	workerBits    uint  = 10
	seqBits       uint  = 12
	maxWorker     int64 = (1 << workerBits) - 1
	maxSeq        int64 = (1 << seqBits) - 1
	workerShift         = seqBits
	tsShift             = seqBits + workerBits
	clockWaitMax        = 5 * time.Millisecond
)

// Generator is safe for concurrent use.
type Generator struct {
	mu       sync.Mutex
	workerID int64
	lastMs   int64
	seq      int64
}

// New returns a Generator. workerID must be in [0, 1023].
func New(workerID int64) (*Generator, error) {
	if workerID < 0 || workerID > maxWorker {
		return nil, fmt.Errorf("snowflake: worker id %d out of range [0,%d]", workerID, maxWorker)
	}
	return &Generator{workerID: workerID}, nil
}

// NextID returns the next monotonic ID.
func (g *Generator) NextID() (int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now().UnixMilli()
	if now < g.lastMs {
		wait := time.Duration(g.lastMs-now) * time.Millisecond
		if wait > clockWaitMax {
			return 0, errors.New("snowflake: clock moved backwards beyond tolerance")
		}
		time.Sleep(wait)
		now = time.Now().UnixMilli()
	}
	if now == g.lastMs {
		g.seq = (g.seq + 1) & maxSeq
		if g.seq == 0 {
			for now <= g.lastMs {
				now = time.Now().UnixMilli()
			}
		}
	} else {
		g.seq = 0
	}
	g.lastMs = now
	id := ((now - epochMs) << tsShift) | (g.workerID << workerShift) | g.seq
	return id, nil
}
