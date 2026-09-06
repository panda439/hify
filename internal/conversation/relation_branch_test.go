package conversation

import (
	"strings"
	"testing"

	"hify/internal/knowledge"
)

// relation_branch_test.go 守关系查询分支的固定文案（010 T033）。
//
// ⭐ 这一层的立论：**几种"查不到"必须给出不同的话**。
// 把它们都答成"没有找到相关信息"是最省事也最没用的做法——用户无法判断
// 该等一等、该重跑、还是该换个问法。
//
// ⚠️ 这些文本是**固定的**，不经过模型。理由是：这几种结局本身就是系统
// 已经确定的事实（没开启抽取、名字不认识、同名多人），交给模型复述一遍
// 只会引入它自己的措辞和不确定性——它可能把"还没跑完"说成"书里没有"。

// TestRelationBranchTextsAreDistinct——⭐ 每种结局给出不同的话，
// 且都不能说成"书里没有"。
func TestRelationBranchTextsAreDistinct(t *testing.T) {
	subject, object := "赵太爷", "阿Q"
	seen := map[string]string{}
	for _, outcome := range []string{
		knowledge.RelationOutcomeNoRecords,
		knowledge.RelationOutcomeIncomplete,
		knowledge.RelationOutcomeNotExtracted,
		knowledge.RelationOutcomeUnknownName,
		knowledge.RelationOutcomeAmbiguous,
		knowledge.RelationOutcomeSameEntity,
		knowledge.RelationOutcomeOutOfScope,
	} {
		ans := knowledge.RelationAnswer{Outcome: outcome, UnknownName: "根本没这个人"}
		if outcome == knowledge.RelationOutcomeIncomplete {
			n := 42
			ans.RemainingItems = &n
		}
		if outcome == knowledge.RelationOutcomeAmbiguous {
			ans.Candidates = []knowledge.RelationCandidateInfo{
				{DisplayName: "赵太爷", FirstSourceOrder: 100},
				{DisplayName: "赵太爷", FirstSourceOrder: 9000},
			}
		}
		text := relationBranchText(ans, subject, object)
		if text == "" {
			t.Errorf("结局 %q 没有文案", outcome)
			continue
		}
		if prev, dup := seen[text]; dup {
			t.Errorf("结局 %q 与 %q 用了同一句话：%q", outcome, prev, text)
		}
		seen[text] = outcome
	}
}

// TestIncompleteTextSaysHowMuchIsLeft——⚠️ 不说还剩多少，用户无法判断
// 该不该等——而"等一等"和"这书里没写"是完全不同的下一步。
func TestIncompleteTextSaysHowMuchIsLeft(t *testing.T) {
	n := 42
	text := relationBranchText(knowledge.RelationAnswer{
		Outcome: knowledge.RelationOutcomeIncomplete, RemainingItems: &n,
	}, "甲", "乙")
	if !strings.Contains(text, "42") {
		t.Errorf("未完成的文案里没有剩余数量：%q", text)
	}
}

// TestUnknownNameTextNamesTheName——⚠️ 不指出是哪个名字找不到，
// 用户把名字打错了却以为书里真的没写他们的关系。
func TestUnknownNameTextNamesTheName(t *testing.T) {
	text := relationBranchText(knowledge.RelationAnswer{
		Outcome: knowledge.RelationOutcomeUnknownName, UnknownName: "根本没这个人",
	}, "赵太爷", "根本没这个人")
	if !strings.Contains(text, "根本没这个人") {
		t.Errorf("文案里没有指出是哪个名字：%q", text)
	}
}

// TestAmbiguousTextListsCandidates——同名多人时把候选列出来，
// 用户才有得选。⚠️ 只说"有多个同名的人"而不列出来，用户无法澄清。
func TestAmbiguousTextListsCandidates(t *testing.T) {
	text := relationBranchText(knowledge.RelationAnswer{
		Outcome: knowledge.RelationOutcomeAmbiguous,
		Candidates: []knowledge.RelationCandidateInfo{
			{DisplayName: "赵太爷", FirstSourceOrder: 100},
			{DisplayName: "赵太爷", FirstSourceOrder: 9000},
		},
	}, "赵太爷", "阿Q")
	if !strings.Contains(text, "100") || !strings.Contains(text, "9000") {
		t.Errorf("候选没有带上区分信息，用户没法选：%q", text)
	}
}

