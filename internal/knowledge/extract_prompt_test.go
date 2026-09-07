package knowledge

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// extract_prompt_test.go 守第一阶段响应的严格解析与原文定位（010 T024/T025）。
//
// ⭐ 这里几乎每一条用例都是「模型给了个看起来没问题的东西」：重复 key、
// 尾随第二个对象、引用了一句读起来像原文但书里没有的话。它们全都不会让
// encoding/json 报错，也不会有任何运行时症状——只会让一条没有原文支持的
// 关系被当成书里的事实发布出去。

// chunkFixture 造一个带完整来源坐标的块：正文 40 rune，落在原文 [100,140)。
func chunkFixture(content string, docStart int) extractionChunkView {
	runes := []rune(content)
	end := docStart + len(runes)
	return extractionChunkView{
		ChunkID: "c-1", DocumentVersion: 1, Content: content,
		Meta: narrativeMetadata{
			SchemaVersion: narrativeMetadataSchemaVersion,
			BoundaryKind:  boundaryChapter, SourceOrder: 12,
			Segments: []narrativeSegment{{
				ChunkStart: 0, ChunkEnd: len(runes),
				DocumentStart: intPtr(docStart), DocumentEnd: intPtr(end),
			}},
		},
	}
}

const sampleChunk = "赵太爷跳过去给了他一个嘴巴。阿Q回到土谷祠，赵太爷还在骂阿Q。"

func validResponseJSON() string {
	return `{"mentions":[{"ref":"m1","surface":"阿Q","occurrence":0},
	                     {"ref":"m2","surface":"赵太爷","occurrence":0}],
	         "relations":[{"subject_ref":"m2","object_ref":"m1","type":"欺凌",
	                       "evidence":[{"quote":"赵太爷跳过去给了他一个嘴巴","occurrence":0}]}],
	         "alias_proposals":[]}`
}

func mustParse(t *testing.T, raw string) extractResponse {
	t.Helper()
	resp, err := parseExtractionResponse([]byte(raw))
	if err != nil {
		t.Fatalf("expected a valid response, got %v", err)
	}
	return resp
}

// TestValidResponseParsesAndResolves 是正例：合法响应 → 引用落到原文坐标。
func TestValidResponseParsesAndResolves(t *testing.T) {
	chunk := chunkFixture(sampleChunk, 100)
	resolved, err := resolveExtraction(chunk, mustParse(t, validResponseJSON()))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(resolved.Mentions) != 2 || len(resolved.Relations) != 1 {
		t.Fatalf("got %d mentions / %d relations", len(resolved.Mentions), len(resolved.Relations))
	}
	// 赵太爷在块首，原文坐标就是块的起点。
	if got := resolved.Mentions[1]; got.DocumentStart != 100 || got.DocumentEnd != 103 {
		t.Errorf("赵太爷 mapped to [%d,%d), want [100,103)", got.DocumentStart, got.DocumentEnd)
	}
	ev := resolved.Relations[0].Evidence[0]
	if ev.SourceStart != 100 || ev.SourceEnd != 113 {
		t.Errorf("evidence mapped to [%d,%d), want [100,113)", ev.SourceStart, ev.SourceEnd)
	}
	if ev.SourceOrder != 12 || ev.ChunkID != "c-1" || ev.DocumentVersion != 1 {
		t.Errorf("evidence lost its chunk identity: %+v", ev)
	}
	if !resolved.Relations[0].IsDirected {
		t.Error("欺凌 must stay directed — 施加者→承受者")
	}
	var segs []narrativeSegment
	if err := json.Unmarshal(ev.SourceSegments, &segs); err != nil {
		t.Fatalf("evidence segments are not valid JSON: %v", err)
	}
	if len(segs) != 1 || segs[0].ChunkStart != 0 || segs[0].ChunkEnd != 13 {
		t.Errorf("evidence segments not clipped to the quote: %+v", segs)
	}
}

// TestEmptyResultIsALegalSuccess：全空对象是合法的成功结果（契约 §1 末段）。
// ⚠️ 把"这一块没有关系"当失败，会让失败率里混进大量正常情况，
// 真正的故障就此淹没在噪音里。
func TestEmptyResultIsALegalSuccess(t *testing.T) {
	chunk := chunkFixture(sampleChunk, 100)
	resp := mustParse(t, `{"mentions":[],"relations":[],"alias_proposals":[]}`)
	resolved, err := resolveExtraction(chunk, resp)
	if err != nil {
		t.Fatalf("empty result must be accepted: %v", err)
	}
	if len(resolved.Mentions) != 0 || len(resolved.Relations) != 0 {
		t.Errorf("empty in, non-empty out: %+v", resolved)
	}
}

