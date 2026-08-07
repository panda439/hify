package conversation

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"hify/internal/knowledge"
)

func candidate(id, content string, score float64) knowledge.RetrievedChunk {
	return knowledge.RetrievedChunk{
		Chunk: knowledge.Chunk{ID: id, KnowledgeBaseID: "kb-1", DocumentID: "doc-1", DocumentName: "doc.txt", Content: content},
		Score: score,
	}
}

func refs(evidence []Evidence) []string {
	out := make([]string, len(evidence))
	for i, e := range evidence {
		out[i] = e.Ref
	}
	return out
}

// evidenceLenAfterInclusion is the exact rendered length of the nth
// (1-indexed) accepted candidate's <source> element — tests use this
// instead of hand-computed byte counts so the budgets they construct are
// always in terms of "how selectEvidence actually measures things" (its
// own renderedSourceLen), per the review fix's "不要写死数字，核对实际
// 消息长度" requirement.
func sourceRenderedLen(ref, documentName, content string) int {
	return len([]rune(formatSource(Evidence{Ref: ref, DocumentName: documentName, Content: content})))
}

// truncatedSourceRenderedLen is sourceRenderedLen's counterpart for
// budgets a test expects to trigger truncation — it must include the
// `truncated="true"` attribute's own overhead, since that's what
// truncateEvidenceToFit's binary search actually measures against (see
// its doc comment: the attribute is part of every candidate evaluated
// during the search, not added after the fact).
func truncatedSourceRenderedLen(ref, documentName, content string) int {
	return len([]rune(formatSource(Evidence{Ref: ref, DocumentName: documentName, Content: content, Truncated: true})))
}

func TestSelectEvidenceFiltersLowScoreChunks(t *testing.T) {
	// 完全无关的低分 chunk（低于 ragMinSimilarityScore）不能永远进入上下文
	// —— 这是阈值必须有测试覆盖的那条规则。给足够大的预算，确保这里测的
	// 是分数过滤，不是预算过滤。
	candidates := []knowledge.RetrievedChunk{
		candidate("c-relevant", "相关内容", 0.85),
		candidate("c-irrelevant", "完全无关内容", 0.05),
	}
	evidence, filteredByScore, filteredByBudget := selectEvidence(candidates, 10000)
	if len(evidence) != 1 || evidence[0].ChunkID != "c-relevant" {
		t.Fatalf("evidence = %+v, want only c-relevant", evidence)
	}
	if filteredByScore != 1 {
		t.Fatalf("filteredByScore = %d, want 1", filteredByScore)
	}
	if filteredByBudget != 0 {
		t.Fatalf("filteredByBudget = %d, want 0", filteredByBudget)
	}
	if evidence[0].Ref != "S1" {
		t.Fatalf("ref = %q, want S1", evidence[0].Ref)
	}
}

func TestSelectEvidenceNoChunkClearsThresholdDegradesToNoRAG(t *testing.T) {
	candidates := []knowledge.RetrievedChunk{
		candidate("c1", "无关内容一", 0.01),
		candidate("c2", "无关内容二", -0.2),
	}
	evidence, filteredByScore, _ := selectEvidence(candidates, 10000)
	if len(evidence) != 0 {
		t.Fatalf("evidence = %+v, want empty (degrade to no-RAG answer)", evidence)
	}
	if filteredByScore != 2 {
		t.Fatalf("filteredByScore = %d, want 2", filteredByScore)
	}
}

func TestSelectEvidenceDedupesByChunkID(t *testing.T) {
	dup := candidate("c-dup", "重复内容", 0.9)
	candidates := []knowledge.RetrievedChunk{dup, dup, candidate("c-other", "其他内容", 0.8)}
	evidence, _, _ := selectEvidence(candidates, 10000)
	if len(evidence) != 2 {
		t.Fatalf("evidence = %+v, want 2 (deduped by chunk id)", evidence)
	}
}

func TestSelectEvidenceSkipsWholeChunkWhenBudgetExceeded(t *testing.T) {
	// 预算刚好够渲染出第一个 source（含标签/属性开销，不是裸内容长度），
	// 放不下第二个：必须整块跳过（不做字节截断），且不获得 ref。
	c1Content := strings.Repeat("a", 10)
	c2Content := strings.Repeat("b", 10)
	budget := sourceRenderedLen("S1", "doc.txt", c1Content)

	candidates := []knowledge.RetrievedChunk{
		candidate("c1", c1Content, 0.9),
		candidate("c2", c2Content, 0.8),
	}
	evidence, _, filteredByBudget := selectEvidence(candidates, budget)
	if len(evidence) != 1 || evidence[0].ChunkID != "c1" {
		t.Fatalf("evidence = %+v, want only c1", evidence)
	}
	if filteredByBudget != 1 {
		t.Fatalf("filteredByBudget = %d, want 1", filteredByBudget)
	}
	if evidence[0].Truncated {
		t.Fatalf("c1 fit whole, must not be marked truncated")
	}
}

