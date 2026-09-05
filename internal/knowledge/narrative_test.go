package knowledge

import (
	"strconv"
	"strings"
	"testing"
)

// narrative_test.go 守 010 US1 的纯函数切分（FR-001/002/004/008）。
//
// ⚠️ 这里的夹具**大部分是构造的，不是节选的**，这不是图省事：
// 手上两份公版语料（西游记 100 回、阿Q正传 9 章）里
// **只含分隔符的行数都是 0**——显式场景分隔线这条路径真实语料一次也走不到。
// 真实语料能覆盖的只有「章节标题」这一条，剩下的（分隔符、缺章、从中间章开始、
// 楔子）不构造就等于没验。同样的判断在长段降级那边也成立。

// ---- 章节号解析：真实的书里连数字写法都不统一 ----

func TestChineseNumeralRealForms(t *testing.T) {
	// ⭐ 前四行全部取自 Project Gutenberg 版《西遊記》真实标题，
	// 同一本书里同时存在「第十四回」和「第一五回」两种写法。
	cases := map[string]int{
		"一": 1, "十四": 14, "一三": 13, "二○": 20,
		"〇":  0, // U+3007 单独出现是 0，即「无法确定」，不是第 0 章
		"九九": 99, "一〇〇": 100, "一○○": 100,
		"十": 10, "二十": 20, "二十一": 21, "三十": 30,
		"一百": 100, "一百零八": 108, "百": 100,
		"7": 7, "007": 7, "100": 100,
		"": 0, "abc": 0, "第": 0,
	}
	for in, want := range cases {
		if got := chineseNumeral(in); got != want {
			t.Errorf("chineseNumeral(%q) = %d, want %d", in, got, want)
		}
	}
}

// TestChapterHeadingKnowsBothZeroGlyphs 是一次真实事故的回归。
//
// ⚠️ 第一版的字符类只认 `〇`(U+3007)，不认 `○`(U+25CB)，于是在《西遊記》
// 全书 100 个标题里**静默漏掉 10 个**——90 个被找到，不报错、不告警。
// 章节号是每条关系记录挂载的时间坐标（FR-008），这种漏法会毒化下游全部数据，
// 而表面上一切正常。所以这条断言盯的不是「能解析数字」，是「两个零都认识」。
func TestChapterHeadingKnowsBothZeroGlyphs(t *testing.T) {
	for _, zero := range []string{"〇", "○"} {
		text := "第二" + zero + "回　题目\n正文。\n"
		marks := findChapterMarks(text)
		if len(marks) != 1 {
			t.Fatalf("零字形 %q：找到 %d 个标题，want 1", zero, len(marks))
		}
		if marks[0].number != 20 {
			t.Errorf("零字形 %q：章节号 = %d, want 20", zero, marks[0].number)
		}
	}
}

// ---- 场景切分 ----

func TestSplitOnExplicitDivider(t *testing.T) {
	// 构造：真实语料里没有分隔线（见文件头注释）。
	text := "第一章　开端\n场景甲的正文。\n\n* * *\n\n场景乙的正文。\n"
	units := splitNarrativeScenes(text)
	if len(units) != 2 {
		t.Fatalf("场景数 = %d, want 2：%+v", len(units), units)
	}
	for i, u := range units {
		if u.ChapterNumber != 1 {
			t.Errorf("场景 %d 的章节号 = %d, want 1", i, u.ChapterNumber)
		}
		if u.SceneIndex != i+1 {
			t.Errorf("场景 %d 的 SceneIndex = %d, want %d", i, u.SceneIndex, i+1)
		}
	}
	if !strings.Contains(units[0].Text, "场景甲") || strings.Contains(units[0].Text, "场景乙") {
		t.Errorf("第一个场景串味了：%q", units[0].Text)
	}
	// ⚠️ 分隔线本身是标记，不是正文，不得留在任何场景里。
	for _, u := range units {
		if strings.Contains(u.Text, "* * *") {
			t.Errorf("分隔线漏进了正文：%q", u.Text)
		}
	}
}

// TestDividerMustBeTheWholeLine——一行里**出现**星号是行文，一行**只有**星号才是分隔。
// 放松成「包含」会把对白密集的正文切成碎片。
func TestDividerMustBeTheWholeLine(t *testing.T) {
	text := "第一章\n他说：「这是 *** 重点」，然后走了。\n下一句。\n"
	units := splitNarrativeScenes(text)
	if len(units) != 1 {
		t.Fatalf("行内星号被当成了分隔线：切出 %d 个场景", len(units))
	}
}

