package knowledge

import (
	"strings"
	"testing"
)

// alias_test.go 守人物身份归一（010 T026）。
//
// ⭐ 这一段的取舍方向必须先说清楚，因为它决定了所有规则的写法：
//
// **误合并比漏合并严重得多。** 漏合并的表现是一个人碎成两个节点——
// 难看、指标上体现为碎片化，但**每一条关系仍然指向正确的人**，
// 且人工看一眼就能发现。误合并把两个人的关系混进一个节点，
// 之后任何查询都会把甲的事算到乙头上，而**没有任何办法从结果反推**
// 哪些是错的——两个人的记录已经不可分了。
//
// 所以这里的规则一律**宁可拒绝**。它们会误伤一些本该合并的提案，
// 那部分损失体现在"碎片化"这个可见的指标里，而不是藏在关系数据里。

// --- 假设 / 否定语句：不得作为合并依据 ---

// TestHypotheticalQuoteCannotSupportAlias——⭐ 来自《阿Q正传》第一章的真实反例。
//
// 原文在讨论阿Q的名字时明说了「阿桂还是阿贵呢？」——那是一句**存疑**，
// 不是一句认定。把它当成别名依据，就会把「阿贵」和「阿Q」合成一个人，
// 而小说本身在那一段的意思恰恰相反。
//
// ⚠️ 这条规则是**保守的字面过滤**，不是语义理解。它会误伤一些本该通过的
// 提案（例如引文里恰好出现"可能"但整句是肯定的）。这个方向是刻意选的：
// 误伤降低的是合并率（可见），放行降低的是关系数据的可信度（不可见）。
func TestHypotheticalQuoteCannotSupportAlias(t *testing.T) {
	rejected := []string{
		"阿Quei，阿桂还是阿贵呢？",
		"他也许姓赵，也未可知。",
		"倘使他姓赵，那便是本家。",
		"我不知道他究竟叫什么名字。",
		"并没有证据说明这两个人是同一个。",
		"大约就是那个人罢。",
		"似乎是同一个人。",
		"他未必就是阿Q。",
		"这恐怕不是一个人。",
		"假如阿贵就是阿Q的话。",
		// ⚠️ 上面每一条都同时含有假设或否定标记，所以问号那道检查
		// 从来没被单独执行过——变异测试把它删掉时测试照样全绿。
		// 这两条是**只靠问号**才能挡住的：整句在发问，不在认定。
		"他叫阿贵。是阿Q吗",
		"阿贵与阿Q同为一人？",
	}
	for _, q := range rejected {
		if quoteSupportsIdentity(q) {
			t.Errorf("存疑/否定的引文被当成了身份依据：%q", q)
		}
	}
	accepted := []string{
		"阿Quei，人称阿Q。",
		"赵太爷的儿子赵秀才，人都叫他茂才公。",
		"王胡，因为他又癞又胡。",
		"这就是阿Q，未庄人都认得。",
	}
	for _, q := range accepted {
		if !quoteSupportsIdentity(q) {
			t.Errorf("明确的身份依据被拒了：%q", q)
		}
	}
}

// TestSameSurfaceAloneIsNotIdentity——⭐ **只凭同名不合并**。
//
// 两条规则各挡一半：
//   - 一句只有名字的引文携带的信息量是零，它只说明"这个名字在这里出现过"，
//     而那是我们本来就知道的。⚠️ 当 surface 与候选同名时更明显：
//     "引文同时包含两者"会变成**恒真**，任何一次出现都成了合并依据。
//     第一版就是这么放行的，这条用例把它抓了出来。
//   - 同名候选不止一个时只能 ambiguous：两个「老王」谁都符合，
//     挑一个就是把"分不清"记成了确定的合并。
func TestSameSurfaceAloneIsNotIdentity(t *testing.T) {
	base := aliasDecision{
		MentionRef: "m2", Action: aliasActionLink, CharacterID: "ch-1",
		ReasonCode: reasonExplicitAlias,
		Supports: []aliasSupport{
			{SourceRef: "m2", Quote: "老王", Occurrence: 0},
		},
	}
	in := aliasInput{
		Mentions:   map[string]string{"m1": "老王", "m2": "老王"},
		Candidates: map[string]string{"ch-1": "老王"},
		ChunkText:  "老王来了。后来另一个老王也来了。",
	}
	if err := validateAliasDecision(base, in); err == nil {
		t.Error("一句只有名字的引文被当成了合并依据")
	}
	// 带上下文的引文可以（同名的两个 mention 指向同一个候选是正常的）。
	withContext := base
	withContext.Supports = []aliasSupport{{SourceRef: "m2", Quote: "老王来了", Occurrence: 0}}
	if err := validateAliasDecision(withContext, in); err != nil {
		t.Errorf("带上下文的同名 link 被拒：%v", err)
	}
	// 但同名候选有两个时，必须 ambiguous。
	two := in
	two.Candidates = map[string]string{"ch-1": "老王", "ch-2": "老王"}
	if err := validateAliasDecision(withContext, two); err == nil {
		t.Error("两个同名候选却仍然放行了 link——谁都符合，挑一个就是编")
	}
}

