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