func TestNoChapterStructureDegradesNotFabricates(t *testing.T) {
	// FR-004：认不出结构就留空，绝不编一个章节号出来。
	text := "一段没有任何标题的散文。\n\n又一段。\n"
	units := splitNarrativeScenes(text)
	if len(units) != 1 {
		t.Fatalf("场景数 = %d, want 1", len(units))
	}
	if units[0].ChapterNumber != 0 {
		t.Errorf("无结构文本被安上了章节号 %d——0 表示未知，不是第 0 章", units[0].ChapterNumber)
	}
}

func TestPrologueGetsNoFabricatedNumber(t *testing.T) {
	text := "楔子\n楔子的正文。\n第一章　开端\n第一章正文。\n"
	units := splitNarrativeScenes(text)
	if len(units) != 2 {
		t.Fatalf("场景数 = %d, want 2：%+v", len(units), units)
	}
	if units[0].ChapterNumber != 0 {
		t.Errorf("楔子被编了章节号 %d", units[0].ChapterNumber)
	}
	if units[0].ChapterTitle != "楔子" {
		t.Errorf("楔子标题 = %q", units[0].ChapterTitle)
	}
	if units[1].ChapterNumber != 1 {
		t.Errorf("楔子后的第一章号 = %d, want 1", units[1].ChapterNumber)
	}
}

// TestLongParagraphOpeningLikeAHeadingIsProse——变异测试逼出来的缺口。
// 「分隔符必须存在」这条挡住了 `第一章正文。`，但挡不住
// `第一章　那时候他……`（有分隔符、后面接一整段行文）。真实语料里没有这种段落，
// 所以只有构造夹具能覆盖；漏了它的表现同样是**切错**而不是标错。
func TestLongParagraphOpeningLikeAHeadingIsProse(t *testing.T) {
	long := "第三回　" + strings.Repeat("那时候他还不知道后来会发生什么，", 8)
	text := "第一章　开端\n" + long + "\n"
	units := splitNarrativeScenes(text)
	if len(units) != 1 {
		t.Fatalf("一整段行文被当成了章节标题：切出 %d 个场景：%+v", len(units), units)
	}
	if units[0].ChapterNumber != 1 {
		t.Errorf("章节号 = %d, want 1（不该被段落里的「第三回」改写）", units[0].ChapterNumber)
	}
}

// TestExcerptStartingMidBook——用户上传的可能就是从第 11 章开始的节选。
// 章节号必须照原文记 11，不能重编成 1；连续性自检也不该因此判假。
func TestExcerptStartingMidBook(t *testing.T) {
	text := "第十一章　甲\n甲正文。\n第十二章　乙\n乙正文。\n"
	marks := findChapterMarks(text)
	if len(marks) != 2 || marks[0].number != 11 || marks[1].number != 12 {
		t.Fatalf("节选章节号解析错了：%+v", marks)
	}
	if !chapterNumbersConsecutive(marks) {
		t.Error("11、12 是连续的，自检不该判假——否则每份节选都会误报")
	}
}

// TestGapWarnsButDiscardsNothing——⭐ 缺章只报告，绝不丢弃。
// 因为一个缺口就扔掉全部已识别的标题，会把「部分已知的结构」变成「完全未知」，
// 严格更糟。自检存在的意义只是让调用方能记一条日志：
// 它防的那个故障（数字写法不认识 → 静默漏标题）本身不产生任何错误。
func TestGapWarnsButDiscardsNothing(t *testing.T) {
	text := "第一章\n甲。\n第五章\n乙。\n"
	marks := findChapterMarks(text)
	if len(marks) != 2 {
		t.Fatalf("缺章导致标题被丢弃了：只剩 %d 个", len(marks))
	}
	if chapterNumbersConsecutive(marks) {
		t.Error("1 到 5 有缺口，自检该判假")
	}
	units := splitNarrativeScenes(text)
	if len(units) != 2 || units[1].ChapterNumber != 5 {
		t.Errorf("缺章后切分结果不对：%+v", units)
	}
}

func TestTextBeforeFirstHeadingIsKept(t *testing.T) {
	// 丢掉开头不会报错，只会静默少一段正文——所以要有断言盯着。
	text := "这是标题之前的引言。\n第一章\n正文。\n"
	units := splitNarrativeScenes(text)
	if len(units) != 2 {
		t.Fatalf("场景数 = %d, want 2", len(units))
	}
	if !strings.Contains(units[0].Text, "引言") {
		t.Errorf("首个标题之前的正文被丢了：%+v", units)
	}
	if units[0].ChapterNumber != 0 {
		t.Errorf("引言被安上了章节号 %d", units[0].ChapterNumber)
	}
}

