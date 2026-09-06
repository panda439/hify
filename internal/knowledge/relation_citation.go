package knowledge

import (
	"reflect"
	"sort"
	"strings"
)

// relation_citation.go 选出送进 prompt 的关系证据（010 T032）。
//
// ⭐ 两条硬约束：
//   - **只用一份既有的 RAG 预算**，不另开一份。两份预算的后果是上下文悄悄
//     超长，而超长的表现不是报错——是历史被多截掉几轮，回答慢慢变差，
//     没人会把它和某次改动联系起来。
//   - **不伪造相似度**。关系记录没有向量分数；编一个出来会让它们和检索到的
//     片段在同一个排序里比较，而那个比较毫无意义——一个 0.87 的关系记录
//     凭什么排在 0.85 的原文片段前面？

const (
	// maxRelationCitations 是进 prompt 的引用条数上限。
	//
	// ⚠️ 上限不是为了省钱：几十条引用会把 prompt 变成一份清单，而模型对
	// 长清单的中段注意力最差——多给的那些反而稀释了关键的几条。
	maxRelationCitations = 12

	// maxRelationCitationCandidates 是进入挑选的候选上限。
	maxRelationCitationCandidates = 200
)

// relationCitation 是一条送进 prompt 的关系证据。
//
// ⚠️ 这里**故意没有相似度字段**。见文件头的第二条约束；
// relation_citation_test.go 里有一条断言专门盯着这件事，
// 一旦有人加上那个字段它就红。
type relationCitation struct {
	RelationType  string
	IsDirected    bool
	SubjectName   string
	ObjectName    string
	ChapterNumber *int
	ChapterTitle  *string

	Quote       string
	SourceOrder int64
	SourceStart int
	SourceEnd   int
	ChunkID     string
}

// citationRunes 是这条引用渲染进 prompt 后占的 rune 数。
//
// ⚠️ 算的是**渲染后**的长度，不是引文本身：类型、人名、章节号都要占位置。
// 只算引文的后果是每条都少算十几个 rune，十二条下来就超预算几百——
// 而那部分超支会从对话历史里扣掉。
func citationRunes(c relationCitation) int {
	return len([]rune(renderCitation(c)))
}

func renderCitation(c relationCitation) string {
	var sb strings.Builder
	sb.WriteString(c.SubjectName)
	if c.IsDirected {
		sb.WriteString(" → ")
	} else {
		sb.WriteString(" — ")
	}
	sb.WriteString(c.ObjectName)
	sb.WriteString("（")
	sb.WriteString(c.RelationType)
	if c.ChapterNumber != nil {
		sb.WriteString("，第")
		sb.WriteString(itoa(*c.ChapterNumber))
		sb.WriteString("章")
	}
	sb.WriteString("）：")
	sb.WriteString(c.Quote)
	return sb.String()
}

// budgetForCitations 是测试用的辅助：恰好装下 n 条短引用的预算。
func budgetForCitations(n int) int {
	sample := relationCitation{
		RelationType: "欺凌", SubjectName: "甲", ObjectName: "乙",
		ChapterNumber: intPtrValue(1), Quote: "另一种关系",
	}
	return citationRunes(sample) * n
}

func intPtrValue(n int) *int { return &n }

// selectRelationCitations 在预算内挑证据，返回引用与是否有截断。
//
// ⭐ 挑选策略：**先覆盖不同的关系（类型 + 章节），再补同一条关系的更多证据**。
//
// ⚠️ 贪心地按原文顺序取满的后果是：一条证据很多的关系把预算吃光，而另外
// 三种关系一条都没进去——模型看到的是"他们只有这一种关系"，
// 而"关系随剧情变化"正是这个功能要回答的东西。
//
// ⚠️ 一条都装不下时返回空**并报告截断**。静默返回空最坏：模型收到"没有任何
// 关系证据"，会照着这个前提回答"书里没有提到他们的关系"——而实际上有，
// 只是我们没能放进去。一个因为预算而产生的错误答案，看起来和一个真实的
// "没有"完全一样。
func selectRelationCitations(relations []relationRecord, budgetRunes int) ([]relationCitation, bool) {
	// 每条关系的证据按原文顺序排好，形成若干"轮次"。
	type group struct {
		cites []relationCitation
	}
	groups := make([]group, 0, len(relations))
	candidates := 0
	for _, r := range relations {
		var g group
		for _, ev := range r.Evidence {
			if candidates >= maxRelationCitationCandidates {
				break
			}
			candidates++
			g.cites = append(g.cites, relationCitation{
				RelationType: r.Type, IsDirected: r.IsDirected,
				SubjectName: r.SubjectName, ObjectName: r.ObjectName,
				ChapterNumber: r.ChapterNumber, ChapterTitle: r.ChapterTitle,
				Quote: ev.Quote, SourceOrder: ev.SourceOrder,
				SourceStart: ev.SourceStart, SourceEnd: ev.SourceEnd,
				ChunkID: ev.ChunkID,
			})
		}
		sort.SliceStable(g.cites, func(i, j int) bool {
			return g.cites[i].SourceOrder < g.cites[j].SourceOrder
		})
		if len(g.cites) > 0 {
			groups = append(groups, g)
		}
	}
	// ⚠️ 组之间按各自最早的原文位置排序，让轮转顺序可复现（宪法第 V 条）。
	sort.SliceStable(groups, func(i, j int) bool {
		return groups[i].cites[0].SourceOrder < groups[j].cites[0].SourceOrder
	})

	var picked []relationCitation
	used, truncated := 0, false
	for round := 0; ; round++ {
		progressed := false
		for _, g := range groups {
			if round >= len(g.cites) {
				continue
			}
			progressed = true
			if len(picked) >= maxRelationCitations {
				truncated = true
				continue
			}
			c := g.cites[round]
			if cost := citationRunes(c); used+cost <= budgetRunes {
				picked = append(picked, c)
				used += cost
			} else {
				truncated = true
			}
		}
		if !progressed {
			break
		}
	}
	if candidates >= maxRelationCitationCandidates {
		truncated = true
	}

	// ⭐ 最终**按原文位置**排序输出，不是按轮转顺序。
	// ⚠️ 倒叙的书里章节号与叙述顺序不一致；给模型的顺序必须是叙述顺序，
	// 否则它会照着一个错误的先后关系去理解因果。
	sort.SliceStable(picked, func(i, j int) bool {
		return picked[i].SourceOrder < picked[j].SourceOrder
	})
	return picked, truncated
}

// hasScoreField 是给测试用的反射检查：确认 relationCitation 上没有
// 任何形如相似度/分数的字段。
func hasScoreField(v any) bool {
	t := reflect.TypeOf(v)
	for i := 0; i < t.NumField(); i++ {
		name := strings.ToLower(t.Field(i).Name)
		if strings.Contains(name, "score") || strings.Contains(name, "similarity") {
			return true
		}
	}
	return false
}
