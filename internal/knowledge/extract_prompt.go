package knowledge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// extract_prompt.go 是第一阶段抽取响应的严格解析与原文定位（010 T024/T025）。
//
// ⭐ 这里的每一条校验都在防同一件事：**一条看上去合理、其实指不到原文的引用**。
// 关系抽取的产出最终会被当成"书里确实这么写了"的证据展示给用户，而模型完全
// 可以生成一句读起来像原文的话。所以引用不是"参考"，是**必须在当前块原文里
// 精确命中**的坐标；命不中就整次拒绝，绝不"尽力保留能用的部分"。
//
// ⚠️ 部分采用是最诱人的降级：32 个 mention 里 31 个能定位，丢掉那 1 个看起来
// 只损失一点召回。但被丢掉的往往正是模型编的那一个，而它引用的关系仍然留在
// relations 数组里——于是一条**没有任何原文支持**的关系被发布出去，且没有
// 任何症状。要么整份响应合法，要么这次调用失败重试（FR-009）。
//
// 这个文件里没有任何数据库调用，也不发起模型调用：给定同一份响应和同一个块，
// 输出必须逐字节可重复（宪法第 V 条）。

const (
	// extractionSchemaVersion 是第一阶段响应的协议版本，随作业固定
	// （见 relation_extraction_jobs.config_snapshot）。
	// ⚠️ 恢复中的作业**不升级**版本：换了协议再接着跑，等于同一份账目里
	// 混了两种口径的结果。
	extractionSchemaVersion = 2

	// extractPromptVersion 标识指令文本本身。与 schema 分开是因为两者
	// 变化频率不同：措辞调整不改结构，但**足以改变结果**，所以也必须
	// 进快照，否则两次实验的差异无法归因。
	extractPromptVersion = "extract/v5"
)

// 契约 §1 的规模上限。超限整次拒绝，不截断——截断会让"模型只抽到这些"和
// "我们只留了这些"变得无法区分。
const (
	maxExtractMentions       = 32
	maxExtractRelations      = 64
	maxExtractAliasProposals = 32
	maxExtractRefRunes       = 32
	maxExtractSurfaceRunes   = 128
	maxExtractQuoteRunes     = 2000
	minRelationEvidence      = 1
	maxRelationEvidence      = 4
)

// relationTypeDirected 是标注指南 §2 的封闭关系集合，值为**是否有向**。
//
// ⭐ 故意没有"其他"这个类型。一个兜底类型会让模型把所有拿不准的东西倒进去，
// 而那一类永远无法被判对错——误报率因此看起来很低，代价是这个指标失去意义。
// 集合外的关系按 FR-009 整次拒绝，调用和失败照常记账。
var relationTypeDirected = map[string]bool{
	"雇佣":   true,
	"亲属":   false,
	"同乡邻里": false,
	"冲突":   false,
	"欺凌":   true,
	"追求":   true,
	"权势压迫": true,
	"同伙":   false,
}

var (
	// errExtractionResponseInvalid 是第一阶段响应不合法的统一哨兵。
	// ⚠️ 刻意**不是** apperr：这不是用户能修的输入问题，是模型这次没按协议
	// 输出。调用方据此重试（至多 3 次），耗尽后这个 item 失败。
	errExtractionResponseInvalid = errors.New("knowledge: extraction response invalid")

	// errQuoteNotFound：引用在当前块原文里一次都找不到。
	//
	// ⚠️ 2026-09-07 起它**只**表示"这句话不在原文里"（模型编造或改写了引文）。
	// 之前它还兼职表示"occurrence 越界"，两者混在一起会让报告读不出
	// 模型到底是编造引文还是数错次数——而这两件事的严重程度差得很远。
	errQuoteNotFound = errors.New("knowledge: quote not found in chunk")

	// errQuoteNotCitable：引用跨过了系统拼进去的分隔符。
	//
	// ⚠️ 这段字符不在原文里，所以它没有坐标。允许跨过去的话，产出的区间会把
	// 两段本不相邻的原文说成一句连续的话——引用逐字比对还能过，因为块内容里
	// 它确实是连着的。
	errQuoteNotCitable = errors.New("knowledge: quote crosses a generated separator")
)

