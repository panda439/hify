package knowledge

import (
	"strings"
	"testing"
)

// Characterization tests for chunkText — 链路 2（文档入库流水线）的分块
// 环节。按 rune 而非 byte 切分（中文内容 byte 切会把多字节字符切半），
// 步长 = size - overlap。改坏的表现是 chunk 内容乱码或流水线死循环。

func TestChunkTextBasic(t *testing.T) {
	got := chunkText("一二三四五六七", 3, 1)
	want := []string{"一二三", "三四五", "五六七"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestChunkTextRunesNotBytes(t *testing.T) {
	// 每个中文字符 3 字节；若按 byte 切分，size=2 会产出乱码半字符。
	got := chunkText("中文", 1, 0)
	if len(got) != 2 || got[0] != "中" || got[1] != "文" {
		t.Fatalf("rune-based split broken: %v", got)
	}
}

func TestChunkTextEdgeCases(t *testing.T) {
	cases := []struct {
		name          string
		text          string
		size, overlap int
		want          int // 期望 chunk 数；-1 表示只要不 panic/不超时
	}{
		{"empty text", "", 10, 2, 0},
		{"whitespace-only chunks skipped", "   ", 2, 0, 0},
		{"text shorter than size", "abc", 10, 2, 1},
		{"zero size falls back to default 500", strings.Repeat("a", 1200), 0, 0, 3},
		{"overlap >= size falls back to no overlap", "abcdef", 2, 5, 3},
		{"negative overlap falls back to no overlap", "abcd", 2, -1, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chunkText(tc.text, tc.size, tc.overlap)
			if len(got) != tc.want {
				t.Fatalf("chunk count = %d (%v), want %d", len(got), got, tc.want)
			}
		})
	}
}

func TestChunkTextTrimsButKeepsInterior(t *testing.T) {
	got := chunkText("  ab  ", 6, 0)
	if len(got) != 1 || got[0] != "ab" {
		t.Fatalf("expected single trimmed chunk, got %v", got)
	}
}

// Characterization tests for batchStrings — ProcessDocument's embed
// batching (链路 2). Broken batching either drops/duplicates pieces
// across batch boundaries or produces a batch bigger than embedBatchSize.

func TestBatchStringsSplitsEvenly(t *testing.T) {
	items := []string{"a", "b", "c", "d"}
	got := batchStrings(items, 2)
	want := [][]string{{"a", "b"}, {"c", "d"}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if strings.Join(got[i], ",") != strings.Join(want[i], ",") {
			t.Fatalf("batch %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestBatchStringsLastBatchIsRemainder(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e"}
	got := batchStrings(items, 2)
	if len(got) != 3 {
		t.Fatalf("got %d batches %v, want 3", len(got), got)
	}
	if len(got[2]) != 1 || got[2][0] != "e" {
		t.Fatalf("last batch = %v, want [e]", got[2])
	}
}

func TestBatchStringsPreservesOrderAndTotalCount(t *testing.T) {
	items := make([]string, 100)
	for i := range items {
		items[i] = strings.Repeat("x", i+1) // 每个元素内容不同，方便按顺序核对
	}
	got := batchStrings(items, 32)
	if len(got) != 4 { // 32+32+32+4
		t.Fatalf("got %d batches, want 4", len(got))
	}
	for _, batch := range got {
		if len(batch) > 32 {
			t.Fatalf("batch size %d exceeds cap 32", len(batch))
		}
	}
	var flattened []string
	for _, batch := range got {
		flattened = append(flattened, batch...)
	}
	if len(flattened) != len(items) {
		t.Fatalf("flattened length = %d, want %d (no dropped/duplicated items)", len(flattened), len(items))
	}
	for i := range items {
		if flattened[i] != items[i] {
			t.Fatalf("order not preserved at index %d: got %q, want %q", i, flattened[i], items[i])
		}
	}
}

func TestBatchStringsEdgeCases(t *testing.T) {
	if got := batchStrings(nil, 10); got != nil {
		t.Fatalf("batchStrings(nil, 10) = %v, want nil", got)
	}
	if got := batchStrings([]string{"a"}, 0); got != nil {
		t.Fatalf("batchStrings with size=0 = %v, want nil", got)
	}
	if got := batchStrings([]string{"a", "b"}, 10); len(got) != 1 || len(got[0]) != 2 {
		t.Fatalf("batch size larger than input = %v, want a single batch of 2", got)
	}
}
