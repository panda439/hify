package knowledge

import (
	"strings"
	"testing"
)

func TestReviewExactSourceMap(t *testing.T) {
	src := "第一章　甲\n甲。\n\n\n\n乙。"
	for _, p := range chunkNarrative(src, 500, 0) {
		for _, s := range p.Narrative.Segments {
			if s.DocumentStart == nil {
				continue
			}
			a := string([]rune(p.Content)[s.ChunkStart:s.ChunkEnd])
			b := string([]rune(src)[*s.DocumentStart:*s.DocumentEnd])
			if a != b {
				t.Errorf("source mismatch chunk=%q source=%q; validator=%v", a, b, validateNarrativeMetadata(*p.Narrative, len([]rune(p.Content))))
			}
		}
	}
}
func TestReviewMarkdownChapter(t *testing.T) {
	p := chunkDocument(FileTypeMD, parsedContent{Text: "# 第一章 初见\n甲。\n# 第二章 别离\n乙。"}, 500, 0, true)
	if len(p) != 2 || p[0].ChapterNumber == nil {
		t.Fatalf("markdown chapters lost: pieces=%d chapter=%v", len(p), p[0].ChapterNumber)
	}
}
func TestReviewMarkdownFence(t *testing.T) {
	src := "第一章　示例\n```text\n第二章　这是代码\n***\n```\n正文。"
	p := chunkDocument(FileTypeMD, parsedContent{Text: src}, 500, 0, true)
	if len(p) != 1 {
		t.Fatalf("code fence became chapters/scenes: pieces=%d", len(p))
	}
}
func TestReviewExplicitBoundaryForms(t *testing.T) {
	for _, sep := range []string{"\n※\n", "\n\n\n"} {
		p := chunkNarrative("甲。"+sep+"乙。", 500, 0)
		if len(p) != 2 {
			t.Errorf("boundary %q ignored, pieces=%d", sep, len(p))
		}
	}
}
func TestReviewUnstructuredSceneUnknown(t *testing.T) {
	p := chunkNarrative("没有任何结构的普通正文。", 500, 0)
	if p[0].Narrative.SceneKey != nil || p[0].Narrative.BoundaryKind != "none" {
		t.Fatalf("invented scene: %+v", *p[0].Narrative)
	}
}
func TestReviewOverlapRemainsLocatable(t *testing.T) {
	p := chunkNarrative("第一章　甲\n"+strings.Repeat("甲", 20)+"。\n\n"+strings.Repeat("乙", 20)+"。", 30, 5)
	for _, c := range p {
		for _, s := range c.Narrative.Segments {
			if s.IsOverlapCopy && !s.IsGeneratedSeparator && s.DocumentStart == nil {
				t.Fatalf("real overlap text has no source interval: %q", string([]rune(c.Content)[s.ChunkStart:s.ChunkEnd]))
			}
		}
	}
}
func TestReviewPDFSupported(t *testing.T) {
	if e := validateUploadOptions(FileTypePDF, UploadOptions{Narrative: true}, ""); e != nil {
		t.Fatal(e)
	}
}
func TestReviewUploadContract(t *testing.T) {
	spy := &uploadOptionsSpy{}
	code, _ := doUpload(t, spy, "kb", "a.txt", []byte("正文"), map[string]string{"narrative_mode": "true"})
	if code != 200 || !spy.got.Narrative {
		t.Fatalf("documented field silently ignored: code=%d opts=%+v", code, spy.got)
	}
}

func TestReviewStrictUploadBooleans(t *testing.T) {
	for _, fields := range []map[string]string{{"narrative_mode": "ture"}, {"extract_relations": "1"}, {"narrative_mode": "true", "is_narrative": "false"}} {
		spy := &uploadOptionsSpy{}
		code, _ := doUpload(t, spy, "kb", "a.txt", []byte("正文"), fields)
		if code != 400 {
			t.Errorf("invalid flags accepted: %v status=%d", fields, code)
		}
	}
}
func TestReviewDefaultResponseUnchanged(t *testing.T) {
	spy := &uploadOptionsSpy{}
	_, body := doUpload(t, spy, "kb", "a.txt", []byte("正文"), nil)
	if strings.Contains(body, "is_narrative") || strings.Contains(body, "is_relation_extraction_enabled") {
		t.Fatalf("default response gained fields: %s", body)
	}
}
func TestReviewPDFPageSegments(t *testing.T) {
	pages := []pdfPage{{Number: 1, Text: "第一章 初见\n甲。\n***\n乙。"}, {Number: 2, Text: "乙。\n\n\n丙。"}}
	pieces := chunkDocument(FileTypePDF, parsedContent{Pages: pages}, 500, 0, true)
	if len(pieces) != 2 {
		t.Fatalf("PDF explicit divider ignored or layout blank used: %d", len(pieces))
	}
	seen := map[int]bool{}
	for _, p := range pieces {
		if p.PageNumber == nil || p.PageEnd == nil {
			t.Fatal("missing page range")
		}
		for _, s := range p.Narrative.Segments {
			if !s.IsGeneratedSeparator {
				if s.Page == nil {
					t.Fatal("missing segment page")
				}
				seen[*s.Page] = true
			}
		}
	}
	if !seen[1] || !seen[2] {
		t.Fatalf("page provenance lost: %v", seen)
	}
}
func TestReviewDividerRetainedAndBoundaryHonest(t *testing.T) {
	p := chunkNarrative("甲。\n※\n乙。", 500, 0)
	if len(p) != 2 || !strings.Contains(p[0].Content, "※") {
		t.Fatalf("divider lost: %+v", p)
	}
	p = chunkNarrative("第一章 初见\n甲。", 500, 0)
	if p[0].Narrative.BoundaryKind != "chapter_fallback" {
		t.Fatalf("chapter falsely claimed as scene: %+v", p[0].Narrative)
	}
}