// TestLinkNeedsSupportForBothSides——link 必须同时支撑当前 mention 和候选身份，
// 或者有一句明确把两者连起来的话。
func TestLinkNeedsSupportForBothSides(t *testing.T) {
	in := aliasInput{
		Mentions:   map[string]string{"m1": "老Q"},
		Candidates: map[string]string{"ch-1": "阿Q"},
		ChunkText:  "老Q，就是从前的阿Q。旁边还有别人。",
	}
	// 只引了一句提到"老Q"的话，没有把它和"阿Q"连起来。
	weak := aliasDecision{
		MentionRef: "m1", Action: aliasActionLink, CharacterID: "ch-1",
		ReasonCode: reasonContextIdentity,
		Supports:   []aliasSupport{{SourceRef: "m1", Quote: "旁边还有别人", Occurrence: 0}},
	}
	if err := validateAliasDecision(weak, in); err == nil {
		t.Error("没有连接两侧身份的依据却放行了 link")
	}
	// 一句同时出现两个称呼的话是合格的连接依据。
	strong := weak
	strong.Supports = []aliasSupport{{SourceRef: "m1", Quote: "老Q，就是从前的阿Q", Occurrence: 0}}
	if err := validateAliasDecision(strong, in); err != nil {
		t.Errorf("合格的连接依据被拒：%v", err)
	}
}

// TestLinkMustPointAtAProvidedCandidate——link 只能指向本次输入里给出的候选。
// ⚠️ 指向别处的 ID 意味着模型在编 ID，而那个 ID 可能恰好是另一个真实人物。
func TestLinkMustPointAtAProvidedCandidate(t *testing.T) {
	in := aliasInput{
		Mentions:   map[string]string{"m1": "老Q"},
		Candidates: map[string]string{"ch-1": "阿Q"},
		ChunkText:  "老Q，就是从前的阿Q。",
	}
	for name, id := range map[string]string{"不存在的候选": "ch-999", "空 ID": ""} {
		dec := aliasDecision{
			MentionRef: "m1", Action: aliasActionLink, CharacterID: id,
			ReasonCode: reasonExplicitAlias,
			Supports:   []aliasSupport{{SourceRef: "m1", Quote: "老Q，就是从前的阿Q", Occurrence: 0}},
		}
		if err := validateAliasDecision(dec, in); err == nil {
			t.Errorf("「%s」被放行了", name)
		}
	}
}

// TestAmbiguousKeepsIdentitiesApart——⭐ ambiguous 是**成功处理**，不是失败。
//
// ⚠️ 它必须各自独立组号、不带 character_id。强行挑一个"最可能"的候选
// 就是把一次"分不清"记成了一次确定的合并，而那个合并再也没人会去复查。
func TestAmbiguousKeepsIdentitiesApart(t *testing.T) {
	in := aliasInput{
		Mentions:   map[string]string{"m1": "老王", "m2": "老王"},
		Candidates: map[string]string{"ch-1": "老王", "ch-2": "老王"},
		ChunkText:  "老王来了。后来另一个老王也来了。",
	}
	ok := aliasDecision{
		MentionRef: "m1", Action: aliasActionAmbiguous, NewGroup: 1,
		ReasonCode: reasonInsufficient,
		Supports:   []aliasSupport{{SourceRef: "m1", Quote: "老王来了", Occurrence: 0}},
	}
	if err := validateAliasDecision(ok, in); err != nil {
		t.Errorf("合法的 ambiguous 被拒：%v", err)
	}
	bad := ok
	bad.CharacterID = "ch-1"
	if err := validateAliasDecision(bad, in); err == nil {
		t.Error("ambiguous 却带着 character_id——那是把「分不清」记成了确定的合并")
	}
}

