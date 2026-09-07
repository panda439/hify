package knowledge

import (
	"strings"
	"time"

	"hify/internal/platform/apperr"
)

// extraction_limits.go 是单次调用的硬限与输入裁剪（010 T019）。
//
// ⭐ 三重预算各自独立，任一耗尽都停，因为它们防的是不同的失控：
//   - item 上限：一份太大的文档，在**初始化时**就拒绝（见 extraction.go），
//     不该先跑掉几百次调用再停——那些钱白花了；
//   - 调用上限：重试导致的调用膨胀；
//   - 活跃时间上限：模型变慢导致的墙钟失控，此时**调用数可能完全正常**，
//     只看调用数的预算对它一点作用都没有。
//
// ⚠️ 活跃时间这一维事先不可知：一次调用要花多久，只有它结束了才知道。
// 所以它是一道**闸门**（超了就不再开始新调用），不是预留式核算。
// 可能的超支上界是精确的：**一次调用的时长上限**，即 extractionCallTimeout。
// 这个边界要写进报告，不能含糊成"预算是 7200 秒"。

const (
	// extractionCallTimeout 是单次调用的墙钟上限（宪法第 V 条）。
	// ⚠️ 它必须显著小于 extractionLeaseTTL，否则一次正常的慢调用会让
	// 租约过期被别人抢走，而那次调用已经发出去了。
	extractionCallTimeout = 60 * time.Second

	// maxOutputTokens 限制单次输出。抽取产出的是结构化 JSON，
	// 2048 token 足够装下契约允许的最大结果（32 mention + 64 relation）。
	maxOutputTokens = 2048

	// maxInputRunes 限制单次输入。按 rune 不按 token：token 数依赖分词器，
	// 换个模型就变，而 rune 数是确定的——同一份语料在任何模型下算出的
	// 输入规模都一样，账目才能横向比较。
	maxInputRunes = 12000
)

// ErrExtractionInputTooLarge：输入超限且无法通过删候选压下来。
//
// ⭐ 这时让**这个 item 失败**，绝不悄悄截断正文。截断正文的后果不是
// "少抽几条关系"，是**引用不可核验**：模型可能引用一句话，而那句话落在
// 被我们剪掉的那一段里，于是原文里找不到它——看起来就像模型编造了引用。
var ErrExtractionInputTooLarge = apperr.InvalidInput(
	"knowledge.extraction_input_too_large",
	"该片段的正文过长，超出单次抽取的输入上限")

// ErrExtractionActiveTimeExhausted：活跃时间预算用尽。
//
// ⚠️ 与调用额度分开是有用的：两者的下一步不同。调用额度用尽可以追加额度
// 继续；活跃时间用尽说明模型变慢了，追加时间未必解决问题。
var ErrExtractionActiveTimeExhausted = apperr.Conflict(
	"knowledge.extraction_active_time_exhausted",
	"本次抽取的活跃时间已用尽，模型响应可能变慢了")

// fitExtractionInput 拼抽取阶段的输入并校验长度。
// 抽取阶段没有可删的东西——要么装得下，要么这个 item 失败。
func fitExtractionInput(instruction, chunk string) (string, error) {
	rendered := instruction + "\n\n" + chunk
	if len([]rune(rendered)) > maxInputRunes {
		return "", ErrExtractionInputTooLarge
	}
	return rendered, nil
}

// fitAliasInput 拼归一阶段的输入，超限时按排名**从尾部删候选人物**。
//
// ⭐ 方向不可反：绝不动当前正文。删候选只是少了几个可以链接的对象，
// 模型会退回"新建人物"——那是一个**保守且可见**的降级（人物碎片化会
// 体现在指标里），不是一个静默的错误。而剪正文会让引用无法核验。
//
// 返回被删掉的候选数量，调用方据此在响应里标 candidate_truncated——
// ⚠️ 不标的话，一次因为候选被删而没能归一的结果，会被读成模型判断失误。
func fitAliasInput(instruction, chunk string, mentions, candidates []string) (string, int, error) {
	var prefix strings.Builder
	prefix.WriteString(instruction)
	prefix.WriteString("\n\n正文：\n")
	prefix.WriteString(chunk)
	prefix.WriteString("\n\n当前片段称呼：")
	for _, mention := range mentions {
		prefix.WriteString("\n")
		prefix.WriteString(mention)
	}
	prefix.WriteString("\n\n已有候选人物：")
	base := prefix.String()
	baseLen := len([]rune(base))
	if baseLen > maxInputRunes {
		// 正文加固定指令本身就超限：这个 item 失败。
		// ⚠️ 不能靠"那就把正文也截一点"来兜底，理由见上。
		return "", 0, ErrExtractionInputTooLarge
	}

	kept := len(candidates)
	for kept > 0 {
		if baseLen+renderedCandidateLen(candidates[:kept]) <= maxInputRunes {
			break
		}
		kept--
	}
	var sb strings.Builder
	sb.WriteString(base)
	for _, c := range candidates[:kept] {
		sb.WriteString("\n")
		sb.WriteString(c)
	}
	return sb.String(), len(candidates) - kept, nil
}

func renderedCandidateLen(candidates []string) int {
	n := 0
	for _, c := range candidates {
		n += len([]rune(c)) + 1 // 每个候选前面一个换行
	}
	return n
}
