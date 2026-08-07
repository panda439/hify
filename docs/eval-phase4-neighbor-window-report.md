# Phase 4：邻接分块扩展（Neighbor Window Retrieval）验证报告

日期：2026-08-07（二次更新：修复审核发现的预算优先级问题，见第 0 节）。基线：761b97b（Eval + 结构感知分块）+ Hybrid Search（Phase 3，本地记为 c55dd4e，本次会话所在沙箱的 git 历史里以未提交工作区改动的形式存在，内容与该提交等价）。本报告只覆盖 Phase 4 本身新增的内容，不重复 Phase 3 报告（`docs/eval-phase3-hybrid-search-report.md`）已经说清楚的 Hybrid Search 设计。

## 0. 二次更新：修复审核发现的预算优先级问题

**问题**：初版 `expandWithNeighbors` 的输出顺序是"每个核心块紧跟自己的邻接块"（`anchor1, anchor1.previous, anchor1.next, anchor2, anchor2.previous, anchor2.next, ...`）。`conversation/budget.go` 的 `selectEvidence` 严格按输入顺序贪心消耗预算，不回看、不重排。在预算只够容纳两个完整 source 的场景下，这个顺序会导致 `anchor1` 的邻接块排在 `anchor2` 前面被消费，`anchor2`——一个真实的、排名第二的核心检索命中——因为预算已经被 `anchor1` 的邻接块占用而被过滤掉。这直接违反了三条既定原则："邻接块不能改变核心结果排名"、"邻接块不能把核心块挤出结果"、"预算不足时核心块优先于所有邻接块"。

**修复**：把输出从"每核心块一组紧跟其邻接块"改成两个完整分层——所有核心块（保持原始 RRF 排名）全部排在前面，所有邻接块（按所属核心块排名分组，组内仍是 previous、next 顺序）整体排在后面：

```
anchor1, anchor2, anchor3, ...,
anchor1.previous, anchor1.next, anchor2.previous, anchor2.next, anchor3.previous, anchor3.next, ...
```

因为 `selectEvidence` 是严格按输入顺序的贪心消费者，这个两层布局保证了任何邻接块在输入序列里的位置都不可能早于任何核心块——预算不足时，`selectEvidence` 必然先把所有核心块过一遍（能放下就放，放不下就跳过继续试下一个核心块），再考虑第一个邻接块。一个邻接块不可能抢在任何核心块之前消耗预算，因此也不可能把任何核心块挤出结果。

**改动范围**：只改了 `internal/knowledge/neighbor.go` 里 `expandWithNeighbors` 的组装顺序（dedup 规则、Score 继承规则、`NeighborOf` 赋值规则、`anchorCount*3` 上界全部不变，见第 8 节"去重和 NeighborOf 是否保持"的验证），以及相关文档/注释里对旧顺序的错误描述（`neighbor.go`、`internal/conversation/context.go`、本文件、`README.md`/`docs/critical-paths.md` 里的引用性描述）。新增 `internal/conversation/budget_test.go` 的预算回归测试（见第 8 节）。没有扩展到 LLM Reranker、动态窗口、Citation 协议改动等范围之外的内容，也没有提高 `ragBudgetTokens`/RAG 字符预算。

## 1. 为什么邻接分块扩展不需要 LLM

Phase 4 要解决的问题是纯粹的检索完整性问题，不是理解问题：Hybrid Search 命中的是"和 query 最相关的那个 chunk"，但一个 chunk 的切分边界是分块器按字数/结构启发式划的，不是按语义完整性划的——真实答案经常跨越这条边界，命中的半句话前面缺主语、后面缺结论。要补全这段上下文，需要的是"这个 chunk 在原文档里前后紧邻的是哪一段"，这个问题的答案完全由 `(document_id, document_version, chunk_index)` 三元组决定，是一次确定性的数据库查询，不需要任何语义判断、排序判断或生成能力。

