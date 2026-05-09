//go:build integration

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

const redisImageTagAudit = "redis:8.4-alpine"

func startRedis(t *testing.T) *redis.Client {
	t.Helper()
	ensureDockerAvailable(t)

	ctx := context.Background()
	redisContainer, err := tcredis.Run(ctx, redisImageTagAudit)
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisContainer.Terminate(ctx) })

	redisHost, err := redisContainer.Host(ctx)
	require.NoError(t, err)
	redisPort, err := redisContainer.MappedPort(ctx, "6379/tcp")
	require.NoError(t, err)

	rdb := redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%d", redisHost, redisPort.Int()),
		DB:   0,
	})
	require.NoError(t, rdb.Ping(ctx).Err())
	t.Cleanup(func() { _ = rdb.Close() })

	return rdb
}

func ensureDockerAvailable(t *testing.T) {
	t.Helper()
	if dockerAvailable() {
		return
	}
	t.Skip("Docker 未启用，跳过依赖 testcontainers 的集成测试")
}

func dockerAvailable() bool {
	if os.Getenv("DOCKER_HOST") != "" {
		return true
	}
	socketCandidates := []string{
		"/var/run/docker.sock",
		filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "docker.sock"),
		filepath.Join(userHomeDir(), ".docker", "run", "docker.sock"),
		filepath.Join(userHomeDir(), ".docker", "desktop", "docker.sock"),
		filepath.Join("/run/user", strconv.Itoa(os.Getuid()), "docker.sock"),
	}
	for _, socket := range socketCandidates {
		if socket == "" {
			continue
		}
		if _, err := os.Stat(socket); err == nil {
			return true
		}
	}
	return false
}

func userHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

func redisTestEvent(requestID string) *PayloadAuditEvent {
	return &PayloadAuditEvent{
		RequestID:    requestID,
		Endpoint:     "/v1/chat/completions",
		Provider:     "openai",
		Model:        "gpt-4",
		StatusCode:   200,
		DurationMs:   100,
		InputExcerpt: "hello",
		InputBody:    `{"messages":[{"role":"user","content":"hello"}]}`,
		InputFormat:  "json",
		InputBytes:   48,
		CreatedAt:    time.Now(),
	}
}

func redisTestBigEvent(idx int, size int) *PayloadAuditEvent {
	body := strings.Repeat("x", size)
	return &PayloadAuditEvent{
		RequestID:  fmt.Sprintf("req-big-%d", idx),
		Endpoint:   "/v1/chat/completions",
		Provider:   "openai",
		Model:      "gpt-4",
		StatusCode: 200,
		DurationMs: 50,
		InputBody:  body,
		InputBytes: size,
		InputFormat: "json",
		CreatedAt:  time.Now(),
	}
}

func TestRedisBuffer_DrainAndRecover(t *testing.T) {
	rdb := startRedis(t)
	buf := NewPayloadAuditRedisBuffer(rdb)

	events := []*PayloadAuditEvent{
		redisTestEvent("req-1"),
		redisTestEvent("req-2"),
		redisTestEvent("req-3"),
	}

	err := buf.DrainBatch(context.Background(), events, 5*time.Second)
	require.NoError(t, err)

	// verify list length
	n, err := rdb.LLen(context.Background(), redisKeyShutdownBuffer).Result()
	require.NoError(t, err)
	require.Equal(t, int64(3), n)

	recovered, err := buf.Recover(context.Background())
	require.NoError(t, err)
	require.Len(t, recovered, 3)

	// all request_ids should be present
	seen := map[string]bool{}
	for _, e := range recovered {
		seen[e.RequestID] = true
	}
	for _, want := range []string{"req-1", "req-2", "req-3"} {
		require.True(t, seen[want], "missing %s", want)
	}

	// key should have been deleted
	n, err = rdb.LLen(context.Background(), redisKeyShutdownBuffer).Result()
	require.NoError(t, err)
	require.Equal(t, int64(0), n)
}

func TestRedisBuffer_DrainEmpty(t *testing.T) {
	rdb := startRedis(t)
	buf := NewPayloadAuditRedisBuffer(rdb)
	require.NoError(t, buf.DrainBatch(context.Background(), nil, time.Second))
}

func TestRedisBuffer_RecoverEmpty(t *testing.T) {
	rdb := startRedis(t)
	buf := NewPayloadAuditRedisBuffer(rdb)
	out, err := buf.Recover(context.Background())
	require.NoError(t, err)
	require.Empty(t, out)
}

func TestRedisBuffer_DrainTimesOutPartial(t *testing.T) {
	rdb := startRedis(t)
	buf := NewPayloadAuditRedisBuffer(rdb)

	// 200 events of ~100KB each with an extremely short deadline
	events := make([]*PayloadAuditEvent, 200)
	for i := range events {
		events[i] = redisTestBigEvent(i, 100*1024)
	}
	err := buf.DrainBatch(context.Background(), events, 1*time.Millisecond)
	var pe *PartialDrainError
	if !errors.As(err, &pe) {
		// If the deadline was not hit (e.g. fast machine), skip rather than fail.
		if err == nil {
			t.Skip("drain completed before deadline; cannot test partial drain on this machine")
		}
		t.Fatalf("expected PartialDrainError, got %v", err)
	}
	require.Less(t, pe.Drained, pe.Total, "should be partial, but Drained==Total")
}

func TestRedisBuffer_ChunkedLPush(t *testing.T) {
	rdb := startRedis(t)
	buf := NewPayloadAuditRedisBuffer(rdb)

	// 100 events should trigger multiple chunk flushes (drainChunkCount=50)
	events := make([]*PayloadAuditEvent, 100)
	for i := range events {
		events[i] = redisTestEvent(fmt.Sprintf("req-%d", i))
	}

	err := buf.DrainBatch(context.Background(), events, 5*time.Second)
	require.NoError(t, err)

	n, err := rdb.LLen(context.Background(), redisKeyShutdownBuffer).Result()
	require.NoError(t, err)
	require.Equal(t, int64(100), n)

	// Recover all and verify count
	recovered, err := buf.Recover(context.Background())
	require.NoError(t, err)
	require.Len(t, recovered, 100)
}
