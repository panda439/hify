package knowledge

import "strings"

// extract_instructions.go 是两个阶段送给模型的固定指令（010 R6-01）。
//
// ⭐ 指令与解析器是**同一份契约的两半**。extract_prompt.go 那边是"收到什么
// 才接受"，这边是"要求模型产出什么"。两边任何一处改动都必须同时改另一处，
// 否则表现是模型永远被拒、item 反复重试三次然后失败——而失败原因看起来
// 像是模型能力不够。
//
// ⚠️ 指令里**只描述格式与判据，不写"请谨慎""不确定就别写"**这类行为诱导。
// 这是 009 已经确立的口径：诱导对冲会让模型少输出真实存在的关系，
// 而少掉的那部分在召回率里表现为"模型抽不出来"，我们看不出是自己要求的。
// 拿不准的东西有 alias_proposals 和 ambiguous 这两个**结构化出口**，
// 不需要靠措辞去暗示。

// extractInstruction 是抽取阶段的指令。
//
// ⭐ 三件事必须说死，因为解析器会逐条核对：
//  1. 只输出一个 JSON 对象，不能有代码块围栏、前言、后记；
//  2. quote 必须是正文的**精确子串**——解析器要拿它回原文定位，
//     改一个标点就定位不到，这条 evidence 连同整条关系一起被拒；
//  3. occurrence 是"这段文字在正文里第几次出现"，从 0 开始。
//
// ⚠️ 第 2、3 条是最容易被忽略、也最容易整批失败的两条。模型天然倾向于
// 把引文"整理"得通顺一些（补主语、去掉半个标点），而那样每一条都定位不到。
var extractInstruction = strings.Join([]string{
	"你在读一部小说的一个片段。任务是抽取其中出现的人物，以及人物之间的关系。",
	"",
	"输出**一个 JSON 对象**，不要代码块围栏，不要任何解释文字。结构如下：",
	`{"mentions":[{"ref":"m1","surface":"称呼","occurrence":0}],`,
	` "relations":[{"subject_ref":"m1","object_ref":"m2","type":"关系类型",`,
	`               "evidence":[{"quote":"正文原句","occurrence":0}]}],`,
	` "alias_proposals":[{"left":"m1","right":"m2","quote":"正文原句","occurrence":0}]}`,
	"",
	"字段规则：",
	"- mentions：正文里出现的每一个人物称呼各一条。ref 由你自己编号（m1、m2……），",
	"  同一条响应内不重复。surface 是称呼在正文里的原样写法。",
	"- occurrence：这段文字在正文里是第几次出现，从 0 开始数。只出现一次就填 0。",
	"- quote 与 surface 都必须是正文的**精确子串**：一字不改，标点、空格、",
	"  换行都要与正文一致。不要补主语，不要改写通顺，不要合并两个不相连的句子。",
	"  定位不到的引文会让整条关系作废。",
	"- relations：subject_ref / object_ref 必须是本次 mentions 里的 ref。",
	"  type 只能取下面这个封闭集合中的一个：" + strings.Join(relationTypes, "、") + "。",
	"  集合之外的关系**不要输出**，也不要新造类型名。",
	"  每条关系最多 " + itoa(maxEvidencePerRelation) + " 条 evidence，至少 1 条。",
	"- alias_proposals：当你认为两个 ref 指的是同一个人时给出，并附上支持这个判断的",
	"  正文原句。只是同名或都出现在附近**不算**依据。拿不准就不要提出。",
	"",
	"数量上限：mentions 最多 " + itoa(maxMentionsPerResponse) + " 条，",
	"relations 最多 " + itoa(maxRelationsPerResp) + " 条，",
	"alias_proposals 最多 " + itoa(maxAliasProposals) + " 条。",
	"正文里没有人物或没有关系时，对应的数组输出为空数组。",
	"",
	"正文：",
}, "\n")

// aliasInstruction 是归一阶段的指令。
//
// ⭐ 三个动作是**封闭集合**，其中 ambiguous 是有意留出的出口：
// 逼模型在 new 和 link 之间二选一，它会去猜，而猜错的两个方向后果不同——
// 错误的 link 把两个人合并成一个，之后所有关系都挂在错的人身上，
// 且**没有任何迹象**；错误的 new 只是多一个人物碎片，会体现在指标里。
//
// ⚠️ 指令要求给 supports 原文引用，而不是一个置信度分数。
// 分数没法复核：人工只能对着一堆已经合并好的人物干瞪眼。
var aliasInstruction = strings.Join([]string{
	"下面是一段小说正文，以及此前已经识别出的人物候选。",
	"对正文中的每一个称呼，判断它指向一个新人物、某个已有候选，还是无法确定。",
	"",
	"输出**一个 JSON 对象**，不要代码块围栏，不要任何解释文字。结构如下：",
	`{"decisions":[{"mention_ref":"m1","action":"link","character_id":"...",`,
	`               "new_group":0,"reason_code":"...",`,
	`               "supports":[{"source_ref":"m1","quote":"正文原句","occurrence":0}]}]}`,
	"",
	"字段规则：",
	"- 每个 mention_ref 恰好一条 decision，不多不少。",
	"- action 只能是 " + aliasActionNew + " / " + aliasActionLink + " / " + aliasActionAmbiguous + "：",
	"  " + aliasActionLink + " 表示指向已有候选，此时 character_id 填那个候选的 id，new_group 填 0；",
	"  " + aliasActionNew + " 表示这是一个新人物，此时 character_id 留空，new_group 填一个正整数，",
	"  同一个新人物的多个称呼填同一个 new_group；",
	"  " + aliasActionAmbiguous + " 表示正文不足以判断，character_id 留空，new_group 填 0。",
	"- supports 是支持这条判断的正文原句，必须是正文的**精确子串**，",
	"  occurrence 是它在正文里第几次出现（从 0 开始）。",
	"  仅仅同名、或者两个称呼出现在相近位置，都**不是**依据。",
	"- reason_code 用一个简短的英文小写标识说明判据来源，例如 same_name_and_role。",
	"",
	"正文与候选：",
}, "\n")
