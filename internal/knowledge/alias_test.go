package knowledge

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// alias_test.go 守身份归一（010 T026/T027）。
//
// ⭐ 用例的重心全在**拒绝**上，因为这一阶段唯一昂贵的错误是误合并：
// 把"赵太爷"和"赵秀才"合成一个人之后，儿子的每条关系都变成父亲的关系，
// 而每一条关系的原文引用都是真的——错的是身份，身份不在引用里。

const aliasChunk = "阿Q姓什么，向来不很清楚。他活着的时候，人都叫他阿Q。老Q，就是阿Q，赵太爷这样叫他。阿Q大约是阿贵，也未可知。"

func aliasFixture(t *testing.T) aliasInput {
	t.Helper()
	chunk := chunkFixture(aliasChunk, 0)
	loc := newQuoteLocator(chunk)
	mention := func(ref, surface string, _ int) resolvedMention {
		t.Helper()
		s, e, _, err := loc.locate(surface)
		if err != nil {
			t.Fatalf("fixture mention %q: %v", surface, err)
		}
		return resolvedMention{Ref: ref, Surface: surface, ChunkStart: s, ChunkEnd: e, DocumentStart: s, DocumentEnd: e}
	}
	return aliasInput{
		Chunk:    chunk,
		Mentions: []resolvedMention{mention("m1", "阿Q", 0), mention("m2", "老Q", 0), mention("m3", "赵太爷", 0)},
		Proposals: []resolvedAliasProposal{{
			Left: "m1", Right: "m2",
			Evidence: evidenceDraft{Quote: "老Q，就是阿Q", SourceStart: 0, SourceEnd: 8},
		}},
	}
}

func decisionJSON(decisions ...string) string {
	return `{"decisions":[` + strings.Join(decisions, ",") + `]}`
}

func newDecision(ref, group, quote string, occ int) string {
	return fmt.Sprintf(`{"mention_ref":%q,"action":"new","character_id":"","new_group":%q,`+
		`"supports":[{"source_ref":"chunk","quote":%q,"occurrence":%d}],"reason_code":"explicit_alias"}`,
		ref, group, quote, occ)
}

