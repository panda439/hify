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
	extractionSchemaVersion = 1

	// extractPromptVersion 标识指令文本本身。与 schema 分开是因为两者
	// 变化频率不同：措辞调整不改结构，但**足以改变结果**，所以也必须
	// 进快照，否则两次实验的差异无法归因。
	extractPromptVersion = "extract/v2"
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

	// errQuoteNotFound：引用在当前块原文里找不到，或 occurrence 越界。
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

最终回复必须只有一个 JSON 对象。不要输出 Markdown 代码围栏，不要输出 ```，不要输出解释、前后缀或第二个对象。回复的第一个字符必须是 {，最后一个字符必须是 }：
{"mentions":[...],"relations":[...],"alias_proposals":[...]}
三个数组都必须出现，没有内容就给空数组。

mentions：片段中出现的人物称呼，每项 {"ref","surface","occurrence"}。
  ref 是你在本次输出里自定的短编号（如 m1），surface 逐字复制原文中的称呼，
  occurrence 是它在片段中的第几次出现，从 0 开始数，重叠也算一次。

relations：人物之间有原文支持的关系，每项
  {"subject_ref","object_ref","type","evidence":[{"quote","occurrence"}]}。
  type 只能是：雇佣、亲属、同乡邻里、冲突、欺凌、追求、权势压迫、同伙。
  evidence 给 1～4 条引用。先在上面的原文中逐字找到支持关系的连续文字，再复制到 quote；不要凭记忆或常识补写，不要改写、不要拼接不相邻的句子；
  跨越多处的支持请分成多条引用。

alias_proposals：你认为指同一个人的两个称呼，每项 {"left","right","quote","occurrence"}，
  quote 是原文中支持这个判断的那句话。只在原文明确写出时提出，
  "可能是"、"也许"、"不知道是不是"这类说法不要提。

不确定就不要输出。宁可少给，也不要给出原文里找不到的引用。

回复前逐项检查：
1. 回复只有一个 JSON 对象，且没有 ``` 或任何说明文字。
2. 三个顶层数组都存在；每个 ref 唯一且关系端点都来自 mentions。
3. 每个 surface 和每个 quote 都能在上面的原文中逐字找到；不能确认逐字相同就删掉该 mention、relation 或 alias_proposal。
4. 每个 occurrence 从 0 开始按原文出现顺序计数，重叠出现也计数；不要猜 occurrence。`

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
	Ref        string `json:"ref"`
	Surface    string `json:"surface"`
	Occurrence int    `json:"occurrence"`
}

type extractRelation struct {
	SubjectRef string          `json:"subject_ref"`
	ObjectRef  string          `json:"object_ref"`
	Type       string          `json:"type"`
	Evidence   *[]extractQuote `json:"evidence"`
}

type extractQuote struct {
	Quote      string `json:"quote"`
	Occurrence int    `json:"occurrence"`
}

type extractAliasProposal struct {
	Left       string `json:"left"`
	Right      string `json:"right"`
	Quote      string `json:"quote"`
	Occurrence int    `json:"occurrence"`
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
