package handler

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// fakePutStore implements service.PayloadAuditBlobStore for settle tests.
type fakePutStore struct{ fail bool }

func (f fakePutStore) Put(_ context.Context, _ string, _ []byte, _ string) error {
	if f.fail {
		return errors.New("upload boom")
	}
	return nil
}

func (f fakePutStore) Get(_ context.Context, _ string) ([]byte, error) {
	return nil, errors.New("fakePutStore: Get not implemented")
}

func (f fakePutStore) GetStream(_ context.Context, _ string) (io.ReadCloser, error) {
	return nil, errors.New("fakePutStore: GetStream not implemented")
}

func offloadCollectorWithPending(t *testing.T) *service.PayloadAuditCollector {
	t.Helper()
	snap := &service.ConfigSnapshot{Enabled: true, AllGroups: true, OffloadEnabled: true, BlobOffloadMinBytes: 512, InputMaxBytes: 0}
	coll := service.NewPayloadAuditCollector(snap)
	coll.SetMetadata(service.PayloadAuditMetadata{Provider: "openai", Endpoint: "/v1/responses"})
	raw := make([]byte, 1024)
	body := []byte(`{"image_url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString(raw) + `"}`)
	coll.SetInput(body, "json")
	if len(coll.PendingBlobs()) != 1 {
		t.Fatalf("setup: want 1 pending blob, got %d", len(coll.PendingBlobs()))
	}
	return coll
}

func TestSettleOffload_SuccessCommitsPointer(t *testing.T) {
	coll := offloadCollectorWithPending(t)
	up := service.NewPayloadAuditUploader(fakePutStore{}, "payload-audit/", 1)
	settleOffload(coll, up)
	evt := coll.Finalize(200, 0, "")
	if evt == nil || !evt.InputOffloaded {
		t.Fatalf("expected InputOffloaded=true on success; evt=%+v", evt)
	}
	if !strings.Contains(evt.InputBody, "s2a-blob://") {
		t.Fatalf("expected pointer in body, got %q", evt.InputBody)
	}
}

func TestSettleOffload_FailureRevertsInline(t *testing.T) {
	coll := offloadCollectorWithPending(t)
	up := service.NewPayloadAuditUploader(fakePutStore{fail: true}, "payload-audit/", 1)
	settleOffload(coll, up)
	evt := coll.Finalize(200, 0, "")
	if evt == nil || evt.InputOffloaded {
		t.Fatalf("expected InputOffloaded=false on upload failure; evt=%+v", evt)
	}
	if strings.Contains(evt.InputBody, "s2a-blob://") {
		t.Fatalf("expected inline body after revert, got pointer: %q", evt.InputBody)
	}
	if !strings.Contains(evt.InputBody, "data:image/png;base64,") {
		t.Fatalf("expected original base64 restored after revert, got %q", evt.InputBody)
	}
}

func TestSettleOffload_NilUploaderRevertsInline(t *testing.T) {
	coll := offloadCollectorWithPending(t)
	settleOffload(coll, nil) // misconfig: staged but no uploader
	evt := coll.Finalize(200, 0, "")
	if evt.InputOffloaded {
		t.Fatal("nil uploader must not mark offloaded")
	}
	if strings.Contains(evt.InputBody, "s2a-blob://") {
		t.Fatalf("nil uploader must revert to inline (no dangling pointer), got: %q", evt.InputBody)
	}
}