// TestStrictJSONRejections 覆盖 encoding/json 会默默接受的那几种输出。
func TestStrictJSONRejections(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"重复 key（后者覆盖前者，两种读法）", `{"mentions":[],"mentions":[{"ref":"m1","surface":"阿Q","occurrence":0}],"relations":[],"alias_proposals":[]}`},
		{"尾随第二个对象（结果输出了两遍）", `{"mentions":[],"relations":[],"alias_proposals":[]}{"mentions":[]}`},
		{"未知字段", `{"mentions":[],"relations":[],"alias_proposals":[],"confidence":0.9}`},
		{"Markdown 围栏", "```json\n{\"mentions\":[],\"relations\":[],\"alias_proposals\":[]}\n```"},
		{"空输出", "   "},
		{"截断（finish_reason=length）", `{"mentions":[{"ref":"m1","surface":"阿Q","occ`},
		{"缺少 relations 数组", `{"mentions":[],"alias_proposals":[]}`},
		{"顶层是数组", `[{"mentions":[]}]`},
		{"嵌套对象里的重复 key", `{"mentions":[{"ref":"m1","ref":"m2","surface":"阿Q","occurrence":0}],"relations":[],"alias_proposals":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseExtractionResponse([]byte(tc.raw)); !errors.Is(err, errExtractionResponseInvalid) {
				t.Fatalf("accepted a malformed response, err = %v", err)
			}
		})
	}
}

// TestSemanticRejections：结构合法但语义违约，同样整次拒绝。
func TestSemanticRejections(t *testing.T) {
	mention := `{"ref":"m1","surface":"阿Q","occurrence":0},{"ref":"m2","surface":"赵太爷","occurrence":0}`
	cases := []struct {
		name string
		raw  string
	}{
		{"集合外的关系类型", `{"mentions":[` + mention + `],"relations":[{"subject_ref":"m1","object_ref":"m2","type":"其他","evidence":[{"quote":"阿Q","occurrence":0}]}],"alias_proposals":[]}`},
		{"关系端点不是 mention", `{"mentions":[` + mention + `],"relations":[{"subject_ref":"m1","object_ref":"m9","type":"冲突","evidence":[{"quote":"阿Q","occurrence":0}]}],"alias_proposals":[]}`},
		{"关系指向自己", `{"mentions":[` + mention + `],"relations":[{"subject_ref":"m1","object_ref":"m1","type":"冲突","evidence":[{"quote":"阿Q","occurrence":0}]}],"alias_proposals":[]}`},
		{"证据为空数组", `{"mentions":[` + mention + `],"relations":[{"subject_ref":"m1","object_ref":"m2","type":"冲突","evidence":[]}],"alias_proposals":[]}`},
		{"证据超过 4 条", `{"mentions":[` + mention + `],"relations":[{"subject_ref":"m1","object_ref":"m2","type":"冲突","evidence":[{"quote":"a","occurrence":0},{"quote":"b","occurrence":0},{"quote":"c","occurrence":0},{"quote":"d","occurrence":0},{"quote":"e","occurrence":0}]}],"alias_proposals":[]}`},
		{"重复的 mention ref", `{"mentions":[{"ref":"m1","surface":"阿Q","occurrence":0},{"ref":"m1","surface":"赵太爷","occurrence":0}],"relations":[],"alias_proposals":[]}`},
		{"surface 为空", `{"mentions":[{"ref":"m1","surface":"","occurrence":0}],"relations":[],"alias_proposals":[]}`},
		{"occurrence 为负", `{"mentions":[{"ref":"m1","surface":"阿Q","occurrence":-1}],"relations":[],"alias_proposals":[]}`},
		{"别名提案引用了不存在的 mention", `{"mentions":[` + mention + `],"relations":[],"alias_proposals":[{"left":"m1","right":"m9","quote":"阿Q","occurrence":0}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseExtractionResponse([]byte(tc.raw)); !errors.Is(err, errExtractionResponseInvalid) {
				t.Fatalf("accepted an out-of-contract response, err = %v", err)
			}
		})
	}
}

