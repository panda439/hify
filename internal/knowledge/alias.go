package knowledge

import (
	"fmt"
	"sort"
	"strings"

	"context"

	"hify/internal/db/gen"
)

// alias.go 是人物身份归一（010 T026）。
//
// ⭐ 取舍方向先说清楚，因为它决定了所有规则的写法：
//
// **误合并比漏合并严重得多。** 漏合并的表现是一个人碎成两个节点——难看、
// 指标上体现为碎片化，但**每一条关系仍然指向正确的人**，人工看一眼就能发现。
// 误合并把两个人的关系混进一个节点，之后任何查询都会把甲的事算到乙头上，
// 而**没有任何办法从结果反推**哪些是错的——两个人的记录已经不可分了。
//
// 所以这里的规则一律**宁可拒绝**。它们会误伤一些本该合并的提案，
// 那部分损失体现在"碎片化"这个可见的指标里，而不是藏在关系数据里。

const (
	aliasActionNew       = "new"
	aliasActionLink      = "link"
	aliasActionAmbiguous = "ambiguous"

	reasonExplicitAlias   = "explicit_alias"
	reasonContextIdentity = "context_identity"
	reasonInsufficient    = "insufficient"
	reasonContradictory   = "contradictory"

	// maxAliasCandidates / maxIdentityEvidencePerCharacter 来自契约 §2 与
	// data-model 的 narrative_characters：候选 32 个、每人至多 2 条身份依据。
	// ⚠️ 后者不是省空间——它是在拒绝"存一份人物传记"。传记会随书变长、
	// 无上限，而没有任何查询需要它。
	maxAliasCandidates              = 32
	maxIdentityEvidencePerCharacter = 2

	maxAliasSupports = 4
)

// aliasReasonCodes 是封闭集合。
//
// ⚠️ 契约明确**不接受一个数字 confidence**：一个 0.83 无法被复核，
// 而 explicit_alias / context_identity 说明了依据的**种类**，可以逐条查。
var aliasReasonCodes = []string{
	reasonExplicitAlias, reasonContextIdentity, reasonInsufficient, reasonContradictory,
}

var aliasReasonSet = func() map[string]bool {
	m := map[string]bool{}
	for _, c := range aliasReasonCodes {
		m[c] = true
	}
	return m
}()

type aliasSupport struct {
	SourceRef  string
	Quote      string
	Occurrence int
}

type aliasDecision struct {
	MentionRef  string
	Action      string
	CharacterID string
	NewGroup    int
	Supports    []aliasSupport
	ReasonCode  string
}

type aliasProposalPair struct{ Left, Right string }

type aliasCandidate struct {
	ID          string
	DisplayName string
}

// aliasInput 是归一阶段的全部输入。
type aliasInput struct {
	Mentions   map[string]string // ref -> surface
	Candidates map[string]string // character id -> display name
	Proposals  []aliasProposalPair
	ChunkText  string
}

// --- 假设 / 否定语句的保守过滤 ---

// hedgeMarkers 是"这句话在存疑而不是在认定"的字面标记。
//
// ⚠️ 这是**字面过滤，不是语义理解**。它会误伤一些本该通过的提案
// （引文里恰好出现"可能"但整句是肯定的）。方向是刻意选的：
// 误伤降低的是合并率（可见，体现在碎片化指标里），
// 放行降低的是关系数据的可信度（不可见，且不可逆）。
//
// ⭐ 反例来自《阿Q正传》第一章：原文在讨论阿Q的名字时明说
// 「阿桂还是阿贵呢？」——那是一句存疑，不是认定。把它当成别名依据，
// 就会把「阿贵」和「阿Q」合成一个人，而小说那一段的意思恰恰相反。
var hedgeMarkers = []string{
	"可能", "或许", "也许", "大约", "大概", "恐怕", "似乎", "好像", "仿佛",
	"倘使", "倘若", "假如", "如果", "未必", "未可知", "不知", "无从",
	"疑", "罢了", "还是", "究竟",
}

