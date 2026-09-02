# 内部契约：检索元数据过滤

**Feature**: `002-metadata-filter` | **Date**: 2026-09-02

本期**不新增任何 HTTP 端点或请求参数**（spec Out of Scope：本期只提供后端能力与契约），
因此没有 HTTP 契约文件。以下是模块内与跨模块的调用契约。

---

## C1. `knowledge.Service.Retrieve`（跨模块契约）

```go
Retrieve(ctx context.Context, knowledgeBaseIDs []string, query string, topK int,
         opts RetrieveOptions) ([]RetrievedChunk, error)
```

**调用方**：`internal/conversation/context.go`、`internal/workflow/executor.go`。
本期两者都传 `RetrieveOptions{}`（零值）。

**契约条款**：

1. `opts` 为零值时，行为与本功能上线前**逐字一致**——返回的片段集合、顺序、`Score`、
   邻接扩展结果全部相同。这条是可断言的（SC-003），不是口头承诺。
2. `opts.Filter` 非空但 `RAGMetadataFilterEnabled=false` 时，返回 `ErrMetadataFilterDisabled`，
   **不执行任何检索**。绝不静默降级为无过滤检索（FR-009）。
3. `opts.Filter` 非法（文档数超上限、页码非正、min > max）时返回对应中文错误，
   **不截断、不检索**（FR-015）。
4. 过滤器合法且开关开启时，返回的每一个**anchor**（非邻接块）MUST 满足全部过滤条件。
5. 返回的**邻接块**MUST 满足文档级过滤，MUST NOT 被要求满足页码过滤（FR-011）。
   识别方式：`RetrievedChunk.NeighborOf != ""`。
6. 过滤器引用不存在的 `document_id` 不是错误——返回空结果（FR-010）。
7. 过滤**不改变** `RetrievedChunk.Score` 的语义，不改变 RRF 融合、准入阈值、去重行为（FR-012）。

**错误分类**：三个新错误都是 `apperr.InvalidInput`（400 类），
经既有的 `classifyRetrieveErr` 之外的路径直接返回——它们发生在任何数据库调用之前，
与 `classifyRetrieveErr` 处理的"检索过程中的数据库/上游故障"是不同类别，不得混入其降级逻辑。

---

## C2. `RetrieveFilter` 的语义（包内契约）

| 字段 | 空值 | 非空语义 | 条件间关系 |
|---|---|---|---|
| `DocumentIDs []string` | `nil` 或 `len==0` | chunk 的 `document_id` ∈ 该集合 | 集合内**或**；与其他字段**与** |
| `PageMin *int` | `nil` | `page_number >= *PageMin` | 与 `PageMax` 构成闭区间；与其他字段**与** |
| `PageMax *int` | `nil` | `page_number <= *PageMax` | 同上 |

FR-008 的落点：同一条件多取值是**或**（`document_id = ANY(...)`），
不同条件之间是**与**（三行独立的 `AND` 谓词）。

**`page_number IS NULL` 的 chunk**：只要指定了 `PageMin` 或 `PageMax` 中任意一个，
该 chunk **不匹配**。这依赖 SQL 三值逻辑（research.md R2），
MUST 由 `TestFilterPageRangeExcludesNullPageChunks` 锁定，
MUST NOT 被改写成 `COALESCE(page_number, 0)` 之类"给无页码的行编一个页码"的写法。

---

## C3. `Repository.searchVectorChunks` / `searchKeywordChunks`（包内契约）

各增加一个 `filter RetrieveFilter` 参数，**原样翻译**成 sqlc 可空参数：

| Go | sqlc 参数 | 空值时传 |
|---|---|---|
| `filter.DocumentIDs` | `filter_document_ids ::text[]` | `nil`（SQL 侧 `IS NULL` 恒真短路） |
| `filter.PageMin` | `filter_page_min ::int` | `sql.NullInt32{}` |
| `filter.PageMax` | `filter_page_max ::int` | `sql.NullInt32{}` |

**repository 层不做校验**（宪法第 IV 条：repository 只做 CRUD 不含业务判断）。
校验在 `service.Retrieve` 入口一次性完成，早于任何数据库调用。

---

## C4. 不变量清单（回归断言的目标）

本功能上线前后，以下每一条都 MUST 保持不变，且都有对应断言：

| 不变量 | 断言位置 |
|---|---|
| 空过滤器时检索输出逐字一致 | `eval_gate_test.go`（门禁全部既有用例 + 报告 JSON 比对） |
| `rrfFuse` 的融合/准入/去重行为 | 既有 `hybrid_test.go`/`admission_test.go`/`dedup_test.go` 全绿 |
| 邻接块两级 tier 不可交错 | 既有 `neighbor_test.go` 全绿 |
| PDF 页码产出链路 | 新增 `structure_test.go` 回归断言（FR-001 修订） |
| Citation 字段随 chunk 返回 | 既有 `hybrid_test.go`/`dedup_test.go` 全绿 |
