package knowledge

import (
	"fmt"
	"strings"
)

// extract_projection.go 连接「送给模型的正文」与文档坐标（010 T025）。
//
// ⭐ 核心决定：**送给模型的正文里不包含不可引用的部分**。
//
// 一个块的内容可能带着 overlap 拷贝（前一块的尾巴）和系统拼接的分隔符。
// 那些字符是真的，但它们在这个块的元数据里没有文档区间——它们的家在别处。
// 原样喂给模型的话，模型完全可能从那一段里引一句话，而我们拿不出它在书里
// 的位置。
//
// ⚠️ 两种事后补救都更糟：
//   - 丢掉那条证据 → 一条真实的关系因为我们的分块方式而消失，
//     且它在召回率里表现为**模型漏抽**；
//   - 给它一个近似位置 → 引用打开是对的文字、错的位置，没有任何东西报错。
// 所以在**入口**就把不可引用的部分去掉：模型看不见它，就不会引用它。

// projSegment 是一段可引用正文：模型看到的区间 → 文档区间。
type projSegment struct {
	textStart int // 在 chunkProjection.Text 中的 rune 起点
	textEnd   int
	docStart  int
	docEnd    int
}

// chunkProjection 是一个块送给模型的正文，以及它到文档坐标的映射。
type chunkProjection struct {
	Text string
	segs []projSegment
}

// newChunkProjection 按元数据裁出可引用正文并建立映射。
//
// ⚠️ 元数据与内容对不上时**报错**，不"尽力而为"。尽力而为的结果是一批位置
// 略微偏移的引用，而偏移量取决于对不上的程度，没有任何规律可循——
// 那种数据比没有数据更难处理。
func newChunkProjection(content string, meta narrativeMetadata) (chunkProjection, error) {
	runes := []rune(content)
	if len(meta.Segments) == 0 {
		return chunkProjection{}, fmt.Errorf("knowledge: chunk projection: metadata has no segments")
	}

	var sb strings.Builder
	var segs []projSegment
	cursor := 0
	for i, seg := range meta.Segments {
		if seg.ChunkStart < 0 || seg.ChunkStart >= seg.ChunkEnd || seg.ChunkEnd > len(runes) {
			return chunkProjection{}, fmt.Errorf(
				"knowledge: chunk projection: segment %d range [%d,%d) outside content of %d runes",
				i, seg.ChunkStart, seg.ChunkEnd, len(runes))
		}
		if seg.IsOverlapCopy || seg.IsGeneratedSeparator {
			continue
		}
		if seg.DocumentStart == nil || seg.DocumentEnd == nil {
			return chunkProjection{}, fmt.Errorf(
				"knowledge: chunk projection: segment %d is citable but has no document interval", i)
		}
		chunkLen := seg.ChunkEnd - seg.ChunkStart
		if docLen := *seg.DocumentEnd - *seg.DocumentStart; docLen != chunkLen {
			// data-model §4：复制源段长度必须一致。不一致说明上游算错了，
			// 而据此映射出的每一条引用都会偏移。
			return chunkProjection{}, fmt.Errorf(
				"knowledge: chunk projection: segment %d length mismatch (chunk %d, document %d)",
				i, chunkLen, docLen)
		}
		sb.WriteString(string(runes[seg.ChunkStart:seg.ChunkEnd]))
		segs = append(segs, projSegment{
			textStart: cursor, textEnd: cursor + chunkLen,
			docStart: *seg.DocumentStart, docEnd: *seg.DocumentEnd,
		})
		cursor += chunkLen
	}
	if len(segs) == 0 {
		// ⚠️ 全部不可引用的块喂给模型只会换回一份必然作废的响应，
		// 与其花一次调用，不如在入口就说清楚。
		return chunkProjection{}, fmt.Errorf("knowledge: chunk projection: no citable content")
	}
	return chunkProjection{Text: sb.String(), segs: segs}, nil
}

// toDocument 把模型正文里的 rune 区间映射回文档坐标。
//
// ⭐ 跨越两个**文档上不相邻**的段时返回 false。给一个把中间几百字都圈进去的
// 区间，会让引用打开时显示一大段模型根本没引用的文字。
// 文档上相邻的两段可以合并——它们本来就是连续的原文，只是分块时被拆开了。
//
// ⚠️ 越界一律拒绝，不钳到边界上：钳边界会产出一个**比模型引用的文字更短**
// 的区间，用户点开看到半句话。
func (p chunkProjection) toDocument(start, end int) (int, int, bool) {
	if start < 0 || end <= start || end > len([]rune(p.Text)) {
		return 0, 0, false
	}
	docStart, docEnd := 0, 0
	found := false
	for _, seg := range p.segs {
		if end <= seg.textStart || start >= seg.textEnd {
			continue
		}
		lo := max(start, seg.textStart)
		hi := min(end, seg.textEnd)
		ds := seg.docStart + (lo - seg.textStart)
		de := seg.docStart + (hi - seg.textStart)
		if !found {
			docStart, docEnd, found = ds, de, true
			continue
		}
		if ds != docEnd {
			// 与上一段在文档上不相邻。
			return 0, 0, false
		}
		docEnd = de
	}
	return docStart, docEnd, found
}

// dedupeEvidenceByDocumentInterval 按**文档区间 + 引文**去重。
//
// ⭐ 相邻块因 overlap 会覆盖同一段原文，同一处出处必须只算一条证据。
// ⚠️ 按 chunk_id 去重会把它记成两条，虚增报告里的证据条数——而证据条数是
// "每条关系有多少支持"这个指标的分母。
//
// 保留**先出现**的那条，顺序确定（宪法第 V 条）。
func dedupeEvidenceByDocumentInterval(evs []evidenceDraft) []evidenceDraft {
	seen := make(map[string]bool, len(evs))
	out := make([]evidenceDraft, 0, len(evs))
	for _, ev := range evs {
		key := string(evidenceKeyHash(ev))
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ev)
	}
	return out
}
