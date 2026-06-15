package service

import (
	"context"
	"fmt"
	"sync"
)

// PayloadAuditUploader 用 per-sha singleflight 去重上传；成功后才记入 done。
//
// 注意：done map 无上限增长；对长期运行进程应换成 LRU 缓存（已知后续项）。
type PayloadAuditUploader struct {
	store  PayloadAuditBlobStore
	prefix string
	sem    chan struct{}

	mu     sync.Mutex
	done   map[string]struct{}    // 已确证上传成功的 sha
	flight map[string]*uploadCall // 进行中的上传
}

type uploadCall struct {
	wg  sync.WaitGroup
	err error
}

// NewPayloadAuditUploader 构造一个限并发的去重上传器。
func NewPayloadAuditUploader(store PayloadAuditBlobStore, prefix string, concurrency int) *PayloadAuditUploader {
	if concurrency < 1 {
		concurrency = 2
	}
	return &PayloadAuditUploader{
		store:  store,
		prefix: prefix,
		sem:    make(chan struct{}, concurrency),
		done:   map[string]struct{}{},
		flight: map[string]*uploadCall{},
	}
}

// PutBlob 幂等上传一个大对象；同 sha 并发只发一次。
func (u *PayloadAuditUploader) PutBlob(ctx context.Context, b ExtractedBlob) error {
	return u.put(ctx, blobKey(u.prefix, b.SHA256), b.SHA256, b.Data, b.MIME)
}

// PutBody 幂等上传一段超大正文。
func (u *PayloadAuditUploader) PutBody(ctx context.Context, sha string, data []byte) error {
	return u.put(ctx, bodyKey(u.prefix, sha), "body:"+sha, data, "application/json")
}

func (u *PayloadAuditUploader) put(ctx context.Context, key, dedupKey string, data []byte, ct string) (err error) {
	u.mu.Lock()
	if _, ok := u.done[dedupKey]; ok {
		u.mu.Unlock()
		return nil
	}
	if call, ok := u.flight[dedupKey]; ok {
		u.mu.Unlock()
		call.wg.Wait()
		return call.err
	}
	call := &uploadCall{}
	call.wg.Add(1)
	u.flight[dedupKey] = call
	u.mu.Unlock()

	u.sem <- struct{}{}
	// err is a NAMED return so the deferred recover below propagates a panic to
	// the caller as an error; with a local var, a panic would return nil and
	// settleOffload would wrongly commit a dangling pointer.
	// 用 defer 收尾，保证即便 store.Put panic 也能释放信号量、清理 flight、唤醒等待者，
	// 不会永久泄漏一个并发槽或让同 sha 的后续调用死等。panic 转成 error → 不写 done、可重试。
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("payload_audit upload panic: %v", r)
		}
		<-u.sem
		u.mu.Lock()
		delete(u.flight, dedupKey)
		if err == nil {
			u.done[dedupKey] = struct{}{} // 仅成功后入 done
		}
		u.mu.Unlock()
		call.err = err
		call.wg.Done()
	}()

	err = u.store.Put(ctx, key, data, ct)
	return err
}
