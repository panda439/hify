package conversation

import (
	"strings"
	"testing"
)

// 005-tool-loop-guard：重复调用检测的纯逻辑单测，零依赖。

func TestNormalizeToolArgsIgnoresKeyOrder(t *testing.T) {
	// 模型两次吐出的 JSON key 顺序不同、语义相同——必须判定为同一次调用，
	// 否则死循环检测会被 key 顺序抖动绕过。
	a := normalizeToolArgs(`{"city":"北京","days":3}`)
	b := normalizeToolArgs(`{"days":3,"city":"北京"}`)
	if a != b {
		t.Fatalf("key 顺序不同的等价参数应规范化成同一结果：\n%s\n%s", a, b)
	}
}

func TestNormalizeToolArgsIgnoresWhitespace(t *testing.T) {
	a := normalizeToolArgs(`{"city":"北京"}`)
	b := normalizeToolArgs("{\n  \"city\" : \"北京\"\n}")
	if a != b {
		t.Fatalf("空白差异应被规范化掉：\n%s\n%s", a, b)
	}
}

// TestNormalizeToolArgsKeepsArrayOrder：数组顺序有语义，**不能**排序。
// [1,2] 和 [2,1] 是不同的调用，把它们判成相同会误杀合法的第二次尝试。
func TestNormalizeToolArgsKeepsArrayOrder(t *testing.T) {
	if normalizeToolArgs(`{"ids":[1,2]}`) == normalizeToolArgs(`{"ids":[2,1]}`) {
		t.Fatal("数组顺序不同必须视为不同调用")
	}
}

// TestNormalizeToolArgsFallsBackOnInvalidJSON：参数不是合法 JSON 时不能报错。
// 这里的职责是「两次调用是否相同」，参数合法性是工具执行层的事。
func TestNormalizeToolArgsFallsBackOnInvalidJSON(t *testing.T) {
	for _, bad := range []string{`{"city":`, "", "not json at all", "{{{"} {
		got := normalizeToolArgs(bad)
		if got != strings.TrimSpace(bad) {
			t.Fatalf("非法 JSON %q 应原样退化，got %q", bad, got)
		}
	}
	// 两个相同的非法串仍然要判定为同一次调用
	if toolCallFingerprint("t", "{{{") != toolCallFingerprint("t", "{{{") {
		t.Fatal("相同的非法参数应产生相同指纹")
	}
}

// TestNormalizeToolArgsDoesNotDoSemanticEquivalence 锁定一条**有意为之**的边界：
// 不做语义级等价判断。这条断言存在的意义是防止将来有人"顺手加强一下"——
// 语义等价没有边界，且会误杀参数确实不同、只是看起来像的合法重试。
func TestNormalizeToolArgsDoesNotDoSemanticEquivalence(t *testing.T) {
	if normalizeToolArgs(`{"city":"北京"}`) == normalizeToolArgs(`{"city":"Beijing"}`) {
		t.Fatal("不应做语义级等价判断——这是有意的取舍，见 normalizeToolArgs 的注释")
	}
}

func TestToolCallFingerprintSeparatesNameAndArgs(t *testing.T) {
	// 名字和参数之间有分隔符，否则 ("ab","c") 与 ("a","bc") 会撞。
	if toolCallFingerprint("ab", `"c"`) == toolCallFingerprint("a", `b"c"`) {
		t.Fatal("工具名与参数的边界必须明确，不能拼接后产生歧义")
	}
}

func TestDetectorBlocksOnThirdIdenticalCall(t *testing.T) {
	d := newToolLoopDetector()
	args := `{"q":"same"}`

	if d.observe("search", args) {
		t.Fatal("第 1 次不该拦截")
	}
	// 第 2 次也不拦：工具瞬时失败后重试一次是合法行为，拦了就是误杀。
	if d.observe("search", args) {
		t.Fatal("第 2 次不该拦截——合法重试必须放过")
	}
	if !d.observe("search", args) {
		t.Fatal("第 3 次必须拦截")
	}
	if !d.isBlocked("search") {
		t.Fatal("被拦截的工具必须进入本轮黑名单")
	}
}

// TestDetectorResetsOnDifferentCall：「连续」的定义——中间出现别的调用就重新计数。
// A、A、B、A 不算 A 出现三次，因为那次 B 说明模型换过策略、拿到过新信息。
func TestDetectorResetsOnDifferentCall(t *testing.T) {
	d := newToolLoopDetector()
	d.observe("search", `{"q":"a"}`)
	d.observe("search", `{"q":"a"}`)
	d.observe("weather", `{"city":"北京"}`) // 换了调用
	if d.observe("search", `{"q":"a"}`) {
		t.Fatal("中间隔了别的调用，计数必须重置，不该在这里拦截")
	}
}

func TestDetectorTreatsDifferentArgsAsDifferentCalls(t *testing.T) {
	d := newToolLoopDetector()
	for i, args := range []string{`{"p":1}`, `{"p":2}`, `{"p":3}`} {
		if d.observe("search", args) {
			t.Fatalf("参数不同的第 %d 次调用不该被拦截", i+1)
		}
	}
	if d.isBlocked("search") {
		t.Fatal("参数不同不构成死循环，工具不该被停用")
	}
}

// TestDetectorSeesKeyOrderVariantsAsSame：模型每次输出的 key 顺序可能抖动，
// 死循环检测不能被这个绕过。
func TestDetectorSeesKeyOrderVariantsAsSame(t *testing.T) {
	d := newToolLoopDetector()
	d.observe("search", `{"a":1,"b":2}`)
	d.observe("search", `{"b":2,"a":1}`)
	if !d.observe("search", `{"a":1,  "b":2}`) {
		t.Fatal("key 顺序/空白抖动的三次相同调用必须在第 3 次被拦截")
	}
}

func TestFingerprintPrefixIsShortAndSafe(t *testing.T) {
	fp := toolCallFingerprint("search", `{"q":"敏感的用户输入"}`)
	p := fingerprintPrefix(fp)
	if len(p) != 8 {
		t.Fatalf("前缀长度 = %d, want 8", len(p))
	}
	if strings.Contains(fp, "敏感") || strings.Contains(p, "敏感") {
		t.Fatal("指纹不得包含参数原文")
	}
}

// TestInterventionMessageGivesAnExit —— 这条断言的对象是**措辞**，不是代码路径。
// 注入消息如果只说「别再调了」，模型往往还是不知道该干嘛，于是继续转圈或者
// 干脆沉默。必须给出可执行的下一步。
func TestInterventionMessageGivesAnExit(t *testing.T) {
	msg := loopInterventionMessage("search", 3)
	for _, want := range []string{"search", "停用", "其他", "查不到"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("注入消息缺少关键要素 %q —— 它必须给模型一条明确的出路，而不只是制止。实际：%s", want, msg)
		}
	}
}

func TestExhaustedMessageDeclaresIncompleteness(t *testing.T) {
	msg := toolLoopExhaustedMessage()
	// 「可能并不完整」这句是程序强制拼上去的，不依赖模型遵守提示词——
	// 否则用户会把一个基于残缺信息的回答当成完整答案。
	if !strings.Contains(msg, "不完整") {
		t.Fatalf("收尾消息必须明确声明信息不完整，实际：%s", msg)
	}
	if !strings.Contains(msg, "停止") {
		t.Fatalf("收尾消息必须说明是主动停止而不是出错，实际：%s", msg)
	}
}
