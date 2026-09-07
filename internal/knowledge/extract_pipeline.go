package knowledge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"

	"hify/internal/provider"
)

// extract_pipeline.go 把两个阶段串成一条链（010 T027）。
//
// ⭐ 最要紧的一条：**归一阶段不合法时，第一阶段的结果也一条都不发布**。
//
// 部分发布听起来更宽容——关系已经抽出来了，人物归一没做成而已。但那样产出
// 的关系端点是一批**未经归一的临时人物**：同一个人在不同块里各建一个，
// 而这些碎片再也没有机会被合并（item 已经标成功，不会重跑）。结果是关系
// 数据看起来完整、人物图却是碎的，且碎得没有规律。

// phaseCaller 发一次某个阶段的调用。
// ⚠️ 它**不重试**——重试是 phaseRunner 的职责。
type phaseCaller func(ctx context.Context, phase, input string, attempt int) (provider.ChatAttemptResult, error)

type itemInput struct {
	JobID           string
	ItemID          string
	Epoch           int
	ChunkID         string
	DocumentVersion int64
	Content         string
	Metadata        narrativeMetadata
}

type extractionPipeline struct {
	repo   *Repository
	runner *phaseRunner
	call   phaseCaller
}

// processItem 处理一个 chunk：抽取 → 归一 → 一次性发布。
func (p extractionPipeline) processItem(ctx context.Context, in itemInput) error {
	// ⭐ 送给模型的正文里不含不可引用的部分（见 extract_projection.go）。
	// 模型看不见它，就不会引用它。
	proj, err := newChunkProjection(in.Content, in.Metadata)
	if err != nil {
		return fmt.Errorf("knowledge: item %s: %w", in.ItemID, err)
	}

	// ⭐ 指令与正文一起进入 runOrReplay，与归一阶段同一口径。
	// ⚠️ 只把正文送进去、指令留在调用层拼，会让 request_hash 不覆盖指令——
	// 改了 prompt 之后旧响应照样被当成可回放的，新规则一次都不生效，
	// 而账目上看起来一切正常。
	rendered, err := fitExtractionInput(extractInstruction, proj.Text)
	if err != nil {
		return fmt.Errorf("knowledge: item %s: extract input: %w", in.ItemID, err)
	}
	body, err := p.runOrReplay(ctx, in, phaseExtract, rendered, func(b string) error {
		_, err := parseExtractionResponse(b, proj.Text)
		return err
	})
	if err != nil {
		return err
	}
	parsed, err := parseExtractionResponse(body, proj.Text)
	if err != nil {
		return fmt.Errorf("knowledge: item %s: extract phase: %w", in.ItemID, err)
	}

	decisions, err := p.resolveIdentities(ctx, in, proj, parsed)
	if err != nil {
		return err
	}

	outcome, err := buildOutcome(in, proj, parsed, decisions)
	if err != nil {
		return fmt.Errorf("knowledge: item %s: %w", in.ItemID, err)
	}
	// ⚠️ 只有走到这里才发布。上面任何一步失败都不会留下半份结果。
	return p.repo.publishItemOutcome(ctx, publishInput{
		JobID: in.JobID, ItemID: in.ItemID, Epoch: in.Epoch,
		Outcome: outcome, ExtractResponse: []byte(body),
	})
}

