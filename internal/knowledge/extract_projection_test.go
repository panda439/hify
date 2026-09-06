package knowledge

import (
	"strings"
	"testing"
)

// extract_projection_test.go 守「送给模型的正文」与文档坐标之间的映射
// （010 T025）。
//
// ⭐ 核心决定：**送给模型的正文里不包含不可引用的部分**。
//
// 一个块的内容可能带着 overlap 拷贝（前一块的尾巴）和系统拼接的分隔符。
// 那些字符是真的，但它们在这个块的元数据里**没有文档区间**——它们的家在
// 别处。如果原样喂给模型，模型完全可能从那一段里引一句话，而我们拿不出
// 它在书里的位置。
//
// ⚠️ 两种补救都更糟：
//   - 丢掉那条证据 → 一条真实的关系因为我们的分块方式而消失，
//     且它在召回率里表现为模型漏抽；
//   - 给它一个近似位置 → 引用打开是对的文字、错的位置，没有任何东西报错。
// 所以在**入口**就把不可引用的部分去掉：模型看不见它，就不会引用它。

func projMeta(segs ...narrativeSegment) narrativeMetadata {
	return narrativeMetadata{
		SchemaVersion: narrativeMetadataSchemaVersion,
		BoundaryKind:  boundaryNone,
		Segments:      segs,
	}
}

func docSeg(chunkStart, chunkEnd, docStart, docEnd int) narrativeSegment {
	return narrativeSegment{
		ChunkStart: chunkStart, ChunkEnd: chunkEnd,
		DocumentStart: &docStart, DocumentEnd: &docEnd,
	}
}

// TestProjectionDropsUnquotableParts——⭐ overlap 拷贝不进入模型输入。
func TestProjectionDropsUnquotableParts(t *testing.T) {
	content := "前一块的尾巴。这一块的正文。"
	meta := projMeta(
		narrativeSegment{ChunkStart: 0, ChunkEnd: 7, IsOverlapCopy: true},
		docSeg(7, 14, 1000, 1007),
	)
	p, err := newChunkProjection(content, meta)
	if err != nil {
		t.Fatalf("newChunkProjection: %v", err)
	}
	if p.Text != "这一块的正文。" {
		t.Errorf("送给模型的正文 = %q，overlap 拷贝没有被去掉", p.Text)
	}
	// 整段映射回去。
	start, end, ok := p.toDocument(0, len([]rune(p.Text)))
	if !ok {
		t.Fatal("整段映射失败")
	}
	if start != 1000 || end != 1007 {
		t.Errorf("映射结果 [%d,%d)，want [1000,1007)", start, end)
	}
	// 局部映射也要对。
	start, end, ok = p.toDocument(2, 5)
	if !ok || start != 1002 || end != 1005 {
		t.Errorf("局部映射 [%d,%d) ok=%v，want [1002,1005)", start, end, ok)
	}
}

// TestProjectionDropsGeneratedSeparators——系统拼接进去的分隔符同理。
// ⚠️ 它们**不在原文里**，引用它们等于引用一段书里不存在的字符。
func TestProjectionDropsGeneratedSeparators(t *testing.T) {
	content := "甲段。\n\n乙段。"
	meta := projMeta(
		docSeg(0, 3, 100, 103),
		narrativeSegment{ChunkStart: 3, ChunkEnd: 5, IsGeneratedSeparator: true},
		docSeg(5, 8, 200, 203),
	)
	p, err := newChunkProjection(content, meta)
	if err != nil {
		t.Fatal(err)
	}
	if p.Text != "甲段。乙段。" {
		t.Errorf("送给模型的正文 = %q", p.Text)
	}
	// ⭐ 跨越两段（文档上不相邻）的引用必须**被拒绝**，不能给一个
	// [100,203) 这样把中间 100 个字都圈进去的区间。
	if _, _, ok := p.toDocument(2, 4); ok {
		t.Error("跨越两个不相邻文档区间的引用被接受了")
	}
	// 各自段内的引用正常。
	if s, e, ok := p.toDocument(0, 3); !ok || s != 100 || e != 103 {
		t.Errorf("甲段映射 [%d,%d) ok=%v", s, e, ok)
	}
	if s, e, ok := p.toDocument(3, 6); !ok || s != 200 || e != 203 {
		t.Errorf("乙段映射 [%d,%d) ok=%v", s, e, ok)
	}
}

// TestProjectionKeepsAdjacentDocumentRegionsJoinable——文档上**相邻**的两段
// 可以跨段引用：它们本来就是连续的原文，只是分块时被拆成了两个 segment。
func TestProjectionKeepsAdjacentDocumentRegionsJoinable(t *testing.T) {
	content := "甲段。乙段。"
	meta := projMeta(docSeg(0, 3, 100, 103), docSeg(3, 6, 103, 106))
	p, err := newChunkProjection(content, meta)
	if err != nil {
		t.Fatal(err)
	}
	s, e, ok := p.toDocument(1, 5)
	if !ok {
		t.Fatal("文档上相邻的两段之间不该拒绝跨段引用")
	}
	if s != 101 || e != 105 {
		t.Errorf("跨段映射 [%d,%d)，want [101,105)", s, e)
	}
}

