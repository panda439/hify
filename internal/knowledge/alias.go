package knowledge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// alias.go 是第二阶段身份归一的候选选取、响应校验与身份落地（010 T026/T027）。
//
// ⭐ 这一阶段唯一要防的错误是**误合并**：把两个不同的人当成同一个人。
// 它比漏合并贵得多，且不对称：
//   - 漏合并的表现是人物碎片化——"阿Q"和"老Q"各算一个人。难看，但每条
//     关系仍然挂在有原文支持的那个称呼上，指标里看得见（同一实体的关系被
//     拆散，召回率下降）;
//   - 误合并会**凭空制造关系**：把"赵太爷"和他儿子"赵秀才"合成一个人之后，
//     儿子的每一条关系都变成了父亲的关系。这些关系每一条都有真实的原文引用，
//     逐条核对都对——错的是身份，而身份不在引用里。
//
// 所以这里的规则一律偏向保守：拿不准就各自独立，宁可碎片化。
// ⚠️ 但"保守"不等于"安全"：碎片化会压低召回，这一项必须在报告里如实写成
// 本设计的代价，不能只报误合并为零。

const (
	// aliasPromptVersion 与 extractPromptVersion 分开：两阶段的指令各自演进，
	// 混成一个版本号会让"只改了归一措辞"的实验无法与上一次对比。
	aliasPromptVersion = "alias/v2"

	// 候选窗口（plan §6）。⚠️ 超出窗口的候选**不是"就当不存在"**：
	// 被删掉多少必须回传给调用方标 candidate_truncated，否则一次因为候选
	// 没进窗口而没能归一的结果，会被读成模型判断失误。
	maxAliasCandidates        = 32
	maxAliasCandidateEvidence = 2
	maxAliasSupports          = 4
	minAliasSupports          = 1
)

// 归一决策的三种动作与四种理由（契约 §2）。
const (
	aliasActionNew       = "new"
	aliasActionLink      = "link"
	aliasActionAmbiguous = "ambiguous"

	reasonExplicitAlias   = "explicit_alias"
	reasonContextIdenty   = "context_identity"
	reasonInsufficient    = "insufficient"
	reasonContradictory   = "contradictory"
	aliasSourceRefCurrent = "chunk"
)

var aliasReasonCodes = map[string]struct{}{
	reasonExplicitAlias: {}, reasonContextIdenty: {},
	reasonInsufficient: {}, reasonContradictory: {},
}

var (
	// errAliasResponseInvalid：归一响应不合法，整次拒绝，候选关系不发布。
	// ⚠️ 与第一阶段分开是有用的：第一阶段的成功响应可以复用，只重试归一。
	errAliasResponseInvalid = errors.New("knowledge: alias response invalid")

	// errAliasMergeUnsupported：模型要合并两个称呼，但输入里没有支持这次
	// 合并的依据。这是本文件存在的理由，见文件头。
	errAliasMergeUnsupported = errors.New("knowledge: alias merge is not supported by the input")
)

// aliasHedges 是"这不是一个确定的断言"的固定标记词。
//
// ⭐ 反例来自阿Q正传本身：原文只说"阿Quei"可能写作"阿桂"或"阿贵"，
// 标注指南据此判定后两者**无佐证**。而一个只看字面的归一器会把
// "阿Q 大约是阿贵" 当成一句漂亮的别名连接句——它确实同时出现了两个称呼，
// 确实是原文，逐字比对完全通过。区别只在那两个字上。
//
// ⚠️ 这是一份**固定词表**，不是语义理解。它挡得住原文里明写的犹疑和否定，
// 挡不住需要读懂上下文才能判断的假设句。这条边界必须写进报告，
// 不能拿"零误合并"的测试结果暗示这里做了指代消解。
var aliasHedges = []string{
	"大约", "也许", "可能", "或许", "恐怕", "说不定", "未必", "不一定",
	"不知", "不清楚", "难说", "据说", "传说", "似乎", "好像", "仿佛",
	"不是", "并非", "并不是", "无从", "没有证据", "假如", "倘若", "如果",
}

// --- 输入 ---

