# Phase 1 数据模型：RAG 查询优化与结果重排序

**Feature**: `001-rag-query-rerank` | **Date**: 2026-08-24

## 1. 持久化变更

### 1.1 `provider_models.capability`（MySQL，migration `000012`）

唯一的 schema 变更：能力枚举增加 `rerank`。

```sql
-- up
ALTER TABLE provider_models DROP CHECK chk_provider_models_capability;
ALTER TABLE provider_models
    ADD CONSTRAINT chk_provider_models_capability
    CHECK (capability IN ('chat', 'embedding', 'rerank'));

-- down（回退前必须确认不存在 capability='rerank' 的行，否则约束添加会失败）
ALTER TABLE provider_models DROP CHECK chk_provider_models_capability;
ALTER TABLE provider_models
    ADD CONSTRAINT chk_provider_models_capability
    CHECK (capability IN ('chat', 'embedding'));
```

字段类型（`VARCHAR(16)`）、索引 `idx_provider_models_provider_capability` 均不变，
无需重新生成 sqlc（列定义未变）——但仍按宪法第 IV 条在 migration 之后跑一次 `make sqlc` 确认无 diff。

**其余表零变更**：chunk 表（PostgreSQL）、`message_citations`、`trace_spans` 结构全部不动。

## 2. 领域类型变更

### 2.1 `internal/provider`（新增）

```go
const CapabilityRerank = "rerank"   // model.go，与 CapabilityChat/CapabilityEmbedding 并列

// llm.go
type RerankRequest struct {
    Model     string
    Query     string
    Documents []string   // 与候选顺序一一对应，索引即候选下标
    TopN      int        // 0 表示全部返回
}

type RerankResult struct {
    Scores []RerankScore  // 顺序由服务端决定，消费方必须按 Index 回填，不得假定有序
}

type RerankScore struct {
    Index int      // 对应 RerankRequest.Documents 的下标
    Score float64  // 相关度，量纲由服务端定义，只用于比较，不对外暴露
}
```

`Client` 接口新增 `Rerank(ctx context.Context, req RerankRequest) (RerankResult, error)`。

### 2.2 `internal/knowledge`（新增，仅包内可见）

```go
// rerank.go —— 只存在于 Retrieve 内部，不进入 RetrievedChunk
type rerankedCandidate struct {
    chunk         RetrievedChunk
    originalIndex int      // 重排前的位置，确定性 tie-break 用
    rerankScore   float64
    haveScore     bool     // 与 fusionEntry.haveVector 同样的理由：区分"没打分"和"打了 0 分"
}

type rerankStats struct {
    Enabled    bool
    Applied    bool
    Degraded   bool
    InputCount int
    DurationMs int64
}
```

**硬约束**：`RetrievedChunk` 结构体本身**不新增任何字段**。rerank 分数一旦写进它，
就会顺着 `Retrieve` 的返回值流到 conversation，撞上 `ragMinSimilarityScore=0.2` 的分数线语义
（FR-008 禁止）。

### 2.3 `internal/conversation`（新增，仅包内可见）

```go
// queryrewrite.go
type rewriteOutcome struct {
    SearchQuery string  // 真正送去检索的问题：改写成功则为独立问题，否则为原问题
    Skipped     bool    // 走了快速路径或开关关闭，未调用 LLM
    Applied     bool    // 实际使用了改写结果
    Degraded    bool    // 调用失败/超时/校验不过而退回原问题
    DurationMs  int64
}

// LLM 返回的 JSON 形状
type rewriteResponse struct {
    StandaloneQuestion string `json:"standalone_question"`
    Ambiguous          bool   `json:"ambiguous"`
}
```

## 3. 配置项（`internal/config.Config`）

| 环境变量 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `HIFY_RAG_QUERY_REWRITE_ENABLED` | bool | `false` | 查询优化总开关。默认关闭以满足 SC-003 的"上线即与基线一致" |
| `HIFY_RAG_QUERY_REWRITE_MODEL_ID` | string | `""` | 留空则用当前 Agent 自己的 chat 模型 |
| `HIFY_RAG_QUERY_REWRITE_TIMEOUT` | duration | `1500ms` | 超时立即降级 |
| `HIFY_RAG_RERANK_ENABLED` | bool | `false` | 重排总开关 |
| `HIFY_RAG_RERANK_MODEL_ID` | string | `""` | 必须指向 `capability='rerank'` 且 `is_active` 的模型；为空视同关闭 |
| `HIFY_RAG_RERANK_TIMEOUT` | duration | `1500ms` | 超时立即降级 |

**校验规则**：
- 两个 duration 解析失败 → `config.Load` 返回错误（与既有 TTL 解析一致，启动即失败）。
- `HIFY_RAG_RERANK_ENABLED=true` 但 `MODEL_ID` 为空 → **不**让启动失败，降级为关闭并 `slog.Warn`
  （配置错误不应该拖垮整个进程，且与 FR-014 的"任何失败都只降级"一致）。
- rerank 模型 ID 的合法性（存在、能力正确、启用中）在 `knowledge` 首次使用时校验，
  校验失败即本轮降级并 `slog.Warn`，不缓存失败状态。

## 4. 包内常量（不进配置，与 Phase 8 门槛同样的理由）

| 常量 | 值 | 位置 | 说明 |
|---|---|---|---|
| `rerankInputLimit` | `50` | `knowledge/rerank.go` | 单次送入 rerank 的候选上限，其余保持原相对顺序排其后 |
| `maxRewriteHistoryTurns` | `4` | `conversation/queryrewrite.go` | 参与改写的最近对话轮数 |
| `maxRewriteQuestionRunes` | `200` | `conversation/queryrewrite.go` | 改写结果长度硬上限 |
| `minRewriteTriggerRunes` | `2` | `conversation/queryrewrite.go` | 短于此长度的问题不值得改写 |

## 5. 可观测字段

### 5.1 新增 trace span（conversation）

`kind = "query_rewrite"`，`parent_span_id` 指向本轮根 span，attrs：

| key | 类型 | 说明 |
|---|---|---|
| `rag.rewrite.enabled` | bool | 开关状态 |
| `rag.rewrite.skipped` | bool | 走快速路径 / 开关关闭 |
| `rag.rewrite.applied` | bool | 实际采用改写结果 |
| `rag.rewrite.degraded` | bool | 失败/超时/校验不过 |
| `rag.rewrite.duration_ms` | int | 耗时 |

**禁止出现**：问题原文、改写结果原文、历史消息内容、任何内容指纹。

### 5.2 扩展既有 slog 行（knowledge）

在 Phase 8 的 `"knowledge: retrieval candidate admission and dedup"` 结构化日志上补 5 个字段：
`rerank_enabled`、`rerank_applied`、`rerank_degraded`、`rerank_input_count`、`rerank_duration_ms`。
触发条件同步放宽为"发生过拒绝/去重/邻接去重 **或** 重排被应用/降级"。

同样禁止记录 query、片段正文、逐条 rerank 分数。
