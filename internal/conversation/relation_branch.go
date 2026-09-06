package conversation

import (
	"fmt"
	"strconv"
	"strings"

	"hify/internal/knowledge"
)

// relation_branch.go 是关系查询在对话里的分支（010 T033）。
//
// ⭐ 立论：**几种"查不到"必须给出不同的话**。把它们都答成"没有找到相关
// 信息"是最省事也最没用的做法——用户无法判断该等一等、该重跑、还是该
// 换个问法。
//
// ⚠️ 这些文本是**固定的，不经过模型**。这几种结局本身就是系统已经确定的
// 事实（没开启抽取、名字不认识、同名多人），交给模型复述一遍只会引入它
// 自己的措辞和不确定性——它完全可能把"还没跑完"说成"书里没有"。
//
// ⛔ 送给模型的文本里**不写行为指令**（"请谨慎回答""请说明你不确定"）。
// 这是 009 已经确立的口径，见 context.go 的同类注释：给指令等于主动诱导
// 对冲，而本仓库没有能力发现它已经发生。陈述事实，把判断留给模型。

// relationBranchText 是不需要模型参与的那几种结局的固定回复。
//
// 返回空字符串表示"这个结局需要模型来回答"（只有 found 是这样）。
func relationBranchText(ans knowledge.RelationAnswer, subject, object string) string {
	switch ans.Outcome {
	case knowledge.RelationOutcomeFound:
		return ""

	case knowledge.RelationOutcomeNoRecords:
		// ⭐ 不能说成"他们没有关系"。抽取只覆盖八类关系，而且是模型抽的；
		// "库里没有这条记录"与"书里没有这层关系"是两回事，把前者说成后者
		// 就是拿系统的局限冒充事实。
		return fmt.Sprintf(
			"在已抽取的人物关系记录里，没有找到「%s」与「%s」之间的记录。"+
				"抽取只覆盖有限的几类关系，书中未必没有其他形式的关联。", subject, object)

	case knowledge.RelationOutcomeIncomplete:
		// ⚠️ 必须说出还剩多少：不说的话用户无法判断该不该等，
		// 而"等一等"和"这书里没写"是完全不同的下一步。
		remaining := "部分"
		if ans.RemainingItems != nil {
			remaining = strconv.Itoa(*ans.RemainingItems) + " 个"
		}
		return fmt.Sprintf(
			"这份文档的关系抽取还没有完成，还有 %s片段没有处理。"+
				"在已经处理的部分里没有找到「%s」与「%s」的关系记录。",
			remaining, subject, object)

	case knowledge.RelationOutcomeNotExtracted:
		return "这份文档还没有做过人物关系抽取，因此没有可以查询的关系记录。"

	case knowledge.RelationOutcomeUnknownName:
		// ⚠️ 必须指出是哪个名字：不指出的话，用户把名字打错了
		// 却以为书里真的没写他们的关系。
		return fmt.Sprintf(
			"在已抽取的人物里没有找到「%s」这个称呼。可能是写法不同，"+
				"也可能这个人物还没有被抽取到。", ans.UnknownName)

	case knowledge.RelationOutcomeAmbiguous:
		// ⚠️ 把候选列出来，用户才有得选。只说"有多个同名的人"
		// 而不列出来，用户无法澄清。
		var sb strings.Builder
		sb.WriteString("书中有多个同名的人物，无法确定你问的是哪一位：")
		for i, c := range ans.Candidates {
			if i > 0 {
				sb.WriteString("；")
			}
			fmt.Fprintf(&sb, "「%s」（首次出现在原文第 %d 个字符处）",
				c.DisplayName, c.FirstSourceOrder)
		}
		sb.WriteString("。请补充说明是哪一位。")
		return sb.String()

	case knowledge.RelationOutcomeSameEntity:
		// ⭐ 这本身就是答案，不是"没有关系"。
		return fmt.Sprintf("「%s」与「%s」在这本书里指的是同一个人。", subject, object)

	case knowledge.RelationOutcomeOutOfScope:
		return "当前助手可访问的文档范围里没有这份资料，无法查询其中的人物关系。"

	default:
		return "关系查询没有返回可用的结果。"
	}
}

// renderRelationEvidence 把证据渲染成给模型看的文本。
func renderRelationEvidence(cites []knowledge.RelationCitation) string {
	var sb strings.Builder
	for i, c := range cites {
		if i > 0 {
			sb.WriteString("\n")
		}
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
			sb.WriteString(strconv.Itoa(*c.ChapterNumber))
			sb.WriteString("章")
		}
		sb.WriteString("）：")
		sb.WriteString(c.Quote)
	}
	return sb.String()
}

// renderRelationContext 是有证据时送进 prompt 的那一段。
//
// ⭐ 截断必须告诉模型。⚠️ 不说的话，模型会把手上这几条当成全部，
// 答出一个"他们之间只有这些关系"的结论——而实际上还有更多没放进来。
func renderRelationContext(ans knowledge.RelationAnswer) string {
	var sb strings.Builder
	sb.WriteString("<relation_records>\n")
	sb.WriteString(renderRelationEvidence(ans.Citations))
	sb.WriteString("\n")
	if ans.Truncated {
		// ⛔ 只陈述事实，不加"请说明这一点"之类的指令。
		sb.WriteString("以上不是全部记录，还有更多未列出。\n")
	}
	sb.WriteString("</relation_records>")
	return sb.String()
}