// aliasCandidate 是一个可以被链接到的既有人物。
type aliasCandidate struct {
	CharacterID      string
	DisplayName      string
	FirstSourceOrder int64
	// Evidence 是这个人物**已确认的身份依据**，最多 2 条。
	// ⚠️ 不是它参与的关系证据：给模型看"我们凭什么认为这个人是这个人"，
	// 而不是"这个人干过什么"。后者会诱导模型按情节相似度合并。
	Evidence []aliasCandidateEvidence
}

type aliasCandidateEvidence struct {
	Ref   string `json:"ref"`
	Quote string `json:"quote"`
}

type aliasInput struct {
	Chunk      extractionChunkView
	Mentions   []resolvedMention
	Proposals  []resolvedAliasProposal
	Candidates []aliasCandidate
	// CandidateTruncated 是因为长度上限被删掉的候选数（见 fitAliasInput）。
	CandidateTruncated int
}

// needsAliasPhase 判断这个块要不要第二次调用。
//
// ⭐ 既没有别名提案、又没有候选人物的块**不需要**第二次调用（契约 §2 末段）。
// 这不是优化：让模型在没有任何可合并对象的情况下"确认一遍"，除了花钱之外
// 只会引入它自己发明的合并。
func needsAliasPhase(in aliasInput) bool {
	return len(in.Proposals) > 0 || len(in.Candidates) > 0
}

// selectAliasCandidates 按 plan §6 的口径选候选：精确名称命中优先，
// 然后源顺序，最后 ID。返回选中的候选和被截掉的数量。
//
// ⚠️ 排序里**没有相似度**这一项。按名字相似度排候选，等于在候选选取阶段
// 就替模型做了一次"长得像就是同一个人"的判断，而那正是要防的错误。
// 名称只用来检索候选，不用来直接合并。
func selectAliasCandidates(pool []aliasCandidate, surfaces []string) ([]aliasCandidate, int) {
	exact := make(map[string]bool, len(surfaces))
	for _, s := range surfaces {
		exact[s] = true
	}
	ranked := append([]aliasCandidate(nil), pool...)
	sort.SliceStable(ranked, func(i, j int) bool {
		hi, hj := exact[ranked[i].DisplayName], exact[ranked[j].DisplayName]
		if hi != hj {
			return hi
		}
		if ranked[i].FirstSourceOrder != ranked[j].FirstSourceOrder {
			return ranked[i].FirstSourceOrder < ranked[j].FirstSourceOrder
		}
		return ranked[i].CharacterID < ranked[j].CharacterID
	})
	truncated := 0
	if len(ranked) > maxAliasCandidates {
		truncated = len(ranked) - maxAliasCandidates
		ranked = ranked[:maxAliasCandidates]
	}
	for i := range ranked {
		if len(ranked[i].Evidence) > maxAliasCandidateEvidence {
			ranked[i].Evidence = ranked[i].Evidence[:maxAliasCandidateEvidence]
		}
	}
	return ranked, truncated
}