反过来说，这也是本阶段刻意不做 LLM Reranker、不做 Query Rewrite/Multi Query/HyDE 的原因：那些技术解决的是"检索到的候选是不是真的相关"这个语义问题，Phase 4 要解决的是"已经确认相关的那个 chunk，边上还有没有没被检索到的必要上下文"这个结构问题，两者是正交的，混在一起做只会让这次改动的验证边界变得模糊——真实结果掺进了不确定性输出，本来能用确定性单测和真实 DB 集成测试锁死的行为，会退化成"看起来合理但没法断言具体值"。

## 2. 核心块（anchor）和邻接块（neighbor）的区别

| | 核心块（`NeighborOf == ""`） | 邻接块（`NeighborOf != ""`） |
|---|---|---|
| 怎么来的 | Hybrid Search（向量 `<=>` + pg_trgm word-similarity + RRF 融合）直接命中 | `findPublishedNeighborChunks` 按核心块的 `document_id`/`document_version`/`chunk_index±1` 查出来的，从未参与过向量或关键词打分 |
| 排名 | rrfFuse 决定的全局相关度排名，Phase 4 绝不改变这个排名 | 没有独立排名——固定跟在拉它进来的那个核心块后面 |
| `RetrievedChunk.Score` | 真实测量的 0..1 相关度（向量命中=余弦相似度，关键词命中=word-similarity，两路命中取较大值） | 继承所属核心块的 Score，不是它自己的相关度（见第 5 节） |
| `RetrievedChunk.NeighborOf` | `""` | 所属核心块的 chunk ID |
| Citation 元数据（`DocumentName`/`PageNumber`/`SectionTitle`） | 自己的真实值 | 自己的真实值（不是复制核心块的，见 `TestExpandWithNeighborsKeepsNeighborsOwnCitationMetadata` / `TestIntegrationFindPublishedNeighborChunksPreservesCitationMetadata`） |
| 能不能把核心块挤出结果 | — | 不能——`expandWithNeighbors` 的 dedup 规则保证一个 chunk 如果本身是某个核心块，它只会以核心块身份出现一次，绝不会先被当成别的核心块的邻接块插入（见 `TestExpandWithNeighborsNeighborThatIsAlsoAnAnchorAppearsOnlyOnceAtItsOwnRank`） |

## 3. topK 的新语义

Phase 3 里 `Retrieve(ctx, kbIDs, query, topK)` 返回的切片长度恒 `<= topK`。Phase 4 之后这个恒等式不再成立：

- `topK` 现在只控制一件事——Hybrid Search 挑出多少个核心命中块（anchor 数量），语义和 Phase 3 完全一样，`clampTopK`/`maxTopK`/`defaultTopK` 都没变。
- 每个核心块最多再带 2 个邻接块（前一个、后一个），所以最终返回的切片长度上界是 `topK*3`，不是 `topK`。
- `conversation/context.go` 的 `retrievalTopK=5` 常量含义相应更新：它仍然是"要几个核心命中"，不是"最终要几个 chunk"——doc 注释已经改写，明确写了 `len(candidates)` 现在可能到 `retrievalTopK*3`。
- `knowledge.Service` 接口上 `Retrieve` 的 doc 注释、`service.go` 里 `Retrieve` 函数体的 doc 注释都同步更新，写明这个不再成立的旧假设，避免以后有人凭直觉认为"topK=5 意味着最多 5 条"。
- **准确的顺序描述（二次更新，见第 0 节）**：`topK` 个核心块全部排在最前面，保持它们各自完整的 RRF 排名；邻接块整体属于第二优先级上下文，作为一个单独的、排在全部核心块之后的区块出现，不与任何核心块交替。`conversation/budget.go` 的 `selectEvidence` 没有改代码——它本来就是对输入切片做"分数过滤 → 去重 → 按输入顺序贪心填充预算"，因为 `Retrieve` 现在把全部核心块放在全部邻接块之前，`selectEvidence` 严格按输入顺序消费预算这件事，天然就等价于"先把预算优先分给核心块，核心块用不完的预算才轮到邻接块"——不需要 `selectEvidence` 认识 anchor/neighbor 这两个概念,也不需要提高 RAG 字符预算（`ragBudgetTokens` 未变）。旧版本"每个核心块紧跟自己的邻接块"的描述是错误的,已在第 0 节记录修复过程,不应再被引用。