// TestNoContentLost 是本文件里最值钱的一条：
// 除分隔线（标记，不是正文）外，输入的每一个非空白字符都必须出现在输出里，顺序不变。
// 上面每条用例只看自己关心的那一小块，只有这条能抓住「某一段整体消失」。
func TestNoContentLost(t *testing.T) {
	text := "引言。\n楔子\n楔子正文。\n第一章　甲\n甲一。\n\n···\n\n甲二。\n第二章　乙\n乙正文。\n"
	var want strings.Builder
	for _, ln := range strings.Split(text, "\n") {
		if sceneDividerPattern.MatchString(ln) {
			continue
		}
		want.WriteString(strings.Join(strings.Fields(ln), ""))
	}
	var got strings.Builder
	for _, u := range splitNarrativeScenes(text) {
		got.WriteString(strings.Join(strings.Fields(u.Text), ""))
	}
	if got.String() != want.String() {
		t.Errorf("正文有增删或错序：\n got=%q\nwant=%q", got.String(), want.String())
	}
}

func TestSplitIsDeterministic(t *testing.T) {
	text := "第一章\n甲。\n---\n乙。\n第二章\n丙。\n"
	first := splitNarrativeScenes(text)
	for i := 0; i < 20; i++ {
		again := splitNarrativeScenes(text)
		if len(again) != len(first) {
			t.Fatalf("第 %d 次场景数不同", i)
		}
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("第 %d 次的场景 %d 与首次不同：%+v vs %+v", i, j, again[j], first[j])
			}
		}
	}
}

// ---- 按场景分块（chunkNarrative，FR-002/003/008）----

// TestNarrativeChunksNeverExceedSize——最硬的一条不变量。
// 场景完整性是偏好，长度上限是**规则**：任何一块超限都会伤到 embedding 质量。
func TestNarrativeChunksNeverExceedSize(t *testing.T) {
	text := "第一章　甲\n" + strings.Repeat("这是一段很长的正文，没有任何标点会让它自然断开", 60) + "\n" +
		"第二章　乙\n" + strings.Repeat("短句。", 200) + "\n"
	for _, size := range []int{50, 120, 500} {
		for _, piece := range chunkNarrative(text, size, size/5) {
			if n := len([]rune(piece.Content)); n > size {
				t.Errorf("size=%d 时产出了 %d 字的块：%.40q", size, n, piece.Content)
			}
		}
	}
}

// TestOverlapDoesNotCrossScenes——⭐ 本期最核心的那条。
// overlap 的作用是让**同一个场景**被切开的两半还能互相衔接；
// 如果它跨场景渗透，就等于在块这一层重新缝上了本功能要拆掉的那道缝，
// 而且这种污染不报错——只会让检索结果里混进另一个场景的尾巴。
func TestOverlapDoesNotCrossScenes(t *testing.T) {
	const marker = "甲场景的独有暗号"
	text := "第一章\n" + strings.Repeat("甲的正文。", 40) + marker + "。\n" +
		"第二章\n" + strings.Repeat("乙的正文。", 40) + "\n"
	pieces := chunkNarrative(text, 100, 40)
	for _, p := range pieces {
		if p.ChapterNumber != nil && *p.ChapterNumber == 2 && strings.Contains(p.Content, marker) {
			t.Fatalf("第一章的尾巴渗进了第二章的块：%q", p.Content)
		}
	}
}

func TestNarrativeChunkCarriesChapterMetadata(t *testing.T) {
	text := "引言。\n第七章　标题甲\n甲的正文。\n第八章　标题乙\n乙的正文。\n"
	pieces := chunkNarrative(text, 500, 50)
	if len(pieces) != 3 {
		t.Fatalf("块数 = %d, want 3：%+v", len(pieces), pieces)
	}
	// FR-008：认不出章节就留空，不得拿块序号冒充章节号。
	if pieces[0].ChapterNumber != nil {
		t.Errorf("引言被安上了章节号 %d", *pieces[0].ChapterNumber)
	}
	if pieces[1].ChapterNumber == nil || *pieces[1].ChapterNumber != 7 {
		t.Errorf("第七章的块章节号不对：%v", pieces[1].ChapterNumber)
	}
	if pieces[1].SectionTitle == nil || *pieces[1].SectionTitle != "标题甲" {
		t.Errorf("章节标题没带上：%v", pieces[1].SectionTitle)
	}
	if pieces[2].ChapterNumber == nil || *pieces[2].ChapterNumber != 8 {
		t.Errorf("第八章的块章节号不对：%v", pieces[2].ChapterNumber)
	}
}

