package knowledge

import (
	"errors"
	"strings"
	"testing"
)

// extract_prompt_test.go 守抽取响应的**严格解析与校验**（010 T024）。
//
// ⭐ 这是整条链路上模型输出唯一的入口。它之后的每一层都假定拿到的是
// 干净的、能在原文里定位的数据；这里放过去的每一条脏数据，都会以
// "一条看起来正常的关系"的形式出现在最终结果里。
//
// ⚠️ 因此这里的原则是**整次拒绝，绝不部分采用**。挑出能用的那几条听起来
// 更宽容，但那样一次结构损坏的响应会产出"少了几条关系"的结果，而失败率
// 显示为 0——比直接失败更难发现，也更难解释。

const okChunk = "赵太爷跳过去给了他一个嘴巴。阿Q摸着左颊，和地保退出去了。阿Q又被赵太爷骂了一顿。"

func validResponse() string {
	return `{"mentions":[{"ref":"m1","surface":"赵太爷","occurrence":0},
	                     {"ref":"m2","surface":"阿Q","occurrence":0}],
	         "relations":[{"subject_ref":"m1","object_ref":"m2","type":"欺凌",
	                       "evidence":[{"quote":"赵太爷跳过去给了他一个嘴巴","occurrence":0}]}],
	         "alias_proposals":[]}`
}

// TestParseAcceptsAValidResponse——先确认正常路径能过，否则下面的拒绝断言
// 可能只是"什么都拒绝"。
func TestParseAcceptsAValidResponse(t *testing.T) {
	out, err := parseExtractionResponse(validResponse(), okChunk)
	if err != nil {
		t.Fatalf("合法响应被拒：%v", err)
	}
	if len(out.Mentions) != 2 || len(out.Relations) != 1 {
		t.Fatalf("解析结果不对：%+v", out)
	}
	rel := out.Relations[0]
	if rel.Type != "欺凌" || rel.SubjectRef != "m1" || rel.ObjectRef != "m2" {
		t.Errorf("关系字段不对：%+v", rel)
	}
	if len(rel.Evidence) != 1 {
		t.Fatalf("证据条数 = %d", len(rel.Evidence))
	}
	// ⭐ 引文必须被解析成**原文里的 rune 区间**，而不是只留一段字符串。
	// 只留字符串的话，下游要引用它就只能回原文搜——重复出现的句子会
	// 全部指向第一次出现的位置。
	ev := rel.Evidence[0]
	if ev.Start < 0 || ev.End <= ev.Start || ev.End > len([]rune(okChunk)) {
		t.Fatalf("证据区间不合法：[%d,%d)", ev.Start, ev.End)
	}
	if got := string([]rune(okChunk)[ev.Start:ev.End]); got != "赵太爷跳过去给了他一个嘴巴" {
		t.Errorf("证据区间取出来是 %q", got)
	}
}

// TestParseAcceptsEmptyArrays——⭐ 全空是**合法的成功结果**。
// 一个块里没有关系是正常的；判成失败会让失败率里混进一堆处理正确的块。
func TestParseAcceptsEmptyArrays(t *testing.T) {
	out, err := parseExtractionResponse(
		`{"mentions":[],"relations":[],"alias_proposals":[]}`, okChunk)
	if err != nil {
		t.Fatalf("空结果被拒：%v", err)
	}
	if len(out.Mentions) != 0 || len(out.Relations) != 0 {
		t.Errorf("空结果解析出了内容：%+v", out)
	}
}

// TestParseRejectsStructuralDamage——结构层面的拒绝。
func TestParseRejectsStructuralDamage(t *testing.T) {
	base := `{"mentions":[],"relations":[],"alias_proposals":[]}`
	cases := map[string]string{
		"Markdown 围栏":  "```json\n" + base + "\n```",
		"前后有解释文字":      "好的，这是结果：" + base,
		"尾随第二个对象":      base + base,
		"尾随非空白字符":      base + " x",
		"不是对象是数组":      `[` + base + `]`,
		"空字符串":         "",
		"只有空白":         "   \n  ",
		"截断的 JSON":     base[:len(base)-3],
		"未知顶层字段":       `{"mentions":[],"relations":[],"alias_proposals":[],"note":"hi"}`,
		"缺少 mentions":  `{"relations":[],"alias_proposals":[]}`,
		"缺少 relations": `{"mentions":[],"alias_proposals":[]}`,
		"缺少 alias":     `{"mentions":[],"relations":[]}`,
		// ⚠️ 重复 key 在 encoding/json 里**默认后者覆盖前者、不报错**。
		// 放过去的后果是模型输出了两份 relations 而我们只看到一份，
		// 少掉的那份没有任何迹象。
		"重复 key":         `{"mentions":[],"relations":[{"subject_ref":"m1","object_ref":"m2","type":"冲突","evidence":[]}],"relations":[],"alias_proposals":[]}`,
		"mention 里有未知字段": `{"mentions":[{"ref":"m1","surface":"阿Q","occurrence":0,"conf":0.9}],"relations":[],"alias_proposals":[]}`,
	}
	for name, body := range cases {
		if _, err := parseExtractionResponse(body, okChunk); err == nil {
			t.Errorf("「%s」本该被拒绝", name)
		}
	}
}