## 4. 如何防止跨 document version 混合

这是本阶段唯一的高风险点，设计上用了三层防线，不是单点保证：

1. **数据模型层**：`Chunk` 新增内部字段 `DocumentVersion int64`，`SearchVectorChunks`/`SearchKeywordChunks` 的 SELECT 列表都加上了 `document_version`（`internal/db/pgqueries/chunks.sql`，真实 `make sqlc` 重新生成，未手改生成代码），`repository.go` 的 `searchVectorChunks`/`searchKeywordChunks` 正确把它映射进返回的 `RetrievedChunk`。核心块从被检索出来的那一刻起，就带着它属于哪一次处理尝试的信息，不需要额外一次查询去反查。
2. **分组层**：`neighbor.go` 的 `buildNeighborGroups` 按 `(document_id, document_version)` 二元组分组，不是只按 `document_id`。同一个文档的两次处理尝试（重新处理产生的新旧版本）永远落在不同的组里，各自只查自己那个版本需要的 index。
3. **SQL 层（最终防线）**：`FindPublishedNeighborChunks` 的 `WHERE` 子句里 `document_id = $1 AND document_version = $2` 两个条件都是硬性的，不是可选过滤。如果一个核心块的所属版本在检索返回之后、邻接查询发起之前，恰好被重新处理流程（`publishDocumentVersion`/`DeleteObsoleteChunkVersions`，同一个 PG 事务里把旧版本物理删除）清空了，这条 SQL 会因为 `document_version` 条件天然匹配不到任何行，返回空集合——不会、也没有办法退化成去匹配新版本里那个 index 的 chunk，因为查询里根本没有"找不到就换个 version 再试"这种逻辑。`TestIntegrationFindPublishedNeighborChunksOldVersionDeletedReturnsEmpty` 用真实的 `publishDocumentVersion` 调用序列（不是伪造的测试专用状态）复现了这个场景：先发布 v1，用 v1 查到邻接块，再发布 v2（这一步会真的删除 v1 的所有行），再用 v1 查同一个 index，断言返回空；对照用 v2 查同一个 index，断言能查到新版本的内容——证明"空"是版本隔离生效，不是查询写错了。

`TestIntegrationFindPublishedNeighborChunksExcludesOtherVersions` 额外用原始 SQL 构造了一个正常生产代码路径永远不会产生的状态（同一个 `document_id` 下两个 `document_version` 同时 `is_published=true`），专门压测 `WHERE document_version = $2` 这一个条件本身，不依赖"两个版本不会共存"这个假设侥幸通过。

## 5. Score 继承的真实含义

`RetrievedChunk.Score` 的文档注释（`model.go`）已经改写为按 `NeighborOf` 分两种语义描述,这里摘要:

- 核心块的 Score 是真实测量值——向量命中是余弦相似度，关键词命中是 word-similarity，两路命中取较大值，这条规则从 Phase 3 延续下来没有变化。
- 邻接块从来没有独立经过向量或关键词检索，没有自己的余弦相似度或 word-similarity 可言。它的 Score 是`expandWithNeighbors`赋值时直接拷贝自它所属核心块的 Score——用于满足 `conversation/budget.go` 的 `ragMinSimilarityScore` 门槛判断，避免邻接块因为 Score 恒为 0 被门槛无差别刷掉。真正决定邻接块相对核心块的预算优先级的是第 0/3 节说明的两层输出顺序（全部核心块在前，全部邻接块在后），不是 Score 数值本身——`ragMinSimilarityScore` 门槛判断和输出顺序是两件独立的事，继承 Score 只保证邻接块不会被门槛拒之门外，不代表它能在预算竞争里和核心块平起平坐。