// TestProjectionRejectsOutOfRange——越界必须拒绝，不能钳到边界上。
// ⚠️ 钳边界会产出一个**比模型引用的文字更短**的区间，用户点开看到半句话。
func TestProjectionRejectsOutOfRange(t *testing.T) {
	content := "正文。"
	p, err := newChunkProjection(content, projMeta(docSeg(0, 3, 10, 13)))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]int{{-1, 2}, {0, 4}, {2, 2}, {3, 1}} {
		if _, _, ok := p.toDocument(c[0], c[1]); ok {
			t.Errorf("越界区间 [%d,%d) 被接受了", c[0], c[1])
		}
	}
}

// TestProjectionRejectsInconsistentMetadata——元数据与内容对不上时报错，
// 不"尽力而为"。⚠️ 尽力而为的结果是一批位置略微偏移的引用，
// 而偏移量取决于对不上的程度，没有任何规律可循。
func TestProjectionRejectsInconsistentMetadata(t *testing.T) {
	if _, err := newChunkProjection("正文。", projMeta(docSeg(0, 99, 10, 109))); err == nil {
		t.Error("segment 超出内容长度却没报错")
	}
	if _, err := newChunkProjection("正文。", projMeta()); err == nil {
		t.Error("空 segments 却没报错")
	}
	// 文档区间长度与块内区间长度不一致：复制源段长度必须相同（data-model §4）。
	if _, err := newChunkProjection("正文。", projMeta(docSeg(0, 3, 10, 20))); err == nil {
		t.Error("块内区间与文档区间长度不一致却没报错")
	}
}

// TestProjectionAllUnquotableIsAnError——一个块的内容**全部**不可引用时报错。
// ⚠️ 那样的块喂给模型只会让它无从下手，而返回的任何引用都定位不了；
// 与其花一次调用换一份必然作废的响应，不如在入口就说清楚。
func TestProjectionAllUnquotableIsAnError(t *testing.T) {
	meta := projMeta(narrativeSegment{ChunkStart: 0, ChunkEnd: 3, IsOverlapCopy: true})
	if _, err := newChunkProjection("正文。", meta); err == nil {
		t.Error("全部不可引用的块没有被拒绝")
	}
}

// TestProjectedEvidenceDedupesByDocumentInterval——⭐ 相邻块因 overlap 会
// 覆盖同一段原文。同一处出处必须只算一条证据。
//
// ⚠️ 按 chunk_id 去重会把它记成两条，虚增报告里的证据条数；
// 而证据条数是"每条关系有多少支持"这个指标的分母。
func TestProjectedEvidenceDedupesByDocumentInterval(t *testing.T) {
	evs := []evidenceDraft{
		{ChunkID: "c-1", SourceStart: 100, SourceEnd: 140, Quote: "同一句话"},
		{ChunkID: "c-2", SourceStart: 100, SourceEnd: 140, Quote: "同一句话"},
		{ChunkID: "c-2", SourceStart: 300, SourceEnd: 340, Quote: "另一句话"},
	}
	got := dedupeEvidenceByDocumentInterval(evs)
	if len(got) != 2 {
		t.Fatalf("去重后 %d 条, want 2", len(got))
	}
	// ⚠️ 保留的必须是**先出现**的那条，顺序确定（宪法第 V 条）。
	if got[0].ChunkID != "c-1" || got[1].SourceStart != 300 {
		t.Errorf("去重结果顺序或取舍不对：%+v", got)
	}
}

// TestProjectionOnRealNarrativeChunks——拿真实分块产物走一遍：
// 每个块的 projection 都要能建起来，且映射回去的文字与原文一致。
func TestProjectionOnRealNarrativeChunks(t *testing.T) {
	text := "第一章　甲\n" + strings.Repeat("甲的正文。", 60) + "\n" +
		"第二章　乙\n" + strings.Repeat("乙的正文。", 60) + "\n"
	normalized := []rune(strings.ReplaceAll(text, "\r\n", "\n"))
	var checked int
	for i, piece := range chunkNarrative(text, 150, 40) {
		p, err := newChunkProjection(piece.Content, *piece.Narrative)
		if err != nil {
			t.Fatalf("第 %d 块建 projection 失败：%v", i, err)
		}
		if p.Text == "" {
			t.Fatalf("第 %d 块可引用正文为空", i)
		}
		// 整段映射回文档，取出来的文字必须与 projection 的正文一致。
		s, e, ok := p.toDocument(0, len([]rune(p.Text)))
		if !ok {
			continue // 跨不相邻区间，属于合法拒绝
		}
		checked++
		want := strings.Join(strings.Fields(p.Text), "")
		got := strings.Join(strings.Fields(string(normalized[s:e])), "")
		if got != want {
			t.Errorf("第 %d 块映射回去对不上：\n got=%.40q\nwant=%.40q", i, got, want)
		}
	}
	if checked < 3 {
		t.Fatalf("只验证了 %d 块，夹具太弱", checked)
	}
}
