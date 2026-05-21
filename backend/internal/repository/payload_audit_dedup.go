package repository

import (
	"crypto/sha1"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// BatchToken returns a stable 16-char hex digest of the batch's IDs,
// suitable for ClickHouse insert_deduplication_token.
// Order-invariant: same IDs in any order → same token. Retrying the same
// batch produces the same token so CH dedup catches duplicates.
func BatchToken(events []*PayloadAuditEvent) string {
	ids := make([]int64, 0, len(events))
	for _, e := range events {
		ids = append(ids, e.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	sum := sha1.Sum([]byte(strings.Join(parts, ",")))
	return fmt.Sprintf("%x", sum)[:16]
}
