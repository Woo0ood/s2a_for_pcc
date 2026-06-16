package service

import (
	"context"
	"io"
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

func (f *fakeBlobStore) Get(_ context.Context, _ string) ([]byte, error) { return nil, nil }
func (f *fakeBlobStore) GetStream(_ context.Context, _ string) (io.ReadCloser, error) {
	return nil, nil
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

type panicBlobStore struct{}

func (panicBlobStore) Put(_ context.Context, _ string, _ []byte, _ string) error { panic("boom") }
func (panicBlobStore) Get(_ context.Context, _ string) ([]byte, error)           { return nil, nil }
func (panicBlobStore) GetStream(_ context.Context, _ string) (io.ReadCloser, error) {
	return nil, nil
}

func TestUploader_PanicSurfacesAsError(t *testing.T) {
	up := NewPayloadAuditUploader(panicBlobStore{}, "payload-audit/", 2)
	// A panic in store.Put MUST surface as a non-nil error (named return),
	// otherwise settleOffload would treat it as success and commit a dangling pointer.
	if err := up.PutBlob(context.Background(), ExtractedBlob{SHA256: "p", Data: []byte("x")}); err == nil {
		t.Fatal("panic in store.Put must return an error, got nil")
	}
	// Not cached as done → a retry attempts again rather than silently succeeding.
	if err := up.PutBlob(context.Background(), ExtractedBlob{SHA256: "p", Data: []byte("x")}); err == nil {
		t.Fatal("retry after panic must still error (not cached as done)")
	}
}
