package service

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/Wei-Shaw/sub2api/pkg/snowflake"
)

func newConvExportTestService(t *testing.T) (*PayloadAuditService, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	gen, err := snowflake.New(1)
	require.NoError(t, err)

	svc := &PayloadAuditService{rdb: rdb, idgen: gen}
	return svc, mr
}

func TestConvExportJob_RoundTrip(t *testing.T) {
	svc, _ := newConvExportTestService(t)
	ctx := context.Background()

	// Create → status should be "running"
	jobID, err := svc.CreateConvExportJob(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, jobID)

	job, err := svc.GetConvExportJob(ctx, jobID)
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, "running", job.Status)
	require.Empty(t, job.Error)

	// Finish → status should be "done", result retrievable
	html := []byte("<html>hello</html>")
	svc.FinishConvExportJob(ctx, jobID, html)

	job, err = svc.GetConvExportJob(ctx, jobID)
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, "done", job.Status)
	require.Equal(t, len(html), job.SizeBytes)

	result, err := svc.GetConvExportResult(ctx, jobID)
	require.NoError(t, err)
	require.Equal(t, html, result)
}

func TestConvExportJob_FailPath(t *testing.T) {
	svc, _ := newConvExportTestService(t)
	ctx := context.Background()

	jobID, err := svc.CreateConvExportJob(ctx)
	require.NoError(t, err)

	svc.FailConvExportJob(ctx, jobID, "something went wrong")

	job, err := svc.GetConvExportJob(ctx, jobID)
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, "failed", job.Status)
	require.Equal(t, "something went wrong", job.Error)
}

func TestConvExportJob_NotFound(t *testing.T) {
	svc, _ := newConvExportTestService(t)
	ctx := context.Background()

	// Non-existent job → nil, no error
	job, err := svc.GetConvExportJob(ctx, "99999999999")
	require.NoError(t, err)
	require.Nil(t, job)

	// Non-existent result → nil, no error
	result, err := svc.GetConvExportResult(ctx, "99999999999")
	require.NoError(t, err)
	require.Nil(t, result)
}

func TestConvExportJob_NilRedis(t *testing.T) {
	gen, err := snowflake.New(2)
	require.NoError(t, err)
	svc := &PayloadAuditService{rdb: nil, idgen: gen}
	ctx := context.Background()

	_, err = svc.CreateConvExportJob(ctx)
	require.Error(t, err)

	job, err := svc.GetConvExportJob(ctx, "1")
	require.Error(t, err)
	require.Nil(t, job)

	// FinishConvExportJob and FailConvExportJob are fire-and-forget, must not panic
	require.NotPanics(t, func() { svc.FinishConvExportJob(ctx, "1", []byte("x")) })
	require.NotPanics(t, func() { svc.FailConvExportJob(ctx, "1", "err") })
}
