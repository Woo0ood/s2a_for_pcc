package service

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 目标 < 50 ns/op
func BenchmarkCollector_DisabledFastPath(b *testing.B) {
	c := NewPayloadAuditCollector(nil) // disabled
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.AppendOutput("hello")
	}
}

// 目标 < 100 ns/op
func BenchmarkCollector_EnabledAppend(b *testing.B) {
	snap := &ConfigSnapshot{
		Enabled:        true,
		AllGroups:      true,
		OutputMaxBytes: 1 << 20,
		ExcerptBytes:   512,
		Generation:     1,
	}
	c := NewPayloadAuditCollector(snap)
	c.SetMetadata(PayloadAuditMetadata{})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.AppendOutput("hello world delta")
	}
}

// 目标 < 50 µs/op（含 SetInput 拷贝 10KB）
func BenchmarkCollector_FinalizeOnly(b *testing.B) {
	snap := &ConfigSnapshot{
		Enabled:        true,
		AllGroups:      true,
		OutputMaxBytes: 1 << 20,
		ExcerptBytes:   512,
		InputMaxBytes:  1 << 20,
		Generation:     1,
	}
	body := []byte(strings.Repeat("a", 10*1024)) // 10KB
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := NewPayloadAuditCollector(snap)
		c.SetMetadata(PayloadAuditMetadata{Provider: "openai", Endpoint: "/v1/chat/completions"})
		c.SetInput(body, "json")
		c.AppendOutput("hello")
		_ = c.Finalize(200, time.Second, "")
	}
}

// 目标：单 worker 1k/s，4 worker 应 4k+/s
func BenchmarkSink_Throughput(b *testing.B) {
	repo := &noopBenchRepo{}
	sink := NewPayloadAuditSink(repo, SinkConfig{
		WorkerCount:   4,
		QueueSize:     10000,
		QueueMaxBytes: 1 << 30,
		BatchSize:     100,
		BatchFlushMs:  50,
	})
	sink.Start(context.Background())
	defer sink.Stop(context.Background(), 5*time.Second)

	snap := &ConfigSnapshot{
		Enabled:        true,
		AllGroups:      true,
		OutputMaxBytes: 1 << 20,
		ExcerptBytes:   512,
		InputMaxBytes:  1 << 20,
		Generation:     1,
	}
	body := []byte(strings.Repeat("a", 10*1024))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := NewPayloadAuditCollector(snap)
		c.SetMetadata(PayloadAuditMetadata{Provider: "openai", Endpoint: "/v1/chat/completions"})
		c.SetInput(body, "json")
		evt := c.Finalize(200, time.Second, "")
		sink.TryEnqueue(evt)
	}
}

type noopBenchRepo struct{}

func (r *noopBenchRepo) BatchInsert(_ context.Context, _ []*PayloadAuditEvent) error {
	return nil
}

func (r *noopBenchRepo) BatchInsertWithToken(_ context.Context, _ []*PayloadAuditEvent, _ string) error {
	return nil
}