// TestParseRejectsFieldViolations——字段层面的拒绝。
func TestParseRejectsFieldViolations(t *testing.T) {
	cases := map[string]string{
		"空 ref": `{"mentions":[{"ref":"","surface":"阿Q","occurrence":0}],"relations":[],"alias_proposals":[]}`,
		"重复 ref": `{"mentions":[{"ref":"m1","surface":"阿Q","occurrence":0},
		              {"ref":"m1","surface":"赵太爷","occurrence":0}],"relations":[],"alias_proposals":[]}`,
		"ref 过长":        `{"mentions":[{"ref":"` + strings.Repeat("x", 33) + `","surface":"阿Q","occurrence":0}],"relations":[],"alias_proposals":[]}`,
		"surface 为空":    `{"mentions":[{"ref":"m1","surface":"","occurrence":0}],"relations":[],"alias_proposals":[]}`,
		"surface 过长":    `{"mentions":[{"ref":"m1","surface":"` + strings.Repeat("名", 129) + `","occurrence":0}],"relations":[],"alias_proposals":[]}`,
		"occurrence 为负": `{"mentions":[{"ref":"m1","surface":"阿Q","occurrence":-1}],"relations":[],"alias_proposals":[]}`,
	}
	for name, body := range cases {
		if _, err := parseExtractionResponse(body, okChunk); err == nil {
			t.Errorf("「%s」本该被拒绝", name)
		}
	}
}

// TestParseRejectsUnknownRelationType——⭐ 类型集是**封闭**的。
//
// ⚠️ 契约明确不设 `其他` 这个出口：有了它，模型可以把任何拿不准的东西
// 丢进去，而那会变成一个**可以规避误报的类型**——精确率好看，
// 却什么也没说明。集合外的关系走 issues.jsonl 的人工清单。
func TestParseRejectsUnknownRelationType(t *testing.T) {
	mk := func(typ string) string {
		return `{"mentions":[{"ref":"m1","surface":"赵太爷","occurrence":0},
		         {"ref":"m2","surface":"阿Q","occurrence":0}],
		         "relations":[{"subject_ref":"m1","object_ref":"m2","type":"` + typ + `",
		         "evidence":[{"quote":"赵太爷跳过去给了他一个嘴巴","occurrence":0}]}],
		         "alias_proposals":[]}`
	}
	for _, typ := range relationTypes {
		if _, err := parseExtractionResponse(mk(typ), okChunk); err != nil {
			t.Errorf("合法类型 %q 被拒：%v", typ, err)
		}
	}
	for _, typ := range []string{"其他", "朋友", "", "欺凌 ", "Bullying"} {
		if _, err := parseExtractionResponse(mk(typ), okChunk); err == nil {
			t.Errorf("集合外类型 %q 本该被拒绝", typ)
		}
	}
}

// TestParseRejectsDanglingRefs——关系两端必须引用已声明的 mention。
func TestParseRejectsDanglingRefs(t *testing.T) {
	for name, rel := range map[string]string{
		"主体悬空": `{"subject_ref":"m9","object_ref":"m2","type":"冲突","evidence":[{"quote":"阿Q摸着左颊","occurrence":0}]}`,
		"客体悬空": `{"subject_ref":"m2","object_ref":"m9","type":"冲突","evidence":[{"quote":"阿Q摸着左颊","occurrence":0}]}`,
		"两端相同": `{"subject_ref":"m2","object_ref":"m2","type":"冲突","evidence":[{"quote":"阿Q摸着左颊","occurrence":0}]}`,
	} {
		body := `{"mentions":[{"ref":"m2","surface":"阿Q","occurrence":0}],
		          "relations":[` + rel + `],"alias_proposals":[]}`
		if _, err := parseExtractionResponse(body, okChunk); err == nil {
			t.Errorf("「%s」本该被拒绝", name)
		}
	}
}