这条继承规则被反复强调"不能称为邻接块自己的余弦相似度或关键词相似度"，是因为如果不把这个边界讲清楚，未来任何读这段代码或读 Trace/SSE 调试信息的人都可能误以为 Phase 4 悄悄给邻接块也做了一次独立打分——实际上完全没有，`findPublishedNeighborChunks` 返回的行 Score 字段是零值，`expandWithNeighbors` 才是唯一赋值的地方。`RetrievedChunk.NeighborOf` 就是用来区分这两种语义的判别字段：空字符串走"真实相关度"语义，非空走"继承的预算优先级"语义。

Citation/SSE 协议没有变化——`conversation.Evidence` 类型没有新增字段，`NeighborOf`/`DocumentVersion` 都是 `knowledge.RetrievedChunk`/`Chunk` 的内部字段,不对外暴露。一条邻接块变成 Evidence 之后，它的 `DocumentName`/`PageNumber`/`SectionTitle` 依然是它自己的真实来源信息,不是复制核心块的——这一点在 Citation 上比 Score 更重要,因为 Score 只影响排序/过滤,Citation 元数据错了是直接把错误的出处展示给用户。

## 6. 失败降级方式

`service.go` 新增的 `expandWithNeighborWindow` 遵循和 Phase 3 的 Hybrid Search 完全一致的 best-effort 约定，不是另起一套规则:

- **按 `(document_id, document_version)` 分组隔离失败**：一个分组的 `findPublishedNeighborChunks` 调用失败，只丢弃这一组的邻接块，其他分组（其他文档版本）和全部核心块不受影响，继续往下走。
- **邻接扩展整体失败不影响核心结果**：如果所有分组都失败（比如数据库连接整体不可用），`expandWithNeighborWindow` 返回的就是原封不动的 `anchors`——`expandWithNeighbors(anchors, nil)` 在没有任何邻接块输入时的行为就是"原样返回核心块"，这是纯逻辑函数自身的规则（第 9 节单测第 13/14 项覆盖），不需要 `expandWithNeighborWindow` 再额外写一次"失败了就返回 anchors"的分支逻辑。
- **日志不带 query 或正文**：失败时只记录 `err`、`document_id`、`document_version`，不记录 chunk 内容或用户的检索 query，和这个文件里其他 `slog.Warn` 调用的约定一致。
- **`context.Canceled`/`context.DeadlineExceeded` 必须传播，不能被当成"这个分组失败了"吞掉**：复用 Phase 3 已有的 `classifyRetrieveErr`，一旦某个分组的查询返回的错误被判定为 ctx 本身已经结束，`expandWithNeighborWindow` 立刻返回 `(nil, err)`，`Retrieve` 把这个错误原样透传给调用方，不会先把已经处理过的其他分组的邻接块拼好再返回一个"看起来正常"的结果。`TestIntegrationExpandWithNeighborWindowPropagatesContextCancellation` 直接对一个已取消的 context 调用这个方法，断言返回的 err 满足 `errors.Is(err, context.Canceled)`。
- **一个真实的、非取消性质的数据库错误必须能触发降级而不是被误判成需要传播**：`TestIntegrationExpandWithNeighborWindowDegradesToAnchorsOnOrdinaryFailure` 用一个真实的 `*sql.DB`（不是 mock）连一个没有监听者的端口，产生一个真正的驱动层拒绝连接错误，断言 `expandWithNeighborWindow` 返回 `(anchors, nil)`——错误被识别为"不是 context 取消"，走 best-effort 降级路径，不会被 `classifyRetrieveErr` 误判成需要向上传播。

## 7. 修改文件

