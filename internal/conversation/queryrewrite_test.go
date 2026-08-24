package conversation

import "testing"

// 001-rag-query-rerank US1 的纯函数单测：shouldSkipRewrite/parseRewriteResult/
// validateRewrite 全部零依赖（不碰网络、不碰数据库），对应宪法第 V 条"判定
// 逻辑必须可以抽成不依赖数据库的纯函数并被单元测试覆盖"的硬要求。这三个
// 函数此时尚未实现（queryrewrite.go 还不存在）——先写、先跑失败，再实现。

func TestShouldSkipRewrite(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		hasHistory bool
		want       bool
	}{
		{name: "无历史+无指代词+长度达标 -> skip", query: "Hify 的分块策略是什么", hasHistory: false, want: true},
		{name: "含它 -> 不skip", query: "那它的上限呢", hasHistory: false, want: false},
		{name: "含这个 -> 不skip", query: "这个怎么配置", hasHistory: false, want: false},
		{name: "含那个 -> 不skip", query: "那个上限是多少", hasHistory: false, want: false},
		{name: "含上述 -> 不skip", query: "上述策略适用于所有文档吗", hasHistory: false, want: false},
		{name: "含前者 -> 不skip", query: "前者的性能更好吗", hasHistory: false, want: false},
		{name: "含后者 -> 不skip", query: "后者是什么意思", hasHistory: false, want: false},
		{name: "含这些 -> 不skip", query: "这些配置在哪里改", hasHistory: false, want: false},

		// Review 修正：单字指代词「该/其/这/那」已从模式集合里删除。中文没有
		// 词边界，strings.Contains 对单字必然假阳性，而这些句子全是首轮就完整、
		// 根本不需要改写的问题——误判一次就白付一次 LLM 调用，直接打在 SC-006
		// 的「完整问题快速路径命中率 ≥90%」上。这组用例就是防它回潮的。
		{name: "假阳性防回归：应该 不含指代 -> skip", query: "我应该怎么配置 Hify 的分块大小", hasHistory: false, want: true},
		{name: "假阳性防回归：其他 不含指代 -> skip", query: "Hify 和其他 RAG 框架的区别是什么", hasHistory: false, want: true},
		{name: "假阳性防回归：其它 不含指代 -> skip", query: "除了分块还有其它优化手段吗", hasHistory: false, want: true},
		{name: "假阳性防回归：尤其 不含指代 -> skip", query: "尤其是大文档的处理流程是怎样的", hasHistory: false, want: true},
		{name: "假阳性防回归：这里 不含指代 -> skip", query: "文档上传失败时这里会记录什么日志", hasHistory: false, want: true},
		{name: "假阳性防回归：那么 不含指代 -> skip", query: "如果分块过大那么检索质量会下降吗", hasHistory: false, want: true},
		{name: "无指代词但有历史 -> 不skip", query: "Hify 的分块策略是什么", hasHistory: true, want: false},
		{name: "含指代词且有历史 -> 不skip", query: "那它呢", hasHistory: true, want: false},
		{name: "空串 -> skip", query: "", hasHistory: false, want: true},
		{name: "纯标点 -> skip", query: "？！。", hasHistory: false, want: true},
		{name: "空串但有历史 -> 仍skip（没有可改写的内容）", query: "   ", hasHistory: true, want: true},
		{name: "英文 it -> 不skip", query: "what about it", hasHistory: false, want: false},
		{name: "英文 this -> 不skip", query: "how does this work", hasHistory: false, want: false},
		{name: "英文 those -> 不skip", query: "what about those limits", hasHistory: false, want: false},
		{name: "英文完整问题无指代词 -> skip", query: "what is the chunk size limit for Hify documents", hasHistory: false, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldSkipRewrite(tc.query, tc.hasHistory)
			if got != tc.want {
				t.Errorf("shouldSkipRewrite(%q, %v) = %v, want %v", tc.query, tc.hasHistory, got, tc.want)
			}
		})
	}
}