// TestParseRejectsQuoteNotInSource——⭐ 引文必须在**当前块原文**里逐字存在。
//
// ⚠️ 这是唯一能挡住"编造引用"的检查。模型生成一句读起来很像原文、
// 实际书里没有的话，是最难被人发现的一种错误——它通过了类型校验、
// 通过了引用完整性，只有拿原文比一次才看得出来。
func TestParseRejectsQuoteNotInSource(t *testing.T) {
	mk := func(quote string, occ int) string {
		return `{"mentions":[{"ref":"m1","surface":"赵太爷","occurrence":0},
		         {"ref":"m2","surface":"阿Q","occurrence":0}],
		         "relations":[{"subject_ref":"m1","object_ref":"m2","type":"欺凌",
		         "evidence":[{"quote":"` + quote + `","occurrence":` + itoa(occ) + `}]}],
		         "alias_proposals":[]}`
	}
	for name, body := range map[string]string{
		"原文里没有这句":       mk("赵太爷赏了他一锭银子", 0),
		"只差一个字":         mk("赵太爷跳过去给了他两个嘴巴", 0),
		"occurrence 越界": mk("赵太爷", 5),
		"空引文":           mk("", 0),
	} {
		if _, err := parseExtractionResponse(body, okChunk); err == nil {
			t.Errorf("「%s」本该被拒绝", name)
		}
	}
}

// TestOccurrenceSelectsTheRightPosition——⭐ occurrence 决定**第几次出现**。
//
// ⚠️ 忽略它、一律取第一次出现的后果是：书里重复出现的句子（章回体套语、
// 人物口头禅）全部被指到第一处。引用打开是对的文字、错的位置，
// 而没有任何东西报错。
func TestOccurrenceSelectsTheRightPosition(t *testing.T) {
	// ⚠️ 夹具里每个 surface 都必须真的在原文里——第一版用了「别人」而原文
	// 是「别的话」，于是用例挂在 mention 定位那一步，看起来像 occurrence
	// 逻辑坏了，其实是夹具本身没通过同一道校验。
	chunk := "阿Q说了一句。中间隔着王胡的话。阿Q说了一句。"
	mk := func(occ int) string {
		return `{"mentions":[{"ref":"m1","surface":"阿Q","occurrence":0},
		         {"ref":"m2","surface":"王胡","occurrence":0}],
		         "relations":[{"subject_ref":"m1","object_ref":"m2","type":"冲突",
		         "evidence":[{"quote":"阿Q说了一句","occurrence":` + itoa(occ) + `}]}],
		         "alias_proposals":[]}`
	}
	first, err := parseExtractionResponse(mk(0), chunk)
	if err != nil {
		t.Fatal(err)
	}
	second, err := parseExtractionResponse(mk(1), chunk)
	if err != nil {
		t.Fatal(err)
	}
	a := first.Relations[0].Evidence[0]
	b := second.Relations[0].Evidence[0]
	if a.Start == b.Start {
		t.Fatalf("occurrence 0 和 1 指到了同一个位置 %d", a.Start)
	}
	if b.Start <= a.Start {
		t.Errorf("第二次出现的位置 %d 不在第一次 %d 之后", b.Start, a.Start)
	}
	for _, ev := range []evidenceSpanRef{a, b} {
		if got := string([]rune(chunk)[ev.Start:ev.End]); got != "阿Q说了一句" {
			t.Errorf("区间取出来是 %q", got)
		}
	}
}

func TestStrictDecodeRejectsTrailingJSONObject(t *testing.T) {
	var out extractionResponse
	if err := strictDecode(`{"mentions":[],"relations":[],"alias_proposals":[]} {"mentions":[]}`, &out); err == nil {
		t.Fatal("尾随第二个 JSON 对象被接受")
	}
}

// TestOverlappingOccurrencesAreCounted——⚠️ 契约要求"允许重叠匹配"。
// 用 strings.Index 逐段跳过会漏掉重叠的那次，于是 occurrence=1 被判越界，
// 一条完全合法的响应被整次拒绝。
func TestOverlappingOccurrencesAreCounted(t *testing.T) {
	chunk := "啊啊啊"
	positions := locateOccurrences([]rune(chunk), []rune("啊啊"))
	if len(positions) != 2 {
		t.Errorf("重叠匹配数 = %d, want 2（位置 0 和 1）", len(positions))
	}
}