// buildAliasInstruction 拼归一阶段的固定指令。
//
// ⭐ 指令里**明写四个 reason_code 的含义**，而不是让模型自由发挥一句理由。
// 一个自报的 0.87 confidence 无法核对；四个封闭理由每一个都对应
// resolveAliasDecisions 里可以检查的输入条件。
func buildAliasInstruction(in aliasInput) string {
	var sb strings.Builder
	sb.WriteString(`下面依次给出：这一段里出现的人物称呼、原文、以及已知的候选人物
（在最末尾，每行一个；可能因为长度限制只列出其中一部分）。
请判断每一个称呼指的是谁。

输出一个 JSON 对象，不要任何解释、不要 Markdown 代码围栏：
{"decisions":[{"mention_ref","action","character_id","new_group","supports","reason_code"}]}
每个称呼恰好一条决策，不能多也不能少。

action 三选一：
  link  —— 就是末尾候选人物中的某一个，character_id 填那个人的 id，new_group 留空;
  new   —— 本段里新出现的人物，character_id 留空，new_group 填一个你自定的组号;
  ambiguous —— 拿不准，character_id 留空，new_group 填一个**只属于它自己**的组号。
只有原文明确写出是同一个人的称呼，才可以填同一个 new_group。

supports 给 1～4 条依据，每条 {"source_ref","quote"}：
  source_ref 填 "chunk" 表示引自下面的原文，或填某个候选依据的 ref（形如 xxx#0）；
  quote 逐字复制即可，不需要指出是第几次出现，位置由系统在原文里查。
link 需要两侧都有依据（原文一条 + 该候选的依据一条），
或者原文里有一句同时写出两个称呼、明确说明是同一个人的话。

reason_code 四选一：
  explicit_alias —— 原文明写"某某就是某某";
  context_identity —— 上下文足以确定;
  insufficient —— 依据不足;
  contradictory —— 原文里有相互矛盾的说法。
拿不准就用 ambiguous，不要猜。`)
	sb.WriteString("\n\n称呼：")
	for _, m := range in.Mentions {
		sb.WriteString(fmt.Sprintf("\n- %s（ref=%s）", m.Surface, m.Ref))
	}
	// ⚠️ 这里必须以"原文："结尾：fitAliasInput 紧接着拼的就是正文，
	// 再往后才是候选行（它们排在最末尾，才能按长度从尾部逐行删）。
	// 指令里写的顺序和实际拼出来的顺序不一致，模型会去一个没有内容的
	// 位置找候选，而这件事在日志里完全看不出来。
	sb.WriteString("\n\n原文：\n")
	return sb.String()
}

// renderAliasCandidates 把候选渲染成可以逐条删尾的行（见 fitAliasInput）。
// ⚠️ 一个候选**一行**：删候选是按行删的，一个候选跨多行会被删成半截，
// 那半截仍然会被模型当成一个可以链接的对象。
func renderAliasCandidates(candidates []aliasCandidate) []string {
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("- %s（id=%s）", c.DisplayName, c.CharacterID))
		for _, ev := range c.Evidence {
			sb.WriteString(fmt.Sprintf("；依据 %s：%s", ev.Ref, ev.Quote))
		}
		out = append(out, sb.String())
	}
	return out
}

// --- 响应（契约 §2）---

type aliasResponse struct {
	Decisions *[]aliasDecision `json:"decisions"`
}

type aliasDecision struct {
	MentionRef  string          `json:"mention_ref"`
	Action      string          `json:"action"`
	CharacterID string          `json:"character_id"`
	NewGroup    string          `json:"new_group"`
	Supports    *[]aliasSupport `json:"supports"`
	ReasonCode  string          `json:"reason_code"`
}

type aliasSupport struct {
	// SourceRef 是这条依据的出处：当前块（"chunk"）或某个候选的既有依据 ref。
	// ⚠️ 不接受空出处。一条没有出处的"依据"是模型的断言本身，
	// 拿它当依据等于让模型自己给自己作证。
	SourceRef string `json:"source_ref"`
	Quote     string `json:"quote"`
	// 同 extractMention.Occurrence：保留但忽略，为的是旧响应仍能回放。
	Occurrence int `json:"occurrence"`
}

func parseAliasResponse(raw []byte) (aliasResponse, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return aliasResponse{}, fmt.Errorf("%w: empty output", errAliasResponseInvalid)
	}
	if err := checkStrictJSONObject(raw, errAliasResponseInvalid); err != nil {
		return aliasResponse{}, err
	}
	var resp aliasResponse
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		return aliasResponse{}, fmt.Errorf("%w: %v", errAliasResponseInvalid, err)
	}
	if resp.Decisions == nil {
		return aliasResponse{}, fmt.Errorf("%w: decisions must be present", errAliasResponseInvalid)
	}
	return resp, nil
}

// --- 身份落地 ---

// identityAssignment 是归一之后"每个 mention 归谁"的结论。
type identityAssignment struct {
	// Characters 是本块新建的人物，LocalRef 就是决策里的组号。
	Characters []characterDraft
	// Existing 把组号映射到既有人物 ID（action=link 的结果）。
	Existing map[string]string
	// MentionToGroup 是 mention ref -> 组号，用来改写关系端点。
	MentionToGroup map[string]string
}