// runOrReplay 优先复用已经落盘的响应。
//
// ⭐ 再打一次的后果是那笔钱白花第二遍，而账目上两次都是真实调用，
// 看起来完全正常——没有任何异常可查。
func (p extractionPipeline) runOrReplay(
	ctx context.Context, in itemInput, phase, input string, validate func(string) error,
) (string, error) {
	replay, ok, err := p.repo.findReplayableResponse(ctx, in.ItemID, phase)
	if err != nil {
		return "", err
	}
	if ok {
		// ⭐ 与首次调用**同一个判据**（010 R6-06）：截断的响应不可用作结果。
		//
		// ⚠️ 如实说明：这一道**当前不可达**。FindReplayableAttempt 已经在
		// SQL 里排除了 error_code='response_truncated' 与
		// finish_reason='length' 的记录，包括修复之前落盘的旧行——变异测试
		// 证实了这一点（把这个判断改成恒假，没有任何用例失败）。
		// 留着它的理由只有一条：判据集中在 extractionResultUnusable 一处，
		// 将来 SQL 那边放宽或改写时，行为不会静默改变。
		// 它不是一道正在生效的守卫，不要把它当成那样的东西读。
		if reason := extractionResultUnusable(replay.FinishReason, replay.Body); reason != "" {
			slog.Warn("knowledge: skipping unusable replayable response",
				"item_id", in.ItemID, "phase", phase, "reason", reason)
		} else if validate == nil || validate(replay.Body) == nil {
			// ⚠️ 回放之前先校验。一份**格式坏掉**的响应如果照样回放，恢复之后
			// 每一轮都会拿它重来一次，item 永远好不了，而且不再花钱也不再产出
			// ——一个安静的死循环。校验不过就当作没有可回放的，重新调。
			return replay.Body, nil
		}
	}

	sum := sha256.Sum256([]byte(phase + "\x00" + input))
	res, err := p.runner.runPhase(ctx, phaseInput{
		JobID: in.JobID, ItemID: in.ItemID, Epoch: in.Epoch, Phase: phase,
		RequestHash: sum[:], MaxOutputTokens: maxOutputTokens,
		Validate: func(r provider.ChatAttemptResult) error {
			if validate == nil {
				return nil
			}
			return validate(r.Message.Content)
		},
	}, func(c context.Context, attempt int) (provider.ChatAttemptResult, error) {
		return p.call(c, phase, input, attempt)
	})
	if err != nil {
		return "", err
	}
	if res.Outcome != provider.AttemptCompleted {
		return "", fmt.Errorf("knowledge: item %s: %s phase ended as %s (%s)",
			in.ItemID, phase, res.Outcome, res.ErrorCode)
	}
	// ⭐ 与回放共用同一个判据。⚠️ 此前这里只看 finish_reason，
	// 超过 64KiB 而被落盘截断的响应会被**原样接受**——首次调用用的是完整
	// 正文、恢复之后回放到的却是截短的那份，同一个 item 两次跑出不同结果，
	// 而两条路径各自看起来都正常（010 R6-06）。
	if reason := extractionResultUnusable(res.FinishReason, res.Message.Content); reason != "" {
		return "", fmt.Errorf("knowledge: item %s: %s phase result unusable (%s)",
			in.ItemID, phase, reason)
	}
	return res.Message.Content, nil
}