func TestReviewExactMappingAcrossFallbacks(t *testing.T) {
	sources := []string{"甲。\n\n乙。\n\n甲。", "第一章 标题\n甲。\n\n\n\n乙。", strings.Repeat("甲乙丙丁", 25), "  甲。   乙！\n丙？  丁。", "第一章 标题\n" + strings.Repeat("甲乙。", 60) + "\n***\n甲乙。"}
	for _, src := range sources {
		for _, size := range []int{1, 2, 5, 10, 30, 500} {
			for _, ov := range []int{0, 1, size / 2, size - 1} {
				normalized := []rune(src)
				for _, p := range chunkNarrative(src, size, ov) {
					if len([]rune(p.Content)) > size {
						t.Fatal("size limit exceeded")
					}
					if err := validateNarrativeMetadata(*p.Narrative, len([]rune(p.Content))); err != nil {
						t.Fatal(err)
					}
					for _, s := range p.Narrative.Segments {
						if s.IsGeneratedSeparator {
							continue
						}
						got := string(normalized[*s.DocumentStart:*s.DocumentEnd])
						want := string([]rune(p.Content)[s.ChunkStart:s.ChunkEnd])
						if got != want {
							t.Fatalf("size=%d overlap=%d source=%q chunk=%q", size, ov, got, want)
						}
					}
				}
			}
		}
	}
}
func TestReviewMarkdownTildeAndLongFences(t *testing.T) {
	for _, fence := range []string{"~~~", "````"} {
		src := "# 第两百章 标题\n" + fence + "text\n第二章 代码\n```\n※\n\n\n代码\n" + fence + "\n正文。"
		p := chunkDocument(FileTypeMD, parsedContent{Text: src}, 500, 0, true)
		if len(p) != 1 || p[0].ChapterNumber == nil || *p[0].ChapterNumber != 200 {
			t.Fatalf("fence/heading parse failed: %+v", p)
		}
	}
}

func TestReviewPDFMergedParagraphKeepsExactPages(t *testing.T) {
	pages := []pdfPage{streamPage(1, "the story crosses the page and"), streamPage(2, "continues here before ending.", "***", "a new scene.")}
	text, pageAt := narrativePDFSource(pages)
	if !strings.Contains(text, "and\ncontinues") {
		t.Fatal("existing cross-page merge lost")
	}
	for _, size := range []int{15, 500} {
		pieces := chunkNarrativePDF(pages, size, 5)
		for _, p := range pieces {
			if err := validateNarrativeMetadata(*p.Narrative, len([]rune(p.Content))); err != nil {
				t.Fatal(err)
			}
			for _, s := range p.Narrative.Segments {
				if s.IsGeneratedSeparator {
					if s.DocumentStart != nil || s.Page != nil {
						t.Fatal("generated separator became evidence")
					}
					continue
				}
				for pos := *s.DocumentStart; pos < *s.DocumentEnd; pos++ {
					if s.Page == nil || pageAt[pos] != *s.Page {
						t.Fatal("segment spans wrongly attributed pages")
					}
				}
				if string([]rune(text)[*s.DocumentStart:*s.DocumentEnd]) != string([]rune(p.Content)[s.ChunkStart:s.ChunkEnd]) {
					t.Fatal("PDF source mismatch")
				}
			}
		}
		if size == 500 && (len(pieces) != 2 || *pieces[0].PageNumber != 1 || *pieces[0].PageEnd != 2) {
			t.Fatalf("scene broken at physical page: %+v", pieces)
		}
	}
}

func TestReviewStructuredSentenceKeepsClosingQuote(t *testing.T) {
	pieces := chunkNarrative("第一章 甲\n\n“甲。” “乙。”", 5, 0)
	for _, p := range pieces {
		if strings.HasSuffix(p.Content, "甲。") || strings.HasPrefix(p.Content, "”") {
			t.Fatalf("sentence closing quote detached: %q", p.Content)
		}
	}
}