// TestNoRecordsTextDoesNotOverclaim——⭐ "没有记录"的文案不能说成
// "他们没有关系"。
//
// ⚠️ 抽取只覆盖八类关系，而且是模型抽的。"库里没有这条记录"与
// "书里没有这层关系"是两回事，把前者说成后者就是拿系统的局限冒充事实。
func TestNoRecordsTextDoesNotOverclaim(t *testing.T) {
	text := relationBranchText(knowledge.RelationAnswer{
		Outcome: knowledge.RelationOutcomeNoRecords,
	}, "甲", "乙")
	for _, forbidden := range []string{"没有关系", "毫无关系", "不存在关系"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("文案把「没有记录」说成了「没有关系」：%q", text)
		}
	}
	// 必须说清楚这是**已抽取记录**的范围。
	if !strings.Contains(text, "记录") {
		t.Errorf("文案没有点明这是已抽取记录的范围：%q", text)
	}
}

// TestCitationsRenderWithChapterAndQuote——有证据时，送进 prompt 的文本
// 必须带章节与原文引用。⚠️ 只给结论不给引文，用户无法核验，
// 而这个功能的全部价值就在于"能指出书里哪一句"。
func TestCitationsRenderWithChapterAndQuote(t *testing.T) {
	ch := 3
	text := renderRelationEvidence([]knowledge.RelationCitation{{
		RelationType: "欺凌", IsDirected: true,
		SubjectName: "赵太爷", ObjectName: "阿Q", ChapterNumber: &ch,
		Quote: "赵太爷跳过去给了他一个嘴巴",
	}})
	for _, want := range []string{"欺凌", "赵太爷", "阿Q", "第3章", "跳过去给了他一个嘴巴"} {
		if !strings.Contains(text, want) {
			t.Errorf("证据文本里缺少 %q：%q", want, text)
		}
	}
}

// TestTruncationIsDisclosed——⭐ 截断必须告诉模型。
// ⚠️ 不说的话，模型会把手上这几条当成全部，答出一个"他们之间只有这些关系"
// 的结论——而实际上还有更多没放进来。
func TestTruncationIsDisclosed(t *testing.T) {
	ch := 1
	with := renderRelationContext(knowledge.RelationAnswer{
		Outcome:   knowledge.RelationOutcomeFound,
		Truncated: true,
		Citations: []knowledge.RelationCitation{{
			RelationType: "冲突", SubjectName: "甲", ObjectName: "乙",
			ChapterNumber: &ch, Quote: "吵了一架"}},
	})
	without := renderRelationContext(knowledge.RelationAnswer{
		Outcome: knowledge.RelationOutcomeFound,
		Citations: []knowledge.RelationCitation{{
			RelationType: "冲突", SubjectName: "甲", ObjectName: "乙",
			ChapterNumber: &ch, Quote: "吵了一架"}},
	})
	if with == without {
		t.Error("截断与未截断给模型的文本完全相同——模型会把手上这几条当成全部")
	}
}

// TestRelationContextStatesFactsNotInstructions——⭐ 送给模型的文本
// **只陈述事实，不给行为指令**。
//
// ⚠️ 这是 009 已经确立的口径（见 conversation/context.go 的 ⛔ 注释）：
// 给指令等于主动诱导对冲，而本仓库没有能力发现它已经发生。
// 这里同样适用——"请谨慎回答""请说明你不确定"一律不写。
func TestRelationContextStatesFactsNotInstructions(t *testing.T) {
	ch := 1
	for _, ans := range []knowledge.RelationAnswer{
		{Outcome: knowledge.RelationOutcomeFound, Truncated: true,
			Citations: []knowledge.RelationCitation{{
				RelationType: "冲突", SubjectName: "甲", ObjectName: "乙",
				ChapterNumber: &ch, Quote: "吵了一架"}}},
		{Outcome: knowledge.RelationOutcomeFound,
			Citations: []knowledge.RelationCitation{{
				RelationType: "冲突", SubjectName: "甲", ObjectName: "乙",
				ChapterNumber: &ch, Quote: "吵了一架"}}},
	} {
		text := renderRelationContext(ans)
		for _, forbidden := range []string{"请谨慎", "请说明", "请勿", "不要编造", "请注意"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("给模型的文本里出现了行为指令 %q：%q", forbidden, text)
			}
		}
	}
}