func TestParseRewriteResult(t *testing.T) {
	t.Run("裸JSON", func(t *testing.T) {
		got, err := parseRewriteResult(`{"standalone_question":"Hify 文档分块策略的上限是多少","ambiguous":false}`)
		if err != nil {
			t.Fatalf("parseRewriteResult: %v", err)
		}
		if got.StandaloneQuestion != "Hify 文档分块策略的上限是多少" || got.Ambiguous != false {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("带json围栏", func(t *testing.T) {
		raw := "```json\n{\"standalone_question\":\"改写后的问题\",\"ambiguous\":false}\n```"
		got, err := parseRewriteResult(raw)
		if err != nil {
			t.Fatalf("parseRewriteResult: %v", err)
		}
		if got.StandaloneQuestion != "改写后的问题" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("首尾空白", func(t *testing.T) {
		raw := "  \n\t {\"standalone_question\":\"问题\",\"ambiguous\":true} \n  "
		got, err := parseRewriteResult(raw)
		if err != nil {
			t.Fatalf("parseRewriteResult: %v", err)
		}
		if !got.Ambiguous {
			t.Fatalf("got %+v, want ambiguous=true", got)
		}
	})

	t.Run("非法JSON返回error", func(t *testing.T) {
		_, err := parseRewriteResult("这不是 JSON，模型直接开始回答问题了")
		if err == nil {
			t.Fatal("expected error for unparsable output, got nil")
		}
	})

	t.Run("缺字段返回零值不报错", func(t *testing.T) {
		got, err := parseRewriteResult(`{}`)
		if err != nil {
			t.Fatalf("parseRewriteResult: %v", err)
		}
		if got.StandaloneQuestion != "" || got.Ambiguous != false {
			t.Fatalf("got %+v, want zero value", got)
		}
	})
}

func TestValidateRewrite(t *testing.T) {
	t.Run("ambiguous=true不采用", func(t *testing.T) {
		_, ok := validateRewrite("那它的上限呢", rewriteResponse{StandaloneQuestion: "随便写的", Ambiguous: true})
		if ok {
			t.Fatal("ambiguous=true must not be accepted")
		}
	})

	t.Run("空字符串不采用", func(t *testing.T) {
		_, ok := validateRewrite("那它的上限呢", rewriteResponse{StandaloneQuestion: "", Ambiguous: false})
		if ok {
			t.Fatal("empty standalone_question must not be accepted")
		}
	})

	t.Run("纯空白不采用", func(t *testing.T) {
		_, ok := validateRewrite("那它的上限呢", rewriteResponse{StandaloneQuestion: "   \n\t ", Ambiguous: false})
		if ok {
			t.Fatal("whitespace-only standalone_question must not be accepted")
		}
	})

	t.Run("超过200 runes不采用", func(t *testing.T) {
		long := make([]rune, 201)
		for i := range long {
			long[i] = '字'
		}
		_, ok := validateRewrite("这是一个不算短的原始问题示例内容", rewriteResponse{StandaloneQuestion: string(long), Ambiguous: false})
		if ok {
			t.Fatal("standalone_question over 200 runes must not be accepted")
		}
	})

	t.Run("超过max(3×原长,60)不采用", func(t *testing.T) {
		// 原问题很短（5 个字），3×原长=15，但下限是 60，所以阈值是 60；
		// 构造一个 61 rune 的候选，必须被拒绝（既没超 200，也没超 3×原长
		// 本身太小的边界，专门测 max(...) 里 60 这条下限分支）。
		original := "分块策略" // 4 runes, 3x=12, max(12,60)=60
		candidate := make([]rune, 61)
		for i := range candidate {
			candidate[i] = '问'
		}
		_, ok := validateRewrite(original, rewriteResponse{StandaloneQuestion: string(candidate), Ambiguous: false})
		if ok {
			t.Fatal("standalone_question over max(3x original, 60) runes must not be accepted")
		}
	})

	t.Run("超过3×原长（原问题较长时）不采用", func(t *testing.T) {
		// 原问题 30 runes，3×原长=90 > 60，用 91 runes 候选触发这条分支
		// （而不是 60 这条下限）。
		original := make([]rune, 30)
		for i := range original {
			original[i] = '原'
		}
		candidate := make([]rune, 91)
		for i := range candidate {
			candidate[i] = '答'
		}
		_, ok := validateRewrite(string(original), rewriteResponse{StandaloneQuestion: string(candidate), Ambiguous: false})
		if ok {
			t.Fatal("standalone_question over 3x a long original must not be accepted")
		}
	})

	// FR-005 的第三项校验「与原问题的最小相关性」——review 时补上的，之前
	// contracts §5 漏写了这一条，实现也就跟着没做。
	t.Run("与原问题完全无关不采用", func(t *testing.T) {
		_, ok := validateRewrite("Hify 的分块策略上限是多少", rewriteResponse{
			StandaloneQuestion: "北京今天的天气怎么样",
		})
		if ok {
			t.Fatal("a rewrite sharing no content signal with the original must not be accepted")
		}
	})

	t.Run("原问题只剩指代词时放行（fail open）", func(t *testing.T) {
		// 「它呢」剥掉指代词和虚词后什么都不剩，无从比较——这恰恰是本功能
		// 存在的理由（省略式追问），此时拒绝会直接废掉核心场景，必须放行。
		got, ok := validateRewrite("它呢", rewriteResponse{
			StandaloneQuestion: "Hify 文档分块策略的分块大小上限是多少",
		})
		if !ok {
			t.Fatal("must fail open when the original carries no content signal")
		}
		if got != "Hify 文档分块策略的分块大小上限是多少" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("正常改写采用并去除首尾空白", func(t *testing.T) {
		got, ok := validateRewrite("那它的上限呢", rewriteResponse{StandaloneQuestion: "  Hify 文档分块策略的分块大小上限是多少  ", Ambiguous: false})
		if !ok {
			t.Fatal("a normal, well-formed rewrite must be accepted")
		}
		if got != "Hify 文档分块策略的分块大小上限是多少" {
			t.Fatalf("got %q, want trimmed content", got)
		}
	})
}

func TestSharesRelevanceSignal(t *testing.T) {
	cases := []struct {
		name      string
		original  string
		candidate string
		want      bool
	}{
		{name: "省略式追问补全后共享实词 上限", original: "那它的上限呢", candidate: "Hify 文档分块策略的分块大小上限是多少", want: true},
		{name: "完全跑题", original: "Hify 的分块策略上限是多少", candidate: "北京今天的天气怎么样", want: false},
		{name: "原问题只剩虚词时 fail open", original: "它呢", candidate: "Hify 的分块大小上限是多少", want: true},
		{name: "英文 token 共享", original: "what about the chunk limit", candidate: "what is the chunk size limit in Hify", want: true},
		{name: "英文完全跑题", original: "what about the chunk limit", candidate: "how do I reset my password", want: false},
		{name: "只共享单个虚词不算相关", original: "分块策略是什么", candidate: "登录失败是什么原因", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sharesRelevanceSignal(tc.original, tc.candidate); got != tc.want {
				t.Errorf("sharesRelevanceSignal(%q, %q) = %v, want %v", tc.original, tc.candidate, got, tc.want)
			}
		})
	}
}