// TestNewGroupRulesAreEnforced——组号规则。
func TestNewGroupRulesAreEnforced(t *testing.T) {
	in := aliasInput{
		Mentions:   map[string]string{"m1": "阿Q", "m2": "老Q", "m3": "王胡"},
		Candidates: map[string]string{},
		ChunkText:  "阿Q和老Q本是一人。王胡在旁边。",
		// 只有 m1/m2 之间有明确的别名提案。
		Proposals: []aliasProposalPair{{Left: "m1", Right: "m2"}},
	}
	sup := func(ref, quote string) []aliasSupport {
		return []aliasSupport{{SourceRef: ref, Quote: quote, Occurrence: 0}}
	}
	group := func(ref string, g int, q string) aliasDecision {
		return aliasDecision{MentionRef: ref, Action: aliasActionNew, NewGroup: g,
			ReasonCode: reasonExplicitAlias, Supports: sup(ref, q)}
	}
	// 有提案的两个可以共用组号。
	if err := validateAliasGroups([]aliasDecision{
		group("m1", 1, "阿Q和老Q本是一人"),
		group("m2", 1, "阿Q和老Q本是一人"),
		group("m3", 2, "王胡在旁边"),
	}, in); err != nil {
		t.Errorf("合法的组号安排被拒：%v", err)
	}
	// ⚠️ 没有提案的两个共用组号 = 凭空合并。
	if err := validateAliasGroups([]aliasDecision{
		group("m1", 1, "阿Q和老Q本是一人"),
		group("m3", 1, "王胡在旁边"),
	}, in); err == nil {
		t.Error("没有别名提案的两个 mention 共用了组号——这是凭空合并")
	}
	// link 的组号必须为空。
	linked := aliasDecision{MentionRef: "m1", Action: aliasActionLink, CharacterID: "ch-1",
		NewGroup: 1, ReasonCode: reasonExplicitAlias, Supports: sup("m1", "阿Q和老Q本是一人")}
	if err := validateAliasGroups([]aliasDecision{linked}, in); err == nil {
		t.Error("link 带着组号却被放行")
	}
}

// TestEveryMentionGetsExactlyOneDecision——每个 mention 恰好一条决策。
// ⚠️ 漏掉一个的表现是那个 mention 被静默丢弃：它参与的关系跟着消失，
// 而关系总数少了几条没有任何迹象。
func TestEveryMentionGetsExactlyOneDecision(t *testing.T) {
	in := aliasInput{
		Mentions:  map[string]string{"m1": "阿Q", "m2": "王胡"},
		ChunkText: "阿Q和王胡打了一架。",
	}
	one := aliasDecision{MentionRef: "m1", Action: aliasActionNew, NewGroup: 1,
		ReasonCode: reasonContextIdentity,
		Supports:   []aliasSupport{{SourceRef: "m1", Quote: "阿Q和王胡打了一架", Occurrence: 0}}}
	two := one
	two.MentionRef, two.NewGroup = "m2", 2

	if err := validateAliasResponse([]aliasDecision{one, two}, in); err != nil {
		t.Errorf("完整的决策集被拒：%v", err)
	}
	if err := validateAliasResponse([]aliasDecision{one}, in); err == nil {
		t.Error("漏了一个 mention 却被放行——它参与的关系会跟着静默消失")
	}
	if err := validateAliasResponse([]aliasDecision{one, two, two}, in); err == nil {
		t.Error("同一个 mention 有两条决策却被放行")
	}
	extra := one
	extra.MentionRef = "m9"
	if err := validateAliasResponse([]aliasDecision{one, two, extra}, in); err == nil {
		t.Error("为不存在的 mention 下了决策却被放行")
	}
}

// TestReasonCodeIsClosed——⚠️ 契约要求封闭的 reason_code，
// 且**不接受一个数字 confidence**：一个 0.83 无法被复核，
// 而 explicit_alias / context_identity 说明了依据的种类，可以逐条查。
func TestReasonCodeIsClosed(t *testing.T) {
	in := aliasInput{Mentions: map[string]string{"m1": "阿Q"}, ChunkText: "阿Q来了。"}
	mk := func(code string) aliasDecision {
		return aliasDecision{MentionRef: "m1", Action: aliasActionNew, NewGroup: 1,
			ReasonCode: code,
			Supports:   []aliasSupport{{SourceRef: "m1", Quote: "阿Q来了", Occurrence: 0}}}
	}
	for _, code := range aliasReasonCodes {
		if err := validateAliasDecision(mk(code), in); err != nil {
			t.Errorf("合法 reason_code %q 被拒：%v", code, err)
		}
	}
	for _, code := range []string{"", "0.83", "high", "probably"} {
		if err := validateAliasDecision(mk(code), in); err == nil {
			t.Errorf("非法 reason_code %q 被放行", code)
		}
	}
}

// TestSupportBoundsAreEnforced——每条决策 1~4 条依据。
func TestSupportBoundsAreEnforced(t *testing.T) {
	in := aliasInput{Mentions: map[string]string{"m1": "阿Q"}, ChunkText: "阿Q来了。阿Q走了。"}
	mk := func(n int) aliasDecision {
		var sups []aliasSupport
		for i := 0; i < n; i++ {
			sups = append(sups, aliasSupport{SourceRef: "m1", Quote: "阿Q来了", Occurrence: 0})
		}
		return aliasDecision{MentionRef: "m1", Action: aliasActionNew, NewGroup: 1,
			ReasonCode: reasonContextIdentity, Supports: sups}
	}
	if err := validateAliasDecision(mk(0), in); err == nil {
		t.Error("没有依据的决策被放行——它不可核验")
	}
	if err := validateAliasDecision(mk(maxAliasSupports+1), in); err == nil {
		t.Errorf("超过 %d 条依据被放行", maxAliasSupports)
	}
	if err := validateAliasDecision(mk(1), in); err != nil {
		t.Errorf("一条依据被拒：%v", err)
	}
}