// extractionChunkView 是定位引用需要的全部块信息。
type extractionChunkView struct {
	ChunkID         string
	DocumentVersion int64
	Content         string
	Meta            narrativeMetadata
}

// extractInstruction 是第一阶段的固定指令。
//
// ⭐ 三处措辞是**协议的一部分**，改动等于改协议，必须同时动
// extractPromptVersion：
//   - "第 N 次出现，从 0 开始数，重叠也算一次"——服务端的 occurrence 口径
//     就是这么数的（见 quoteLocator.locate）。两边不一致的表现不是报错，
//     是引用系统性地偏到同一句话的另一处;
//   - "逐字复制原文"——引用要拿去和原文精确比对，改一个标点就整条作废;
//   - "不确定就不要输出"——模型在这类任务上倾向于把话说满，而一条编造的
//     关系比漏掉一条贵得多：漏掉只是召回率低，编造会被当成书里的事实展示。
const extractInstruction = `你在为一部中文小说建立人物关系索引。只依据下面给出的原文片段作答。

最终回复必须只有一个 JSON 对象。不要输出 Markdown 代码围栏，不要输出三个反引号，不要输出解释、前后缀或第二个对象。回复的第一个字符必须是 {，最后一个字符必须是 }：
{"mentions":[...],"relations":[...],"alias_proposals":[...]}
三个数组都必须出现，没有内容就给空数组。

mentions：片段中出现的人物称呼，每项 {"ref","surface"}。
  ref 是你在本次输出里自定的短编号（如 m1），surface 逐字复制原文中的称呼。
  不需要指出它是第几次出现，位置由系统在原文里查。

relations：人物之间有原文支持的关系，每项
  {"subject_ref","object_ref","type","evidence":[{"quote"}]}。
  type 只能是：雇佣、亲属、同乡邻里、冲突、欺凌、追求、权势压迫、同伙。
  evidence 给 1～4 条引用。先在上面的原文中逐字找到支持关系的连续文字，再从原文直接复制到 quote；不要凭记忆或常识补写，不要改写、不要拼接不相邻的句子；如果不能在原文中用眼睛逐字核对 quote，就不要输出这条关系；
  跨越多处的支持请分成多条引用。

alias_proposals：你认为指同一个人的两个称呼，每项 {"left","right","quote"}。
  left 和 right 必须填 mentions 数组里的 ref（如 m1、m2），不是人物称呼；例如
  {"left":"m1","right":"m2","quote":"..."}。
  quote 是原文中支持这个判断的那句话。只在原文明确写出时提出，
  "可能是"、"也许"、"不知道是不是"这类说法不要提。

不确定就不要输出。宁可少给，也不要给出原文里找不到的引用。

回复前逐项检查：
1. 回复只有一个 JSON 对象，且没有代码围栏或任何说明文字。
2. 三个顶层数组都存在；每个 ref 唯一且关系端点都来自 mentions。
3. 每个 surface 和每个 quote 都能在上面的原文中逐字找到；不能确认逐字相同就删掉该 mention、relation 或 alias_proposal。尤其不能把符合情节但原文没有的句子当 quote。
4. 不要输出 occurrence 或任何位置编号——同一句话在片段里出现多次时由系统定位，你只需保证引文逐字正确。`

// buildExtractInstruction 目前返回固定文本；留成函数是因为下一步要把
// 章节标题这类块级上下文拼进去，而调用方不该关心它是常量还是拼出来的。
func buildExtractInstruction() string { return extractInstruction }

// --- 协议结构（契约 §1）---
//
// ⚠️ 三个数组都是**指针**：契约要求"必须存在，可空"，而 []T 分不清
// "给了空数组"和"整个字段没给"。少一个字段说明模型没在按协议输出，
// 这时把它当空数组接受，等于替模型把话补全了。

type extractResponse struct {
	Mentions       *[]extractMention       `json:"mentions"`
	Relations      *[]extractRelation      `json:"relations"`
	AliasProposals *[]extractAliasProposal `json:"alias_proposals"`
}