func TestSelectEvidenceFilteredChunkGetsNoRef(t *testing.T) {
	c1Content := strings.Repeat("a", 10)
	c2Content := strings.Repeat("b", 10)
	c3Content := strings.Repeat("c", 5)
	candidates := []knowledge.RetrievedChunk{
		candidate("c1", c1Content, 0.9),
		candidate("c2", c2Content, 0.8),
		candidate("c3", c3Content, 0.7),
	}
	// 预算刚好够 c1 + c3 两个渲染后的 source（c2 因为放不下被跳过）；ref
	// 必须只分配给最终真正进入上下文的 c1/c3，且连续编号。
	budget := sourceRenderedLen("S1", "doc.txt", c1Content) + sourceRenderedLen("S2", "doc.txt", c3Content)
	evidence, _, _ := selectEvidence(candidates, budget)
	if len(evidence) != 2 {
		t.Fatalf("evidence = %+v, want 2", evidence)
	}
	if got := refs(evidence); got[0] != "S1" || got[1] != "S2" {
		t.Fatalf("refs = %v, want [S1 S2] continuous numbering skipping the filtered chunk", got)
	}
	for _, e := range evidence {
		if e.ChunkID == "c2" {
			t.Fatal("filtered-by-budget chunk must not appear in evidence at all")
		}
	}
}

func TestSelectEvidenceOversizedSingleChunkRuneSafeTruncated(t *testing.T) {
	// 单个 chunk 渲染后本身就超过整个 RAG 预算：必须走 rune 安全截断，而
	// 不是被整体跳过（否则预算很小的模型永远拿不到任何证据）。中文字符
	// 验证 UTF-8 不被截断破坏。内容要足够长，让"截到 5 个字符"确实比
	// "发送整段未截断内容"更省空间——截断attribute本身也有开销，内容太
	// 短时省下来的字符数反而抵不过这个开销。
	big := strings.Repeat("中", 200)                         // 200 runes
	fullRendered := sourceRenderedLen("S1", "doc.txt", big) // whole chunk, untruncated
	budget := truncatedSourceRenderedLen("S1", "doc.txt", string([]rune(big)[:5]))
	if budget >= fullRendered {
		t.Fatalf("test setup broken: budget %d should be smaller than the full rendered length %d", budget, fullRendered)
	}

	candidates := []knowledge.RetrievedChunk{candidate("c-big", big, 0.9)}
	evidence, _, filteredByBudget := selectEvidence(candidates, budget)
	if len(evidence) != 1 {
		t.Fatalf("evidence = %+v, want 1 truncated entry", evidence)
	}
	if filteredByBudget != 0 {
		t.Fatalf("an oversized chunk that got truncated must not also count as filtered-by-budget, got %d", filteredByBudget)
	}
	e := evidence[0]
	if !e.Truncated {
		t.Fatal("oversized chunk must be marked Truncated")
	}
	if !isValidUTF8(e.Content) {
		t.Fatalf("truncated content is not valid UTF-8: %q", e.Content)
	}
	// 渲染后的最终长度必须真的不超预算——这才是"根据实际渲染后的长度截断"
	// 要验证的东西，不是裸 rune 数。
	if got := renderedSourceLen(e); got > budget {
		t.Fatalf("truncated evidence still renders to %d chars, want <= budget %d", got, budget)
	}
}

func TestSelectEvidenceQuoteMatchesWhatWasSentToModel(t *testing.T) {
	// Evidence.Content（后续持久化为 Citation.Quote 的来源）必须是真正
	// 渲染进 <source> 的那段原始文本的精确前缀——截断后同理，不能出现
	// "保存的引用和模型实际看到的证据正文不一致"这种偏差。
	big := strings.Repeat("x", 100)
	budget := truncatedSourceRenderedLen("S1", "doc.txt", strings.Repeat("x", 10))
	candidates := []knowledge.RetrievedChunk{candidate("c-big", big, 0.9)}
	evidence, _, _ := selectEvidence(candidates, budget)
	if len(evidence) != 1 {
		t.Fatalf("evidence = %+v, want 1 truncated entry", evidence)
	}
	if !strings.HasPrefix(big, evidence[0].Content) {
		t.Fatalf("truncated content %q is not a prefix of the original chunk %q", evidence[0].Content, big)
	}
	// formatSource(evidence[0]) is exactly what got sent to the model (as
	// the escaped body inside <source>) — Content itself (pre-escape) is
	// what's persisted as the citation quote, so the two must trace back
	// to the same substring.
	if got := renderedSourceLen(evidence[0]); got > budget {
		t.Fatalf("rendered evidence = %d chars, exceeds budget %d", got, budget)
	}
}