// negationMarkers 是明确否定同一性的标记。
var negationMarkers = []string{
	"不是", "并非", "并无", "没有证据", "无证据", "不曾", "无法确定", "不能确定",
}

// quoteSupportsIdentity 判断一句引文能不能作为身份依据。
//
// ⚠️ 它只回答"这句话有没有在存疑或否定"，不回答"这句话是否真的说明了同一性"。
// 后者是语义判断，本仓库不做，也不承诺能做（annotation-guideline §2）。
// 漏抽与误合并都归真实质量评价，不靠这条规则兜底。
func quoteSupportsIdentity(quote string) bool {
	q := strings.TrimSpace(quote)
	if q == "" {
		return false
	}
	// 问句：整句在发问，不在认定。
	if strings.ContainsAny(q, "?？") {
		return false
	}
	// ⚠️ 疑问语气词——中文里问号常被省略。只收 "吗"/"呢" 两个：
	// "么" 不能收，因为 "什么"/"怎么"/"这么" 都含它，误伤面太大。
	// 这一条同样是保守过滤，不是语法分析。
	for _, p := range []string{"吗", "呢"} {
		if strings.Contains(q, p) {
			return false
		}
	}
	for _, m := range hedgeMarkers {
		if strings.Contains(q, m) {
			return false
		}
	}
	for _, m := range negationMarkers {
		if strings.Contains(q, m) {
			return false
		}
	}
	return true
}

// --- 单条决策的校验 ---

func validateAliasDecision(dec aliasDecision, in aliasInput) error {
	surface, ok := in.Mentions[dec.MentionRef]
	if !ok {
		return fmt.Errorf("knowledge: alias: mention_ref %q is not in the input", dec.MentionRef)
	}
	if !aliasReasonSet[dec.ReasonCode] {
		return fmt.Errorf("knowledge: alias: reason_code %q is not in the closed set", dec.ReasonCode)
	}
	if len(dec.Supports) == 0 || len(dec.Supports) > maxAliasSupports {
		return fmt.Errorf("knowledge: alias: supports count %d out of range 1..%d",
			len(dec.Supports), maxAliasSupports)
	}

	src := []rune(in.ChunkText)
	for i, s := range dec.Supports {
		if _, ok := in.Mentions[s.SourceRef]; !ok {
			if _, ok := in.Candidates[s.SourceRef]; !ok {
				return fmt.Errorf("knowledge: alias: supports[%d].source_ref %q is not in the input",
					i, s.SourceRef)
			}
		}
		// ⚠️ 与关系证据同一条理由：引文必须在原文里逐字存在。
		// 这是唯一能挡住"编造依据"的检查。
		if _, _, err := locate(src, s.Quote, s.Occurrence, "alias supports[%d].quote", i); err != nil {
			return fmt.Errorf("knowledge: alias: %w", err)
		}
	}

	switch dec.Action {
	case aliasActionNew:
		if dec.CharacterID != "" {
			return fmt.Errorf("knowledge: alias: action new must not carry a character_id")
		}
	case aliasActionAmbiguous:
		// ⭐ ambiguous 是**成功处理**，不是失败。
		// ⚠️ 它必须不带 character_id：强行挑一个"最可能"的候选，就是把一次
		// "分不清"记成了确定的合并，而那个合并再也没人会去复查。
		if dec.CharacterID != "" {
			return fmt.Errorf("knowledge: alias: action ambiguous must not carry a character_id")
		}
	case aliasActionLink:
		if _, ok := in.Candidates[dec.CharacterID]; !ok {
			// ⚠️ 指向别处的 ID 意味着模型在编 ID，而那个 ID 可能恰好是
			// 另一个真实人物。
			return fmt.Errorf("knowledge: alias: character_id %q is not among the provided candidates",
				dec.CharacterID)
		}
		if err := checkLinkSupport(dec, in, surface); err != nil {
			return err
		}
	default:
		return fmt.Errorf("knowledge: alias: unknown action %q", dec.Action)
	}
	return nil
}

