package service

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestExcerpt_Disabled(t *testing.T) {
	if got := excerpt("hello", 0); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
	if got := excerpt("hello", -1); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestExcerpt_EmptyText(t *testing.T) {
	if got := excerpt("", 100); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestExcerpt_ShortPassesThrough(t *testing.T) {
	if got := excerpt("hello", 100); got != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestExcerpt_TypicalHeadAndTail(t *testing.T) {
	in := strings.Repeat("a", 100) + "MIDDLE" + strings.Repeat("b", 100)
	out := excerpt(in, 64)
	if !strings.Contains(out, "[truncated") {
		t.Fatal("missing truncate marker")
	}
	if !strings.HasPrefix(out, "a") {
		t.Fatal("missing head")
	}
	if !strings.HasSuffix(out, "b") {
		t.Fatal("missing tail")
	}
	if len(out) > 64 {
		t.Fatalf("over budget: %d", len(out))
	}
}

func TestExcerpt_TinyTotalDegradesToHeadOnly(t *testing.T) {
	in := strings.Repeat("a", 1000)
	out := excerpt(in, 40)
	if !utf8.ValidString(out) {
		t.Fatal("invalid utf-8")
	}
	if len(out) > 40 {
		t.Fatalf("over budget: %d", len(out))
	}
	if !strings.HasSuffix(out, "[truncated]") && !strings.Contains(out, "[truncated") {
		t.Fatalf("missing truncate marker, got %q", out)
	}
}

func TestExcerpt_MultibyteUTF8Boundary(t *testing.T) {
	// 中文每字符 3 byte
	in := strings.Repeat("中", 100) + "M" + strings.Repeat("文", 100)
	out := excerpt(in, 64)
	if !utf8.ValidString(out) {
		t.Fatal("split a multibyte rune")
	}
	if len(out) > 64 {
		t.Fatalf("over budget: %d", len(out))
	}
}

func TestExcerpt_EmojiBoundary(t *testing.T) {
	in := strings.Repeat("😀", 200)
	out := excerpt(in, 64)
	if !utf8.ValidString(out) {
		t.Fatal("split an emoji")
	}
	if len(out) > 64 {
		t.Fatalf("over budget: %d", len(out))
	}
}

func TestExcerpt_HugeTruncatedMakesSepLong(t *testing.T) {
	// truncatedBytes ~100k → sep ~30 字节，total=40 → sep 必然 >= total-minExcerpt
	// 应触发降级路径
	in := strings.Repeat("a", 100000)
	out := excerpt(in, 40)
	if len(out) > 40 {
		t.Fatalf("over budget: %d", len(out))
	}
	if !utf8.ValidString(out) {
		t.Fatal("invalid utf-8")
	}
}

func TestExcerpt_ExactEqualTotal(t *testing.T) {
	in := strings.Repeat("a", 100)
	out := excerpt(in, 100)
	if out != in {
		t.Fatalf("equal-length should pass through")
	}
}

func TestExcerpt_OneByteOverTotal(t *testing.T) {
	in := strings.Repeat("a", 101)
	out := excerpt(in, 100)
	if len(out) > 100 {
		t.Fatalf("over budget: %d", len(out))
	}
}
