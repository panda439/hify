package knowledge

import (
	"errors"
	"strings"
	"testing"
	"time"

	"hify/internal/provider"
)

// extraction_budget_test.go 守三重预算与单次调用的硬限（010 T019）。
//
// ⭐ 三个维度各自独立，任一耗尽都停，因为它们防的是不同的失控：
//   - item 上限（500）：一份太大的文档，**在初始化时就拒绝**，
//     不该先跑掉几百次调用再停——那些钱白花了；
//   - 调用上限（3000）：重试导致的调用膨胀；
//   - 活跃时间上限（7200s）：模型变慢导致的墙钟失控，调用数可能完全正常。
//
// ⚠️ 活跃时间这一维有一个**事先不可知**的性质：一次调用要花多久，
// 只有它结束了才知道。所以它是一道**闸门**（超了就不再开始新调用），
// 不是预留式核算。可能的超支上界是精确的：一次调用的时长上限。
// 这个边界必须写进报告，不能含糊成"预算是 7200 秒"。

// TestItemLimitIsRejectedAtInitialization——⭐ 在初始化时拒绝，
// 不是跑到一半才停。跑到一半停的话，已经花掉的调用换不回任何完整结果。
func TestItemLimitIsRejectedAtInitialization(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedNarrativeDocument(t, repo, "doc-items", 6)

	spec := newExtractionJobSpec("job-items", "doc-items", 1, "m-1")
	spec.ApprovedItemLimit = 5
	if _, err := repo.initializeExtractionJob(ctx, spec); !errors.Is(err, ErrExtractionTooManyItems) {
		t.Fatalf("err = %v, want ErrExtractionTooManyItems", err)
	}
	var n int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM relation_extraction_jobs WHERE id='job-items'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("超限却建出了作业")
	}
	// 恰好等于上限必须允许。
	spec2 := newExtractionJobSpec("job-items-ok", "doc-items", 1, "m-1")
	spec2.ApprovedItemLimit = 6
	if _, err := repo.initializeExtractionJob(ctx, spec2); err != nil {
		t.Errorf("恰好等于上限被拒了：%v", err)
	}
}

