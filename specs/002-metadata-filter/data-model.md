# Phase 1 数据模型：检索元数据过滤（Metadata Filtering）

**Feature**: `002-metadata-filter` | **Date**: 2026-09-02

---

## 1. 持久化变更

**无。本期不新增 migration，不新增列，不新增索引。**

这是本功能最重要的一条设计结论，值得单独成节说明，因为它与 spec 起草时的预设相反
（起草时预设要新增 `metadata jsonb` + GIN）。过滤需要的两个列**已经存在**：

| 列 | 引入于 | 现状 | 本期用途 |
|---|---|---|---|
| `chunks.document_id` | `000001_chunks` | 一直有值；已有 btree 索引 `idx_chunks_document_id` | 文档级过滤 |
| `chunks.page_number` | `000003_chunk_source_metadata` | PDF 有值、非 PDF 为 NULL、迁移前存量行为 NULL；Citation V1 已在读 | 页码范围过滤 |

`page_number` 的写入链路**已经完整存在且本期不改动**：
`parse.go: pdfPage.Number` → `chunk.go: chunkPDFPages` 的 `chunkPiece{PageNumber: &num}` →
`model.go: Chunk.PageNumber` → `service.go: ProcessDocument` → `repository.go: createChunks`
（`intPtrToNullInt32`）→ `chunks.page_number`。

> **待更正的过期注释**：`internal/db/pgqueries/chunks.sql` 中 `CreateChunk` 的注释写着
> "page_number/section_title 当前解析器不产出可靠值，调用方一律传 NULL"。这是 `000003`
> 时期的遗留描述，Phase 4 起就已不符实。本期顺带更正该注释（只改注释，不改 SQL 语义）。

---

## 2. SQL 变更（`internal/db/pgqueries/chunks.sql`）

只改**两条召回查询**，各加三行谓词（写法与否决方案见 [research.md](./research.md) R1）：

- `SearchVectorChunks` — 向量路
- `SearchKeywordChunks` — 关键词路

```sql
  AND (sqlc.narg(filter_document_ids)::text[] IS NULL
       OR document_id = ANY(sqlc.narg(filter_document_ids)::text[]))
  AND (sqlc.narg(filter_page_min)::int IS NULL OR page_number >= sqlc.narg(filter_page_min)::int)
  AND (sqlc.narg(filter_page_max)::int IS NULL OR page_number <= sqlc.narg(filter_page_max)::int)
```

**明确不改的查询**（每一条都是有意的，不是遗漏）：

| 查询 | 为什么不加过滤 |
|---|---|
| `FindPublishedNeighborChunksBatch` | FR-011：文档级过滤已由 anchors 结构性满足，页码过滤必须豁免。见 research.md R3 |
| `FindPublishedNeighborChunks` | 同上（且已非生产路径） |
| `CountChunksByKnowledgeBase` | 统计"知识库有多少分片"，与某一次检索的范围无关 |
| `CreateChunk` / `Delete*` / `Publish*` | 写路径，与检索过滤无关 |

---

## 3. 领域类型变更（`internal/knowledge`，仅包内可见 + 接口入参）

### 3.1 `model.go` 新增

```go
// RetrieveFilter 是一次检索的范围限定。零值合法且是默认值，语义为"不限定"。
type RetrieveFilter struct {
    DocumentIDs []string // 或关系；nil/空 = 不按文档限定
    PageMin     *int     // 1-indexed 起始页（含）；nil = 不限下界
    PageMax     *int     // 1-indexed 结束页（含）；nil = 不限上界
}

// RetrieveOptions 是 Retrieve 的可选入参聚合。零值 = 今天的行为。
type RetrieveOptions struct {
    Filter RetrieveFilter
}
```

- `IsEmpty()`：三个字段都为空即为空过滤器。这是 FR-006「空过滤器等价于无过滤」的判定入口，
  MUST 是纯函数、MUST 被单测覆盖。
- `Validate()`：执行 FR-015 的上限（见 §5 常量）。**MUST NOT 做任何截断**——超限返回错误。

### 3.2 常量（包内，不进配置）