- `internal/db/pgqueries/chunks.sql`：`SearchVectorChunks`/`SearchKeywordChunks` 补充 `document_version` 到 SELECT 列表；新增 `FindPublishedNeighborChunks` 查询。
- `internal/db/pggen/chunks.sql.go`、`internal/db/pggen/querier.go`：真实 `make sqlc` 重新生成，未手改。
- `internal/knowledge/model.go`：`Chunk` 新增 `DocumentVersion int64`；`RetrievedChunk` 新增 `NeighborOf string`，`Score` 的 doc 注释按 anchor/neighbor 两种语义重写。
- `internal/knowledge/repository.go`：`searchVectorChunks`/`searchKeywordChunks` 正确映射 `DocumentVersion`；新增 `findPublishedNeighborChunks`。
- `internal/knowledge/neighbor.go`（新文件；二次更新修复了 `expandWithNeighbors` 的输出顺序，见第 0 节）：`neighborIndexesFor`、`neighborGroupKey`/`buildNeighborGroups`、`neighborLookupKey`、`expandWithNeighbors`——本阶段的核心纯逻辑，DB-free。
- `internal/knowledge/neighbor_test.go`（新文件）：邻接扩展的纯逻辑单测（二次更新调整了部分断言以匹配新输出顺序，规则覆盖范围不变，见第 8 节）。
- `internal/knowledge/service.go`：`Retrieve` 在 `rrfFuse` 之后接入 `expandWithNeighborWindow`；新增 `expandWithNeighborWindow` 方法；`Service` 接口和 `Retrieve` 函数体的 doc 注释更新 topK 新语义（二次更新修正了其中对输出顺序的描述）。
- `internal/knowledge/model.go`（二次更新）：`RetrievedChunk.Score` 的 doc 注释修正了对输出顺序的描述（不再说"紧跟在 anchor 后面"）。
- `internal/knowledge/integration_test.go`：新增 Phase 4 的 11 个 PostgreSQL 集成测试及配套 seed 辅助函数。
- `internal/conversation/context.go`：`retrievalTopK` 的 doc 注释更新，说明返回切片长度可能到 `retrievalTopK*3`（二次更新修正了对输出顺序的描述，补充说明两层布局如何让 `selectEvidence` 天然先消费核心块）。
- `internal/conversation/budget_test.go`（二次更新，新增）：预算优先级回归测试（见第 8 节），验证代码本身仍是零改动——`selectEvidence`/`renderedSourceLen`/`truncateEvidenceToFit` 都未改。
- `README.md`、`docs/critical-paths.md`：更新 RAG 全流程描述和链路 3 的说明/测试覆盖表格（二次更新修正了其中对输出顺序的描述）。
- `docs/eval-phase4-neighbor-window-report.md`（本文件；二次更新记录本次预算优先级修复）。

未修改：`internal/conversation/budget.go` 本体（`selectEvidence`/`renderedSourceLen`/`truncateEvidenceToFit` 均未改一行代码——第 0 节的修复完全在 `knowledge` 包内完成，`selectEvidence` 严格按输入顺序贪心消费预算的既有行为，配合 `expandWithNeighbors` 修复后的两层输出顺序，天然产生"核心块优先于全部邻接块"的效果，见第 0/3 节）、Citation 协议、SSE 事件格式、前端。

## 8. 实际测试结果

最终验收在本机 Docker 环境中完成，MySQL、PostgreSQL + pgvector/pg_trgm、Redis 均真实可用。全量测试以及本阶段新增的 PostgreSQL 集成测试均实际执行，没有因数据库不可达而跳过：

```
gofmt -l <本次修改的 Go 文件>          # 无输出，已全部格式化
make sqlc                             # 重新生成成功，diff 与手写查询预期一致，未手改生成代码
go vet ./...                          # 0 diagnostics
go test -count=1 ./...                # 全部 PASS，真实连接 MySQL/PostgreSQL
go test -race -count=1 ./...          # 全部 PASS，无 race
make check-deps                       # OK，无跨层/同层依赖违规
git diff --check                      # 无输出（无空白符问题）
git status --short                    # 只有本阶段应改动的文件，无 _to_delete/、hify_stage2.tar.gz 等误动
```

