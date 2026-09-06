package knowledge

import (
	"strings"
	"testing"
)

// relation_citation_test.go 守送进 prompt 的关系证据怎么选（010 T032）。
//
// ⭐ 两条硬约束：
//   - **只用一份既有的 RAG 预算**，不另开一份。两份预算的后果是上下文
//     悄悄超长，而超长的表现不是报错——是历史被多截掉几轮，回答慢慢变差。
//   - **不伪造相似度**。关系记录没有向量分数；编一个出来会让它们和检索到的
//     片段在同一个排序里比较，而那个比较毫无意义——一个 0.87 的关系记录
//     凭什么排在 0.85 的原文片段前面？

func rel(typ string, chapter int, order int64, quotes ...string) relationRecord {
	r := relationRecord{
		Type: typ, SubjectName: "甲", ObjectName: "乙",
		FirstSourceOrder: order, ChapterNumber: &chapter,
	}
	for i, q := range quotes {
		r.Evidence = append(r.Evidence, relationEvidenceRecord{
			Quote: q, SourceOrder: order + int64(i),
			SourceStart: int(order) + i, SourceEnd: int(order) + i + len([]rune(q)),
			ChunkID: "c-1",
		})
	}
	return r
}

// TestCitationsAreOrderedBySourcePosition——按**原文位置**排，不是按类型或章节。
// ⚠️ 倒叙的书里章节号与叙述顺序不一致；按章节排会把后写的事排在前面，
// 而模型会照着那个顺序理解因果。
func TestCitationsAreOrderedBySourcePosition(t *testing.T) {
	// ⚠️ 夹具必须让**轮转顺序与原文顺序不一致**，否则最终那次排序去掉了
	// 也看不出来。第一版每条关系只有一条证据，轮转顺序恰好就是原文顺序，
	// 变异因此逃逸。这里第一组的两条证据跨在第二组两边：
	// 轮转产出 100、500、900、600，而正确输出必须是 100、500、600、900。
	rels := []relationRecord{
		rel("欺凌", 1, 100, "第一章的事"),
		rel("亲属", 3, 500, "第三章的事", "第三章后面一点"),
	}
	rels[0].Evidence = append(rels[0].Evidence, relationEvidenceRecord{
		Quote: "第九章的事", SourceOrder: 900, SourceStart: 900, SourceEnd: 905, ChunkID: "c-1",
	})
	rels[1].Evidence[1].SourceOrder = 600
	rels[1].Evidence[1].SourceStart, rels[1].Evidence[1].SourceEnd = 600, 606

	cites, _ := selectRelationCitations(rels, 10000)
	if len(cites) != 4 {
		t.Fatalf("选出 %d 条, want 4", len(cites))
	}
	var last int64 = -1
	for _, c := range cites {
		if c.SourceOrder <= last {
			t.Errorf("顺序不是按原文位置递增：%v", cites)
		}
		last = c.SourceOrder
	}
}

// TestCitationsCoverMultipleTypesBeforeDeepeningOne——⭐ 预算紧张时，
// **先覆盖不同类型/不同章节，再补同一条关系的更多证据**。
//
// ⚠️ 贪心地按顺序取满的后果是：一条证据很多的关系把预算吃光，
// 而另外三种关系一条都没进去——模型看到的是"他们只有这一种关系"，
// 而那正是这个功能要回答的问题。
func TestCitationsCoverMultipleTypesBeforeDeepeningOne(t *testing.T) {
	// 第一条关系有 5 条证据，另外两条各 1 条。预算只够 3 条。
	rels := []relationRecord{
		rel("欺凌", 1, 100, "证据一", "证据二", "证据三", "证据四", "证据五"),
		rel("冲突", 2, 300, "另一种关系"),
		rel("亲属", 3, 500, "第三种关系"),
	}
	cites, _ := selectRelationCitations(rels, budgetForCitations(3))
	types := map[string]bool{}
	for _, c := range cites {
		types[c.RelationType] = true
	}
	if len(types) != 3 {
		t.Errorf("只覆盖了 %d 种关系类型：%v——一条关系的证据把预算吃光了",
			len(types), cites)
	}
}