// TestParseRejectsOversizedCollections——上限来自契约。
func TestParseRejectsOversizedCollections(t *testing.T) {
	// ⚠️ 每个 surface 都必须真的在原文里，而且 occurrence 各不相同。
	// 第一版全用 "人"（原文里根本没有），于是无论上限检查在不在，
	// 解析都会在定位那一步失败——断言 errors.Is(...Invalid) 两种情况下
	// 都成立，**它一直是因为错误的理由通过的**。变异测试是这么发现的。
	chunk := strings.Repeat("阿Q", maxMentionsPerResponse+1)
	var mentions []string
	for i := 0; i <= maxMentionsPerResponse; i++ {
		mentions = append(mentions,
			`{"ref":"m`+itoa(i)+`","surface":"阿Q","occurrence":`+itoa(i)+`}`)
	}
	body := `{"mentions":[` + strings.Join(mentions, ",") + `],"relations":[],"alias_proposals":[]}`
	err := parseAndDiscard(body, chunk)
	if !errors.Is(err, ErrExtractionResponseInvalid) {
		t.Fatalf("mention 超上限没有被拒：%v", err)
	}
	if !strings.Contains(err.Error(), "exceeds limit") {
		t.Errorf("拒绝理由不是「超上限」，而是 %v——夹具可能在别处就挂了", err)
	}
	// 恰好等于上限必须通过（否则上面那条可能只是"什么都拒绝"）。
	okBody := `{"mentions":[` + strings.Join(mentions[:maxMentionsPerResponse], ",") +
		`],"relations":[],"alias_proposals":[]}`
	if err := parseAndDiscard(okBody, chunk); err != nil {
		t.Errorf("恰好等于上限被拒：%v", err)
	}
}

func parseAndDiscard(body, chunk string) error {
	_, err := parseExtractionResponse(body, chunk)
	return err
}

// TestParseRejectsRelationWithoutEvidence——⭐ 变异测试逼出来的缺口。
//
// ⚠️ 一条没有证据的关系是**不可核验**的：它说 A 和 B 有某种关系，
// 而书里没有任何一句话被指出来支持它。这种记录进了库就再也分不清
// 是模型抽出来的还是编出来的，且它照样会进精确率的分母。
func TestParseRejectsRelationWithoutEvidence(t *testing.T) {
	mk := func(evidence string) string {
		return `{"mentions":[{"ref":"m1","surface":"赵太爷","occurrence":0},
		         {"ref":"m2","surface":"阿Q","occurrence":0}],
		         "relations":[{"subject_ref":"m1","object_ref":"m2","type":"欺凌",
		         "evidence":` + evidence + `}],"alias_proposals":[]}`
	}
	if err := parseAndDiscard(mk(`[]`), okChunk); err == nil {
		t.Error("没有证据的关系本该被拒绝——它不可核验")
	}
	// 超过上限同样拒绝。
	one := `{"quote":"赵太爷","occurrence":0}`
	var many []string
	for i := 0; i <= maxEvidencePerRelation; i++ {
		many = append(many, one)
	}
	if err := parseAndDiscard(mk(`[`+strings.Join(many, ",")+`]`), okChunk); err == nil {
		t.Errorf("证据超过 %d 条本该被拒绝", maxEvidencePerRelation)
	}
	// 1 条正常。
	if err := parseAndDiscard(mk(`[`+one+`]`), okChunk); err != nil {
		t.Errorf("一条证据被拒：%v", err)
	}
}

// TestTruncatedOutputIsRejectedBeforeParsing——⭐ finish_reason == "length"
// 的响应**在解析之前**就要拒绝。
//
// ⚠️ 恰好在一个合法边界被截断的输出能解析成功，少掉的关系没有任何迹象——
// 而那正是会让召回率虚高的一类错。所以判据是 finish_reason，不是能否解析。
func TestTruncatedOutputIsRejectedBeforeParsing(t *testing.T) {
	// 一份自身完全合法的 JSON，只是模型说它被截断了。
	if _, err := parseExtractionAttempt(validResponse(), finishReasonLength, okChunk); err == nil {
		t.Error("finish_reason=length 的响应没有被拒绝——它可能恰好断在合法边界上")
	}
	if _, err := parseExtractionAttempt(validResponse(), "stop", okChunk); err != nil {
		t.Errorf("正常结束的响应被拒：%v", err)
	}
	// 空 finish_reason 表示供应商没返回，不代表被截断。
	if _, err := parseExtractionAttempt(validResponse(), "", okChunk); err != nil {
		t.Errorf("finish_reason 未知的响应被拒：%v", err)
	}
}
