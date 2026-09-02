# Phase 0 研究：检索元数据过滤（Metadata Filtering）

**Feature**: `002-metadata-filter` | **Date**: 2026-09-02 | **Spec**: [spec.md](./spec.md)

本文件记录进入设计前必须先定下来的技术判断。每一条都写明**被否决的方案和否决理由**——
只写结论的研究文档没有价值，半年后没人知道为什么不能用另一种写法。

---

## R1. 可选过滤条件如何进入 sqlc 的静态 SQL

**约束**：FR-007 要求过滤下推到两路召回 SQL；FR-016 要求过滤条件当作数据、不得字符串拼接；
仓库用 sqlc 从 `internal/db/pgqueries/chunks.sql` 生成代码，SQL 文本是**编译期静态**的。
这三条合起来排除了"按过滤器形状动态拼 WHERE"这个最直觉的做法。

| 方案 | 结论 |
|---|---|
| A. 按过滤组合写多条查询（无过滤/仅文档/仅页码/两者） | ❌ 否决。两个维度就要 4 条，第三个维度来时 8 条，且每条都要重复维护 `ORDER BY`、`LIMIT`、SELECT 列表和那些解释性注释——四份必须逐字同步的 SQL 是确定性的敌人 |
| B. 在 Go 里拼 WHERE 片段，绕开 sqlc | ❌ 否决。直接违反 FR-016，也丢掉 sqlc 的静态类型检查——`chunks.sql` 现有注释反复强调"SQL 文本本身不包含任何调用方数据"，不能为这个功能开例外 |
| C. **可空参数 + 恒真短路谓词**（选定） | ✅ 一条静态 SQL 覆盖全部组合，参数化绑定，无拼接 |

**选定写法**：

```sql
AND (sqlc.narg(filter_document_ids)::text[] IS NULL
     OR document_id = ANY(sqlc.narg(filter_document_ids)::text[]))
AND (sqlc.narg(filter_page_min)::int IS NULL OR page_number >= sqlc.narg(filter_page_min)::int)
AND (sqlc.narg(filter_page_max)::int IS NULL OR page_number <= sqlc.narg(filter_page_max)::int)
```

`sqlc.narg` 生成可空参数（`[]string` / `sql.NullInt32`）。未指定该维度时传 NULL，
`NULL IS NULL` 为 TRUE，整个 OR 短路成恒真，等价于这一行谓词不存在。

**这为什么满足 FR-013 / SC-003（关闭时逐字一致）**：全部参数为 NULL 时三条谓词都是常量 TRUE，
**结果集合与现在完全相同**。行序也不受影响——两条召回查询的 `ORDER BY` 都以 `id ASC` 收尾
（见 `chunks.sql` 中 `SearchVectorChunks` 关于 rank 稳定性的注释），最终排序键与查询计划无关，
即使 PostgreSQL 因为多了三个常量谓词而选了不同的扫描方式，返回顺序依然是确定的同一个。

---

## R2. 页码为 NULL 的 chunk 在页码过滤下如何表现

**要求**（Edge Cases + FR-014 修订）：非 PDF 文档、以及 `000003` 迁移之前入库的存量行，
`page_number` 为 NULL，页码过滤时 **MUST 视为不匹配**，MUST NOT 当作"无元数据即通过"。

**结论：R1 的写法天然满足这一条，不需要任何额外谓词。** 依据是 SQL 的三值逻辑：
`page_number >= 10` 在 `page_number IS NULL` 时求值为 NULL（不是 FALSE），
`OR` 的另一侧 `filter_page_min IS NULL` 此时为 FALSE，`FALSE OR NULL` = NULL，
而 `WHERE` 只接受 **TRUE**，NULL 与 FALSE 一样被排除。

**明确不写 `page_number IS NOT NULL AND ...`**：那是一个冗余谓词。但"依赖三值逻辑"是一个
容易被后来者误改的隐式约定（典型的错误修法是加 `COALESCE(page_number, 0)`，那会把无页码的
chunk 变成第 0 页，直接违反"绝不伪造"），因此这条依赖 MUST 由一条命名明确的测试锁定
（tasks 里的 T0xx `TestFilterPageRangeExcludesNullPageChunks`），而不是只靠 SQL 注释。

---

## R3. 邻接块（Phase 4/7）需要改多少代码

**要求**（FR-011）：邻接块必须满足文档级过滤，但豁免 chunk 级（页码）过滤。

**结论：需要的代码改动量是零；需要的是一条断言。** 推导如下：

1. 邻接坐标由 `neighbor.go` 的 `buildNeighborRequests` 从 **anchors 自身**的
   `(document_id, document_version, chunk_index±1)` 生成。
2. anchors 是两路召回 → 融合 → 准入 → 去重 → 重排 → 截断的产物，**每一个 anchor 都已经满足
   文档级过滤**（过滤在第一步的 SQL 里就生效了）。
3. `FindPublishedNeighborChunksBatch` 按 `document_id` 等值 JOIN，
   所以取回的邻接块**必然与某个 anchor 同属一份文档**。

因此文档级过滤对邻接块是**结构性满足**的——不是"我们记得也加了一遍"，而是"它不可能不满足"。
这与 `chunks.sql` 里 `FindPublishedNeighborChunks` 已有的论证同构（那条注释说版本隔离是
"WHERE 条件本身的结构性保证，不需要额外代码"）。