func TestSelectEvidenceEmptyCandidatesReturnsEmpty(t *testing.T) {
	evidence, filteredByScore, filteredByBudget := selectEvidence(nil, 1000)
	if len(evidence) != 0 || filteredByScore != 0 || filteredByBudget != 0 {
		t.Fatalf("expected all-zero result for empty candidates, got %+v %d %d", evidence, filteredByScore, filteredByBudget)
	}
}

func TestSelectEvidenceDocumentNameFallsBackToDocumentIDWhenEmpty(t *testing.T) {
	c := candidate("c1", "内容", 0.9)
	c.DocumentName = "" // 历史 chunk，未回填 document_name
	evidence, _, _ := selectEvidence([]knowledge.RetrievedChunk{c}, 1000)
	if evidence[0].DocumentName != "doc-1" {
		t.Fatalf("DocumentName fallback = %q, want document id 'doc-1'", evidence[0].DocumentName)
	}
}

// --- review fix: rendered-length accounting (tags/metadata/escaping) ---

func TestSelectEvidenceAccountsForTagAndMetadataOverhead(t *testing.T) {
	// 两个候选内容完全一样长，但一个有更长的 document_name/section —— 渲染
	// 后的实际长度不同，预算判断必须跟着变，不能只看裸内容长度。
	content := strings.Repeat("a", 20)
	shortDocLen := sourceRenderedLen("S1", "a.md", content)
	longDocLen := sourceRenderedLen("S1", strings.Repeat("very-long-document-name", 5)+".md", content)
	if shortDocLen >= longDocLen {
		t.Fatalf("test setup broken: expected long document name to render longer (%d) than short (%d)", longDocLen, shortDocLen)
	}

	// 预算够放下短文档名版本，但放不下长文档名版本。
	c := knowledge.RetrievedChunk{
		Chunk: knowledge.Chunk{ID: "c1", KnowledgeBaseID: "kb-1", DocumentID: "doc-1",
			DocumentName: strings.Repeat("very-long-document-name", 5) + ".md", Content: content},
		Score: 0.9,
	}
	evidence, _, filteredByBudget := selectEvidence([]knowledge.RetrievedChunk{c}, shortDocLen)
	if len(evidence) != 0 || filteredByBudget != 1 {
		t.Fatalf("evidence = %+v filteredByBudget=%d, want the long-document-name source skipped (tag/metadata overhead must count)", evidence, filteredByBudget)
	}
}

func TestSelectEvidenceXMLEscapeInflationCountsAgainstBudget(t *testing.T) {
	// content 里全是会被转义放大的字符（& < > 各自变成多字符实体），裸
	// rune 数很小，但渲染后远大于此——budget 按裸内容长度给的话会误判
	// "放得下"，必须按转义后的真实长度判断。
	content := strings.Repeat("<&>", 20) // 60 runes raw, but escaped: &lt;&amp;&gt; per group = far longer
	rawLen := len([]rune(content))
	rendered := sourceRenderedLen("S1", "doc.txt", content)
	if rendered <= rawLen {
		t.Fatalf("test setup broken: escaped rendering (%d) should be much longer than raw content (%d)", rendered, rawLen)
	}

	// 预算刚好够放下裸内容长度但放不下转义后的真实长度：必须走截断或跳过，
	// 不能假装"content 有 rawLen 个字符所以放得下"就整块塞入。
	budget := rawLen + 5 // 明显小于 rendered，但如果错误地只看裸内容会被误判为能放下
	candidates := []knowledge.RetrievedChunk{candidate("c-esc", content, 0.9)}
	evidence, _, filteredByBudget := selectEvidence(candidates, budget)
	if len(evidence) == 1 {
		if got := renderedSourceLen(evidence[0]); got > budget {
			t.Fatalf("included evidence renders to %d chars, exceeds budget %d — escaping inflation was not accounted for", got, budget)
		}
		if !evidence[0].Truncated {
			t.Fatalf("evidence included whole despite escaped rendering exceeding budget without truncation: %+v", evidence[0])
		}
	} else if len(evidence) != 0 || filteredByBudget != 1 {
		t.Fatalf("unexpected result: evidence=%+v filteredByBudget=%d", evidence, filteredByBudget)
	}
}