// TestOversizedResponseIsRejected：规模上限。
func TestOversizedResponseIsRejected(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"mentions":[`)
	for i := 0; i <= maxExtractMentions; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"ref":"m`)
		sb.WriteString(string(rune('a' + i%26)))
		sb.WriteString(string(rune('a' + i/26)))
		sb.WriteString(`","surface":"阿Q","occurrence":0}`)
	}
	sb.WriteString(`],"relations":[],"alias_proposals":[]}`)
	if _, err := parseExtractionResponse([]byte(sb.String())); !errors.Is(err, errExtractionResponseInvalid) {
		t.Fatalf("accepted %d mentions, err = %v", maxExtractMentions+1, err)
	}

	long := strings.Repeat("原", maxExtractQuoteRunes+1)
	raw := `{"mentions":[{"ref":"m1","surface":"阿Q","occurrence":0},{"ref":"m2","surface":"赵太爷","occurrence":0}],
	         "relations":[{"subject_ref":"m1","object_ref":"m2","type":"冲突","evidence":[{"quote":"` + long + `","occurrence":0}]}],
	         "alias_proposals":[]}`
	if _, err := parseExtractionResponse([]byte(raw)); !errors.Is(err, errExtractionResponseInvalid) {
		t.Fatalf("accepted an oversized quote, err = %v", err)
	}
}

// TestQuoteMustExistInTheChunk：引用必须在当前块原文里精确命中。
//
// ⭐ 这是整个抽取里最重要的一道关卡：模型可以生成一句读起来完全像原文的话。
func TestQuoteMustExistInTheChunk(t *testing.T) {
	chunk := chunkFixture(sampleChunk, 100)
	raw := `{"mentions":[{"ref":"m1","surface":"阿Q","occurrence":0},{"ref":"m2","surface":"赵太爷","occurrence":0}],
	        "relations":[{"subject_ref":"m2","object_ref":"m1","type":"欺凌",
	                      "evidence":[{"quote":"赵太爷狠狠地打了阿Q一顿","occurrence":0}]}],
	        "alias_proposals":[]}`
	_, err := resolveExtraction(chunk, mustParse(t, raw))
	if !errors.Is(err, errQuoteNotFound) {
		t.Fatalf("accepted a fabricated quote, err = %v", err)
	}
}

// TestOccurrenceOutOfRangeRejectsTheWholeResponse：越界的 occurrence 不是
// "退回第 0 次"，是整次拒绝——退回去会静默地把引用指到另一处。
func TestOccurrenceOutOfRangeRejectsTheWholeResponse(t *testing.T) {
	chunk := chunkFixture(sampleChunk, 100)
	raw := `{"mentions":[{"ref":"m1","surface":"阿Q","occurrence":7}],"relations":[],"alias_proposals":[]}`
	if _, err := resolveExtraction(chunk, mustParse(t, raw)); !errors.Is(err, errQuoteNotFound) {
		t.Fatalf("accepted an out-of-range occurrence, err = %v", err)
	}
}

// TestOccurrenceCountsOverlappingMatches 固定 occurrence 的口径。
// ⚠️ "aaa" 里找 "aa"：重叠算两次，不重叠算一次。两种数法都自洽，但服务端
// 必须单方面定死一种并写进指令，否则引用会系统性地偏到同一句话的另一处。
func TestOccurrenceCountsOverlappingMatches(t *testing.T) {
	chunk := chunkFixture("阿阿阿Q", 0)
	loc := newQuoteLocator(chunk)
	start, end, err := loc.locate("阿阿", 1)
	if err != nil {
		t.Fatalf("overlapping match not found: %v", err)
	}
	if start != 1 || end != 3 {
		t.Errorf("second overlapping match at [%d,%d), want [1,3)", start, end)
	}
	if !strings.Contains(extractInstruction, "重叠也算一次") {
		t.Error("指令里必须写明重叠计数口径，否则模型数的和我们数的不是一回事")
	}
}