// TestSupportQuoteMustExistInSource——依据的引文同样要在原文里。
// ⚠️ 与关系证据同一条理由：这是唯一能挡住"编造依据"的检查。
func TestSupportQuoteMustExistInSource(t *testing.T) {
	in := aliasInput{Mentions: map[string]string{"m1": "阿Q"}, ChunkText: "阿Q来了。"}
	dec := aliasDecision{MentionRef: "m1", Action: aliasActionNew, NewGroup: 1,
		ReasonCode: reasonContextIdentity,
		Supports:   []aliasSupport{{SourceRef: "m1", Quote: "阿Q其实姓赵", Occurrence: 0}}}
	if err := validateAliasDecision(dec, in); err == nil {
		t.Error("原文里没有的引文被当成了依据")
	}
}

// TestCandidateCapIsEnforced——候选上限 32、每人 2 条身份依据（契约 §2）。
func TestCandidateCapIsEnforced(t *testing.T) {
	if maxAliasCandidates != 32 {
		t.Errorf("候选上限 = %d, want 32", maxAliasCandidates)
	}
	if maxIdentityEvidencePerCharacter != 2 {
		t.Errorf("每人身份依据上限 = %d, want 2", maxIdentityEvidencePerCharacter)
	}
}

// TestNoCandidatesNoProposalsSkipsTheSecondCall——⭐ 没有候选也没有别名提案时
// **不需要第二次调用**：每个 mention 各自独立身份，直接产出决策。
//
// ⚠️ 照样调一次的后果是白花一半的钱——大多数块里没有任何可归一的东西。
func TestNoCandidatesNoProposalsSkipsTheSecondCall(t *testing.T) {
	in := aliasInput{
		Mentions:  map[string]string{"m1": "阿Q", "m2": "王胡"},
		ChunkText: "阿Q和王胡打了一架。",
	}
	decs, needCall := planAliasResolution(in)
	if needCall {
		t.Error("没有候选也没有提案，却仍然要调模型")
	}
	if len(decs) != 2 {
		t.Fatalf("本地产出 %d 条决策, want 2", len(decs))
	}
	groups := map[int]bool{}
	for _, d := range decs {
		if d.Action != aliasActionNew || d.CharacterID != "" {
			t.Errorf("本地决策不是独立新建：%+v", d)
		}
		if groups[d.NewGroup] {
			t.Errorf("两个 mention 共用了组号 %d", d.NewGroup)
		}
		groups[d.NewGroup] = true
	}
	// 有候选就必须调。
	in.Candidates = map[string]string{"ch-1": "阿Q"}
	if _, needCall := planAliasResolution(in); !needCall {
		t.Error("有候选却跳过了归一调用")
	}
}

// TestCrossBookCandidatesAreNotOffered——⭐ 候选只能来自**同一次作业**。
// ⚠️ 跨书的同名人物（两本书都有「张三」）合并之后，一本书的关系会出现在
// 另一本书的查询结果里，而用户完全无法解释那些记录从哪来。
func TestCrossBookCandidatesAreNotOffered(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	jobA, epochA, itemA := publishFixture(t, repo, "doc-alias-a", "job-alias-a")
	jobB, epochB, itemB := publishFixture(t, repo, "doc-alias-b", "job-alias-b")
	for _, x := range []struct {
		job, item string
		epoch     int
	}{{jobA.ID, itemA, epochA}, {jobB.ID, itemB, epochB}} {
		if err := repo.publishItemOutcome(ctx, publishInput{
			JobID: x.job, ItemID: x.item, Epoch: x.epoch,
			Outcome: extractionOutcome{Characters: []characterDraft{
				{LocalRef: "m1", DisplayName: "张三", FirstSourceOrder: 1},
			}}, ExtractResponse: []byte(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	cands, err := repo.listAliasCandidates(ctx, jobA.ID, "张三", maxAliasCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("候选数 = %d, want 1——另一本书的同名人物不该出现", len(cands))
	}
	if !strings.HasPrefix(cands[0].ID, "") || cands[0].DisplayName != "张三" {
		t.Errorf("候选内容不对：%+v", cands[0])
	}
	var jobID string
	if err := repo.db.QueryRowContext(ctx,
		`SELECT job_id FROM narrative_characters WHERE id=?`, cands[0].ID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if jobID != jobA.ID {
		t.Errorf("候选来自作业 %s，而查询的是 %s", jobID, jobA.ID)
	}
}