func TestFormatSourceAndSelectEvidenceUseTheSameLengthAccounting(t *testing.T) {
	// renderedSourceLen 必须和 formatRetrievedSources 实际渲染出的长度
	// 完全一致——这是 selectEvidence 的"放得下"判断和最终真正发给模型的
	// 内容不会走样的唯一保证。
	e := Evidence{Ref: "S1", DocumentName: "架构 & 设计<v2>.md", Content: "内容里有 <tag> 和 & 符号，还有\"引号\"。"}
	if got, want := renderedSourceLen(e), len([]rune(formatSource(e))); got != want {
		t.Fatalf("renderedSourceLen = %d, want %d (must match formatSource's real output)", got, want)
	}
	full := formatRetrievedSources([]Evidence{e})
	if got, want := len([]rune(full)), wrapperOverheadChars()+renderedSourceLen(e); got != want {
		t.Fatalf("formatRetrievedSources total = %d, want wrapper(%d)+source(%d)=%d", got, wrapperOverheadChars(), renderedSourceLen(e), want)
	}
}

// --- Phase 4 review fix: 邻接块绝不能在预算不足时挤掉排名更低的核心块 ---
//
// knowledge.Service.Retrieve (via knowledge/neighbor.go's expandWithNeighbors,
// fixed in this round) now guarantees its output is two full priority tiers:
// every core Hybrid Search hit (anchor) first, in full RRF rank order, and
// only THEN every anchor's own neighbor chunks. selectEvidence itself is
// unchanged — it was always a strict-input-order greedy budget fill — but
// that's exactly why the FIX had to live in the input ordering, not here:
// as long as knowledge.Retrieve keeps handing selectEvidence "anchors then
// neighbors", a tight budget can never let a neighbor chunk consume the
// budget slot a lower-ranked-but-still-core anchor needed. These tests pin
// that guarantee at the selectEvidence boundary, independent of whether
// knowledge.Retrieve's own ordering ever regresses.

// neighborCandidate is candidate()'s Phase-4 counterpart — sets NeighborOf
// the way knowledge.expandWithNeighbors actually does (Score inherited from
// the owning anchor, NeighborOf set to the owning anchor's chunk ID).
// selectEvidence itself never reads NeighborOf (it's an opaque pass-through
// field, same as any other Chunk field) — these tests set it purely so a
// human reading the test can tell anchors and neighbors apart at a glance,
// exactly like knowledge.RetrievedChunk's real fields would look.
func neighborCandidate(id, content string, score float64, neighborOf string) knowledge.RetrievedChunk {
	c := candidate(id, content, score)
	c.NeighborOf = neighborOf
	return c
}

// 需求里明确要求的场景：anchor1（高分核心块）、neighbor1（NeighborOf=anchor1）、
// anchor2（第二个核心块），预算只够两个完整 source。anchor1 和 anchor2 都
// 必须被保留，neighbor1 必须因为预算不足被过滤——邻接块绝不能抢在任何核心
// 块之前消耗预算。
func TestSelectEvidenceNeighborNeverDisplacesALowerRankedAnchor(t *testing.T) {
	anchor1Content := strings.Repeat("a", 10)
	anchor2Content := strings.Repeat("b", 10)
	neighbor1Content := strings.Repeat("c", 10)

	// 预算严格只够 S1（anchor1）+ S2（anchor2）两个完整渲染的 source，
	// 放不下第三个（neighbor1）。
	budget := sourceRenderedLen("S1", "doc.txt", anchor1Content) + sourceRenderedLen("S2", "doc.txt", anchor2Content)

	// 顺序照抄 expandWithNeighbors 修复后的真实输出布局：全部核心块在前，
	// 邻接块在后——不是 anchor1、neighbor1、anchor2 交替。
	candidates := []knowledge.RetrievedChunk{
		candidate("anchor1", anchor1Content, 0.95),
		candidate("anchor2", anchor2Content, 0.80),
		neighborCandidate("neighbor1", neighbor1Content, 0.95, "anchor1"),
	}

	evidence, _, filteredByBudget := selectEvidence(candidates, budget)

	if len(evidence) != 2 {
		t.Fatalf("evidence = %+v, want exactly 2 (anchor1, anchor2)", evidence)
	}
	if evidence[0].ChunkID != "anchor1" {
		t.Fatalf("evidence[0].ChunkID = %q, want anchor1", evidence[0].ChunkID)
	}
	if evidence[1].ChunkID != "anchor2" {
		t.Fatalf("evidence[1].ChunkID = %q, want anchor2 — a core hit must never be displaced by a neighbor chunk", evidence[1].ChunkID)
	}
	for _, e := range evidence {
		if e.ChunkID == "neighbor1" {
			t.Fatal("neighbor1 must not appear in evidence — it must lose the budget contest to anchor2, a lower-ranked but still core hit")
		}
	}
	if filteredByBudget != 1 {
		t.Fatalf("filteredByBudget = %d, want 1 (neighbor1)", filteredByBudget)
	}
}