type extractMention struct {
	Ref     string `json:"ref"`
	Surface string `json:"surface"`
	// Occurrence 已废弃：位置由服务端定位（见 quoteLocator.locate）。
	//
	// ⚠️ 字段**保留但忽略**，不是遗漏。DisallowUnknownFields 会拒绝任何
	// 未知字段，而 relation_extraction_attempts 里已经存着一批带 occurrence
	// 的旧响应——删掉这个字段，那些响应在回放时会整批失效，等于把已经花过
	// 的钱作废。指令里已经不再要求它，新响应不会再带。
	Occurrence int `json:"occurrence"`
}

type extractRelation struct {
	SubjectRef string          `json:"subject_ref"`
	ObjectRef  string          `json:"object_ref"`
	Type       string          `json:"type"`
	Evidence   *[]extractQuote `json:"evidence"`
}

type extractQuote struct {
	Quote string `json:"quote"`
	// 同 extractMention.Occurrence：保留但忽略，为的是旧响应仍能回放。
	Occurrence int `json:"occurrence"`
}

type extractAliasProposal struct {
	Left  string `json:"left"`
	Right string `json:"right"`
	Quote string `json:"quote"`
	// 同上：保留但忽略。
	Occurrence int `json:"occurrence"`
}

// --- 解析后的领域视图 ---

type resolvedMention struct {
	Ref     string
	Surface string
	// Span 是这次称呼在**块内容**里的 rune 区间；Document* 是它在原文里的
	// 位置，供归一阶段和证据回溯使用。
	ChunkStart, ChunkEnd       int
	DocumentStart, DocumentEnd int
}

type resolvedRelation struct {
	SubjectRef string
	ObjectRef  string
	Type       string
	IsDirected bool
	Evidence   []evidenceDraft
}

type resolvedAliasProposal struct {
	Left, Right string
	Evidence    evidenceDraft
}

type resolvedExtraction struct {
	Mentions       []resolvedMention
	Relations      []resolvedRelation
	AliasProposals []resolvedAliasProposal
	// AmbiguousPositions 是有多少个**称呼**落在块内出现不止一次的位置上。
	// ⚠️ 这个数通常很大而且基本无害：人名在一段里重复出现是常态，
	// 而 mention 只是"这个人物在本块登场"的锚点，落在哪一次没有实际差别。
	AmbiguousPositions int
	// AmbiguousEvidence 是有多少条**证据引文**在块内出现不止一次。
	//
	// ⭐ 真正要盯的是这个数，不是上面那个。证据是要展示给用户、让他翻回
	// 原文的东西：一句话在块里出现两次而我们指了第一次，用户翻过去看到的
	// 上下文就可能不是支持这条关系的那一处。
	// 两个数分开报，是因为把它们加在一起会让一个几乎无害的大数字
	// 盖住一个真正要紧的小数字。
	AmbiguousEvidence int
}

// parseExtractionResponse 把一次第一阶段的原始输出解析成校验过的结构。
//
// ⭐ 三道分开的关卡，顺序不可换：
//  1. 词法：重复 key、尾随第二个对象——encoding/json 对这两个都**默默接受**
//     （重复 key 后者覆盖前者，尾随内容根本不读），所以必须自己扫一遍;
//  2. 结构：未知字段、类型不符;
//  3. 语义：规模上限、ref 唯一性、类型在封闭集合内、引用指向存在的 mention。
//
// ⚠️ 不接受 Markdown 围栏。"帮模型把 ```json 剥掉"是个很自然的想法，但它掩盖
// 的是"模型没在按协议输出"这个事实——真正该做的是让这次调用失败并重试。
func parseExtractionResponse(raw []byte) (extractResponse, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return extractResponse{}, fmt.Errorf("%w: empty output", errExtractionResponseInvalid)
	}
	if err := checkStrictJSONObject(raw, errExtractionResponseInvalid); err != nil {
		return extractResponse{}, err
	}

	var resp extractResponse
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		return extractResponse{}, fmt.Errorf("%w: %v", errExtractionResponseInvalid, err)
	}
	if err := validateExtractResponse(resp); err != nil {
		return extractResponse{}, err
	}
	return resp, nil
}

