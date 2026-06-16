package service

import (
	"context"
	"fmt"
	"html/template"
	"io"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// TranscriptStreamer — memory-flat streaming HTML writer
// ─────────────────────────────────────────────────────────────────────────────

// TranscriptStreamer writes a self-contained HTML transcript incrementally to w:
// head first, one <article> per WriteTurn (released after writing), then the
// manifest (at the BOTTOM) + footer on Finish. Memory stays O(one turn),
// independent of conversation size.
//
// Layout differs from RenderTranscriptHTML only in placement of the manifest card:
// the streaming renderer emits it at the BOTTOM (after all turns) rather than at
// the TOP, because the final turn count and time range are only known after
// iterating all rows.
type TranscriptStreamer struct {
	w            io.Writer
	turnTmpl     *template.Template
	manifestTmpl *template.Template
	convKey      string
	count        int
	from         time.Time
	to           time.Time
	gaps         []string
	gapSeen      map[string]bool
}

// NewTranscriptStreamer creates a TranscriptStreamer that writes to w.
// It immediately writes transcriptHeadHTML to w so streaming can start.
func NewTranscriptStreamer(w io.Writer, conversationKey string) (*TranscriptStreamer, error) {
	funcMap := transcriptFuncMap()

	turnTmpl, err := template.New("turn").Funcs(funcMap).Parse(transcriptTurnTemplateStr)
	if err != nil {
		return nil, fmt.Errorf("transcript streamer: parse turn template: %w", err)
	}
	manifestTmpl, err := template.New("manifest").Funcs(funcMap).Parse(transcriptManifestTemplateStr)
	if err != nil {
		return nil, fmt.Errorf("transcript streamer: parse manifest template: %w", err)
	}

	if _, err := io.WriteString(w, transcriptHeadHTML); err != nil {
		return nil, fmt.Errorf("transcript streamer: write head: %w", err)
	}

	return &TranscriptStreamer{
		w:            w,
		turnTmpl:     turnTmpl,
		manifestTmpl: manifestTmpl,
		convKey:      conversationKey,
		gapSeen:      make(map[string]bool),
	}, nil
}

// WriteTurn executes the turn template for t and writes the resulting <article>
// to w. It also tracks the time range (min/max CreatedAt) and turn count.
func (s *TranscriptStreamer) WriteTurn(t Turn) error {
	if err := s.turnTmpl.Execute(s.w, t); err != nil {
		return fmt.Errorf("transcript streamer: execute turn template: %w", err)
	}
	s.count++
	if s.count == 1 || t.CreatedAt.Before(s.from) {
		s.from = t.CreatedAt
	}
	if t.CreatedAt.After(s.to) {
		s.to = t.CreatedAt
	}
	return nil
}

// AddGaps records gap strings, deduplicating them.
func (s *TranscriptStreamer) AddGaps(gaps ...string) {
	for _, g := range gaps {
		if !s.gapSeen[g] {
			s.gapSeen[g] = true
			s.gaps = append(s.gaps, g)
		}
	}
}

// Finish writes the manifest (at the BOTTOM) and the HTML footer.
func (s *TranscriptStreamer) Finish() error {
	if s.count == 0 {
		// No turns written — emit the empty-state paragraph (mirrors the in-memory renderer).
		if _, err := io.WriteString(s.w, "\n<p style=\"color:#999;font-style:italic;\">No turns in this conversation.</p>\n"); err != nil {
			return fmt.Errorf("transcript streamer: write empty state: %w", err)
		}
	}

	m := Manifest{
		ConversationKey: s.convKey,
		TurnCount:       s.count,
		TimeFrom:        s.from,
		TimeTo:          s.to,
		Gaps:            s.gaps,
	}
	if err := s.manifestTmpl.Execute(s.w, m); err != nil {
		return fmt.Errorf("transcript streamer: execute manifest template: %w", err)
	}

	if _, err := io.WriteString(s.w, "\n"+transcriptFooterHTML); err != nil {
		return fmt.Errorf("transcript streamer: write footer: %w", err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// StreamingAssembler — cross-turn dedup + inline budget for streaming render
// ─────────────────────────────────────────────────────────────────────────────

// StreamingAssembler holds the cross-turn dedup + inline budget for a streaming
// render. It is stateful and must be used with rows in ascending order.
type StreamingAssembler struct {
	resolver *BlobResolver
	seen     map[string]bool // seenHashes for input-item dedup
	seenAtts map[string]bool // seenAttachments for cross-turn image dedup
	respIDs  map[string]bool // responseIDSet for chain-gap detection
	budget   *inlineBudget
	idx      int // 1-based turn index
}

// NewStreamingAssembler creates a StreamingAssembler.
//
// resolver may be nil (no blob resolution or inlining).
// responseIDs is the pre-built set of all response_id values in the export
// (used for chain-gap detection); pass nil or an empty map if not needed.
// unlimitedInline=true → no caps on image inlining (worker path).
func NewStreamingAssembler(resolver *BlobResolver, responseIDs map[string]bool, unlimitedInline bool) *StreamingAssembler {
	if responseIDs == nil {
		responseIDs = make(map[string]bool)
	}
	var budget *inlineBudget
	if unlimitedInline {
		// perImageMax=0, totalMax=0 → assembleTurn treats as unlimited.
		budget = &inlineBudget{}
	} else {
		budget = &inlineBudget{
			perImageMax: maxInlineImageBytes,
			totalMax:    maxTotalInlineBytes,
		}
	}
	return &StreamingAssembler{
		resolver: resolver,
		seen:     make(map[string]bool),
		seenAtts: make(map[string]bool),
		respIDs:  responseIDs,
		budget:   budget,
	}
}

// Next converts one PayloadAuditEvent to a Turn + gap strings, advancing the
// internal turn index. Calls assembleTurn with the shared dedup/budget state.
func (a *StreamingAssembler) Next(ctx context.Context, row *PayloadAuditEvent) (Turn, []string) {
	a.idx++
	return assembleTurn(ctx, row, a.idx, a.resolver, a.seen, a.seenAtts, a.respIDs, a.budget)
}
