package repository

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// PayloadAuditEvent represents the writable fields of a payload_audit_logs row.
type PayloadAuditEvent struct {
	ID                                               int64
	RequestID                                        string
	UserID, APIKeyID, GroupID                         *int64
	UserEmail, APIKeyName, GroupName, ClientIP        string
	Endpoint, Provider, Model, UpstreamModel         string
	Stream                                           bool
	StatusCode, DurationMs                           int
	InputExcerpt, OutputExcerpt                      string
	InputBody, OutputBody                            string
	InputFormat, OutputFormat                        string
	InputBytes, OutputBytes                          int
	InputTruncated, OutputTruncated, OutputOmitted   bool
	InputOffloaded                                   bool
	ErrorMessage                                     string
	CreatedAt                                        time.Time
}

// PayloadAuditRow is a read-back row including the generated id.
type PayloadAuditRow struct {
	ID int64
	PayloadAuditEvent
}

// PayloadAuditListFilter controls pagination and filtering for List queries.
type PayloadAuditListFilter struct {
	From, To              time.Time
	UserID, GroupID, APIKeyID *int64
	Cursor                *PayloadAuditCursor
	Limit                 int
	KeywordILike          string
	IncludeBody           bool
}

// PayloadAuditCursor is a keyset-pagination cursor serialised as base64(JSON).
type PayloadAuditCursor struct {
	SchemaVer   int       `json:"v"`
	ToEffective time.Time `json:"to"`
	LastCreated time.Time `json:"lc"`
	LastID      int64     `json:"li"`
}

const payloadAuditCursorSchemaVer = 1

// EncodeCursor serialises a cursor to a base64-encoded JSON string.
func EncodeCursor(c *PayloadAuditCursor) (string, error) {
	if c == nil {
		return "", nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode payload audit cursor: %w", err)
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

// DecodeCursor deserialises a base64-encoded JSON cursor.
func DecodeCursor(s string) (*PayloadAuditCursor, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode payload audit cursor base64: %w", err)
	}
	var c PayloadAuditCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("decode payload audit cursor json: %w", err)
	}
	if c.SchemaVer != payloadAuditCursorSchemaVer {
		return nil, fmt.Errorf("cursor schema mismatch: got %d, want %d", c.SchemaVer, payloadAuditCursorSchemaVer)
	}
	return &c, nil
}

var (
	ErrPayloadAuditKeywordWindowTooLarge = errors.New("payload audit: keyword search requires time window <= 7 days")
)