// checkLinkSupport 是 link 的额外门槛。
//
// ⭐ 两条硬规则：
//  1. 依据里至少有一句**同时连接两侧身份**的话（当前称呼与候选名同现），
//     或者引文本身是一句明确的别名说明；
//  2. 那句话不能是假设 / 否定 / 疑问。
//
// ⚠️ 只凭同名不合并：一本书里可以有两个「老王」，按 surface 合并之后
// 再也分不开，而合并这个动作在数据上不留任何痕迹。
func checkLinkSupport(dec aliasDecision, in aliasInput, surface string) error {
	candidateName := in.Candidates[dec.CharacterID]

	// ⭐ 同名候选不止一个时，只能 ambiguous。
	// ⚠️ 这时任何"按名字连接"的依据都无法区分是哪一个——两个「老王」
	// 谁都符合。挑一个就是把"分不清"记成了确定的合并。
	same := 0
	for _, name := range in.Candidates {
		if name == candidateName {
			same++
		}
	}
	if same > 1 {
		return fmt.Errorf(
			"knowledge: alias: %d candidates share the name %q; the decision must be ambiguous",
			same, candidateName)
	}

	for _, s := range dec.Supports {
		if !quoteSupportsIdentity(s.Quote) {
			// 存疑或否定的句子直接出局，不参与"是否连接两侧"的判断。
			continue
		}
		// ⭐ 引文必须**比名字本身长**。
		// ⚠️ 一句只有名字的引文携带的信息量是零——它只说明"这个名字在这里
		// 出现过"，而那是我们本来就知道的。当 surface 与候选同名时更明显：
		// "同时包含两者"这个条件会变成恒真，任何一次出现都成了合并依据。
		// 第一版就是这么放行了「仅凭同名就合并」。
		if len([]rune(s.Quote)) <= len([]rune(surface)) {
			continue
		}
		if !strings.Contains(s.Quote, surface) {
			continue
		}
		// 名字不同的两侧，要求一句话里同时出现；名字相同的两侧，
		// 上面的长度门槛已经保证引文带了上下文。
		if surface == candidateName || strings.Contains(s.Quote, candidateName) {
			return nil
		}
	}
	return fmt.Errorf(
		"knowledge: alias: link from %q to %q has no unhedged quote with context connecting both names",
		surface, candidateName)
}

// --- 组号与整份响应的校验 ---

// validateAliasGroups 校验 new_group 的用法。
//
// ⭐ 只有**有明确别名提案**的 mention 才可以共用组号。没有提案却共用，
// 就是凭空把两个人合成一个——而这个合并发生在人物还没落库之前，
// 事后连"它们曾经是两个"都查不到。
func validateAliasGroups(decs []aliasDecision, in aliasInput) error {
	proposed := map[string]map[string]bool{}
	for _, p := range in.Proposals {
		if proposed[p.Left] == nil {
			proposed[p.Left] = map[string]bool{}
		}
		if proposed[p.Right] == nil {
			proposed[p.Right] = map[string]bool{}
		}
		proposed[p.Left][p.Right] = true
		proposed[p.Right][p.Left] = true
	}

	byGroup := map[int][]string{}
	for _, d := range decs {
		switch d.Action {
		case aliasActionLink:
			if d.NewGroup != 0 {
				return fmt.Errorf("knowledge: alias: link decision for %q must not carry a new_group",
					d.MentionRef)
			}
			continue
		case aliasActionAmbiguous:
			// ⚠️ ambiguous 必须各自独立组号——见下面的重复检查。
		}
		if d.NewGroup <= 0 {
			return fmt.Errorf("knowledge: alias: %s decision for %q needs a positive new_group",
				d.Action, d.MentionRef)
		}
		byGroup[d.NewGroup] = append(byGroup[d.NewGroup], d.MentionRef)
	}

	// ⚠️ 排序后再比对：组内成员来自 map 遍历，顺序不定，而错误信息要可复现。
	groups := make([]int, 0, len(byGroup))
	for g := range byGroup {
		groups = append(groups, g)
	}
	sort.Ints(groups)
	for _, g := range groups {
		members := byGroup[g]
		sort.Strings(members)
		if len(members) == 1 {
			continue
		}
		for i := 1; i < len(members); i++ {
			if !proposed[members[0]][members[i]] {
				return fmt.Errorf(
					"knowledge: alias: group %d puts %q and %q together without an explicit alias proposal",
					g, members[0], members[i])
			}
		}
	}
	for _, d := range decs {
		if d.Action == aliasActionAmbiguous && len(byGroup[d.NewGroup]) > 1 {
			return fmt.Errorf("knowledge: alias: ambiguous mention %q shares group %d",
				d.MentionRef, d.NewGroup)
		}
	}
	return nil
}