// resolveAliasDecisions 校验归一响应并落成身份分配。
//
// ⭐ 校验和落地写在一个函数里是有意的：任何一条决策不合法，**整次归一拒绝**，
// 候选关系一条都不发布（契约 §2）。分成"先校验、后落地"两步，中间就有了
// 一个"大部分合法，个别丢掉"的诱人状态——而被丢掉的那个 mention 仍然是
// 某条关系的端点。
func resolveAliasDecisions(in aliasInput, resp aliasResponse) (identityAssignment, error) {
	decisions := *resp.Decisions
	byRef := make(map[string]resolvedMention, len(in.Mentions))
	for _, m := range in.Mentions {
		byRef[m.Ref] = m
	}
	if len(decisions) != len(in.Mentions) {
		return identityAssignment{}, fmt.Errorf("%w: %d decisions for %d mentions",
			errAliasResponseInvalid, len(decisions), len(in.Mentions))
	}

	candidates := make(map[string]aliasCandidate, len(in.Candidates))
	candidateEvidenceRefs := map[string]string{} // 依据 ref -> 人物 ID
	for _, c := range in.Candidates {
		candidates[c.CharacterID] = c
		for _, ev := range c.Evidence {
			candidateEvidenceRefs[ev.Ref] = c.CharacterID
		}
	}

	locator := newQuoteLocator(in.Chunk)
	assign := identityAssignment{
		Existing:       map[string]string{},
		MentionToGroup: map[string]string{},
	}
	groupMembers := map[string][]string{}
	groupSupports := map[string][]aliasSupport{}
	groupAmbiguous := map[string]bool{}
	var groupOrder []string

	for _, d := range decisions {
		mention, ok := byRef[d.MentionRef]
		if !ok {
			return identityAssignment{}, fmt.Errorf("%w: decision for unknown mention %q",
				errAliasResponseInvalid, d.MentionRef)
		}
		if _, dup := assign.MentionToGroup[d.MentionRef]; dup {
			return identityAssignment{}, fmt.Errorf("%w: mention %q decided twice",
				errAliasResponseInvalid, d.MentionRef)
		}
		if _, ok := aliasReasonCodes[d.ReasonCode]; !ok {
			// ⚠️ 不接受把 reason_code 换成一个数字 confidence：一个自报的
			// 0.87 无法核对，而四个封闭理由每一个都对应可检查的输入条件。
			return identityAssignment{}, fmt.Errorf("%w: unknown reason_code %q",
				errAliasResponseInvalid, d.ReasonCode)
		}
		if d.Supports == nil {
			return identityAssignment{}, fmt.Errorf("%w: mention %q has no supports array",
				errAliasResponseInvalid, d.MentionRef)
		}
		supports := *d.Supports
		if len(supports) < minAliasSupports || len(supports) > maxAliasSupports {
			return identityAssignment{}, fmt.Errorf("%w: mention %q has %d supports",
				errAliasResponseInvalid, d.MentionRef, len(supports))
		}
		for _, s := range supports {
			if err := checkAliasSupport(locator, candidateEvidenceRefs, s); err != nil {
				return identityAssignment{}, fmt.Errorf("mention %q: %w", d.MentionRef, err)
			}
		}

		switch d.Action {
		case aliasActionLink:
			cand, ok := candidates[d.CharacterID]
			if !ok {
				// ⚠️ 模型给了一个不在输入里的人物 ID。这在纯函数层就能挡住，
				// 也**必须**在这里挡住：让它进到数据库，外键要么报错（好的
				// 情况），要么正好命中另一个作业的人物（坏的情况）。
				return identityAssignment{}, fmt.Errorf("%w: mention %q links to unknown character %q",
					errAliasResponseInvalid, d.MentionRef, d.CharacterID)
			}
			if d.NewGroup != "" {
				return identityAssignment{}, fmt.Errorf("%w: link decision for %q also carries a new_group",
					errAliasResponseInvalid, d.MentionRef)
			}
			if d.ReasonCode != reasonExplicitAlias && d.ReasonCode != reasonContextIdenty {
				return identityAssignment{}, fmt.Errorf("%w: link decision for %q with reason %q",
					errAliasResponseInvalid, d.MentionRef, d.ReasonCode)
			}
			if err := checkLinkSupported(locator, mention, cand, supports); err != nil {
				return identityAssignment{}, fmt.Errorf("mention %q: %w", d.MentionRef, err)
			}
			group := "link:" + d.CharacterID
			if _, seen := assign.Existing[group]; !seen {
				assign.Existing[group] = d.CharacterID
				groupOrder = append(groupOrder, group)
			}
			assign.MentionToGroup[d.MentionRef] = group
			groupMembers[group] = append(groupMembers[group], d.MentionRef)

		case aliasActionNew, aliasActionAmbiguous:
			if d.CharacterID != "" {
				return identityAssignment{}, fmt.Errorf("%w: %s decision for %q carries a character_id",
					errAliasResponseInvalid, d.Action, d.MentionRef)
			}
			if d.NewGroup == "" {
				return identityAssignment{}, fmt.Errorf("%w: %s decision for %q has no new_group",
					errAliasResponseInvalid, d.Action, d.MentionRef)
			}
			if strings.HasPrefix(d.NewGroup, "link:") {
				return identityAssignment{}, fmt.Errorf("%w: new_group %q collides with the link namespace",
					errAliasResponseInvalid, d.NewGroup)
			}
			if _, exists := groupMembers[d.NewGroup]; !exists {
				groupOrder = append(groupOrder, d.NewGroup)
			}
			assign.MentionToGroup[d.MentionRef] = d.NewGroup
			groupMembers[d.NewGroup] = append(groupMembers[d.NewGroup], d.MentionRef)
			groupSupports[d.NewGroup] = append(groupSupports[d.NewGroup], supports...)
			if d.Action == aliasActionAmbiguous {
				groupAmbiguous[d.NewGroup] = true
			}

		default:
			return identityAssignment{}, fmt.Errorf("%w: unknown action %q", errAliasResponseInvalid, d.Action)
		}
	}

	// 组级规则：谁可以和谁共用一个新组。
	proposals := validatedProposalPairs(in)
	for _, group := range groupOrder {
		members := groupMembers[group]
		if _, isLink := assign.Existing[group]; isLink {
			continue
		}
		if groupAmbiguous[group] && len(members) > 1 {
			// ⚠️ 歧义的称呼必须各自独立成组（契约 §2）。把两个都拿不准的
			// 称呼放进同一组，等于用"都不确定"当成了"是同一个人"的理由。
			return identityAssignment{}, fmt.Errorf("%w: ambiguous group %q has %d members",
				errAliasResponseInvalid, group, len(members))
		}
		if len(members) > 1 {
			if err := checkGroupConnected(members, proposals); err != nil {
				return identityAssignment{}, fmt.Errorf("group %q: %w", group, err)
			}
		}
		draft := characterDraft{
			LocalRef:         group,
			DisplayName:      byRef[members[0]].Surface,
			FirstSourceOrder: int64(in.Chunk.Meta.SourceOrder),
			HasAmbiguity:     groupAmbiguous[group],
		}
		// 组里每一个称呼都要能被查到，不只是被选作正名的那一个。
		aliasState := aliasStateSupported
		if groupAmbiguous[group] {
			aliasState = aliasStateAmbiguous
		}
		for _, ref := range members {
			draft.Aliases = append(draft.Aliases, aliasDraft{
				Surface: byRef[ref].Surface, State: aliasState,
			})
		}
		evidence, err := json.Marshal(groupSupports[group])
		if err != nil {
			return identityAssignment{}, fmt.Errorf("knowledge: marshal identity evidence: %w", err)
		}
		draft.IdentityEvidence = evidence
		assign.Characters = append(assign.Characters, draft)
	}
	return assign, nil
}

