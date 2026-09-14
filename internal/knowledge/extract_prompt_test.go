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

func TestExtractInstructionAliasProposalsUseMentionRefs(t *testing.T) {
	instruction := buildExtractInstruction()
	for _, want := range []string{
		"left 和 right 必须填 mentions 数组里的 ref（如 m1、m2），不是人物称呼",
		`{"left":"m1","right":"m2","quote":"..."}`,
	} {
		if !strings.Contains(instruction, want) {
			t.Fatalf("alias proposal instruction missing %q", want)
		}
	}
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
//
// ⚠️ 2026-09-07 起这里**没有** occurrence 相关的用例了：位置由服务端定位，
// 模型给的 occurrence 一律忽略（字段本身还留着，为的是旧响应能回放——
// 见 extractMention.Occurrence 的注释）。一个"occurrence 为负就拒绝"的用例
// 现在守的是一段已经没人读的值，留着只会让人以为它还在起作用。
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

// TestOutOfRangeOccurrenceIsNowIgnored 固定契约变更后的行为：模型给的
// occurrence 一律**忽略**，不再因为它越界而拒整份响应。
//
// ⭐ 这正是变更的收益所在：预检里 14B 的 6 次失败全是越界（引文逐字正确、
// 只是"第几次"数错），按旧契约这 6 条全被拒；现在它们照常可用。
// ⚠️ 字段仍然被解析（旧响应要能回放），只是没有任何地方读它的值。
func TestOutOfRangeOccurrenceIsNowIgnored(t *testing.T) {
	chunk := chunkFixture(sampleChunk, 100)
	raw := `{"mentions":[{"ref":"m1","surface":"阿Q","occurrence":99}],"relations":[],"alias_proposals":[]}`
	resolved, err := resolveExtraction(chunk, mustParse(t, raw))
	if err != nil {
		t.Fatalf("越界的 occurrence 仍然导致失败：%v", err)
	}
	if len(resolved.Mentions) != 1 {
		t.Fatalf("got %d mentions", len(resolved.Mentions))
	}
	// 落在第一处「阿Q」上，而不是第 99 处。
	if got := resolved.Mentions[0]; got.ChunkStart != 14 {
		t.Errorf("落在 chunk[%d,%d)，应当是第一处", got.ChunkStart, got.ChunkEnd)
	}
	// 「阿Q」在这个块里出现两次，多义位置要被计出来。
	if resolved.AmbiguousPositions != 1 {
		t.Errorf("AmbiguousPositions = %d，应当把这处多义位置计出来", resolved.AmbiguousPositions)
	}
}

// TestRepeatedSurfaceTakesTheFirstMatchAndReportsCount 固定 2026-09-07 的
// 契约变更：位置由服务端定位，模型不再给 occurrence。
//
// ⭐ 变更的依据是实测：预检里 14B 的 6 次定位失败**全部**是 occurrence 数错
// （引文逐字正确，只是"第几次"报错了，且多为 off-by-one），也就是说我们把
// 一件模型做不好、服务端做得又快又准的事写进了协议，白白损失三分之一的
// 合法响应。
//
// ⚠️ 代价要能计量：同一句话出现多次时取第一处，matches 报出总次数，
// 调用方据此统计有多少证据落在多义位置上——不计量就等于假装没有这个代价。
func TestRepeatedSurfaceTakesTheFirstMatchAndReportsCount(t *testing.T) {
	chunk := chunkFixture("阿Q打了阿Q的邻居，阿Q笑了", 0)
	loc := newQuoteLocator(chunk)
	start, end, matches, err := loc.locate("阿Q")
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	if start != 0 || end != 2 {
		t.Errorf("取到 [%d,%d)，应当是第一处 [0,2)", start, end)
	}
	if matches != 3 {
		t.Errorf("matches = %d，原文里「阿Q」出现 3 次", matches)
	}

	// ⚠️ 指令里必须**不再**要求模型给 occurrence：两边口径不一致时，
	// 模型会继续输出一个我们已经不看的字段，而它数错了也没人发现。
	if strings.Contains(extractInstruction, "第几次出现，从 0 开始数") {
		t.Error("指令里还在要求模型数第几次出现，但服务端已经自己定位了")
	}
	if !strings.Contains(extractInstruction, "位置由系统在原文里查") {
		t.Error("指令里没有说明位置由系统定位")
	}
}

// TestFabricatedQuoteIsStillRejected：服务端自己定位**不等于**放宽引用校验。
// 原文里一次都找不到的引文，照样整次拒绝。
func TestFabricatedQuoteIsStillRejected(t *testing.T) {
	chunk := chunkFixture(sampleChunk, 100)
	loc := newQuoteLocator(chunk)
	if _, _, _, err := loc.locate("赵太爷狠狠地打了阿Q一顿"); !errors.Is(err, errQuoteNotFound) {
		t.Fatalf("编造的引文被接受了：err = %v", err)
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
	start, end, _, err := loc.locate("赵太爷\n\n打")
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	if _, _, err := loc.documentRange(start, end); !errors.Is(err, errQuoteNotCitable) {
		t.Fatalf("accepted a quote crossing a generated separator, err = %v", err)
	}
	// 只落在一侧的引用仍然可用。
	start, end, _, err = loc.locate("打阿Q")
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	ds, de, err := loc.documentRange(start, end)
	if err != nil || ds != 80 || de != 83 {
		t.Errorf("one-sided quote mapped to [%d,%d), err = %v; want [80,83)", ds, de, err)
	}
}

// TestSameQuoteCollapsesToTheFirstOccurrence 记录服务端定位换来的**代价**。
//
// 旧契约下，模型可以用 occurrence 0 / 1 分别指向 overlap 复制的那一份和正文
// 那一份，于是同一句话的两处出处各留一条证据。现在位置由服务端定位、
// 恒取第一处，同一句引文无论给几次都只会落到同一个区间，去重成一条。
//
// ⚠️ 这条用例存在的意义就是把这个损失钉死在测试里：将来有人看到"证据条数
// 变少了"，能在这里读到为什么，而不是当成回归去修。
// 对"这条关系成不成立"没有影响（任一处出现都同样支持它），但"书里说了两次"
// 这个信息确实拿不到了——除非模型给出两句**不同**的引文。
func TestSameQuoteCollapsesToTheFirstOccurrence(t *testing.T) {
	// 块内容 = overlap 复制的 "阿Q" + 正文 "阿Q挨打"；复制那份指向 [50,52)，
	// 正文那份指向 [52,56)。
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

	got, err := loc.resolveEvidence([]extractQuote{{Quote: "阿Q"}, {Quote: "阿Q"}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("同一句引文应当去重成 1 条，got %d", len(got))
	}
	if got[0].SourceStart != 50 {
		t.Errorf("没有落在第一处：SourceStart=%d，want 50", got[0].SourceStart)
	}

	// 两句**不同**的引文仍然各留一条——去重的键是原文区间，不是引文字符串。
	two, err := loc.resolveEvidence([]extractQuote{{Quote: "阿Q"}, {Quote: "挨打"}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(two) != 2 {
		t.Fatalf("两句不同的引文应当各留一条，got %d", len(two))
	}
}

// TestAmbiguousEvidenceIsCountedSeparatelyFromMentions：两个多义计数必须分开。
//
// ⭐ 称呼的多义几乎无害（人名在一段里重复出现是常态，mention 只是登场锚点），
// 而**证据引文**的多义才要紧：用户会拿着引用翻回原文，指错一处看到的上下文
// 就可能不是支持这条关系的那一处。合成一个数，那个几乎无害的大数字会盖住
// 真正要紧的小数字。
func TestAmbiguousEvidenceIsCountedSeparatelyFromMentions(t *testing.T) {
	// "阿Q" 出现两次（称呼多义），"挨打" 只出现一次（证据不多义）。
	chunk := chunkFixture("阿Q挨打，阿Q回家", 0)
	raw := `{"mentions":[{"ref":"m1","surface":"阿Q"},{"ref":"m2","surface":"回家"}],
	         "relations":[{"subject_ref":"m1","object_ref":"m2","type":"冲突",
	                       "evidence":[{"quote":"挨打"}]}],
	         "alias_proposals":[]}`
	resolved, err := resolveExtraction(chunk, mustParse(t, raw))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.AmbiguousPositions != 1 {
		t.Errorf("AmbiguousPositions = %d，「阿Q」出现两次应当计 1", resolved.AmbiguousPositions)
	}
	if resolved.AmbiguousEvidence != 0 {
		t.Errorf("AmbiguousEvidence = %d，「挨打」只出现一次不该计", resolved.AmbiguousEvidence)
	}

	// 反过来：证据引文本身多义时要计出来。
	chunk2 := chunkFixture("阿Q挨打，阿Q挨打", 0)
	raw2 := `{"mentions":[{"ref":"m1","surface":"阿Q"},{"ref":"m2","surface":"挨打"}],
	          "relations":[{"subject_ref":"m1","object_ref":"m2","type":"冲突",
	                        "evidence":[{"quote":"阿Q挨打"}]}],
	          "alias_proposals":[]}`
	resolved2, err := resolveExtraction(chunk2, mustParse(t, raw2))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved2.AmbiguousEvidence != 1 {
		t.Errorf("AmbiguousEvidence = %d，「阿Q挨打」出现两次应当计 1", resolved2.AmbiguousEvidence)
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
