package knowledge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"hify/internal/platform/apperr"
)

// extract_prompt.go 是模型输出进入系统的**唯一入口**（010 T024）。
//
// ⭐ 它之后的每一层都假定拿到的是干净的、能在原文里定位的数据。
// 这里放过去的每一条脏数据，都会以"一条看起来正常的关系"的形式出现在
// 最终结果里。
//
// ⚠️ 原则是**整次拒绝，绝不部分采用**。挑出能用的那几条听起来更宽容，
// 但那样一次结构损坏的响应会产出"少了几条关系"的结果，而失败率显示为 0——
// 比直接失败更难发现，也更难解释。

// relationTypes 是标注指南 §2 的封闭集合。
//
// ⚠️ 契约明确**不设 `其他`**：有了它，模型可以把任何拿不准的东西丢进去，
// 而那会变成一个可以规避误报的类型——精确率好看，却什么也没说明。
// 集合外的关系走 issues.jsonl 的人工清单。
var relationTypes = []string{
	"雇佣", "亲属", "同乡邻里", "冲突", "欺凌", "追求", "权势压迫", "同伙",
}

var relationTypeSet = func() map[string]bool {
	m := make(map[string]bool, len(relationTypes))
	for _, t := range relationTypes {
		m[t] = true
	}
	return m
}()

// 契约 §1 的规模与长度上限。
const (
	maxMentionsPerResponse = 32
	maxRelationsPerResp    = 64
	maxAliasProposals      = 32
	maxRefRunes            = 32
	maxSurfaceRunes        = 128
	maxQuoteRunes          = 2000
	maxEvidencePerRelation = 4
)

// ErrExtractionResponseInvalid 是本文件唯一的拒绝理由。
//
// ⚠️ 故意只有一个：调用方对所有拒绝的处置完全相同（这次尝试失败、按重试
// 策略处理）。分成十几个哨兵错误只会让调用方产生"可以分别处理"的错觉，
// 而具体是哪一条不合法要看错误信息，不该进分支。
var ErrExtractionResponseInvalid = apperr.InvalidInput(
	"knowledge.extraction_response_invalid", "模型返回的抽取结果不符合约定格式")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrExtractionResponseInvalid, fmt.Sprintf(format, args...))
}

// evidenceSpanRef 是一条已经**在原文里定位过**的引用。
//
// ⭐ 保存 rune 区间而不是只留字符串：只留字符串的话，下游要引用它就只能
// 回原文搜，而书里重复出现的句子会全部指向第一次出现的位置。
type evidenceSpanRef struct {
	Quote      string
	Occurrence int
	Start      int
	End        int
}

type mentionRef struct {
	Ref        string
	Surface    string
	Occurrence int
	Start      int
	End        int
}

type relationRef struct {
	SubjectRef string
	ObjectRef  string
	Type       string
	Evidence   []evidenceSpanRef
}

type aliasProposalRef struct {
	Left  string
	Right string
	evidenceSpanRef
}

type extractionResponse struct {
	Mentions       []mentionRef
	Relations      []relationRef
	AliasProposals []aliasProposalRef
}

// parseExtractionAttempt 是给调用方用的入口：先看结束原因，再解析。
//
// ⭐ finish_reason == "length" 的响应**在解析之前**就拒绝。
// ⚠️ 恰好在一个合法边界被截断的输出能解析成功，少掉的关系没有任何迹象——
// 而那正是会让召回率虚高的一类错。判据是 finish_reason，不是能否解析。
// 空 finish_reason 表示供应商没返回它，不代表被截断（见 provider.Message）。
func parseExtractionAttempt(body, finishReason, chunk string) (extractionResponse, error) {
	if finishReason == finishReasonLength {
		return extractionResponse{}, invalid("output was truncated (finish_reason=length)")
	}
	return parseExtractionResponse(body, chunk)
}

