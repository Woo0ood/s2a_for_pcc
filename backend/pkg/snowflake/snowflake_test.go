package snowflake

import (
	"sync"
	"testing"
	"time"
)

func TestGeneratorMonotonic(t *testing.T) {
	g, err := New(1)
	if err != nil {
		t.Fatal(err)
	}
	var prev int64
	for i := 0; i < 100000; i++ {
		id, err := g.NextID()
		if err != nil {
			t.Fatalf("NextID: %v", err)
		}
		if id <= prev {
			t.Fatalf("not monotonic: %d <= %d", id, prev)
		}
		prev = id
	}
}

func TestGeneratorWorkerIsolation(t *testing.T) {
	g1, _ := New(1)
	g2, _ := New(2)
	id1, _ := g1.NextID()
	id2, _ := g2.NextID()
	if id1>>12&0x3FF == id2>>12&0x3FF {
		t.Fatal("worker ids should differ")
	}
}

func TestGeneratorConcurrency(t *testing.T) {
	g, _ := New(3)
	const n = 10000
	const goroutines = 8
	seen := sync.Map{}
	var wg sync.WaitGroup
	for w := 0; w < goroutines; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < n; i++ {
				id, _ := g.NextID()
				if _, loaded := seen.LoadOrStore(id, true); loaded {
					t.Errorf("duplicate id: %d", id)
				}
			}
		}()
	}
	wg.Wait()
}

func TestInvalidWorkerID(t *testing.T) {
	if _, err := New(-1); err == nil {
		t.Fatal("want err for negative worker")
	}
	if _, err := New(1024); err == nil {
		t.Fatal("want err for worker >= 1024")
	}
}

func TestTimestampInID(t *testing.T) {
	g, _ := New(0)
	id, _ := g.NextID()
	tsMs := id >> 22
	got := time.UnixMilli(epochMs + tsMs)
	if time.Since(got) > 5*time.Second {
		t.Fatalf("ts too far off: %v", got)
	}
}