// checkStrictJSONObject 扫一遍 token 流，拒绝重复 key 和尾随内容。
//
// ⚠️ 这两件事 encoding/json 都不报错，而它们的后果完全不同于"格式错误"：
// 重复 key 意味着同一份响应有两种读法（我们读到后一个，人看日志读到前一个）；
// 尾随第二个对象通常是模型把结果输出了两遍，而我们只用了第一遍——两次抽取
// 结果不一致这件事就此消失。截断的输出（finish_reason=length）也在这里被
// io.ErrUnexpectedEOF 抓住。
func checkStrictJSONObject(raw []byte, sentinel error) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w: %v", sentinel, err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("%w: top level is not an object", sentinel)
	}
	if err := walkStrictObject(dec, sentinel); err != nil {
		return err
	}
	// 顶层对象读完了还有东西 —— 尾随的第二个 JSON 值。
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing content after the JSON object", sentinel)
	}
	return nil
}

// walkStrictObject 递归检查一个已经消费掉 '{' 的对象。
func walkStrictObject(dec *json.Decoder, sentinel error) error {
	seen := map[string]struct{}{}
	for {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("%w: %v", sentinel, err)
		}
		if delim, ok := tok.(json.Delim); ok && delim == '}' {
			return nil
		}
		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("%w: object key is not a string", sentinel)
		}
		if _, dup := seen[key]; dup {
			return fmt.Errorf("%w: duplicate key %q", sentinel, key)
		}
		seen[key] = struct{}{}
		if err := walkStrictValue(dec, sentinel); err != nil {
			return err
		}
	}
}

func walkStrictValue(dec *json.Decoder, sentinel error) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w: %v", sentinel, err)
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil // 标量
	}
	switch delim {
	case '{':
		return walkStrictObject(dec, sentinel)
	case '[':
		for {
			// More 之后仍可能读到 ']'，交给下一轮的 walkStrictValue 之前先看一眼。
			if !dec.More() {
				if _, err := dec.Token(); err != nil { // 消费 ']'
					return fmt.Errorf("%w: %v", sentinel, err)
				}
				return nil
			}
			if err := walkStrictValue(dec, sentinel); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%w: unexpected %v", sentinel, delim)
	}
}