// validateAliasResponse 校验整份决策集。
//
// ⚠️ 每个 mention **恰好一条**决策。漏掉一个的表现是那个 mention 被静默
// 丢弃：它参与的关系跟着消失，而关系总数少了几条没有任何迹象。
func validateAliasResponse(decs []aliasDecision, in aliasInput) error {
	seen := map[string]bool{}
	for _, d := range decs {
		if seen[d.MentionRef] {
			return fmt.Errorf("knowledge: alias: duplicate decision for mention %q", d.MentionRef)
		}
		seen[d.MentionRef] = true
		if err := validateAliasDecision(d, in); err != nil {
			return err
		}
	}
	for ref := range in.Mentions {
		if !seen[ref] {
			return fmt.Errorf("knowledge: alias: mention %q has no decision", ref)
		}
	}
	return validateAliasGroups(decs, in)
}

// planAliasResolution 决定这一块要不要第二次调用。
//
// ⭐ 没有候选也没有别名提案时**不需要调**：每个 mention 各自独立身份。
// ⚠️ 照样调一次的后果是白花一半的钱——大多数块里没有任何可归一的东西。
func planAliasResolution(in aliasInput) ([]aliasDecision, bool) {
	if len(in.Candidates) > 0 || len(in.Proposals) > 0 {
		return nil, true
	}
	// ⚠️ 按 ref 排序后再分配组号：map 遍历顺序不定，而同一份输入两次产出
	// 不同的组号会让回放对不上（宪法第 V 条）。
	refs := make([]string, 0, len(in.Mentions))
	for ref := range in.Mentions {
		refs = append(refs, ref)
	}
	sort.Strings(refs)

	src := []rune(in.ChunkText)
	decs := make([]aliasDecision, 0, len(refs))
	for i, ref := range refs {
		surface := in.Mentions[ref]
		start, end, err := locate(src, surface, 0, "mention %q", ref)
		if err != nil {
			// 定位不了的 mention 在第一阶段就该被拒，走到这里说明上游变了。
			continue
		}
		decs = append(decs, aliasDecision{
			MentionRef: ref, Action: aliasActionNew, NewGroup: i + 1,
			ReasonCode: reasonContextIdentity,
			Supports: []aliasSupport{{
				SourceRef: ref, Quote: string(src[start:end]), Occurrence: 0,
			}},
		})
	}
	return decs, false
}

// listAliasCandidates 取同一次作业里名字匹配的人物作为候选。
//
// ⭐ 候选**只能来自同一次作业**。跨书的同名人物（两本书都有「张三」）
// 合并之后，一本书的关系会出现在另一本书的查询结果里，而用户完全无法
// 解释那些记录从哪来。job_id 的过滤就是这条边界。
func (r *Repository) listAliasCandidates(ctx context.Context, jobID, surface string, limit int) ([]aliasCandidate, error) {
	if limit <= 0 || limit > maxAliasCandidates {
		limit = maxAliasCandidates
	}
	rows, err := r.queries.ListAliasCandidates(ctx, gen.ListAliasCandidatesParams{
		JobID: jobID, DisplayName: surface, Limit: int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("knowledge: list alias candidates: %w", err)
	}
	out := make([]aliasCandidate, 0, len(rows))
	for _, row := range rows {
		out = append(out, aliasCandidate{ID: row.ID, DisplayName: row.DisplayName})
	}
	return out, nil
}