// 建议场景 1：两个核心块 + 多个邻接块，预算充足时全部按新顺序（anchor,
// anchor, ..., neighbor, neighbor, ...）原样返回，ref 编号也跟随这个顺序。
func TestSelectEvidenceSufficientBudgetKeepsAllAnchorsThenAllNeighborsInOrder(t *testing.T) {
	content := strings.Repeat("x", 10)
	candidates := []knowledge.RetrievedChunk{
		candidate("anchor1", content, 0.95),
		candidate("anchor2", content, 0.90),
		neighborCandidate("anchor1-prev", content, 0.95, "anchor1"),
		neighborCandidate("anchor1-next", content, 0.95, "anchor1"),
		neighborCandidate("anchor2-prev", content, 0.90, "anchor2"),
		neighborCandidate("anchor2-next", content, 0.90, "anchor2"),
	}

	evidence, filteredByScore, filteredByBudget := selectEvidence(candidates, 100000)

	if filteredByScore != 0 || filteredByBudget != 0 {
		t.Fatalf("filteredByScore=%d filteredByBudget=%d, want 0/0 (budget generous enough for everything)", filteredByScore, filteredByBudget)
	}
	wantOrder := []string{"anchor1", "anchor2", "anchor1-prev", "anchor1-next", "anchor2-prev", "anchor2-next"}
	if len(evidence) != len(wantOrder) {
		t.Fatalf("evidence = %+v, want %d entries", evidence, len(wantOrder))
	}
	for i, want := range wantOrder {
		if evidence[i].ChunkID != want {
			t.Fatalf("evidence[%d].ChunkID = %q, want %q (order must match the anchors-then-neighbors input order)", i, evidence[i].ChunkID, want)
		}
		if wantRef := fmt.Sprintf("S%d", i+1); evidence[i].Ref != wantRef {
			t.Fatalf("evidence[%d].Ref = %q, want %q", i, evidence[i].Ref, wantRef)
		}
	}
}

// 建议场景 2：预算只够容纳全部核心块，一个邻接块都放不下——全部核心块必须
// 保留，全部邻接块必须被过滤，不能出现"保留了某个邻接块但漏掉了某个核心
// 块"这种情况。
func TestSelectEvidenceBudgetForAllAnchorsFiltersEveryNeighbor(t *testing.T) {
	anchor1Content := strings.Repeat("a", 10)
	anchor2Content := strings.Repeat("b", 10)
	neighborContent := strings.Repeat("c", 10)

	budget := sourceRenderedLen("S1", "doc.txt", anchor1Content) + sourceRenderedLen("S2", "doc.txt", anchor2Content)

	candidates := []knowledge.RetrievedChunk{
		candidate("anchor1", anchor1Content, 0.95),
		candidate("anchor2", anchor2Content, 0.80),
		neighborCandidate("anchor1-prev", neighborContent, 0.95, "anchor1"),
		neighborCandidate("anchor1-next", neighborContent, 0.95, "anchor1"),
		neighborCandidate("anchor2-prev", neighborContent, 0.80, "anchor2"),
		neighborCandidate("anchor2-next", neighborContent, 0.80, "anchor2"),
	}

	evidence, _, filteredByBudget := selectEvidence(candidates, budget)

	if got := refs(evidence); len(got) != 2 || got[0] != "S1" || got[1] != "S2" {
		t.Fatalf("refs = %v, want exactly [S1 S2] (both anchors, no neighbor)", got)
	}
	for _, e := range evidence {
		if e.ChunkID != "anchor1" && e.ChunkID != "anchor2" {
			t.Fatalf("evidence contains a non-anchor chunk %q — every neighbor must be filtered when the budget only covers the anchors", e.ChunkID)
		}
	}
	if filteredByBudget != 4 {
		t.Fatalf("filteredByBudget = %d, want 4 (all four neighbor chunks)", filteredByBudget)
	}
}

