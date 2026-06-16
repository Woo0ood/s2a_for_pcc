package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all configuration for the export-worker, parsed from environment variables.
type Config struct {
	// HTTP server
	Listen string // EXPORT_WORKER_LISTEN (default ":8088")
	Token  string // EXPORT_WORKER_TOKEN (required)

	// ClickHouse
	CHDSN   string // CH_DSN (required)
	CHTable string // CH_TABLE (default "payload_audit_logs")

	// Blob S3 (source of offloaded blobs)
	BlobS3Endpoint        string // BLOB_S3_ENDPOINT
	BlobS3Region          string // BLOB_S3_REGION
	BlobS3Bucket          string // BLOB_S3_BUCKET (required)
	BlobS3AccessKeyID     string // BLOB_S3_ACCESS_KEY_ID
	BlobS3SecretAccessKey string // BLOB_S3_SECRET_ACCESS_KEY (required)
	BlobS3Prefix          string // BLOB_S3_PREFIX (default "payload-audit/")
	BlobS3ForcePathStyle  bool   // BLOB_S3_FORCE_PATH_STYLE

	// Export result S3 (same credentials as blob store; separate prefix)
	ExportResultPrefix string // EXPORT_RESULT_PREFIX (default "payload-audit/exports/")

	// Render parameters
	RenderTimeoutMinutes int // RENDER_TIMEOUT_MINUTES (default 10)
	ConvWindowDays       int // CONV_WINDOW_DAYS (default 7)
}

// parseConfig reads Config from environment variables. Returns an error if any
// required variable is missing or cannot be parsed.
func parseConfig() (*Config, error) {
	c := &Config{}
	var missing []string

	// Required fields
	c.Token = os.Getenv("EXPORT_WORKER_TOKEN")
	if c.Token == "" {
		missing = append(missing, "EXPORT_WORKER_TOKEN")
	}

	c.CHDSN = os.Getenv("CH_DSN")
	if c.CHDSN == "" {
		missing = append(missing, "CH_DSN")
	}

	c.BlobS3Bucket = os.Getenv("BLOB_S3_BUCKET")
	if c.BlobS3Bucket == "" {
		missing = append(missing, "BLOB_S3_BUCKET")
	}

	c.BlobS3SecretAccessKey = os.Getenv("BLOB_S3_SECRET_ACCESS_KEY")
	if c.BlobS3SecretAccessKey == "" {
		missing = append(missing, "BLOB_S3_SECRET_ACCESS_KEY")
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	// Optional with defaults
	c.Listen = envOrDefault("EXPORT_WORKER_LISTEN", ":8088")
	c.CHTable = envOrDefault("CH_TABLE", "payload_audit_logs")
	c.BlobS3Endpoint = os.Getenv("BLOB_S3_ENDPOINT")
	c.BlobS3Region = os.Getenv("BLOB_S3_REGION")
	c.BlobS3AccessKeyID = os.Getenv("BLOB_S3_ACCESS_KEY_ID")
	c.BlobS3Prefix = envOrDefault("BLOB_S3_PREFIX", "payload-audit/")
	c.ExportResultPrefix = envOrDefault("EXPORT_RESULT_PREFIX", "payload-audit/exports/")

	var err error
	if v := os.Getenv("BLOB_S3_FORCE_PATH_STYLE"); v != "" {
		c.BlobS3ForcePathStyle, err = strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("BLOB_S3_FORCE_PATH_STYLE: %w", err)
		}
	}

	c.RenderTimeoutMinutes, err = envIntOrDefault("RENDER_TIMEOUT_MINUTES", 10)
	if err != nil {
		return nil, fmt.Errorf("RENDER_TIMEOUT_MINUTES: %w", err)
	}

	c.ConvWindowDays, err = envIntOrDefault("CONV_WINDOW_DAYS", 7)
	if err != nil {
		return nil, fmt.Errorf("CONV_WINDOW_DAYS: %w", err)
	}

	return c, nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOrDefault(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", v)
	}
	return n, nil
}
