package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

type fakeBlobStore struct {
	mu   sync.Mutex
	puts int32
	keys map[string]int
	fail bool
}

func (f *fakeBlobStore) Put(_ context.Context, key string, _ []byte, _ string) error {
	atomic.AddInt32(&f.puts, 1)
	if f.fail {
		return context.DeadlineExceeded
	}
	f.mu.Lock()
	if f.keys == nil {
		f.keys = map[string]int{}
	}
	f.keys[key]++
	f.mu.Unlock()
	return nil
}

func TestUploader_DedupSameSHA(t *testing.T) {
	fs := &fakeBlobStore{}
	up := NewPayloadAuditUploader(fs, "payload-audit/", 4)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = up.PutBlob(context.Background(), ExtractedBlob{SHA256: "same", MIME: "image/png", Bytes: 3, Data: []byte("abc")})
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&fs.puts); got != 1 {
		t.Fatalf("expected 1 PUT due to dedup, got %d", got)
	}
}

func TestUploader_FailureNotCached(t *testing.T) {
	fs := &fakeBlobStore{fail: true}
	up := NewPayloadAuditUploader(fs, "payload-audit/", 2)
	if err := up.PutBlob(context.Background(), ExtractedBlob{SHA256: "x", Data: []byte("y")}); err == nil {
		t.Fatal("expected error")
	}
	fs.fail = false
	if err := up.PutBlob(context.Background(), ExtractedBlob{SHA256: "x", Data: []byte("y")}); err != nil {
		t.Fatalf("retry after failure should succeed, got %v", err)
	}
}
