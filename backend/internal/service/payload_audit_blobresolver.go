package service

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// Attachment describes a blob-typed pointer that was found while resolving a body.
// ProxyPath is the relative path used by the export blob-proxy endpoint (blobs/<sha>).
// DataURI is populated by the inline pass in AssembleTranscript when the blob is an
// image within size caps; empty when the blob should be served via the proxy instead.
type Attachment struct {
	SHA256    string
	MIME      string
	Bytes     int
	ProxyPath string
	DataURI   string
}

// BlobResolver resolves s2a-blob:// and s2a-body:// pointers found in stored bodies.
// body pointers are downloaded and replaced with their original text.
// blob pointers are replaced with a placeholder and collected as Attachments.
type BlobResolver struct {
	store  PayloadAuditBlobStore
	prefix string
}

// NewBlobResolver constructs a BlobResolver from the given store and object-key prefix.
func NewBlobResolver(store PayloadAuditBlobStore, prefix string) *BlobResolver {
	return &BlobResolver{store: store, prefix: prefix}
}

// ResolveBody scans body for s2a-blob:// and s2a-body:// pointer tokens.
// For each body pointer: downloads the original content and replaces the token.
//   - on success: the token is replaced with the downloaded text.
//   - on error: the token is replaced with a gap placeholder and the gap is recorded.
//
// For each blob pointer: the token is replaced with a human-readable placeholder
// and an Attachment is collected (no download; handler streams it via proxy).
//
// Returns (resolved body, attachments, gaps).
func (r *BlobResolver) ResolveBody(ctx context.Context, body string) (resolved string, atts []Attachment, gaps []string) {
	if r == nil || r.store == nil {
		return body, nil, nil
	}
	if !strings.Contains(body, "s2a-blob://") && !strings.Contains(body, "s2a-body://") {
		return body, nil, nil
	}

	var sb strings.Builder
	remaining := body

	for {
		blobIdx := strings.Index(remaining, "s2a-blob://")
		bodyIdx := strings.Index(remaining, "s2a-body://")

		// Pick whichever pointer comes first.
		idx := -1
		switch {
		case blobIdx >= 0 && bodyIdx >= 0:
			if blobIdx < bodyIdx {
				idx = blobIdx
			} else {
				idx = bodyIdx
			}
		case blobIdx >= 0:
			idx = blobIdx
		case bodyIdx >= 0:
			idx = bodyIdx
		}

		if idx < 0 {
			// No more pointers.
			sb.WriteString(remaining)
			break
		}

		// Write everything before the pointer token.
		sb.WriteString(remaining[:idx])
		remaining = remaining[idx:]

		// Find the end of the pointer token: ends at `"`, whitespace, or end-of-string.
		end := tokenEnd(remaining)
		token := remaining[:end]
		remaining = remaining[end:]

		ptr, ok := ParsePointer(token)
		if !ok {
			// Not a valid pointer — emit verbatim and continue.
			sb.WriteString(token)
			continue
		}

		switch ptr.Kind {
		case PointerKindBody:
			key := bodyKey(r.prefix, ptr.SHA256)
			data, err := r.store.Get(ctx, key)
			if err != nil {
				placeholder := fmt.Sprintf("[s2a-body unavailable sha=%s]", ptr.SHA256)
				sb.WriteString(placeholder)
				gaps = append(gaps, fmt.Sprintf("body pointer download failed (sha=%s): %v", ptr.SHA256, err))
			} else {
				sb.Write(data)
			}

		case PointerKindBlob:
			placeholder := fmt.Sprintf("[image mime=%s bytes=%d]", ptr.MIME, ptr.Bytes)
			sb.WriteString(placeholder)
			atts = append(atts, Attachment{
				SHA256:    ptr.SHA256,
				MIME:      ptr.MIME,
				Bytes:     ptr.Bytes,
				ProxyPath: "blobs/" + ptr.SHA256,
			})
		}
	}

	return sb.String(), atts, gaps
}

// FetchBlob downloads a blob object by its SHA-256 hex digest.
// It returns the raw bytes and a MIME type string.
// If the resolver is nil or the object is not found, an error is returned.
// The MIME type defaults to "application/octet-stream" when unknown.
func (r *BlobResolver) FetchBlob(ctx context.Context, sha string) (data []byte, mime string, err error) {
	if r == nil || r.store == nil {
		return nil, "", fmt.Errorf("blob resolver not configured")
	}
	key := blobKey(r.prefix, sha)
	data, err = r.store.Get(ctx, key)
	if err != nil {
		return nil, "", fmt.Errorf("blob fetch: %w", err)
	}
	// The object store does not persist MIME on retrieval; default to octet-stream.
	return data, "application/octet-stream", nil
}

// StreamObject opens an object at the given FULL object key and returns a
// streaming reader (no buffering). The key is used verbatim — no blobKey/bodyKey
// prefixing — because the export-worker writes its result to an absolute key
// (e.g. "payload-audit/exports/<id>.html") which sub2api relays as-is.
// Caller must Close the returned reader.
func (r *BlobResolver) StreamObject(ctx context.Context, key string) (io.ReadCloser, error) {
	if r == nil || r.store == nil {
		return nil, fmt.Errorf("blob resolver not configured")
	}
	return r.store.GetStream(ctx, key)
}

// tokenEnd returns the index of the first character after the pointer token.
// A pointer token ends at a double-quote, whitespace, or end-of-string — they
// appear inside JSON string values, which are delimited by `"`.
func tokenEnd(s string) int {
	for i, ch := range s {
		if ch == '"' || ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
			return i
		}
	}
	return len(s)
}