// TestSceneKeysAreUnique 是一次实现 bug 的回归：
// scene_key 曾经直接用 narrativeUnit.SceneIndex，而它在每次 scenesWithin
// 调用里都从 1 重新计数——「未知章节」这一组是由**多次**调用产出的
// （开头的节选正文、楔子……），于是它们全都叫 ch0-s1。
// ⚠️ 重复的 key 不会让任何东西报错，只会在下游把两个不同场景悄悄并成一个。
func TestSceneKeysAreUnique(t *testing.T) {
	text := "引言的正文。\n楔子\n楔子的正文。\n序章\n序章的正文。\n" +
		"第一章\n甲一。\n***\n甲二。\n第二章\n乙。\n"
	seen := map[string]string{}
	for _, p := range chunkNarrative(text, 500, 0) {
		if p.SceneKey == nil {
			t.Fatalf("块没有 scene_key：%q", p.Content)
		}
		if prev, dup := seen[*p.SceneKey]; dup && prev != p.Content {
			t.Errorf("scene_key %q 被两个不同场景共用：%q / %q", *p.SceneKey, prev, p.Content)
		}
		seen[*p.SceneKey] = p.Content
	}
	if len(seen) != 6 {
		t.Errorf("不同 scene_key 数 = %d, want 6（引言/楔子/序章/一章两场景/二章）：%v", len(seen), seen)
	}
}

// TestNarrativeIsDeterministic——chunkNarrative 内部用了 map 做计数器，
// 这条盯着它没有被误用成迭代顺序（宪法第 V 条）。
func TestNarrativeIsDeterministic(t *testing.T) {
	text := "引言。\n楔子\n甲。\n第一章\n" + strings.Repeat("正文。", 80) + "\n***\n乙。\n第二章\n丙。\n"
	first := chunkNarrative(text, 120, 30)
	render := func(ps []chunkPiece) string {
		var sb strings.Builder
		for _, p := range ps {
			sb.WriteString(p.Content)
			sb.WriteString("|")
			if p.ChapterNumber != nil {
				sb.WriteString(strconv.Itoa(*p.ChapterNumber))
			}
			sb.WriteString("|")
			if p.SceneKey != nil {
				sb.WriteString(*p.SceneKey)
			}
			sb.WriteString("\n")
		}
		return sb.String()
	}
	want := render(first)
	for i := 0; i < 20; i++ {
		if got := render(chunkNarrative(text, 120, 30)); got != want {
			t.Fatalf("第 %d 次分块结果与首次不同", i)
		}
	}
}

// ---- 原文来源区间（FR-006）----

func stripWS(s string) string { return strings.Join(strings.Fields(s), "") }

// TestSourceIntervalsPointAtRealText——区间必须真的能切回原文。
// 断言用「后缀」而不是「相等」：带 overlap 种子的块内容里多了一段来自
// **前一块**的文字，而区间**故意不覆盖它**（种子的出处算在它原本那一块上，
// 重复计入会让每个区间都虚胖出邻居的一份）。
func TestSourceIntervalsPointAtRealText(t *testing.T) {
	text := "引言。\n第一章　甲\n" + strings.Repeat("甲的正文。", 60) + "\n\n" +
		strings.Repeat("另一段甲。", 60) + "\n第二章　乙\n乙的正文。\n"
	for _, overlap := range []int{0, 40} {
		for _, p := range chunkNarrative(text, 120, overlap) {
			if p.SourceStart == nil || p.SourceEnd == nil {
				t.Fatalf("块没有来源区间：%.20q", p.Content)
			}
			if *p.SourceStart < 0 || *p.SourceEnd > len(text) || *p.SourceStart >= *p.SourceEnd {
				t.Fatalf("区间越界或为空：[%d,%d) len=%d", *p.SourceStart, *p.SourceEnd, len(text))
			}
			src := stripWS(text[*p.SourceStart:*p.SourceEnd])
			if !strings.HasSuffix(stripWS(p.Content), src) {
				t.Errorf("overlap=%d 区间 [%d,%d) 切出来的原文对不上块内容：\n src=%.60q\n got=%.60q",
					overlap, *p.SourceStart, *p.SourceEnd, src, stripWS(p.Content))
			}
		}
	}
}