// parseExtractionResponse 严格解析并在 chunk 原文里定位每一条引用。
func parseExtractionResponse(body, chunk string) (extractionResponse, error) {
	raw, err := decodeStrictObject(body)
	if err != nil {
		return extractionResponse{}, err
	}
	src := []rune(chunk)

	var out extractionResponse
	seenRef := make(map[string]bool, len(raw.Mentions))
	if len(raw.Mentions) > maxMentionsPerResponse {
		return out, invalid("mentions: %d exceeds limit %d", len(raw.Mentions), maxMentionsPerResponse)
	}
	for i, m := range raw.Mentions {
		if err := checkRunes("mentions[%d].ref", m.Ref, maxRefRunes, i); err != nil {
			return out, err
		}
		if seenRef[m.Ref] {
			return out, invalid("mentions[%d]: duplicate ref %q", i, m.Ref)
		}
		seenRef[m.Ref] = true
		if err := checkRunes("mentions[%d].surface", m.Surface, maxSurfaceRunes, i); err != nil {
			return out, err
		}
		start, end, err := locate(src, m.Surface, m.Occurrence, "mentions[%d].surface", i)
		if err != nil {
			return out, err
		}
		out.Mentions = append(out.Mentions, mentionRef{
			Ref: m.Ref, Surface: m.Surface, Occurrence: m.Occurrence, Start: start, End: end,
		})
	}

	if len(raw.Relations) > maxRelationsPerResp {
		return out, invalid("relations: %d exceeds limit %d", len(raw.Relations), maxRelationsPerResp)
	}
	for i, r := range raw.Relations {
		if !relationTypeSet[r.Type] {
			// ⚠️ 整次拒绝，不是跳过这一条：一个集合外的类型说明模型没有
			// 遵守约定，而它同一次输出里的其他条目同样不可信。
			return out, invalid("relations[%d]: type %q is not in the closed set", i, r.Type)
		}
		if !seenRef[r.SubjectRef] {
			return out, invalid("relations[%d]: subject_ref %q is not a declared mention", i, r.SubjectRef)
		}
		if !seenRef[r.ObjectRef] {
			return out, invalid("relations[%d]: object_ref %q is not a declared mention", i, r.ObjectRef)
		}
		if r.SubjectRef == r.ObjectRef {
			return out, invalid("relations[%d]: subject and object are the same mention", i)
		}
		if len(r.Evidence) == 0 || len(r.Evidence) > maxEvidencePerRelation {
			return out, invalid("relations[%d]: evidence count %d out of range 1..%d",
				i, len(r.Evidence), maxEvidencePerRelation)
		}
		rel := relationRef{SubjectRef: r.SubjectRef, ObjectRef: r.ObjectRef, Type: r.Type}
		for j, ev := range r.Evidence {
			span, err := resolveEvidence(src, ev.Quote, ev.Occurrence,
				"relations["+strconv.Itoa(i)+"].evidence[%d]", j)
			if err != nil {
				return out, err
			}
			rel.Evidence = append(rel.Evidence, span)
		}
		out.Relations = append(out.Relations, rel)
	}

	if len(raw.AliasProposals) > maxAliasProposals {
		return out, invalid("alias_proposals: %d exceeds limit %d", len(raw.AliasProposals), maxAliasProposals)
	}
	for i, a := range raw.AliasProposals {
		if !seenRef[a.Left] || !seenRef[a.Right] {
			return out, invalid("alias_proposals[%d]: left/right must be declared mentions", i)
		}
		if a.Left == a.Right {
			return out, invalid("alias_proposals[%d]: left and right are the same mention", i)
		}
		span, err := resolveEvidence(src, a.Quote, a.Occurrence, "alias_proposals[%d]", i)
		if err != nil {
			return out, err
		}
		out.AliasProposals = append(out.AliasProposals, aliasProposalRef{
			Left: a.Left, Right: a.Right, evidenceSpanRef: span,
		})
	}
	return out, nil
}

func resolveEvidence(src []rune, quote string, occurrence int, format string, args ...any) (evidenceSpanRef, error) {
	if err := checkRunes(format+".quote", quote, maxQuoteRunes, args...); err != nil {
		return evidenceSpanRef{}, err
	}
	start, end, err := locate(src, quote, occurrence, format+".quote", args...)
	if err != nil {
		return evidenceSpanRef{}, err
	}
	return evidenceSpanRef{Quote: quote, Occurrence: occurrence, Start: start, End: end}, nil
}

func checkRunes(format, s string, limit int, args ...any) error {
	name := fmt.Sprintf(format, args...)
	if s == "" {
		return invalid("%s: must not be empty", name)
	}
	if n := len([]rune(s)); n > limit {
		return invalid("%s: %d runes exceeds limit %d", name, n, limit)
	}
	return nil
}

// locate 在原文里找到第 occurrence 次出现。
//
// ⭐ 这是唯一能挡住"编造引用"的检查。模型生成一句读起来很像原文、实际书里
// 没有的话，是最难被人发现的一种错误——它通过了类型校验、通过了引用完整性，
// 只有拿原文比一次才看得出来。
func locate(src []rune, needle string, occurrence int, format string, args ...any) (int, int, error) {
	name := fmt.Sprintf(format, args...)
	if occurrence < 0 {
		return 0, 0, invalid("%s: occurrence %d is negative", name, occurrence)
	}
	hits := locateOccurrences(src, []rune(needle))
	if occurrence >= len(hits) {
		return 0, 0, invalid("%s: occurrence %d out of range (%d found in source)",
			name, occurrence, len(hits))
	}
	start := hits[occurrence]
	return start, start + len([]rune(needle)), nil
}

