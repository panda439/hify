package knowledge

import (
	"sort"

	"hify/internal/provider"
)

// 001-rag-query-rerank US2：在候选截断到 topK 之前，用交叉编码器式的
// rerank 模型按"问题—片段"真实相关度重排融合排序（hybrid.go）产出的候选
// 池。这个文件只放纯函数（applyRerank）和它需要的包内类型——真正发起
// provider.Client.Rerank 网络调用、处理超时/降级、把结果接进 Retrieve 的
// 逻辑在 service.go，和 admission.go/dedup.go 的既有分工一致：判定逻辑
// 单独成文件、零数据库依赖，网络调用留给 service.go。

const (
	// rerankInputLimit 是单次送入 rerank 的候选条数上限。融合排序后的候选
	// 池上界是 2*candidateK（hybrid.go 的 candidateK 文档注释），topK=50
	// 时能到 200——全量送 rerank 意味着 200 条正文（未来会更大）打进一次
	// HTTP 请求，延迟和成本都不可接受（plan.md 的 SC-005：单轮叠加延迟
	// p95 ≤ 2s）。只重排融合排名前 50 条，第 51 条及以后保持融合排序的原
	// 相对顺序排在被重排的这些之后——它们本来排名就靠后，被 topK 截断的
	// 概率也高，"没资格进 rerank"和"没资格进 topK"在统计上高度重合。
	rerankInputLimit = 50
)

// rerankedCandidate 只存在于 Retrieve 内部（service.go 用它做重排后的排
// 序），绝不进入 RetrievedChunk——rerank 分数一旦写进 RetrievedChunk.Score，
// 就会顺着 Retrieve 的返回值流到 conversation，撞上
// budget.go 的 ragMinSimilarityScore=0.2 分数线（这个字段现有语义是"向量
// 分与关键词分的较大值"，FR-008 明确禁止覆盖）。
type rerankedCandidate struct {
	chunk         RetrievedChunk
	originalIndex int // 重排前（也就是送进 provider.RerankRequest.Documents 时）的位置，确定性 tie-break 用
	rerankScore   float64
	// haveScore 目前恒为 true（applyRerank 只在每个候选都拿到一个分数时才
	// 会走到排序这一步，校验不通过整体返回 false）——保留这个字段是为了
	// 和 fusionEntry.haveVector/haveKeyword 同一套"区分'没打分'和'打了 0
	// 分'"的约定保持一致，避免将来有人往这个结构里加"部分候选没打分也继
	// 续"的逻辑时，误把 rerankScore 的零值当成真的 0 分。
	haveScore bool
}

// rerankStats 是一次 Retrieve 调用里 rerank 步骤的可观测汇总——只含计数/
// 布尔/耗时，不含 query 原文、片段正文、逐条分数（FR-017，data-model.md
// §5.2）。service.go 的 Retrieve 把它并进既有的
// "knowledge: retrieval candidate admission and dedup" 结构化日志。
type rerankStats struct {
	Enabled    bool
	Applied    bool
	Degraded   bool
	InputCount int
	DurationMs int64
}

// applyRerank 是 FR-007/FR-009/FR-011 的纯函数落点：按 rerank 分数重排
// candidates，分数相同则用重排前的原始位置（也就是 candidates 在这次调用
// 里的下标，等价于送进 provider.RerankRequest.Documents 时的下标）升序做
// tie-break——sort.SliceStable 而不是 sort.Slice，双重保险：即便两条候选的
// (rerankScore, originalIndex) 组合意外相等（理论上不可能，originalIndex
// 各不相同），稳定排序也不会引入不确定的相对顺序（宪法第 V 条）。
//
// 第二个返回值 false 表示 candidates 和 scores 对不上——数量不一致、
// index 越界、index 重复、或 index 覆盖不完整（contracts/
// rerank-http-api.md 的响应校验 1-4 条；第 5 条"JSON 解析失败/分数非数
// 值"发生在 provider.Client.Rerank 内部，根本到不了这里，见
// openai_compat.go 的 validateRerankResponse）。校验不通过时原样返回
// candidates（连同它的相对顺序），调用方（service.go 的 Retrieve）据此整
// 体丢弃 rerank 结果、保持融合排序继续——绝不部分采用（FR-011）。
//
// 绝不触碰 chunk.Score、Citation 相关字段（DocumentName/PageNumber/
// SectionTitle/...）或 NeighborOf——这个函数只重排切片元素的顺序，每个元
// 素本身原样透传。
func applyRerank(candidates []RetrievedChunk, scores []provider.RerankScore) ([]RetrievedChunk, bool) {
	n := len(candidates)
	if len(scores) != n {
		return candidates, false
	}

	seen := make([]bool, n)
	scoreByIndex := make([]float64, n)
	for _, s := range scores {
		if s.Index < 0 || s.Index >= n {
			return candidates, false
		}
		if seen[s.Index] {
			return candidates, false
		}
		seen[s.Index] = true
		scoreByIndex[s.Index] = s.Score
	}
	for _, ok := range seen {
		if !ok {
			return candidates, false
		}
	}

	reranked := make([]rerankedCandidate, n)
	for i, c := range candidates {
		reranked[i] = rerankedCandidate{chunk: c, originalIndex: i, rerankScore: scoreByIndex[i], haveScore: true}
	}
	sort.SliceStable(reranked, func(i, j int) bool {
		if reranked[i].rerankScore != reranked[j].rerankScore {
			return reranked[i].rerankScore > reranked[j].rerankScore
		}
		return reranked[i].originalIndex < reranked[j].originalIndex
	})

	out := make([]RetrievedChunk, n)
	for i, rc := range reranked {
		out[i] = rc.chunk
	}
	return out, true
}
