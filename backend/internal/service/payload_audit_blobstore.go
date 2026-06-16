package service

import (
	"bytes"
	"context"
	"io"
	"strings"
)

// PayloadAuditBlobStore 是旁路上传所需的最小对象存储能力。
type PayloadAuditBlobStore interface {
	Put(ctx context.Context, key string, data []byte, contentType string) error
	// Get downloads an object by key and returns its full contents.
	Get(ctx context.Context, key string) ([]byte, error)
	// GetStream opens an object by key and returns a streaming reader.
	// Caller must Close the returned reader. Used to relay large export
	// results (rendered by the external worker) without buffering in memory.
	GetStream(ctx context.Context, key string) (io.ReadCloser, error)
}

// blobKey / bodyKey 用内容寻址前缀分桶，天然去重。
func blobKey(prefix, sha string) string { return joinPrefix(prefix, "blobs", sha) }
func bodyKey(prefix, sha string) string { return joinPrefix(prefix, "bodies", sha) }

func joinPrefix(prefix, kind, sha string) string {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		prefix = "payload-audit"
	}
	shard := "00"
	if len(sha) >= 2 {
		shard = sha[:2]
	}
	return prefix + "/" + kind + "/" + shard + "/" + sha
}

// backupStoreAdapter 把 service.BackupObjectStore 适配为 PayloadAuditBlobStore。
type backupStoreAdapter struct{ inner BackupObjectStore }

func (a backupStoreAdapter) Put(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := a.inner.Upload(ctx, key, bytes.NewReader(data), contentType)
	return err
}

func (a backupStoreAdapter) Get(ctx context.Context, key string) ([]byte, error) {
	rc, err := a.inner.Download(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func (a backupStoreAdapter) GetStream(ctx context.Context, key string) (io.ReadCloser, error) {
	return a.inner.Download(ctx, key)
}

// NewPayloadAuditBlobStore 用现有工厂从独立配置构建对象存储。
// 备注：BackupObjectStore.Upload 内部 io.ReadAll，单对象瞬时双拷一次；已观测最大输入 24 MiB、
// worker 数有界，内存峰值可控；流式 PutObject 优化列为后续。
func NewPayloadAuditBlobStore(ctx context.Context, factory BackupObjectStoreFactory, cfg *BackupS3Config) (PayloadAuditBlobStore, error) {
	store, err := factory(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return backupStoreAdapter{inner: store}, nil
}