// locateOccurrences 返回 needle 在 src 中每一次出现的起始 rune 下标。
//
// ⚠️ **允许重叠**（契约 §1）。用 strings.Index 逐段跳过会漏掉重叠的那次，
// 于是 occurrence=1 被判越界，一条完全合法的响应被整次拒绝。
func locateOccurrences(src, needle []rune) []int {
	if len(needle) == 0 || len(needle) > len(src) {
		return nil
	}
	var out []int
	for i := 0; i+len(needle) <= len(src); i++ {
		match := true
		for j := range needle {
			if src[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			out = append(out, i)
		}
	}
	return out
}

// --- 严格 JSON 解码 ---

type rawMention struct {
	Ref        string `json:"ref"`
	Surface    string `json:"surface"`
	Occurrence int    `json:"occurrence"`
}

type rawEvidence struct {
	Quote      string `json:"quote"`
	Occurrence int    `json:"occurrence"`
}

type rawRelation struct {
	SubjectRef string        `json:"subject_ref"`
	ObjectRef  string        `json:"object_ref"`
	Type       string        `json:"type"`
	Evidence   []rawEvidence `json:"evidence"`
}

type rawAlias struct {
	Left       string `json:"left"`
	Right      string `json:"right"`
	Quote      string `json:"quote"`
	Occurrence int    `json:"occurrence"`
}

type rawResponse struct {
	Mentions       []rawMention  `json:"mentions"`
	Relations      []rawRelation `json:"relations"`
	AliasProposals []rawAlias    `json:"alias_proposals"`
}

// decodeStrictObject 解出**恰好一个** JSON 对象，且不接受未知字段与重复 key。
//
// ⚠️ encoding/json 默认对这三件事都是宽容的：未知字段被丢弃、重复 key 后者
// 覆盖前者、流里剩下的内容被忽略。三种宽容各自对应一种静默数据丢失：
//   - 未知字段 → 模型加了我们没约定的东西，可能是它理解错了任务，而我们看不见；
//   - 重复 key → 模型输出了两份 relations 而我们只看到一份，少掉的没有迹象；
//   - 尾随内容 → 模型在 JSON 后面又写了一段（很常见），我们当作正常结束。
func decodeStrictObject(body string) (rawResponse, error) {
	var out rawResponse
	dec := json.NewDecoder(bytes.NewReader([]byte(body)))
	dec.DisallowUnknownFields()

	// 先看第一个 token 必须是 '{'：Markdown 围栏、前置解释文字、数组
	// 都在这里被挡下，而且错误信息比"字段类型不匹配"清楚得多。
	tok, err := dec.Token()
	if err != nil {
		return out, invalid("not a JSON document: %v", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return out, invalid("top level must be a JSON object")
	}

	// 手工扫一遍顶层 key，DisallowUnknownFields 抓不到重复 key。
	seen := map[string]bool{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return out, invalid("malformed object: %v", err)
		}
		key, _ := keyTok.(string)
		if seen[key] {
			return out, invalid("duplicate top-level key %q", key)
		}
		seen[key] = true
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return out, invalid("malformed value for %q: %v", key, err)
		}
	}
	if _, err := dec.Token(); err != nil { // 关闭的 '}'
		return out, invalid("unterminated object: %v", err)
	}
	// ⚠️ 流里不许还剩东西。`{...}{...}` 与 `{...} 说明文字` 都在这里被挡下。
	if _, err := dec.Token(); err != io.EOF {
		return out, invalid("unexpected trailing content after the JSON object")
	}

	for _, required := range []string{"mentions", "relations", "alias_proposals"} {
		if !seen[required] {
			// ⚠️ 三个数组**必须存在**（可以为空）。缺一个和"空的"不是一回事：
			// 缺失说明模型没按约定输出，而把它当成空数组就是替模型做了假设。
			return out, invalid("missing required field %q", required)
		}
	}

	// 第二遍：真正解码，这次由 DisallowUnknownFields 兜住嵌套层的未知字段。
	strict := json.NewDecoder(bytes.NewReader([]byte(body)))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&out); err != nil {
		return rawResponse{}, invalid("decode: %v", err)
	}
	return out, nil
}

func itoa(n int) string { return strconv.Itoa(n) }