**纯逻辑单测（`neighbor_test.go`，18 个，全部真实执行且 PASS，无 DB 依赖）**：

`TestNeighborIndexesForBoundaries`、`TestBuildNeighborGroupsMergesSameDocumentVersionAnchors`、`TestExpandWithNeighborsFillsPreviousAndNext`（对应要求 1）、`TestExpandWithNeighborsChunkIndexZeroOnlyGetsNext`（要求 2）、`TestExpandWithNeighborsLastChunkOnlyGetsPrevious`（要求 3）、`TestExpandWithNeighborsSkipsMissingIndexes`（要求 4）、`TestExpandWithNeighborsPreservesAnchorRank`（要求 5）、`TestExpandWithNeighborsNeighborThatIsAlsoAnAnchorAppearsOnlyOnceAtItsOwnRank`（要求 6）、`TestExpandWithNeighborsSharedNeighborAttributedToHigherRankedAnchor`（要求 7）、`TestExpandWithNeighborsInheritsOwningAnchorScore`（要求 8）、`TestExpandWithNeighborsSetsNeighborOfToOwningAnchorID`（要求 9）、`TestExpandWithNeighborsKeepsNeighborsOwnCitationMetadata`（要求 10）、`TestExpandWithNeighborsIsDeterministicAcrossRuns`（要求 11）、`TestExpandWithNeighborsNeverExceedsAnchorCountTimesThree`（要求 12）、`TestExpandWithNeighborsEmptyAnchorsReturnsEmpty`（要求 13）、`TestExpandWithNeighborsEmptyNeighborsKeepsAnchorsUnchanged`（要求 14）。规格要求的 14 项纯逻辑场景全部覆盖，另加 2 个分组逻辑（`neighborIndexesFor`/`buildNeighborGroups`）的边界测试。

**PostgreSQL 集成测试（真实对本机 pgvector+pg_trgm 执行，11 个，全部 PASS）**：

1. `TestIntegrationFindPublishedNeighborChunksReturnsPreviousAndNext` — 查到同文档同版本前后 chunk，断言具体 ID。
2. `TestIntegrationFindPublishedNeighborChunksExcludesOtherDocuments` — 不返回其他文档的相同 chunk_index。
3. `TestIntegrationFindPublishedNeighborChunksExcludesOtherVersions` — 不返回其他 document_version（原始 SQL 构造两版本共存场景压测 WHERE 条件本身）。
4. `TestIntegrationFindPublishedNeighborChunksExcludesUnpublished` — 不返回 is_published=false。
5. `TestIntegrationFindPublishedNeighborChunksChunkZeroNoNegativeIndexQuery` — chunk_index=0 时查询数组不含负数，正常返回。
6. `TestIntegrationFindPublishedNeighborChunksOrderedByChunkIndexThenID` — 返回顺序 chunk_index ASC，附加 id ASC 兜底的防御性验证。
7. `TestIntegrationFindPublishedNeighborChunksPreservesCitationMetadata` — document_name/page_number/section_title 是邻接块自己的真实值。
8. `TestIntegrationSearchVectorAndKeywordChunksReturnDocumentVersion` — 向量、关键词两路都正确返回 document_version。
9. `TestIntegrationFindPublishedNeighborChunksOldVersionDeletedReturnsEmpty` — 用真实 `publishDocumentVersion` 调用序列模拟文档重新处理，旧版本邻接查询返回空，不串新版本。
10. `TestIntegrationExpandWithNeighborWindowDegradesToAnchorsOnOrdinaryFailure` — 真实数据库连接失败（拒绝连接，非 mock）时 Service 降级返回核心块。
11. `TestIntegrationExpandWithNeighborWindowPropagatesContextCancellation` — context 取消正确通过 `errors.Is` 传播。