// 建议场景 3：预算连所有核心块都放不下——selectEvidence 现有的"整块跳过、
// 不做字节截断、继续尝试下一个候选"规则依然按核心块的 RRF 排名顺序依次
// 取舍：排名更靠前但放不下的核心块被跳过，排名更靠后但能放下的核心块仍然
// 保留,不会因为前一个核心块放不下就连带把后一个也漏掉。
func TestSelectEvidenceBudgetTooSmallForAllAnchorsStillTriesEachAnchorInRankOrder(t *testing.T) {
	bigAnchorContent := strings.Repeat("a", 200) // anchor1: 排名更高，但太大放不下
	smallAnchorContent := strings.Repeat("b", 5) // anchor2: 排名更低，但能放下
	neighborContent := strings.Repeat("c", 5)

	budget := sourceRenderedLen("S1", "doc.txt", smallAnchorContent) // 只够 anchor2 那么大的一个 source

	candidates := []knowledge.RetrievedChunk{
		candidate("anchor1", bigAnchorContent, 0.95),
		candidate("anchor2", smallAnchorContent, 0.80),
		neighborCandidate("anchor1-next", neighborContent, 0.95, "anchor1"),
	}

	evidence, _, filteredByBudget := selectEvidence(candidates, budget)

	if len(evidence) != 1 || evidence[0].ChunkID != "anchor2" {
		t.Fatalf("evidence = %+v, want exactly [anchor2] (anchor1 skipped for being too big, anchor2 still tried and kept in its own rank order)", evidence)
	}
	if filteredByBudget != 2 {
		t.Fatalf("filteredByBudget = %d, want 2 (anchor1 and anchor1-next)", filteredByBudget)
	}
}

// 建议场景 4：邻接查询返回顺序变化（模拟不同 findPublishedNeighborChunks
// 分组的返回顺序在多次运行间不同）不影响最终输出——只要核心块本身的相对
// 顺序不变，两种不同的邻接块排列在预算充足时必须产出完全一样的核心块
// 保留结果（邻接块集合本身允许因为顺序不同而在 tie-break 场景下产生不同
// 归属，这里用互不重叠的邻接块规避这个歧义，只验证核心块这条不变量）。
func TestSelectEvidenceAnchorOutcomeUnaffectedByNeighborOrder(t *testing.T) {
	anchor1Content := strings.Repeat("a", 10)
	anchor2Content := strings.Repeat("b", 10)
	neighborContent := strings.Repeat("c", 10)

	budget := sourceRenderedLen("S1", "doc.txt", anchor1Content) + sourceRenderedLen("S2", "doc.txt", anchor2Content)

	orderA := []knowledge.RetrievedChunk{
		candidate("anchor1", anchor1Content, 0.95),
		candidate("anchor2", anchor2Content, 0.80),
		neighborCandidate("anchor1-prev", neighborContent, 0.95, "anchor1"),
		neighborCandidate("anchor2-next", neighborContent, 0.80, "anchor2"),
	}
	orderB := []knowledge.RetrievedChunk{
		candidate("anchor1", anchor1Content, 0.95),
		candidate("anchor2", anchor2Content, 0.80),
		neighborCandidate("anchor2-next", neighborContent, 0.80, "anchor2"), // 邻接块内部顺序反过来
		neighborCandidate("anchor1-prev", neighborContent, 0.95, "anchor1"),
	}

	evidenceA, _, _ := selectEvidence(orderA, budget)
	evidenceB, _, _ := selectEvidence(orderB, budget)

	wantAnchors := []string{"anchor1", "anchor2"}
	if got := refs(evidenceA); len(evidenceA) != 2 || evidenceA[0].ChunkID != wantAnchors[0] || evidenceA[1].ChunkID != wantAnchors[1] {
		t.Fatalf("orderA evidence = %+v (refs %v), want anchors %v kept", evidenceA, got, wantAnchors)
	}
	if got := refs(evidenceB); len(evidenceB) != 2 || evidenceB[0].ChunkID != wantAnchors[0] || evidenceB[1].ChunkID != wantAnchors[1] {
		t.Fatalf("orderB evidence = %+v (refs %v), want anchors %v kept", evidenceB, got, wantAnchors)
	}
}

func isValidUTF8(s string) bool {
	return utf8.ValidString(s)
}