// checkAliasSupport 校验一条依据的出处与可定位性。
func checkAliasSupport(locator *quoteLocator, candidateRefs map[string]string, s aliasSupport) error {
	if s.SourceRef == "" {
		return fmt.Errorf("%w: support without a source_ref", errAliasResponseInvalid)
	}
	if s.Quote == "" {
		return fmt.Errorf("%w: support without a quote", errAliasResponseInvalid)
	}
	if s.SourceRef == aliasSourceRefCurrent {
		// 当前块的依据必须在原文里精确命中，口径与第一阶段完全一致。
		if _, _, _, err := locator.locate(s.Quote); err != nil {
			return err
		}
		return nil
	}
	if _, ok := candidateRefs[s.SourceRef]; !ok {
		// ⚠️ 出处只能是当前块或**我们给它的**候选依据。允许别的 ref，
		// 等于允许模型引用一段我们没有给过、也无法核对的"原文"。
		return fmt.Errorf("%w: support source_ref %q is not in the input",
			errAliasResponseInvalid, s.SourceRef)
	}
	return nil
}

// checkLinkSupported 是链接到既有人物的额外条件（契约 §2）。
//
// ⭐ 两条路，满足其一：
//  1. 同时给出当前块的依据**和**候选身份的依据——两边各自有出处;
//  2. 当前块里有一句**明确的别名连接句**：同时出现这个称呼和候选的名字，
//     且不带犹疑/否定标记词。
//
// ⚠️ 不接受"两个称呼字面相同"作为理由。同名不同人在中文小说里极常见
// （标注指南点名的 `赵太爷` 与 `赵秀才` 只是同姓，`审讯光头老人` 与
// `把总` 更是全靠上下文），而按字面合并恰好在这些地方最容易出错。
func checkLinkSupported(locator *quoteLocator, mention resolvedMention, cand aliasCandidate, supports []aliasSupport) error {
	hasCurrent, hasCandidate := false, false
	for _, s := range supports {
		if s.SourceRef == aliasSourceRefCurrent {
			hasCurrent = true
			continue
		}
		for _, ev := range cand.Evidence {
			if ev.Ref == s.SourceRef {
				hasCandidate = true
			}
		}
	}
	if hasCurrent && hasCandidate {
		return nil
	}
	for _, s := range supports {
		if s.SourceRef != aliasSourceRefCurrent {
			continue
		}
		if !strings.Contains(s.Quote, mention.Surface) || !strings.Contains(s.Quote, cand.DisplayName) {
			continue
		}
		if hedgedAliasQuote(s.Quote) {
			continue
		}
		if _, _, _, err := locator.locate(s.Quote); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("%w: linking %q to %q has neither two-sided support nor an explicit alias sentence",
		errAliasMergeUnsupported, mention.Surface, cand.DisplayName)
}

// checkGroupConnected 要求共用一组的 mentions 在**第一阶段已校验的别名提案**
// 图上连通。
//
// ⭐ 用连通性而不是"两两都有提案"：一个人有三个称呼时，原文通常只写出
// A=B 和 B=C 两句，A=C 是没有句子的。要求两两成对会把这种正常情况判死。
// 但连通性仍然要求**每一次合并都有一句原文**，不允许凭空多拉一个人进来。
func checkGroupConnected(members []string, proposals map[[2]string]bool) error {
	inGroup := make(map[string]bool, len(members))
	for _, m := range members {
		inGroup[m] = true
	}
	seen := map[string]bool{members[0]: true}
	queue := []string{members[0]}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, m := range members {
			if seen[m] {
				continue
			}
			if proposals[aliasPairKey(cur, m)] {
				seen[m] = true
				queue = append(queue, m)
			}
		}
	}
	for _, m := range members {
		if !seen[m] {
			return fmt.Errorf("%w: mention %q shares a group without an alias sentence connecting it",
				errAliasMergeUnsupported, m)
		}
	}
	return nil
}