规格要求的 11 项集成场景全部覆盖，且断言的是具体 ID/版本号/顺序/字段值，不是只断言数量。

**二次更新（预算优先级修复，见第 0 节）的真实测试结果**：本轮改动只涉及 `neighbor.go`（`expandWithNeighbors` 重排为两层输出）、`neighbor_test.go`（更新 1 个既有断言 + 新增 1 个两层顺序测试）、`conversation/budget_test.go`（新增 5 个预算回归测试）以及若干文档/注释；重新完整执行了第十三节要求的全部验证命令，结果如下：

```
gofmt -w internal/knowledge/neighbor.go internal/knowledge/neighbor_test.go \
         internal/knowledge/model.go internal/knowledge/service.go \
         internal/conversation/context.go internal/conversation/budget_test.go
                                       # 无残留 diff，全部已是规范格式
make sqlc                             # 重新生成，和已有生成代码无 diff（本轮未改任何 SQL）
go vet ./...                          # 0 diagnostics
go test -count=1 ./...                # 全部 PASS；Codex 最终验收时真实连接 MySQL/PostgreSQL
go test -race -count=1 ./...          # 全部 PASS，无 race
make check-deps                       # OK，无跨层/同层依赖违规
git diff --check                      # 无输出
git status --short                    # 只有本轮应改动的 8 个文件，无 go.mod/go.sum 残留（临时 replace 已还原），
                                       #   无 _to_delete/、tar 包等误动
```

`internal/knowledge/neighbor_test.go` 变化：`TestExpandWithNeighborsSharedNeighborAttributedToHigherRankedAnchor` 的期望顺序从 `[a1, n-shared, a2]` 更新为 `[a1, a2, n-shared]`（PASS）；新增 `TestExpandWithNeighborsAllAnchorsPrecedeAllNeighbors`（3 个不同文档的 anchor + 6 个乱序邻接行，断言精确输出顺序为 `anchor1, anchor2, anchor3, anchor1.previous, anchor1.next, anchor2.previous, anchor2.next, anchor3.previous, anchor3.next`，PASS）。其余 16 个既有纯逻辑测试原样重跑，全部仍 PASS，未受两层重排影响（`NeighborOf`/Score 继承/去重/边界场景与顺序无关）。

`internal/conversation/budget_test.go` 新增 5 个测试，`go test -count=1 -v ./internal/conversation/...` 确认全部 PASS：

1. `TestSelectEvidenceNeighborNeverDisplacesALowerRankedAnchor` —— 复现规格给出的 anchor1/neighbor1(anchor1)/anchor2 场景，预算只够两条来源：结果为 `[anchor1, anchor2]`，`neighbor1` 因预算不足被过滤（`filteredByBudget == 1`），验证邻接块不会挤占排名更低但仍是核心命中的 anchor2 的预算名额。
2. `TestSelectEvidenceSufficientBudgetKeepsAllAnchorsThenAllNeighborsInOrder` —— 预算充足（2 个 anchor + 4 个邻接块）时，输出顺序精确为 `[anchor1, anchor2, anchor1-prev, anchor1-next, anchor2-prev, anchor2-next]`，引用编号 S1..S6 对应正确。
3. `TestSelectEvidenceBudgetForAllAnchorsFiltersEveryNeighbor` —— 预算恰好只够 2 个 anchor：`refs == [S1, S2]`，4 个邻接块全部因预算过滤（`filteredByBudget == 4`）。
4. `TestSelectEvidenceBudgetTooSmallForAllAnchorsStillTriesEachAnchorInRankOrder` —— anchor1（内容过大放不下）+ anchor2（能放下）+ anchor1 的一个邻接块，预算只够 anchor2：结果为 `[anchor2]`，`filteredByBudget == 2`（anchor1 本身和它的邻接块都被过滤），验证 `selectEvidence` 现有的"跳过后继续尝试下一个"逻辑天然满足"预算不够放下全部核心块时仍按 RRF 排名逐个尝试"这条规则，没有改 `selectEvidence` 本身的代码。
5. `TestSelectEvidenceAnchorOutcomeUnaffectedByNeighborOrder` —— 同一批 anchor+邻接块，邻接块的子顺序打乱两种排列方式输入，两次都产出完全相同的 `[anchor1, anchor2]` 结果，验证邻接查询返回顺序的任何变化都不影响最终核心块的取舍结果。