func mustResolveAlias(t *testing.T, in aliasInput, raw string) identityAssignment {
	t.Helper()
	resp, err := parseAliasResponse([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	assign, err := resolveAliasDecisions(in, resp)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return assign
}

// TestExplicitAliasProposalLetsTwoMentionsShareAGroup 是正例：
// 原文明写"老Q，就是阿Q"，两个称呼可以共用一个新人物。
func TestExplicitAliasProposalLetsTwoMentionsShareAGroup(t *testing.T) {
	in := aliasFixture(t)
	assign := mustResolveAlias(t, in, decisionJSON(
		newDecision("m1", "g1", "老Q，就是阿Q", 0),
		newDecision("m2", "g1", "老Q，就是阿Q", 0),
		newDecision("m3", "g2", "赵太爷这样叫他", 0),
	))
	if len(assign.Characters) != 2 {
		t.Fatalf("want 2 characters (阿Q/老Q 合一 + 赵太爷), got %d", len(assign.Characters))
	}
	if assign.MentionToGroup["m1"] != assign.MentionToGroup["m2"] {
		t.Error("有明确别名句的两个称呼没有合并")
	}
	if assign.MentionToGroup["m3"] == assign.MentionToGroup["m1"] {
		t.Error("赵太爷被并进了阿Q")
	}
}

// TestHedgedAliasProposalCannotMerge 是阿Q正传本身的反例（标注指南 §3）：
// 原文只说"阿Q大约是阿贵，也未可知"，`阿贵` 无佐证，不得合并。
//
// ⚠️ 这句话同时出现两个称呼、逐字属于原文、引用完全可核验——
// 只看字面的归一器会把它当成一句漂亮的别名连接句。
func TestHedgedAliasProposalCannotMerge(t *testing.T) {
	in := aliasFixture(t)
	loc := newQuoteLocator(in.Chunk)
	s, e, _, err := loc.locate("阿贵")
	if err != nil {
		t.Fatal(err)
	}
	in.Mentions = append(in.Mentions, resolvedMention{
		Ref: "m4", Surface: "阿贵", ChunkStart: s, ChunkEnd: e, DocumentStart: s, DocumentEnd: e})
	in.Proposals = append(in.Proposals, resolvedAliasProposal{
		Left: "m1", Right: "m4",
		Evidence: evidenceDraft{Quote: "阿Q大约是阿贵，也未可知"},
	})

	resp, err := parseAliasResponse([]byte(decisionJSON(
		newDecision("m1", "g1", "老Q，就是阿Q", 0),
		newDecision("m2", "g1", "老Q，就是阿Q", 0),
		newDecision("m3", "g2", "赵太爷这样叫他", 0),
		newDecision("m4", "g1", "阿Q大约是阿贵，也未可知", 0), // 想并进阿Q
	)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolveAliasDecisions(in, resp); !errors.Is(err, errAliasMergeUnsupported) {
		t.Fatalf("带犹疑标记的提案被接受了，err = %v", err)
	}
}

// TestSurfaceOnlyMergeIsRejected：没有任何别名提案时，两个称呼不能共组，
// 哪怕字面上看起来就是一个人。
func TestSurfaceOnlyMergeIsRejected(t *testing.T) {
	in := aliasFixture(t)
	in.Proposals = nil // 第一阶段没有提出任何别名
	resp, err := parseAliasResponse([]byte(decisionJSON(
		newDecision("m1", "g1", "人都叫他阿Q", 0),
		newDecision("m2", "g1", "老Q，就是阿Q", 0),
		newDecision("m3", "g2", "赵太爷这样叫他", 0),
	)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolveAliasDecisions(in, resp); !errors.Is(err, errAliasMergeUnsupported) {
		t.Fatalf("无提案的合并被接受了，err = %v", err)
	}
}

// TestAmbiguousMentionsStayIndependent：歧义的称呼各自独立成人物并标歧义，
// 这属于**成功处理**，不是失败，也不强行合并（契约 §2 末段）。
func TestAmbiguousMentionsStayIndependent(t *testing.T) {
	in := aliasFixture(t)
	ambiguous := func(ref, group string) string {
		return fmt.Sprintf(`{"mention_ref":%q,"action":"ambiguous","character_id":"","new_group":%q,`+
			`"supports":[{"source_ref":"chunk","quote":"阿Q姓什么，向来不很清楚","occurrence":0}],`+
			`"reason_code":"insufficient"}`, ref, group)
	}
	assign := mustResolveAlias(t, in, decisionJSON(
		ambiguous("m1", "g1"), ambiguous("m2", "g2"),
		newDecision("m3", "g3", "赵太爷这样叫他", 0),
	))
	if len(assign.Characters) != 3 {
		t.Fatalf("歧义的称呼应当各自保留，got %d characters", len(assign.Characters))
	}
	for _, c := range assign.Characters[:2] {
		if !c.HasAmbiguity {
			t.Errorf("character %q 没有标歧义", c.LocalRef)
		}
	}

	// 两个都拿不准的称呼放进同一组是不允许的：
	// "都不确定"不能当成"是同一个人"的理由。
	resp, err := parseAliasResponse([]byte(decisionJSON(
		ambiguous("m1", "g1"), ambiguous("m2", "g1"),
		newDecision("m3", "g3", "赵太爷这样叫他", 0),
	)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolveAliasDecisions(in, resp); !errors.Is(err, errAliasResponseInvalid) {
		t.Fatalf("两个歧义称呼合并被接受了，err = %v", err)
	}
}

// TestLinkRejections 覆盖链接既有人物的全部拒绝条件。
func TestLinkRejections(t *testing.T) {
	base := aliasFixture(t)
	base.Candidates = []aliasCandidate{{
		CharacterID: "char-1", DisplayName: "阿Q", FirstSourceOrder: 3,
		Evidence: []aliasCandidateEvidence{{Ref: "e1", Quote: "人都叫他阿Q"}},
	}}
	link := func(ref, charID, reason string, supports string) string {
		return fmt.Sprintf(`{"mention_ref":%q,"action":"link","character_id":%q,"new_group":"",`+
			`"supports":[%s],"reason_code":%q}`, ref, charID, supports, reason)
	}
	chunkSupport := `{"source_ref":"chunk","quote":"老Q，就是阿Q","occurrence":0}`
	candSupport := `{"source_ref":"e1","quote":"人都叫他阿Q","occurrence":0}`

	cases := []struct {
		name    string
		raw     string
		wantErr error
	}{
		{
			"链接到输入里没有的人物 ID",
			decisionJSON(link("m1", "char-999", reasonExplicitAlias, chunkSupport+","+candSupport),
				newDecision("m2", "g1", "老Q，就是阿Q", 0), newDecision("m3", "g2", "赵太爷这样叫他", 0)),
			errAliasResponseInvalid,
		},
		{
			"只有单侧依据，也没有明确别名连接句",
			decisionJSON(link("m3", "char-1", reasonContextIdenty, `{"source_ref":"chunk","quote":"赵太爷这样叫他","occurrence":0}`),
				newDecision("m1", "g1", "人都叫他阿Q", 0), newDecision("m2", "g2", "老Q，就是阿Q", 0)),
			errAliasMergeUnsupported,
		},
		{
			"出处不在输入里",
			decisionJSON(link("m1", "char-1", reasonExplicitAlias, `{"source_ref":"e9","quote":"人都叫他阿Q","occurrence":0}`),
				newDecision("m2", "g1", "老Q，就是阿Q", 0), newDecision("m3", "g2", "赵太爷这样叫他", 0)),
			errAliasResponseInvalid,
		},
		{
			"依据在当前块里找不到",
			decisionJSON(link("m1", "char-1", reasonExplicitAlias,
				`{"source_ref":"chunk","quote":"阿Q就是老Q本人","occurrence":0},`+candSupport),
				newDecision("m2", "g1", "老Q，就是阿Q", 0), newDecision("m3", "g2", "赵太爷这样叫他", 0)),
			errQuoteNotFound,
		},
		{
			"link 还带了 new_group",
			`{"decisions":[{"mention_ref":"m1","action":"link","character_id":"char-1","new_group":"g1",` +
				`"supports":[` + chunkSupport + `,` + candSupport + `],"reason_code":"explicit_alias"},` +
				newDecision("m2", "g2", "老Q，就是阿Q", 0) + `,` + newDecision("m3", "g3", "赵太爷这样叫他", 0) + `]}`,
			errAliasResponseInvalid,
		},
		{
			"用 insufficient 当链接理由",
			decisionJSON(link("m1", "char-1", reasonInsufficient, chunkSupport+","+candSupport),
				newDecision("m2", "g1", "老Q，就是阿Q", 0), newDecision("m3", "g2", "赵太爷这样叫他", 0)),
			errAliasResponseInvalid,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := parseAliasResponse([]byte(tc.raw))
			if err != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("parse err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if _, err := resolveAliasDecisions(aliasFixtureWith(t, base), resp); !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// aliasFixtureWith 复制一份夹具，避免子测试之间互相污染 slice。
func aliasFixtureWith(t *testing.T, base aliasInput) aliasInput {
	t.Helper()
	out := base
	out.Mentions = append([]resolvedMention(nil), base.Mentions...)
	out.Proposals = append([]resolvedAliasProposal(nil), base.Proposals...)
	out.Candidates = append([]aliasCandidate(nil), base.Candidates...)
	return out
}

// TestValidLinkTwoSidedSupport 是链接的正例：两侧各有出处。
func TestValidLinkTwoSidedSupport(t *testing.T) {
	in := aliasFixture(t)
	in.Candidates = []aliasCandidate{{
		CharacterID: "char-1", DisplayName: "阿Q", FirstSourceOrder: 3,
		Evidence: []aliasCandidateEvidence{{Ref: "e1", Quote: "人都叫他阿Q"}},
	}}
	assign := mustResolveAlias(t, in, decisionJSON(
		`{"mention_ref":"m1","action":"link","character_id":"char-1","new_group":"",`+
			`"supports":[{"source_ref":"chunk","quote":"人都叫他阿Q","occurrence":0},`+
			`{"source_ref":"e1","quote":"人都叫他阿Q","occurrence":0}],"reason_code":"context_identity"}`,
		newDecision("m2", "g1", "老Q，就是阿Q", 0),
		newDecision("m3", "g2", "赵太爷这样叫他", 0),
	))
	group := assign.MentionToGroup["m1"]
	if assign.Existing[group] != "char-1" {
		t.Fatalf("m1 没有链接到既有人物：%+v", assign.Existing)
	}
	for _, c := range assign.Characters {
		if c.LocalRef == group {
			t.Error("链接到既有人物时又新建了一个人物")
		}
	}
}

// TestEveryMentionNeedsExactlyOneDecision：契约要求每个 mention 恰一决策。
func TestEveryMentionNeedsExactlyOneDecision(t *testing.T) {
	in := aliasFixture(t)
	for _, tc := range []struct{ name, raw string }{
		{"少一个 mention", decisionJSON(newDecision("m1", "g1", "人都叫他阿Q", 0), newDecision("m2", "g2", "老Q，就是阿Q", 0))},
		{"同一个 mention 决策两次", decisionJSON(
			newDecision("m1", "g1", "人都叫他阿Q", 0),
			newDecision("m1", "g2", "人都叫他阿Q", 0),
			newDecision("m3", "g3", "赵太爷这样叫他", 0))},
		{"给了不存在的 mention", decisionJSON(
			newDecision("m1", "g1", "人都叫他阿Q", 0),
			newDecision("m2", "g2", "老Q，就是阿Q", 0),
			newDecision("m9", "g3", "赵太爷这样叫他", 0))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := parseAliasResponse([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := resolveAliasDecisions(in, resp); !errors.Is(err, errAliasResponseInvalid) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// TestNoCandidatesNoProposalsSkipsTheSecondCall：零调用路径。
func TestNoCandidatesNoProposalsSkipsTheSecondCall(t *testing.T) {
	in := aliasFixture(t)
	in.Proposals = nil
	if needsAliasPhase(in) {
		t.Fatal("既无候选也无提案，不该再调一次模型")
	}
	assign := independentIdentities(in)
	if len(assign.Characters) != 3 {
		t.Fatalf("want 3 独立身份, got %d", len(assign.Characters))
	}

	// 同一精确位置被列了两遍 → 只建一个人物。
	dup := in.Mentions[0]
	dup.Ref = "m9"
	in.Mentions = append(in.Mentions, dup)
	if got := len(independentIdentities(in).Characters); got != 3 {
		t.Errorf("同一位置重复列出应当只建一次，got %d characters", got)
	}
	// 但同一个称呼出现在两处原文，仍然是两个人物——位置是唯一有把握的区分依据。
	elsewhere := in.Mentions[0]
	elsewhere.Ref = "m10"
	elsewhere.DocumentStart, elsewhere.DocumentEnd = 900, 902
	in.Mentions = append(in.Mentions, elsewhere)
	if got := len(independentIdentities(in).Characters); got != 4 {
		t.Errorf("不同位置的同名称呼被合并了，got %d characters", got)
	}
}

// TestCandidateSelectionRanking：精确命中优先，然后源顺序，最后 ID；
// 超窗口的部分必须报出被截掉的数量，不能静默丢弃。
func TestCandidateSelectionRanking(t *testing.T) {
	var pool []aliasCandidate
	for i := 0; i < maxAliasCandidates+3; i++ {
		pool = append(pool, aliasCandidate{
			CharacterID:      fmt.Sprintf("char-%02d", i),
			DisplayName:      fmt.Sprintf("路人%02d", i),
			FirstSourceOrder: int64(i),
			Evidence: []aliasCandidateEvidence{
				{Ref: "e1", Quote: "a"}, {Ref: "e2", Quote: "b"}, {Ref: "e3", Quote: "c"},
			},
		})
	}
	// 一个源顺序很靠后、但名字精确命中的候选，必须排到最前面。
	pool = append(pool, aliasCandidate{CharacterID: "char-hit", DisplayName: "阿Q", FirstSourceOrder: 9999})

	selected, truncated := selectAliasCandidates(pool, []string{"阿Q"})
	if len(selected) != maxAliasCandidates {
		t.Fatalf("selected %d candidates", len(selected))
	}
	if truncated != len(pool)-maxAliasCandidates {
		t.Errorf("truncated = %d, want %d", truncated, len(pool)-maxAliasCandidates)
	}
	if selected[0].CharacterID != "char-hit" {
		t.Errorf("精确命中的候选没有排在最前：%s", selected[0].CharacterID)
	}
	if len(selected[1].Evidence) > maxAliasCandidateEvidence {
		t.Errorf("候选依据没有截到 %d 条", maxAliasCandidateEvidence)
	}
}

// TestRelationEndpointsAreRewrittenToIdentities：关系端点改写成人物组号，
// 归一后自指的关系被丢掉，端点缺身份则整体失败。
func TestRelationEndpointsAreRewrittenToIdentities(t *testing.T) {
	in := aliasFixture(t)
	assign := mustResolveAlias(t, in, decisionJSON(
		newDecision("m1", "g1", "老Q，就是阿Q", 0),
		newDecision("m2", "g1", "老Q，就是阿Q", 0),
		newDecision("m3", "g2", "赵太爷这样叫他", 0),
	))
	relations := []resolvedRelation{
		{SubjectRef: "m3", ObjectRef: "m1", Type: "欺凌", IsDirected: true,
			Evidence: []evidenceDraft{{Quote: "赵太爷这样叫他"}}},
		// 归一之后两端都是 g1：自指，丢掉。
		{SubjectRef: "m1", ObjectRef: "m2", Type: "冲突",
			Evidence: []evidenceDraft{{Quote: "老Q，就是阿Q"}}},
	}
	out, err := buildExtractionOutcome(in.Chunk, relations, assign)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Relations) != 1 {
		t.Fatalf("want 1 relation after rewrite, got %d", len(out.Relations))
	}
	if out.Relations[0].SubjectRef != assign.MentionToGroup["m3"] ||
		out.Relations[0].ObjectRef != assign.MentionToGroup["m1"] {
		t.Errorf("端点没有改写成人物组号：%+v", out.Relations[0])
	}

	orphan := []resolvedRelation{{SubjectRef: "m1", ObjectRef: "m9", Type: "冲突"}}
	if _, err := buildExtractionOutcome(in.Chunk, orphan, assign); !errors.Is(err, errAliasResponseInvalid) {
		t.Fatalf("端点缺身份应当整体失败，err = %v", err)
	}
}