// validatedProposalPairs 把第一阶段的别名提案变成可查的边集，
// **带犹疑/否定标记词的提案在这里被丢掉**。
func validatedProposalPairs(in aliasInput) map[[2]string]bool {
	out := map[[2]string]bool{}
	for _, p := range in.Proposals {
		if hedgedAliasQuote(p.Evidence.Quote) {
			continue
		}
		out[aliasPairKey(p.Left, p.Right)] = true
	}
	return out
}

func aliasPairKey(a, b string) [2]string {
	if a > b {
		a, b = b, a
	}
	return [2]string{a, b}
}

// hedgedAliasQuote 判断一句别名依据是不是"没把话说死"。见 aliasHedges。
func hedgedAliasQuote(quote string) bool {
	for _, h := range aliasHedges {
		if strings.Contains(quote, h) {
			return true
		}
	}
	return false
}

// independentIdentities 是零调用路径：没有候选也没有别名提案时，
// 每个 mention 各自成一个人物（契约 §2 末段）。
//
// ⚠️ **同一精确位置只建一次**：同一个称呼在同一处原文被模型列了两遍时，
// 那是一个人物不是两个。按 surface 去重是不对的——同一个称呼出现在两处
// 原文，仍然可能是两个人（"母亲"），而位置是我们唯一有把握的区分依据。
func independentIdentities(in aliasInput) identityAssignment {
	assign := identityAssignment{
		Existing:       map[string]string{},
		MentionToGroup: map[string]string{},
	}
	byPosition := map[[2]int]string{}
	for i, m := range in.Mentions {
		key := [2]int{m.DocumentStart, m.DocumentEnd}
		group, seen := byPosition[key]
		if !seen {
			group = fmt.Sprintf("g%d", i)
			byPosition[key] = group
			// ⭐ 零调用路径也要留下身份依据，哪怕它很弱（"这个称呼出现在
			// 这一段的这个位置"）。不留的话，**第一块里出现的人物永远无法
			// 被后面的块链接**：link 要求两侧各有出处，而候选这一侧永远是空的。
			// 表现是每一章都新建一个"阿Q"，然后每次查询都变成"命中多个实体"。
			evidence, err := json.Marshal([]aliasSupport{{
				SourceRef: aliasSourceRefCurrent, Quote: m.Surface,
			}})
			if err != nil {
				// json.Marshal 对这个固定结构不会失败；真失败了就当没有依据，
				// 保守方向（这个候选更难被链接），不让整个 item 失败。
				evidence = nil
			}
			assign.Characters = append(assign.Characters, characterDraft{
				IdentityEvidence: evidence,
				LocalRef:         group,
				DisplayName:      m.Surface,
				FirstSourceOrder: int64(in.Chunk.Meta.SourceOrder),
				// 零调用路径上每个人物只有一个称呼，但仍然要写进别名表：
				// 查询只走"正名"和"别名表"两条路，少写这一条会让
				// "这本书有没有走过归一阶段"决定同一个称呼能不能被查到。
				Aliases: []aliasDraft{{Surface: m.Surface, State: aliasStateSupported}},
			})
		}
		assign.MentionToGroup[m.Ref] = group
	}
	return assign
}