// TestQuoteAcrossGeneratedSeparatorIsRejected：引用不得跨越拼接分隔符。
//
// ⚠️ 那几个字符不在原文里，所以没有坐标。放过去的话，产出的区间会把两段
// 本不相邻的原文说成一句连续的话，而块内逐字比对照样能通过。
func TestQuoteAcrossGeneratedSeparatorIsRejected(t *testing.T) {
	// 内容 = "赵太爷" + "\n\n"（拼接） + "打阿Q"，两侧原文并不相邻。
	content := "赵太爷\n\n打阿Q"
	chunk := extractionChunkView{
		ChunkID: "c-2", DocumentVersion: 1, Content: content,
		Meta: narrativeMetadata{
			SchemaVersion: narrativeMetadataSchemaVersion, BoundaryKind: boundaryChapter,
			Segments: []narrativeSegment{
				{ChunkStart: 0, ChunkEnd: 3, DocumentStart: intPtr(10), DocumentEnd: intPtr(13)},
				{ChunkStart: 3, ChunkEnd: 5, IsGeneratedSeparator: true},
				{ChunkStart: 5, ChunkEnd: 8, DocumentStart: intPtr(80), DocumentEnd: intPtr(83)},
			},
		},
	}
	loc := newQuoteLocator(chunk)
	start, end, err := loc.locate("赵太爷\n\n打", 0)
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	if _, _, err := loc.documentRange(start, end); !errors.Is(err, errQuoteNotCitable) {
		t.Fatalf("accepted a quote crossing a generated separator, err = %v", err)
	}
	// 只落在一侧的引用仍然可用。
	start, end, err = loc.locate("打阿Q", 0)
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	ds, de, err := loc.documentRange(start, end)
	if err != nil || ds != 80 || de != 83 {
		t.Errorf("one-sided quote mapped to [%d,%d), err = %v; want [80,83)", ds, de, err)
	}
}

// TestOverlapCopyEvidenceDedupsBySourceInterval：overlap 复制进来的那一份
// 和正文里的那一份是**同一处原文**，只能算一条证据。
//
// ⚠️ 按引文字符串去重会把"书里真的说了两次"也压成一条。按坐标去重两者都对。
func TestOverlapCopyEvidenceDedupsBySourceInterval(t *testing.T) {
	// 块内容 = overlap 复制的 "阿Q" + 正文 "阿Q挨打"；复制那份指向 [50,52)，
	// 正文那份指向 [52,56)。模型两次引用 "阿Q"（occurrence 0 和 1）。
	content := "阿Q阿Q挨打"
	chunk := extractionChunkView{
		ChunkID: "c-3", DocumentVersion: 1, Content: content,
		Meta: narrativeMetadata{
			SchemaVersion: narrativeMetadataSchemaVersion, BoundaryKind: boundaryChapter,
			Segments: []narrativeSegment{
				{ChunkStart: 0, ChunkEnd: 2, DocumentStart: intPtr(50), DocumentEnd: intPtr(52), IsOverlapCopy: true},
				{ChunkStart: 2, ChunkEnd: 6, DocumentStart: intPtr(52), DocumentEnd: intPtr(56)},
			},
		},
	}
	loc := newQuoteLocator(chunk)

	// 同一处原文被引用两次（occurrence 0 与 0）→ 去重成一条。
	same, err := loc.resolveEvidence([]extractQuote{{Quote: "阿Q", Occurrence: 0}, {Quote: "阿Q", Occurrence: 0}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(same) != 1 {
		t.Errorf("同一处原文引用两次应当去重成 1 条，got %d", len(same))
	}

	// 两处不同的原文位置 → 保留两条。
	both, err := loc.resolveEvidence([]extractQuote{{Quote: "阿Q", Occurrence: 0}, {Quote: "阿Q", Occurrence: 1}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(both) != 2 {
		t.Fatalf("两处不同原文应当各留一条，got %d", len(both))
	}
	if both[0].SourceStart != 50 || both[1].SourceStart != 52 {
		t.Errorf("overlap 那份丢了自己的真实区间：%d / %d", both[0].SourceStart, both[1].SourceStart)
	}
}

// TestResolveIsDeterministic：同一份响应重复解析，结果必须逐字节一致
// （宪法第 V 条）。
func TestResolveIsDeterministic(t *testing.T) {
	chunk := chunkFixture(sampleChunk, 100)
	resp := mustParse(t, validResponseJSON())
	first, err := resolveExtraction(chunk, resp)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := resolveExtraction(chunk, resp)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(first)
		b, _ := json.Marshal(again)
		if string(a) != string(b) {
			t.Fatalf("run %d differs:\n%s\n%s", i, a, b)
		}
	}
}