而 chunk 级（页码）豁免的实现方式就是**不给这条邻接查询加页码谓词**——豁免是默认状态，
需要主动做的事是"不做"。真正的风险是将来有人"顺手统一一下"给邻接查询也加上过滤条件，
所以这一条 MUST 由集成测试锁定：页码范围过滤命中一个 anchor 时，
它落在范围外的邻接块**仍然必须出现在结果里**。

---

## R4. 关闭开关时，调用方传了非空过滤器怎么办

这是 FR-013（可整体关闭）与 FR-009（禁止悄悄忽略用户指定的过滤条件）之间一个真实的冲突点，
起草时没有被显式回答。

| 方案 | 结论 |
|---|---|
| A. 开关关闭时静默忽略过滤器，照常做无过滤检索 | ❌ 否决。这正是 FR-009 和 spec Clarifications 里"我限定了范围但系统偷偷用了范围外的资料"要防的那件事，只不过原因从"候选不足"换成了"开关没开" |
| B. 开关关闭时返回明确错误（选定） | ✅ 空过滤器 + 开关关闭 = 今天的行为，逐字一致；非空过滤器 + 开关关闭 = 明确失败，不产生任何会被误信的结果 |

**选定 B**。开关的语义因此被精确定义为：**关闭的是"接受过滤请求"这个能力，而不是"过滤是否生效"**。
这样 FR-013 与 FR-009 都能满足，且没有任何输入组合会产生"看起来正常但范围被悄悄放宽"的输出。
两个现有调用方（`conversation`、`workflow`）传零值过滤器，因此开关默认关闭时它们完全不受影响。

---

## R5. 要不要为过滤新建索引

**结论：不新建任何索引。** 依据分两路：

- **向量路**：`SearchVectorChunks` 的形态是 `ORDER BY embedding <=> $1 LIMIT n`，
  且 `embedding` 列**故意不声明维度**因而建不了 HNSW/IVFFlat（见 `000001_chunks.up.sql` 的长注释，
  该取舍本期不动）。这条查询今天就是知识库内的顺序扫描 + 精确打分。加上 `document_id` 过滤后，
  参与打分的行只会**更少**，查询只会更快——不存在"需要索引才能不退化"的情形。
- **关键词路**：`SearchKeywordChunks` 由 `content <% $1` 走 `idx_chunks_content_trgm` 这个 GIN
  索引筛出候选集，过滤条件是候选集上的**残余谓词**（residual predicate），在已经被索引缩小到
  至多 `candidate_k` 数量级的行上求值，成本可忽略。

另外 `idx_chunks_document_id` 本就存在（`000001`），每知识库 5000 chunks 的软上限也没有变化。
为一个残余谓词建索引属于无依据的优化，与宪法第 IX 条最小范围相悖。

**本期不新增 migration。** 这是本功能一个值得强调的结论：过滤所需的两个列
（`document_id`、`page_number`）**都已经存在于 `chunks` 表**，
所以整个功能没有任何 schema 变更（对照宪法第 IV 条的 `migration → sqlc` 顺序：
本期从 `sqlc` 这一步开始，因为没有 migration 要写）。

---

## R6. 过滤器的上限取多少

FR-015 要求过滤器有数量上限且超限不得静默截断（静默截断 = 悄悄放宽范围 = 违反 FR-009）。

- `document_id` 条数上限取 **50**。参照系：`clampTopK` 的 `maxTopK` 是 50，
  `candidateK` 的硬上限是 100。一次检索指定超过 50 份文档，语义上已经不是"缩小范围"了。
- 页码范围校验：下界与上界都 MUST 为正整数（页码是 1-indexed，见 `parse.go` 的 `pdfPage.Number`），
  且 `min <= max`。可以只给一端（"第 10 页之后"是合理诉求）。
- 超限/非法 MUST 返回一个用户可读的中文错误（宪法第 VII 条），MUST NOT 截断后继续。

---

## R7. 可观测落点

沿用 001-rag-query-rerank 的做法：**不新开 slog 行，并进 `Retrieve` 里既有的那一行**
（`knowledge: retrieval candidate admission and dedup`）。理由与当时相同——同一件事
（这一轮 Retrieve 的候选池发生了什么）只记一次。

FR-018 的脱敏口径是硬约束：记**种类与数量**，不记取值。
具体地，记 `filter_document_id_count`（几个）而**不记** document_id 本身，
记 `filter_page_range_set`（布尔）而**不记**页码数值——spec 明确指出取值可能含可识别信息。
新增字段清单见 [data-model.md](./data-model.md) §4。

---

## R8. 效果如何度量（以及为什么本期只能给出机制证明）

**本期无法产出真实的效果幅度数字，报告中 MUST 如实标注为"机制证明"。** 理由：

1. 过滤是**布尔的范围缩小**，不改变打分（FR-012）。它的"效果"完全取决于用户是否指定了
   *正确的*范围——这是调用方的输入质量问题，不是本功能的质量问题。
2. 要度量"元数据过滤提升了检索质量"，需要一份带有"每个问题的正确文档/页码是哪个"标注的
   真实语料。仓库没有这样的语料，`eval/testset.yaml` 也不含范围标注。
3. `make eval` 带 LLM 裁判，同一份代码跑两次都不一致（宪法"技术栈与工程约束"已明确禁止
   用它证明行为未变），更不能用来给一个布尔过滤功能编造提升百分比。

因此本期的验收全部落在**确定性检索门禁**（`make eval-retrieval-gate`）能证明的机制性断言上：
过滤生效、下推发生在召回阶段、空过滤器逐字一致、邻接豁免成立。
这些是"代码验证过"；"效果验证过"本期一条都没有，报告里不得混淆。