**回归确认**：Phase 3 原有的全部单测（`hybrid_test.go`）和集成测试（`integration_test.go` 里 Phase 3 部分）在本轮改动后重新跑过，全部仍然 PASS，没有因为 `SearchVectorChunks`/`SearchKeywordChunks` 新增 `document_version` 列或 `RetrievedChunk` 新增 `NeighborOf` 字段而回归。

**仍未专门覆盖的组合场景**：Codex 已在完整 Docker 环境中运行全量测试，既有 `Service.Retrieve` 集成测试和本阶段邻接查询/扩展测试均通过；不过目前仍没有一条专门的集成测试在同一个用例中从 `Retrieve(...)` 发起，明确断言“Hybrid Search 产出 anchors → 邻接查询 → 两层最终输出”的完整结果。现有测试分别覆盖了两段链路，代码已通过验收，但这项交叉场景仍作为后续可补充的测试覆盖记录。

## 9. 未验证内容与剩余风险

- **邻接窗口大小固定为前后各一个 chunk**：按需求明确排除"动态窗口大小"，这是设计范围内的取舍，不是遗漏，但意味着如果未来发现某些内容类型（比如列表、表格跨块）需要更宽的窗口，需要单独立项评估，不是改一个常量就能解决（`neighborIndexesFor` 硬编码 `±1`）。
- **没有真实语料上的检索质量对比**：本阶段完全没有跑 `make eval`（原因和 Phase 3 报告一致：本沙箱没有 LLM Provider 凭证，也没有 MySQL 承载 `internal/eval` 需要的知识库/对话数据），所以"邻接扩展到底有没有实际提升跨块答案的质量"这个问题，本报告不能给出基于真实数据的量化结论——只能证明代码行为符合规格描述的规则,不能证明这个功能本身在真实问答场景里的收益。
- **性能未压测**：`expandWithNeighborWindow` 的查询次数上界是 `min(topK, 不同 document_version 组合数)`，结构上有明确上限（第 4 节已经说明这是"查询次数有明确上限"要求的实现方式），但没有在 `maxChunksPerKnowledgeBase=5000` 级别的真实数据量、且 topK 取上限 `maxTopK=50`（意味着最多 50 次额外查询）的组合下做过延迟测量。真实场景里 `retrievalTopK=5`（conversation 侧固定值）远小于这个上限,但 `workflow` 侧的可配置 topK 理论上能触达 50,这条路径没有专门测过延迟。
- **缺少单用例覆盖完整组合链路**：完整 Docker 环境下的全量测试已经通过，但 Hybrid Search 与邻接扩展目前由不同测试分别覆盖，尚缺一条在同一集成测试中明确断言完整 `Retrieve(topK)` 最终 anchors/neighbor 顺序的用例。
- **多知识库跨 embedding 模型分组场景下的邻接扩展未被集成测试覆盖**：`buildNeighborGroups` 按 `(document_id, document_version)` 分组，和 anchor 属于哪个 embedding model/知识库无关，理论上不需要额外处理，但目前没有一条集成测试专门构造"两个不同 embedding 模型的知识库，各自贡献的核心块都恰好来自需要邻接扩展的文档"这个组合场景来验证分组逻辑在跨模型输入下确实正确——这是本阶段单测/集成测试目前没有专门覆盖的一个交叉场景，风险较低（分组逻辑本身不读 embedding 相关字段），但如实记录为未验证。
