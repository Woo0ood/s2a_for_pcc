// Package main implements the export-worker binary — a standalone HTTP service
// that renders payload-audit conversation exports off-process.
//
// The worker accepts export jobs via POST /v1/export, renders the conversation
// HTML by streaming rows from ClickHouse through a temp file, then uploads the
// result to S3. It is intentionally decoupled from the sub2api gateway: no
// sub2api wire/DI, no shared process, no shared memory.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func main() {
	cfg, err := parseConfig()
	if err != nil {
		slog.Error("export-worker: bad config", "err", err)
		os.Exit(1)
	}

	ctx := context.Background()

	// ── ClickHouse connection ────────────────────────────────────────────────
	chOpts, err := clickhouse.ParseDSN(cfg.CHDSN)
	if err != nil {
		slog.Error("export-worker: parse CH DSN", "err", err)
		os.Exit(1)
	}
	chConn, err := clickhouse.Open(chOpts)
	if err != nil {
		slog.Error("export-worker: open CH", "err", err)
		os.Exit(1)
	}
	if err := chConn.Ping(ctx); err != nil {
		slog.Warn("export-worker: CH ping failed (continuing)", "err", err)
	}
	repo := repository.NewPayloadAuditCHRepoWithTable(chConn, cfg.CHTable)

	// ── Blob store (source of offloaded blobs) ───────────────────────────────
	blobCfg := &service.BackupS3Config{
		Endpoint:        cfg.BlobS3Endpoint,
		Region:          cfg.BlobS3Region,
		Bucket:          cfg.BlobS3Bucket,
		AccessKeyID:     cfg.BlobS3AccessKeyID,
		SecretAccessKey: cfg.BlobS3SecretAccessKey,
		Prefix:          cfg.BlobS3Prefix,
		ForcePathStyle:  cfg.BlobS3ForcePathStyle,
	}
	blobStore, err := service.NewPayloadAuditBlobStore(ctx, repository.NewS3BackupStoreFactory(), blobCfg)
	if err != nil {
		slog.Error("export-worker: build blob store", "err", err)
		os.Exit(1)
	}
	resolver := service.NewBlobResolver(blobStore, cfg.BlobS3Prefix)

	// ── Result S3 client (raw; streams from *os.File — memory-flat) ─────────
	resultStore, err := newResultS3Store(ctx, blobCfg, cfg.BlobS3Bucket)
	if err != nil {
		slog.Error("export-worker: build result S3 client", "err", err)
		os.Exit(1)
	}

	// ── Job manager ──────────────────────────────────────────────────────────
	jobs := NewJobManager(1 * time.Hour)

	// ── HTTP server ──────────────────────────────────────────────────────────
	srv := NewServer(cfg, repo, resolver, resultStore, jobs)

	httpSrv := &http.Server{
		Addr:         cfg.Listen,
		Handler:      srv,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 10*time.Minute + 30*time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown on SIGTERM / SIGINT.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		slog.Info("export-worker: listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("export-worker: ListenAndServe", "err", err)
			os.Exit(1)
		}
	}()

	<-stop
	slog.Info("export-worker: shutting down")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutCancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		slog.Error("export-worker: shutdown", "err", err)
	}
}

// ─── resultS3Store — raw S3 client for streaming the result HTML ─────────────

// resultS3Store wraps a raw *s3.Client for uploading/downloading export results.
// Upload streams from an *os.File (after stat for content-length) — memory-flat.
type resultS3Store struct {
	client *s3.Client
	bucket string
}

func newResultS3Store(ctx context.Context, cfg *service.BackupS3Config, bucket string) (*resultS3Store, error) {
	region := cfg.Region
	if region == "" {
		region = "auto"
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = &cfg.Endpoint
		}
		if cfg.ForcePathStyle {
			o.UsePathStyle = true
		}
		o.APIOptions = append(o.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})
	return &resultS3Store{client: client, bucket: bucket}, nil
}

// Upload streams f to S3 using PutObject with an explicit ContentLength derived
// from os.File.Stat() — no io.ReadAll, memory stays flat regardless of file size.
func (r *resultS3Store) Upload(ctx context.Context, key string, f *os.File, contentType string) error {
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat temp file: %w", err)
	}
	size := fi.Size()
	_, err = r.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        &r.bucket,
		Key:           &key,
		Body:          f,
		ContentType:   &contentType,
		ContentLength: &size,
	})
	if err != nil {
		return fmt.Errorf("S3 PutObject result: %w", err)
	}
	return nil
}

// Download retrieves an exported HTML object by key and returns a ReadCloser.
func (r *resultS3Store) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	result, err := r.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &r.bucket,
		Key:    &key,
	})
	if err != nil {
		return nil, fmt.Errorf("S3 GetObject result: %w", err)
	}
	return result.Body, nil
}