func validateExtractResponse(resp extractResponse) error {
	if resp.Mentions == nil || resp.Relations == nil || resp.AliasProposals == nil {
		return fmt.Errorf("%w: mentions/relations/alias_proposals must all be present", errExtractionResponseInvalid)
	}
	mentions, relations, proposals := *resp.Mentions, *resp.Relations, *resp.AliasProposals

	if len(mentions) > maxExtractMentions {
		return fmt.Errorf("%w: %d mentions exceed the limit", errExtractionResponseInvalid, len(mentions))
	}
	if len(relations) > maxExtractRelations {
		return fmt.Errorf("%w: %d relations exceed the limit", errExtractionResponseInvalid, len(relations))
	}
	if len(proposals) > maxExtractAliasProposals {
		return fmt.Errorf("%w: %d alias proposals exceed the limit", errExtractionResponseInvalid, len(proposals))
	}

	refs := make(map[string]struct{}, len(mentions))
	for _, m := range mentions {
		if err := checkRunes("mention ref", m.Ref, maxExtractRefRunes); err != nil {
			return err
		}
		if err := checkRunes("mention surface", m.Surface, maxExtractSurfaceRunes); err != nil {
			return err
		}
		if _, dup := refs[m.Ref]; dup {
			return fmt.Errorf("%w: duplicate mention ref %q", errExtractionResponseInvalid, m.Ref)
		}
		refs[m.Ref] = struct{}{}
	}

	for i, rel := range relations {
		if _, ok := relationTypeDirected[rel.Type]; !ok {
			// ⚠️ 集合外的类型不降级成"其他"，见 relationTypeDirected 的说明。
			return fmt.Errorf("%w: relation %d has out-of-scope type %q",
				errExtractionResponseInvalid, i, rel.Type)
		}
		if _, ok := refs[rel.SubjectRef]; !ok {
			return fmt.Errorf("%w: relation %d subject_ref %q is not a mention",
				errExtractionResponseInvalid, i, rel.SubjectRef)
		}
		if _, ok := refs[rel.ObjectRef]; !ok {
			return fmt.Errorf("%w: relation %d object_ref %q is not a mention",
				errExtractionResponseInvalid, i, rel.ObjectRef)
		}
		if rel.SubjectRef == rel.ObjectRef {
			return fmt.Errorf("%w: relation %d relates a mention to itself", errExtractionResponseInvalid, i)
		}
		if rel.Evidence == nil {
			return fmt.Errorf("%w: relation %d has no evidence array", errExtractionResponseInvalid, i)
		}
		ev := *rel.Evidence
		if len(ev) < minRelationEvidence || len(ev) > maxRelationEvidence {
			return fmt.Errorf("%w: relation %d has %d evidence items", errExtractionResponseInvalid, i, len(ev))
		}
		for _, q := range ev {
			if err := checkQuote(q); err != nil {
				return err
			}
		}
	}

	for i, p := range proposals {
		if _, ok := refs[p.Left]; !ok {
			return fmt.Errorf("%w: alias proposal %d left %q is not a mention", errExtractionResponseInvalid, i, p.Left)
		}
		if _, ok := refs[p.Right]; !ok {
			return fmt.Errorf("%w: alias proposal %d right %q is not a mention", errExtractionResponseInvalid, i, p.Right)
		}
		if p.Left == p.Right {
			return fmt.Errorf("%w: alias proposal %d links a mention to itself", errExtractionResponseInvalid, i)
		}
		if err := checkQuote(extractQuote{Quote: p.Quote}); err != nil {
			return err
		}
	}
	return nil
}

func checkQuote(q extractQuote) error {
	if err := checkRunes("quote", q.Quote, maxExtractQuoteRunes); err != nil {
		return err
	}
	return nil
}

func checkRunes(field, value string, max int) error {
	if value == "" {
		return fmt.Errorf("%w: %s is empty", errExtractionResponseInvalid, field)
	}
	if n := utf8.RuneCountInString(value); n > max {
		return fmt.Errorf("%w: %s is %d runes, limit %d", errExtractionResponseInvalid, field, n, max)
	}
	return nil
}

// resolveExtraction 把校验过的响应映射到原文坐标。
//
// ⭐ 定位失败**整次拒绝**，不跳过单条。理由见文件头：留下来的那些关系仍然
// 引用着被丢掉的 mention，最终会以"没有原文支持的关系"的形式发布出去。
func resolveExtraction(chunk extractionChunkView, resp extractResponse) (resolvedExtraction, error) {
	var out resolvedExtraction
	locator := newQuoteLocator(chunk)

	for _, m := range *resp.Mentions {
		start, end, matches, err := locator.locate(m.Surface)
		if err != nil {
			return resolvedExtraction{}, fmt.Errorf("mention %q: %w", m.Ref, err)
		}
		if matches > 1 {
			out.AmbiguousPositions++
		}
		docStart, docEnd, err := locator.documentRange(start, end)
		if err != nil {
			return resolvedExtraction{}, fmt.Errorf("mention %q: %w", m.Ref, err)
		}
		out.Mentions = append(out.Mentions, resolvedMention{
			Ref: m.Ref, Surface: m.Surface,
			ChunkStart: start, ChunkEnd: end,
			DocumentStart: docStart, DocumentEnd: docEnd,
		})
	}

	for i, rel := range *resp.Relations {
		evidence, err := locator.resolveEvidence(*rel.Evidence)
		if err != nil {
			return resolvedExtraction{}, fmt.Errorf("relation %d: %w", i, err)
		}
		out.Relations = append(out.Relations, resolvedRelation{
			SubjectRef: rel.SubjectRef, ObjectRef: rel.ObjectRef,
			Type: rel.Type, IsDirected: relationTypeDirected[rel.Type],
			Evidence: evidence,
		})
	}

	for i, p := range *resp.AliasProposals {
		evidence, err := locator.resolveEvidence([]extractQuote{{Quote: p.Quote}})
		if err != nil {
			return resolvedExtraction{}, fmt.Errorf("alias proposal %d: %w", i, err)
		}
		out.AliasProposals = append(out.AliasProposals, resolvedAliasProposal{
			Left: p.Left, Right: p.Right, Evidence: evidence[0],
		})
	}
	out.AmbiguousEvidence = locator.ambiguousEvidence
	return out, nil
}