// TestRepeatedParagraphGetsDistinctIntervals——⭐ 这条盯的是整个偏移改造的动机。
// 同一段文字在书里出现两次（叙事文本里极常见：重复的口头禅、章回体的套语），
// 如果来源位置是事后拿 strings.Index 搜出来的，两次都会指向**第一次出现的位置**。
// 那样得到的引用看起来完全正常——章节号、页码、文字全对——只是位置是错的，
// 而且错得没有任何迹象。所以偏移必须在切分过程中一路带下来，不能事后回搜。
func TestRepeatedParagraphGetsDistinctIntervals(t *testing.T) {
	const repeated = "话说天下大势，分久必合，合久必分。"
	text := "第一章　甲\n" + repeated + "\n第二章　乙\n" + repeated + "\n"
	var hits []chunkPiece
	for _, p := range chunkNarrative(text, 500, 0) {
		if strings.Contains(p.Content, repeated) {
			hits = append(hits, p)
		}
	}
	if len(hits) != 2 {
		t.Fatalf("重复段落命中 %d 次, want 2", len(hits))
	}
	if *hits[0].SourceStart == *hits[1].SourceStart {
		t.Fatalf("两次出现被指到了同一个位置 %d——这正是事后 substring 回搜的症状",
			*hits[0].SourceStart)
	}
	for i, h := range hits {
		if got := stripWS(text[*h.SourceStart:*h.SourceEnd]); got != stripWS(h.Content) {
			t.Errorf("第 %d 次出现的区间切出来是 %.40q", i+1, got)
		}
	}
	// 第二次出现必须在第一次之后，而不是被折叠回去。
	if *hits[1].SourceStart <= *hits[0].SourceStart {
		t.Errorf("第二次出现的位置 %d 不在第一次 %d 之后", *hits[1].SourceStart, *hits[0].SourceStart)
	}
}

// TestSourceIntervalsSurviveCRLF——区间索引的是 CRLF 归一化后的文本。
// 归一化点只有一个（chunkNarrative 入口），这条确认没有哪一层偷偷再归一化一次，
// 否则两套坐标混用，区间照样"看起来合理"。
func TestSourceIntervalsSurviveCRLF(t *testing.T) {
	raw := "第一章　甲\r\n甲的正文。\r\n\r\n第二段。\r\n"
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	for _, p := range chunkNarrative(raw, 500, 0) {
		if got := stripWS(normalized[*p.SourceStart:*p.SourceEnd]); !strings.HasSuffix(stripWS(p.Content), got) {
			t.Errorf("CRLF 输入下区间对不上：src=%q content=%q", got, stripWS(p.Content))
		}
	}
}

// TestCharacterLevelFallbackKeepsCorrectOffsets——变异测试逼出来的第二个缺口。
// 降级阶梯的最后一级（无标点长句 → 按 rune 硬切）在两份真实语料里**一次也走不到**：
// 每个场景都有充足的句读。所以「硬切出来的块，来源位置有没有平移回文档坐标」
// 这件事只能构造。漏了它的表现是：所有硬切块都声称自己来自句子开头，
// 章节号、文字全对，只有位置错——而且错得很像真的。
func TestCharacterLevelFallbackKeepsCorrectOffsets(t *testing.T) {
	blob := strings.Repeat("无标点长串", 60) // 300 字，没有任何句末标点
	// ⚠️ blob 前面必须先有一句带标点的正文。第一版夹具让 blob 成了场景里的
	// 第一个句子，sentence.Start 本来就是 0——平移与不平移产出完全一样，
	// 断言看着在跑，实际什么都没验（变异逃逸就是这么发现的）。
	text := "第一章　甲\n甲的正文。\n第二章　乙\n先有一句带标点的话。" + blob + "\n"
	var fallback int
	for _, p := range chunkNarrative(text, 100, 0) {
		if !strings.Contains(p.Content, "无标点长串") {
			continue
		}
		fallback++
		if got := stripWS(text[*p.SourceStart:*p.SourceEnd]); got != stripWS(p.Content) {
			t.Errorf("硬切块的区间 [%d,%d) 切出来是 %.30q，块内容是 %.30q",
				*p.SourceStart, *p.SourceEnd, got, stripWS(p.Content))
		}
		if p.ChapterNumber == nil || *p.ChapterNumber != 2 {
			t.Errorf("硬切块的章节号 = %v, want 2", p.ChapterNumber)
		}
	}
	if fallback < 3 {
		// ⚠️ 夹具本身也要验：走不到字符级降级的话，上面的断言等于没跑。
		t.Fatalf("只产生了 %d 个硬切块，夹具没有真正触发字符级降级", fallback)
	}
}