与 Phase 8 准入阈值、Phase 3 `rrfK` 同样的理由：这些是**语义常量**而不是运维旋钮，
放进配置只会制造一个没人知道该怎么调的开关。

```go
maxFilterDocumentIDs = 50 // 参照 maxTopK=50；理由见 research.md R6
```

### 3.3 `errors.go` 新增（Message 中文，宪法第 VII 条）

```go
ErrTooManyFilterDocuments  // 指定的文档数量超出上限（最多 50 份），请缩小范围
ErrInvalidPageRange        // 页码范围不正确：页码必须为正整数，且起始页不得大于结束页
ErrMetadataFilterDisabled  // 检索元数据过滤未启用，无法按指定范围检索
```

`ErrMetadataFilterDisabled` 是 research.md R4 的落点——开关关闭 + 非空过滤器 = 明确失败，
**不是**静默降级成无过滤检索。

### 3.4 `service.go` 接口签名变更

```go
Retrieve(ctx, knowledgeBaseIDs []string, query string, topK int, opts RetrieveOptions) ([]RetrievedChunk, error)
```

两个调用方传 `RetrieveOptions{}`：
`internal/conversation/context.go`、`internal/workflow/executor.go`。
本期**不**给它们增加任何传递过滤器的能力——那属于"过滤条件从哪来"，spec 已排除。

### 3.5 `repository.go` 签名变更

`searchVectorChunks` / `searchKeywordChunks` 各增加一个 `filter RetrieveFilter` 参数，
原样翻译成 sqlc 的可空参数。repository 层**只做翻译，不做校验**（宪法第 IV 条：
repository 只做 CRUD 不含业务判断）——校验在 service 层入口一次性完成。

---

## 4. 配置项（`internal/config.Config`）

| 字段 | 环境变量 | 默认 | 说明 |
|---|---|---|---|
| `RAGMetadataFilterEnabled` | `HIFY_RAG_METADATA_FILTER_ENABLED` | `false` | 默认关闭。关闭时空过滤器走今天的路径（逐字一致），非空过滤器返回 `ErrMetadataFilterDisabled` |

解析失败 MUST 返回错误（与 `RAGRerankEnabled` 的 `strconv.ParseBool` 处理一致）。

---

## 5. 可观测字段（扩展既有 slog 行，不新开一行）

落点：`service.go` `Retrieve` 里既有的 `knowledge: retrieval candidate admission and dedup`。
触发条件相应放宽：原条件 **或** `filter_applied` 为真。

| 字段 | 类型 | 含义 | FR |
|---|---|---|---|
| `filter_applied` | bool | 本轮是否施加了过滤 | FR-017 |
| `filter_document_id_count` | int | 指定了几个 document_id（**不记 ID 本身**） | FR-017 / FR-018 |
| `filter_page_range_set` | bool | 是否指定了页码范围（**不记页码数值**） | FR-017 / FR-018 |
| `vector_candidate_count` | int | 向量路过滤后返回的候选数 | FR-017 |
| `keyword_candidate_count` | int | 关键词路过滤后返回的候选数 | FR-017 |
| `filter_zero_candidates` | bool | 施加了过滤且两路候选合计为 0 | FR-017 / US4 |

**FR-018 脱敏口径**：只记种类与数量，绝不记 document_id 取值、页码数值、query 原文、片段正文。
document_id 与页码都可能反推出文件身份，与 Phase 9 对逐条 rerank 分数的处理是同一个口径。

> 关于"过滤前后候选数量"（FR-017 的字面要求）：**"过滤前"的数量本期不记录，
> 这是一个有意的取舍**。要拿到它必须把两路召回各跑两遍（一遍带过滤、一遍不带），
> 而那正是 FR-007 禁止的"先召回再过滤"形态的一次数据库往返成本。
> 记录 `filter_applied` + 过滤后各路候选数 + `filter_zero_candidates` 已经能回答 US4 要求区分的
> 两件事（过滤没生效 vs 过滤生效但没答案）。此偏离在 plan.md 的复杂度追踪中登记。