// quoteLocator 在一个块内做原文定位。按 rune 工作，允许重叠匹配。
type quoteLocator struct {
	chunk       extractionChunkView
	content     string
	runeIndex   *runeIndex // 块内容的 byte -> rune
	contentRune int
	// ambiguousEvidence 累计有多少条证据引文在块内出现不止一次。
	// 放在定位器上而不是返回值里，是因为 resolveEvidence 会被关系和别名
	// 提案分别调用多次，计数必须跨调用累加。
	ambiguousEvidence int
}

func newQuoteLocator(chunk extractionChunkView) *quoteLocator {
	return &quoteLocator{
		chunk: chunk, content: chunk.Content,
		runeIndex:   newRuneIndex(chunk.Content),
		contentRune: utf8.RuneCountInString(chunk.Content),
	}
}

// locate 找出引文在块内的**第一处**出现，并报告它一共出现了几次。
//
// ⭐ 2026-09-07 的契约变更：occurrence 不再由模型给出，改由服务端定位。
// 原因是实测数据：qwen2.5:14b 在预检里 6 次定位失败**全部**是 occurrence
// 数错（"趙太爺" 报第 5 次而全文只有 5 次、"阿Q" 报第 2 次而只有 2 次——
// 典型的 off-by-one），引文本身逐字正确。也就是说，我们把一件模型做不好、
// 而服务端做得又快又准的事（数第几次出现）当成了协议的一部分，
// 白白损失掉三分之一的合法响应。
//
// ⚠️ 代价是明确的，不能含糊：同一句话在块内出现多次时，我们**取第一处**，
// 而那未必是模型心里想的那一处。对"证据"这个用途这是可接受的——任一处出现
// 都同样支持这条关系——但它确实让"引用指向原文的确切位置"这个保证弱了一档。
// 返回的 matches 就是为了让这件事可计量：调用方据此统计有多少条证据落在
// 多义位置上，报告里如实写出来，而不是假装每条引用都唯一。
//
// ⚠️ 重叠匹配的计数口径保留（每次只前进一个 rune）：它现在只影响 matches
// 这个统计数字，不再影响选哪一处，所以模型数不数得清已经无关紧要。
func (l *quoteLocator) locate(needle string) (start, end, matches int, err error) {
	needleRunes := utf8.RuneCountInString(needle)
	if needleRunes == 0 {
		return 0, 0, 0, fmt.Errorf("%w: empty needle", errQuoteNotFound)
	}
	first := -1
	for pos := 0; pos < len(l.content); {
		idx := strings.Index(l.content[pos:], needle)
		if idx < 0 {
			break
		}
		at := pos + idx
		if first < 0 {
			first = at
		}
		matches++
		_, size := utf8.DecodeRuneInString(l.content[at:])
		pos = at + size
	}
	if first < 0 {
		return 0, 0, 0, fmt.Errorf("%w: %q", errQuoteNotFound, needle)
	}
	start = l.runeIndex.at(first)
	return start, start + needleRunes, matches, nil
}