// resolveIdentities 跑归一阶段，或者在无事可做时本地产出决策。
func (p extractionPipeline) resolveIdentities(
	ctx context.Context, in itemInput, proj chunkProjection, parsed extractionResponse,
) ([]aliasDecision, error) {
	aliasIn := aliasInput{
		Mentions:  map[string]string{},
		ChunkText: proj.Text,
	}
	for _, m := range parsed.Mentions {
		aliasIn.Mentions[m.Ref] = m.Surface
	}
	for _, a := range parsed.AliasProposals {
		aliasIn.Proposals = append(aliasIn.Proposals, aliasProposalPair{Left: a.Left, Right: a.Right})
	}

	// ⚠️ 候选只从**同一次作业**里取（跨书同名合并的边界见 alias.go）。
	// 按 surface 排序遍历，让候选集合与决策顺序可复现（宪法第 V 条）。
	surfaces := make([]string, 0, len(aliasIn.Mentions))
	for _, s := range aliasIn.Mentions {
		surfaces = append(surfaces, s)
	}
	sort.Strings(surfaces)
	aliasIn.Candidates = map[string]string{}
	for _, surface := range surfaces {
		cands, err := p.repo.listAliasCandidates(ctx, in.JobID, surface, maxAliasCandidates)
		if err != nil {
			return nil, err
		}
		for _, c := range cands {
			aliasIn.Candidates[c.ID] = c.DisplayName
		}
	}

	local, needCall := planAliasResolution(aliasIn)
	if !needCall {
		return local, nil
	}

	rendered, dropped, err := fitAliasInput(aliasInstruction, proj.Text,
		renderMentions(aliasIn), renderCandidates(aliasIn))
	if err != nil {
		return nil, fmt.Errorf("knowledge: item %s: alias input: %w", in.ItemID, err)
	}
	_ = dropped // T029 之后随状态一起呈现为 candidate_truncated

	body, err := p.runOrReplay(ctx, in, phaseAlias, rendered, func(b string) error {
		decs, perr := parseAliasResponse(b)
		if perr != nil {
			return perr
		}
		return validateAliasResponse(decs, aliasIn)
	})
	if err != nil {
		return nil, err
	}
	decisions, err := parseAliasResponse(body)
	if err != nil {
		return nil, fmt.Errorf("knowledge: item %s: alias phase: %w", in.ItemID, err)
	}
	if err := validateAliasResponse(decisions, aliasIn); err != nil {
		// ⚠️ 整份拒绝。挑出合法的那几条决策发布，等于让一部分 mention
		// 走归一、另一部分不走——而哪些走了取决于模型这次坏在哪儿。
		return nil, fmt.Errorf("knowledge: item %s: %w", in.ItemID, err)
	}
	return decisions, nil
}

// aliasRulesVersion 是身份判定规则的版本。
//
// ⚠️ 它必须**和每条决策一起存下来**：规则改了之后，旧记录是按旧规则判的，
// 拿新规则去复核它会得出错误结论——而复核正是本期要人做的事。
const aliasRulesVersion = "alias-rules-v1"

// aliasEvidence 是落库的判定依据（FR-014）。
//
// ⭐ 依据是**可读的原文引用**，不是一个分数。
// ⚠️ 查不出来的话，人工复核只能对着一堆已经合并好的人物干瞪眼：
// 「阿Q」和「老Q」为什么被判成同一个人？依据是哪句话？没有记录就答不了。
type aliasEvidence struct {
	RulesVersion string         `json:"rules_version"`
	ReasonCode   string         `json:"reason_code"`
	Action       string         `json:"action"`
	Supports     []aliasSupport `json:"supports"`
}

func renderCandidates(in aliasInput) []string {
	ids := make([]string, 0, len(in.Candidates))
	for id := range in.Candidates {
		ids = append(ids, id)
	}
	// ⚠️ 排序后再渲染：候选超限时删的是**尾部**，顺序不定的话每次删掉的
	// 都不是同一批，同一份输入两次会得到不同的归一结果。
	sort.Strings(ids)
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id+"\t"+in.Candidates[id])
	}
	return out
}

func renderMentions(in aliasInput) []string {
	refs := make([]string, 0, len(in.Mentions))
	for ref := range in.Mentions {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, ref+"\t"+in.Mentions[ref])
	}
	return out
}

// parseAliasResponse 严格解析归一响应。
func parseAliasResponse(body string) ([]aliasDecision, error) {
	var raw struct {
		Decisions []struct {
			MentionRef  string `json:"mention_ref"`
			Action      string `json:"action"`
			CharacterID string `json:"character_id"`
			NewGroup    int    `json:"new_group"`
			ReasonCode  string `json:"reason_code"`
			Supports    []struct {
				SourceRef  string `json:"source_ref"`
				Quote      string `json:"quote"`
				Occurrence int    `json:"occurrence"`
			} `json:"supports"`
		} `json:"decisions"`
	}
	if err := strictDecode(body, &raw); err != nil {
		return nil, err
	}
	out := make([]aliasDecision, 0, len(raw.Decisions))
	for _, d := range raw.Decisions {
		dec := aliasDecision{
			MentionRef: d.MentionRef, Action: d.Action, CharacterID: d.CharacterID,
			NewGroup: d.NewGroup, ReasonCode: d.ReasonCode,
		}
		for _, s := range d.Supports {
			dec.Supports = append(dec.Supports, aliasSupport{
				SourceRef: s.SourceRef, Quote: s.Quote, Occurrence: s.Occurrence,
			})
		}
		out = append(out, dec)
	}
	return out, nil
}