// TestCitationBudgetIsTheSharedRagBudget——⭐ 预算是**传进来的那一份**，
// 不是内部另开的。⚠️ 另开一份的后果是上下文悄悄超长，而超长不报错——
// 只会让历史被多截掉几轮，回答慢慢变差。
func TestCitationBudgetIsTheSharedRagBudget(t *testing.T) {
	long := strings.Repeat("字", 200)
	rels := []relationRecord{rel("欺凌", 1, 100, long, long, long)}

	// ⚠️ 预算值要包含"恰好等于若干条**引文长度**"的那几个：
	// 200/400/600 正是引文本身的整数倍。只算引文不算渲染长度的实现
	// 在这些值上会多放一条进去，而在别的值上和正确实现表现一样——
	// 第一版的 250/450 就凑巧都对，变异因此逃逸。
	for _, budget := range []int{0, 10, 200, 250, 400, 450, 600} {
		cites, _ := selectRelationCitations(rels, budget)
		total := 0
		for _, c := range cites {
			total += citationRunes(c)
		}
		if total > budget {
			t.Errorf("预算 %d 却用了 %d rune", budget, total)
		}
	}

	// ⭐ 上面那条断言**永远抓不到度量错误**：它用被测的 citationRunes
	// 自己去算期望值，实现少算多少、期望就跟着少算多少，两边永远相等。
	// 变异测试正是这么逃逸的。
	//
	// 这里换成一个**独立可核对**的断言：引文 200 rune，渲染后还要加上
	// "甲 → 乙（欺凌，第1章）：" 这十几个字符，所以 600 的预算装得下 2 条、
	// 装不下 3 条。只算引文长度的实现会放进 3 条。
	if got, _ := selectRelationCitations(rels, 600); len(got) != 2 {
		t.Errorf("预算 600、引文各 200 rune：选出 %d 条, want 2"+
			"（渲染后每条约 214 rune，第三条放不下）", len(got))
	}
}

// TestTinyBudgetYieldsNothingAndSaysSo——⭐ 预算小到装不下任何一条时，
// 返回空**并报告截断**。
//
// ⚠️ 静默返回空的后果最坏：模型收到"没有任何关系证据"，会照着这个前提
// 回答"书里没有提到他们的关系"——而实际上有，只是我们没能放进去。
// 一个因为预算而产生的错误答案，看起来和一个真实的"没有"完全一样。
func TestTinyBudgetYieldsNothingAndSaysSo(t *testing.T) {
	rels := []relationRecord{rel("欺凌", 1, 100, strings.Repeat("字", 500))}
	cites, truncated := selectRelationCitations(rels, 10)
	if len(cites) != 0 {
		t.Fatalf("预算 10 rune 却选出了 %d 条", len(cites))
	}
	if !truncated {
		t.Error("一条都没放进去却没有报告截断——模型会以为书里真的没有")
	}
}

// TestCitationCountIsCapped——引用上限 12，候选上限 200。
// ⚠️ 上限不是为了省钱：几十条引用会把 prompt 变成一份清单，
// 而模型对长清单的中段注意力最差——多给的那些反而稀释了关键的几条。
func TestCitationCountIsCapped(t *testing.T) {
	var rels []relationRecord
	for i := 0; i < 40; i++ {
		rels = append(rels, rel("冲突", i+1, int64(i*100), "证据"+itoa(i)))
	}
	cites, truncated := selectRelationCitations(rels, 1_000_000)
	if len(cites) != maxRelationCitations {
		t.Errorf("引用 %d 条, want %d", len(cites), maxRelationCitations)
	}
	if !truncated {
		t.Error("超过上限却没有报告截断")
	}
}

// TestNoFabricatedSimilarityScore——⭐ 关系引用**没有相似度字段**。
//
// ⚠️ 编一个出来会让它们和检索到的片段在同一个排序里比较，而那个比较
// 毫无意义：一个 0.87 的关系记录凭什么排在 0.85 的原文片段前面？
// 这条断言盯的是类型定义本身——一旦有人加上那个字段，它就红。
func TestNoFabricatedSimilarityScore(t *testing.T) {
	var c relationCitation
	if hasScoreField(c) {
		t.Error("关系引用带上了相似度分数——那个分数没有任何来源，" +
			"而它会让关系记录和真实检索结果在同一个排序里比较")
	}
}

// TestCitationsKeepTheirSourcePositions——引用必须带着原文区间，
// ⚠️ 否则前端无法把它定位回原文，用户点开只能看到一段孤立的话。
func TestCitationsKeepTheirSourcePositions(t *testing.T) {
	rels := []relationRecord{rel("欺凌", 1, 100, "赵太爷跳过去给了他一个嘴巴")}
	cites, _ := selectRelationCitations(rels, 10000)
	if len(cites) != 1 {
		t.Fatal("没有选出引用")
	}
	c := cites[0]
	if c.SourceStart <= 0 || c.SourceEnd <= c.SourceStart {
		t.Errorf("引用没有带上原文区间：[%d,%d)", c.SourceStart, c.SourceEnd)
	}
	if c.ChunkID == "" {
		t.Error("引用没有带上 chunk_id——入模前的批量核验需要它")
	}
	if c.ChapterNumber == nil || *c.ChapterNumber != 1 {
		t.Errorf("引用丢了章节号：%v", c.ChapterNumber)
	}
}