// buildExtractionOutcome 把关系端点从 mention ref 改写成人物组号，
// 拼出可以发布的结果。
//
// ⚠️ 端点找不到对应身份时**整体失败**，不是丢掉这条关系：一条端点缺失的
// 关系说明归一和抽取对不上，此时另外那些"看起来对得上"的关系同样不可信。
func buildExtractionOutcome(chunk extractionChunkView, relations []resolvedRelation, assign identityAssignment) (extractionOutcome, error) {
	out := extractionOutcome{Characters: assign.Characters, Existing: assign.Existing}
	for i, rel := range relations {
		subject, ok := assign.MentionToGroup[rel.SubjectRef]
		if !ok {
			return extractionOutcome{}, fmt.Errorf("%w: relation %d subject %q has no identity",
				errAliasResponseInvalid, i, rel.SubjectRef)
		}
		object, ok := assign.MentionToGroup[rel.ObjectRef]
		if !ok {
			return extractionOutcome{}, fmt.Errorf("%w: relation %d object %q has no identity",
				errAliasResponseInvalid, i, rel.ObjectRef)
		}
		if subject == object {
			// 归一之后两个端点变成同一个人：这条关系自指，丢掉它。
			// ⭐ 这一条是**唯一**允许丢单条关系的情况，因为它丢掉的不是
			// 信息而是矛盾——合并本身已经由上面的规则验证过了。
			continue
		}
		out.Relations = append(out.Relations, relationDraft{
			SubjectRef: subject, ObjectRef: object,
			Type: rel.Type, IsDirected: rel.IsDirected,
			FirstSourceOrder: int64(chunk.Meta.SourceOrder),
			ChapterNumber:    chunk.Meta.ChapterNumber,
			ChapterTitle:     chunk.Meta.ChapterTitle,
			Evidence:         rel.Evidence,
		})
	}
	return out, nil
}