// strictDecode 与抽取阶段同一口径：不接受未知字段、尾随内容。
func strictDecode(body string, dst any) error {
	dec := json.NewDecoder(bytes.NewReader([]byte(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return invalid("decode: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return invalid("unexpected trailing content after the JSON object")
		}
		return invalid("trailing content: %v", err)
	}
	return nil
}

// buildOutcome 把解析结果与归一决策拼成待发布的内容。
//
// ⚠️ 关系端点用**归一之后**的组号，不是 mention ref：同一个人的两个称呼
// 在同一块里必须落到同一个人物上，否则这一块自己就先碎了。
func buildOutcome(
	in itemInput, proj chunkProjection, parsed extractionResponse, decisions []aliasDecision,
) (extractionOutcome, error) {
	byRef := make(map[string]aliasDecision, len(decisions))
	for _, d := range decisions {
		byRef[d.MentionRef] = d
	}

	// 组号 -> 本次要新建的人物本地引用。
	groupRef := map[int]string{}
	var out extractionOutcome
	// ⚠️ 按 mention 在原文里的位置排序，让人物创建顺序可复现。
	mentions := append([]mentionRef(nil), parsed.Mentions...)
	sort.SliceStable(mentions, func(i, j int) bool { return mentions[i].Start < mentions[j].Start })

	refToLocal := map[string]string{}
	for _, m := range mentions {
		d, ok := byRef[m.Ref]
		if !ok {
			return out, fmt.Errorf("mention %q has no alias decision", m.Ref)
		}
		// ⭐ 每个称呼都留一条判定记录，无论它是新建、链接还是歧义。
		// ⚠️ 只记"合并成功"的那些，等于把系统判断不了的部分从记录里抹掉——
		// 而那部分恰恰是人工复核最需要看的。
		evJSON, jerr := json.Marshal(aliasEvidence{
			RulesVersion: aliasRulesVersion, ReasonCode: d.ReasonCode,
			Action: d.Action, Supports: d.Supports,
		})
		if jerr != nil {
			return out, fmt.Errorf("marshal alias evidence: %w", jerr)
		}
		mentionFrom, _, located := proj.toDocument(m.Start, m.End)
		if !located {
			return out, fmt.Errorf("mention %q cannot be located in the document", m.Ref)
		}
		out.Aliases = append(out.Aliases, aliasDraft{
			Surface: m.Surface, State: aliasStateFor(d.Action),
			FirstSourceOrder: int64(mentionFrom), Evidence: evJSON,
			DecisionKey: aliasDecisionKey(in.ChunkID, m.Ref, mentionFrom, d),
		})
		aliasIdx := len(out.Aliases) - 1
		if d.Action == aliasActionLink {
			local := "link:" + d.CharacterID
			refToLocal[m.Ref] = local
			out.Aliases[aliasIdx].CharacterRef = local
			continue
		}

		local, seen := groupRef[d.NewGroup]
		if !seen {
			local = fmt.Sprintf("g%d", d.NewGroup)
			groupRef[d.NewGroup] = local
			from, to, ok := proj.toDocument(m.Start, m.End)
			if !ok {
				return out, fmt.Errorf("mention %q cannot be located in the document", m.Ref)
			}
			out.Characters = append(out.Characters, characterDraft{
				LocalRef: local, DisplayName: m.Surface, FirstSourceOrder: int64(from),
				HasAmbiguity: d.Action == aliasActionAmbiguous,
			})
			_ = to
		}
		refToLocal[m.Ref] = local
		out.Aliases[aliasIdx].CharacterRef = local
	}

	for _, rel := range parsed.Relations {
		subject, sok := refToLocal[rel.SubjectRef]
		object, ook := refToLocal[rel.ObjectRef]
		if !sok || !ook {
			return out, fmt.Errorf("relation endpoint has no identity decision")
		}
		if subject == object {
			// 归一之后两端落到同一个人物：这条关系没有意义，整体拒绝。
			// ⚠️ 悄悄丢掉它会让关系总数少一条而没有任何迹象。
			return out, fmt.Errorf("relation collapses onto a single character after normalization")
		}
		var evs []evidenceDraft
		for _, ev := range rel.Evidence {
			from, to, ok := proj.toDocument(ev.Start, ev.End)
			if !ok {
				return out, fmt.Errorf("evidence quote cannot be located in the document")
			}
			evs = append(evs, evidenceDraft{
				ChunkID: in.ChunkID, DocumentVersion: in.DocumentVersion,
				SourceOrder: int64(from), SourceStart: from, SourceEnd: to, Quote: ev.Quote,
			})
		}
		out.Relations = append(out.Relations, relationDraft{
			SubjectRef: subject, ObjectRef: object, Type: rel.Type, IsDirected: isDirected(rel.Type),
			FirstSourceOrder: int64(evs[0].SourceStart),
			ChapterNumber:    in.Metadata.ChapterNumber, ChapterTitle: in.Metadata.ChapterTitle,
			Evidence: dedupeEvidenceByDocumentInterval(evs),
		})
	}
	return out, nil
}

// aliasStateFor 把归一动作映射成 narrative_aliases.state。
//
// ⚠️ ambiguous **不是** rejected：它是一次成功的「分不清」判定，
// 查询时要据此给用户一句歧义提示。混成 rejected 就把"系统看不出来"
// 说成了"系统看出来不是"。
func aliasStateFor(action string) string {
	switch action {
	case aliasActionAmbiguous:
		return "ambiguous"
	default:
		return "supported"
	}
}

// aliasDecisionKey 算这条判定的去重键。
//
// ⭐ 键里含 **mention 的原文位置**、候选实体和规则版本，不只是名字。
// ⚠️ 只按名字去重会让同一个称呼在书里的每一次出现被折叠成一条——
// 而「这个称呼在第 3 章和第 57 章各被判过一次」正是复核要看的东西。
func aliasDecisionKey(chunkID, mentionRef string, sourceOrder int, d aliasDecision) []byte {
	h := sha256.New()
	writeHashField(h, aliasRulesVersion)
	// ⚠️ chunk_id 不可省——它是 data-model 说的「mention 来源」。
	// 模型在每个块里都从 m1 开始编号，所以**不同块的 ref 会重名**；
	// 键里只有 ref 的话，第二个块的 m1 会撞上第一个块的 m1 被 INSERT IGNORE
	// 静默丢掉，那个称呼的判定依据从此查不到，而人物和关系照样在，
	// 看不出少了什么。第一版漏了它，用例当场抓住。
	writeHashField(h, chunkID)
	writeHashField(h, mentionRef)
	writeHashField(h, d.Action)
	writeHashField(h, d.CharacterID)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(sourceOrder))
	h.Write(buf[:])
	return h.Sum(nil)
}

// isDirected 来自标注指南 §2 的方向列。
// ⚠️ 无向类型必须标对：标成有向会让 (A,B) 与 (B,A) 成为两条不同的关系，
// 直接虚增关系总数。
func isDirected(relType string) bool {
	switch relType {
	case "亲属", "同乡邻里", "冲突", "同伙":
		return false
	default:
		return true
	}
}