// TestActiveTimeBudgetStopsNewCalls——⭐ 活跃时间超了就不再开始新调用。
//
// ⚠️ 它拦的是"模型变慢"这条失控路径，而那条路径上**调用数可能完全正常**——
// 只看调用数的预算对它一点作用都没有。
func TestActiveTimeBudgetStopsNewCalls(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedNarrativeDocument(t, repo, "doc-time", 2)
	spec := newExtractionJobSpec("job-time", "doc-time", 1, "m-1")
	spec.ActiveMsLimit = 1000
	job, err := repo.initializeExtractionJob(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	epoch, _, err := repo.claimExtractionJob(ctx, job.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	item := firstItemID(t, repo, job.ID)

	// 第一次调用就把活跃时间用超。
	att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
		JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.settleExtractionAttempt(ctx, att, provider.ChatAttemptResult{
		Outcome: provider.AttemptCompleted, Dispatched: true, ElapsedMs: 1500,
		Message: provider.Message{Content: "{}"},
	}); err != nil {
		t.Fatal(err)
	}
	// 下一次预留必须被拦下。
	_, err = repo.reserveExtractionAttempt(ctx, attemptReservation{
		JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	})
	if !errors.Is(err, ErrExtractionActiveTimeExhausted) {
		t.Fatalf("err = %v, want ErrExtractionActiveTimeExhausted", err)
	}
}

// TestPerCallLimitsAreFixed——单次调用的三个硬限必须是常量，
// 不能由调用方随手传。⚠️ 它们是宪法第 V 条要求的"超时上限"的落点。
func TestPerCallLimitsAreFixed(t *testing.T) {
	if extractionCallTimeout != 60*time.Second {
		t.Errorf("单次调用超时 = %v, want 60s", extractionCallTimeout)
	}
	if maxOutputTokens != 2048 {
		t.Errorf("输出上限 = %d, want 2048", maxOutputTokens)
	}
	if maxInputRunes != 12000 {
		t.Errorf("输入上限 = %d rune, want 12000", maxInputRunes)
	}
	if maxRawResponseBytes != 64*1024 {
		t.Errorf("响应上限 = %d, want 65536", maxRawResponseBytes)
	}
	// ⚠️ 单次超时必须显著小于租约 TTL，否则一次正常的慢调用会让租约过期
	// 被别人抢走，而那次调用已经发出去了。
	if extractionCallTimeout*2 > extractionLeaseTTL {
		t.Errorf("单次超时 %v 相对租约 %v 太长", extractionCallTimeout, extractionLeaseTTL)
	}
}

// TestExtractionInputOverLimitFailsTheItem——⭐ 抽取阶段的输入超限
// **让这个 item 失败，绝不悄悄截断正文**。
//
// ⚠️ 截断正文的后果不是"少抽几条关系"，是**引用不可核验**：模型可能引用
// 一句话，而那句话落在被我们剪掉的那一段里，于是原文里找不到它。
// 那看起来就像模型编造了引用。
func TestExtractionInputOverLimitFailsTheItem(t *testing.T) {
	fits, err := fitExtractionInput(strings.Repeat("正", 100), strings.Repeat("文", 500))
	if err != nil {
		t.Fatalf("正常长度被拒了：%v", err)
	}
	if !strings.Contains(fits, strings.Repeat("文", 500)) {
		t.Error("正文没有完整进入输入")
	}
	if _, err := fitExtractionInput("指令", strings.Repeat("文", maxInputRunes)); !errors.Is(err, ErrExtractionInputTooLarge) {
		t.Fatalf("err = %v, want ErrExtractionInputTooLarge", err)
	}
}

// TestAliasInputDropsCandidatesNotTheChunk——⭐ 归一阶段超限时，
// 按排名**从尾部删候选人物**，绝不动当前正文。
//
// ⚠️ 方向反了的后果同上：正文被剪，模型引用的句子在原文里找不到。
// 而候选删掉只是少了几个可以链接的对象，模型会退回"新建人物"——
// 那是一个**保守且可见**的降级（人物碎片化会体现在指标里），
// 不是一个静默的错误。
func TestAliasInputDropsCandidatesNotTheChunk(t *testing.T) {
	chunk := strings.Repeat("正", 4000)
	// 每个候选 1000 字，塞 20 个，肯定超限。
	var cands []string
	for i := 0; i < 20; i++ {
		cands = append(cands, strings.Repeat("候", 1000))
	}
	rendered, dropped, err := fitAliasInput("指令", chunk, nil, cands)
	if err != nil {
		t.Fatalf("fitAliasInput: %v", err)
	}
	if dropped == 0 {
		t.Fatal("夹具没有触发候选删减，这条断言等于没验")
	}
	if !strings.Contains(rendered, chunk) {
		t.Error("当前正文被截断了——模型引用的句子将无法在原文中核验")
	}
	if n := len([]rune(rendered)); n > maxInputRunes {
		t.Errorf("删减之后仍然超限：%d rune", n)
	}
	// 删的必须是**尾部**（排名靠后的候选），保留的是靠前的。
	if strings.Count(rendered, strings.Repeat("候", 1000)) != 20-dropped {
		t.Errorf("保留的候选数对不上：dropped=%d", dropped)
	}
}

// TestAliasInputFailsWhenChunkAloneIsTooLarge——正文加固定指令本身就超限时，
// 这个 item 失败。⚠️ 不能靠"那就把正文也截一点"来兜底，理由同上。
func TestAliasInputFailsWhenChunkAloneIsTooLarge(t *testing.T) {
	huge := strings.Repeat("正", maxInputRunes)
	if _, _, err := fitAliasInput("指令", huge, nil, []string{"候选"}); !errors.Is(err, ErrExtractionInputTooLarge) {
		t.Fatalf("err = %v, want ErrExtractionInputTooLarge", err)
	}
}

// TestAliasInputKeepsEverythingWhenItFits——没超限时一个候选都不该删。
// ⚠️ 无谓地删候选会让人物归一变差，而那会体现成"误归一率低但碎片化高"，
// 很容易被读成模型能力问题。
func TestAliasInputKeepsEverythingWhenItFits(t *testing.T) {
	rendered, dropped, err := fitAliasInput("指令", "短正文", nil, []string{"甲", "乙", "丙"})
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 0 {
		t.Errorf("没超限却删了 %d 个候选", dropped)
	}
	for _, c := range []string{"甲", "乙", "丙"} {
		if !strings.Contains(rendered, c) {
			t.Errorf("候选 %q 丢了", c)
		}
	}
}

func TestFitAliasInputIncludesCurrentMentionRefsBeforeCandidates(t *testing.T) {
	rendered, dropped, err := fitAliasInput("指令", "阿Q走进酒店。", []string{"m1\t阿Q"}, []string{"char-uuid\t阿Q"})
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0", dropped)
	}
	if !strings.Contains(rendered, "当前片段称呼：\nm1\t阿Q") {
		t.Fatalf("alias input omitted current mention ref: %q", rendered)
	}
	if !strings.Contains(rendered, "已有候选人物：\nchar-uuid\t阿Q") {
		t.Fatalf("alias input omitted candidate: %q", rendered)
	}
	if strings.Index(rendered, "m1\t阿Q") > strings.Index(rendered, "char-uuid\t阿Q") {
		t.Fatalf("mentions must be rendered before candidates: %q", rendered)
	}
}