// documentRange 把块内 rune 区间翻译成原文 rune 区间。
//
// ⭐ 要求整段落在**连续**的可定位区间上。一段引用横跨两处不相邻的原文时，
// 这里返回错误而不是给出 [第一段的起点, 最后一段的终点)——那个区间会把中间
// 没被引用的原文一并算进来，看上去完全正常。契约里"多段支持用 evidence 数组"
// 说的就是这件事：模型该给两条引用，而不是我们替它拼一段。
func (l *quoteLocator) documentRange(start, end int) (int, int, error) {
	docStart, docEnd := -1, -1
	for _, seg := range l.chunk.Meta.Segments {
		if seg.ChunkEnd <= start || seg.ChunkStart >= end {
			continue
		}
		if seg.IsGeneratedSeparator || seg.DocumentStart == nil {
			return 0, 0, errQuoteNotCitable
		}
		// 段内偏移在两个坐标系里是一样的（区间等长，由
		// validateNarrativeMetadata 保证）。
		lo := max(start, seg.ChunkStart)
		hi := min(end, seg.ChunkEnd)
		segStart := *seg.DocumentStart + (lo - seg.ChunkStart)
		segEnd := *seg.DocumentStart + (hi - seg.ChunkStart)
		if docStart < 0 {
			docStart, docEnd = segStart, segEnd
			continue
		}
		if segStart != docEnd {
			return 0, 0, fmt.Errorf("%w: quote spans non-adjacent source intervals", errQuoteNotCitable)
		}
		docEnd = segEnd
	}
	if docStart < 0 {
		return 0, 0, fmt.Errorf("%w: no segment covers [%d,%d)", errQuoteNotCitable, start, end)
	}
	return docStart, docEnd, nil
}

// resolveEvidence 定位一组引用，并按**原文区间**去重。
//
// ⭐ 去重的键是原文坐标，不是引文字符串。同一句话会因为 overlap 出现在相邻
// 两个块里，也会在同一个块里出现两次（overlap 复制的那一份 + 正文那一份）——
// 前者是同一处原文，后者也是。按字符串去重会把"书里说了两次"和"我们复制了
// 一份"混为一谈，而按坐标去重两者都对：真正说了两次的，坐标不同。
func (l *quoteLocator) resolveEvidence(quotes []extractQuote) ([]evidenceDraft, error) {
	seen := map[[2]int]struct{}{}
	out := make([]evidenceDraft, 0, len(quotes))
	for _, q := range quotes {
		start, end, matches, err := l.locate(q.Quote)
		if err != nil {
			return nil, err
		}
		docStart, docEnd, err := l.documentRange(start, end)
		if err != nil {
			return nil, err
		}
		if matches > 1 {
			l.ambiguousEvidence++
		}
		key := [2]int{docStart, docEnd}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}

		segments, err := json.Marshal(l.segmentsFor(start, end))
		if err != nil {
			return nil, fmt.Errorf("knowledge: marshal evidence segments: %w", err)
		}
		out = append(out, evidenceDraft{
			ChunkID:         l.chunk.ChunkID,
			DocumentVersion: l.chunk.DocumentVersion,
			SourceOrder:     int64(l.chunk.Meta.SourceOrder),
			SourceStart:     docStart,
			SourceEnd:       docEnd,
			Quote:           q.Quote,
			SourceSegments:  segments,
		})
	}
	if len(out) == 0 {
		// 只可能发生在全部引用都指向同一处原文时。此时关系仍然成立，
		// 保留第一条即可——但空证据的关系绝不能发布。
		return nil, fmt.Errorf("%w: no citable evidence left after dedup", errQuoteNotCitable)
	}
	return out, nil
}

// segmentsFor 取出覆盖这段引用的 segment 切片，区间裁到引用范围内。
// 保留块内坐标（而不是重新以引用起点为 0）：这样它和 chunks.narrative_metadata
// 里的那份是同一个坐标系，出问题时可以直接对照。
func (l *quoteLocator) segmentsFor(start, end int) []narrativeSegment {
	var out []narrativeSegment
	for _, seg := range l.chunk.Meta.Segments {
		if seg.ChunkEnd <= start || seg.ChunkStart >= end {
			continue
		}
		clipped := seg
		lo := max(start, seg.ChunkStart)
		hi := min(end, seg.ChunkEnd)
		clipped.ChunkStart, clipped.ChunkEnd = lo, hi
		if seg.DocumentStart != nil {
			ds := *seg.DocumentStart + (lo - seg.ChunkStart)
			de := *seg.DocumentStart + (hi - seg.ChunkStart)
			clipped.DocumentStart, clipped.DocumentEnd = &ds, &de
		}
		out = append(out, clipped)
	}
	return out
}
