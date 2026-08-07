# RAG 优化第五阶段：内容去重（Exact Content Dedup）评估报告

日期：2026-08-07（二次更新：修复 Codex 审核发现的 4 个问题，见第 0 节）。前置阶段：[docs/eval-phase3-hybrid-search-report.md](docs/eval-phase3-hybrid-search-report.md)（Hybrid Search）、[docs/eval-phase4-neighbor-window-report.md](docs/eval-phase4-neighbor-window-report.md)（邻接分块扩展，含审核修复的预算优先级问题）。本报告只覆盖第五阶段：在 Hybrid Search 融合结果和邻接分块扩展结果里，剔除规范化后完全相同的重复正文。

## 0. 二次更新：修复审核发现的问题

Codex 第一轮审核（`.claude/CODEX_CLAUDE_HANDOFF.md`）发现 4 个问题，全部已修复：

1. **空正文误判为重复（实现缺陷）**：`dedupExactContentChunks` 原来把规范化后为 `""` 的内容也计入去重判断，导致多个空/纯空白正文的 chunk 被折叠成一条。修复：`normalizeContentForDedup` 返回空字符串的 chunk 现在永远保留、永远不写入 `seen`、永远不计入抑制计数——见 `dedup.go` 新的空内容豁免逻辑和 `dedup_test.go` 新增的 3 个专项测试。
2. **规范化范围越界（实现缺陷）**：`normalizeContentForDedup` 原来除了 `CRLF -> LF` 还额外把单独的 `\r` 也当作换行符处理，超出了"只统一 CRLF"这条阶段约束。修复：删除了 lone-CR 替换那一行，`"a\rb"` 和 `"a\nb"` 现在保持不同——见 `TestNormalizeContentForDedupNeverFoldsLoneCRIntoLF`。
3. **缺少重复抑制计数（验收缺口）**：`dedupExactContentChunks`/`rrfFuse`/`expandWithNeighbors` 现在都多返回一个 `int`（抑制的重复条数），`Retrieve` 把两个阶段的计数分别记为安全的结构化 debug 日志字段 `core_duplicate_count`/`neighbor_duplicate_count`（只有计数，不含正文、查询文本或指纹），只在计数大于 0 时才打印，避免每次调用都产生噪音日志——见 `service.go` 的 `Retrieve` 和 `dedup_test.go`/`hybrid_test.go`/`neighbor_test.go` 里新增的计数专项测试。
4. **端到端测试造数缺陷（测试缺陷）**：`TestIntegrationRetrieveNeighborDedupPrefersCoreOverDuplicateNeighborContentEndToEnd` 原来把 `anchor-prev` 的向量设成和 `anchor`/`core2` 完全相同（`[1,0,0]`），导致真实向量检索里三者打平，靠 ID 排序 `anchor-prev` 反而赢过 `core2` 进了 topK 核心命中，测试因此失败（真实报告：`[anchor anchor-prev]`，不含 `core2`）。修复：把 `anchor-prev` 的向量改成正交方向（`[0,1,0]`，余弦=0），保证它在真实向量检索里排名明显落后于两个真核心命中，只能通过 `anchor` 的邻接查询（按 chunk_index 邻接，不看向量分数）被重新捞回来，再在内容去重阶段被丢弃。新增了一个不依赖 MySQL 的 PG-only 等价测试（`TestIntegrationRealVectorSearchDrivenAnchorSelectionPrefersCoreOverDuplicateNeighborContent`）在本沙箱内提供真实 Postgres 执行证明。

签名变化（仅限 `internal/knowledge` 包内部，`Service.Retrieve` 的公开签名不变）：`dedupExactContentChunks(chunks) ([]RetrievedChunk, int)`、`rrfFuse(vectorChunks, keywordChunks, topK) ([]RetrievedChunk, int)`、`expandWithNeighbors(anchors, neighbors) ([]RetrievedChunk, int)`、`(s *service) expandWithNeighborWindow(ctx, anchors) ([]RetrievedChunk, int, error)`。这个改动波及了包内所有调用点（`hybrid_test.go`/`neighbor_test.go`/`integration_test.go` 里全部 `rrfFuse(...)`/`expandWithNeighbors(...)` 调用都相应更新），但没有改变任何一处的行为语义，纯粹是"多返回一个安全计数"。

未扩展范围：没有新增 LLM/Reranker/模糊阈值，没有修改 Citation 协议，没有提高 RAG 预算，没有执行 git commit，没有创建传输压缩包。

## 1. 问题背景

Hybrid Search（Phase 3）和邻接分块扩展（Phase 4）可能各自独立地把同一段文本送进最终结果两次：

- 两个不同 chunk ID（可能来自不同文档，也可能是同一份文档被重复上传、或分块边界巧合产生了完全相同的一段文本）在向量/关键词检索中都被命中，融合后作为两条独立结果占用了两个 topK 名额。
- 一个核心命中块的邻接块（chunk_index-1/+1）正文，恰好和另一个核心命中块的正文完全相同。
- 两个不同核心命中块各自的邻接块之间正文相同。

把同一段话喂给模型两次，既浪费 RAG 字符预算，也会在 Citation 面板上显得像重复引用同一处内容的 bug。这不是语义相似问题，而是"内容字面重复"的问题，用规范化后的精确匹配就能解决，不需要引入模糊相似度阈值或 LLM 判断。

## 2. 设计原则（对应任务里的核心要求）

1. **只做保守的规范化后完全相同内容去重，不做语义或模糊相似去重。** 规范化仅统一 CRLF/单独 CR 为 LF、清除整段首尾空白、清除每一行末尾的空格/制表符；大小写、标点、行内（非首尾）缩进一律保留不变，两个仅大小写不同或缩进不同的 chunk 永远不会被判定为重复。
2. **原始 `Content` 绝不被规范化文本覆盖。** 规范化函数 `normalizeContentForDedup` 的输出只用作去重的比较键，从不写回 `RetrievedChunk.Content`——保留下来的条目，Content 永远是数据库里原始的、未经处理的文本。
3. **RRF 先保留有界扩大候选，核心块内容去重后再截断到 topK。** `rrfFuse`（hybrid.go）在按 fusionScore/Score/ID 完成排序之后、截断到 topK 之前，先对整个融合候选列表（本身已经是有界的：至多 `len(vectorChunks)+len(keywordChunks) ≤ 2*candidateK ≤ 200`）跑一次内容去重，再截断。这个顺序是"内容不同的第三条候选可以补上被去重掉的重复项名额"的唯一原因——如果反过来先截断再去重，被截断掉的候选就再也没有机会补位了。
4. **核心去重完成后才查询邻接块。** `expandWithNeighborWindow`（service.go）的输入是 `rrfFuse` 返回的 anchors——这些 anchors 已经在第 3 步完成了内容去重，被淘汰的重复核心块根本不在这个切片里，因此 `buildNeighborGroups`/`findPublishedNeighborChunks` 永远不会为它们发起邻接查询，不存在"为已淘汰的重复核心块保留邻接窗口"的问题。这一点不需要额外代码——只是"去重后的 anchors 才被传给邻接扩展"这个既有调用顺序的自然结果。
5. **邻接扩展完成后再次内容去重；所有核心块优先于全部邻接块。** `expandWithNeighbors`（neighbor.go）在拼好两层输出（全部核心块在前，全部邻接块整体在后，见 Phase 4 报告第 0 节的两层布局）之后，对整个两层切片再跑一次同一个 `dedupExactContentChunks` 函数。因为核心块层永远排在邻接块层之前，"先出现的保留、后出现的丢弃"这一条规则自动同时满足了"核心与邻接重复时保留核心"和"邻接之间重复时保留输出顺序靠前者"——不需要为这两条规则分别写特殊逻辑。
6. **保留最高排名结果的 Score、Citation 元数据、DocumentVersion；邻接块的 NeighborOf 不得被改写。** `dedupExactContentChunks`（dedup.go）只做过滤，从不合并或修改字段——一个被保留的条目就是它在去重之前已经计算好的那个 `RetrievedChunk` 值，未经任何修改；一个被淘汰的条目就是完全不出现在输出里，不会把自己的任何字段"泄漏"进保留下来的那一条。
7. **不修改 Citation 协议，不增加 LLM/Reranker/模糊阈值，不提高 RAG 预算。** 去重逻辑完全在 `internal/knowledge` 包内部完成，`conversation` 包对它一无所知——`conversation/budget.go` 的 `selectEvidence`/`renderedSourceLen`/`truncateEvidenceToFit`、`ragMinSimilarityScore`、Citation 序列化逻辑本轮一行代码都没有改动。topK 语义变成"至多 topK 个内容互不相同的核心命中"，而不是"至多 topK 条融合结果"——这本身就是让预算利用率更高，而不是需要更大的预算才能达到同样效果。

## 3. 两处去重调用点

`internal/knowledge/dedup.go` 是唯一的实现文件，提供两个纯函数：`normalizeContentForDedup(content string) string`（计算去重键）和 `dedupExactContentChunks(chunks []RetrievedChunk) []RetrievedChunk`（按"保留第一次出现"的规则过滤，不改变相对顺序，不合并/修改字段）。这一个函数在两个不同调用点被复用，靠的是同一条规则在不同输入顺序下自然产生不同效果：

- **`hybrid.go` 的 `rrfFuse`**：调用点在 fusionScore/Score/ID 三级排序之后、`topK` 截断之前，输入是"已经按融合排名从高到低排好序"的候选列表——"保留第一次出现"因此等价于"保留融合排名最高的重复项"。
- **`neighbor.go` 的 `expandWithNeighbors`**：调用点在两层输出（核心块层 + 邻接块层）拼装完成之后，输入是"核心块全部在前、邻接块全部在后，邻接块内部按所属核心块排名 + previous-then-next 排列"的切片——"保留第一次出现"因此同时给出"核心优先于邻接"和"邻接之间保留靠前者"两条规则，不需要额外判断逻辑区分"这是核心块还是邻接块"。

## 4. topK 语义的变化

`topK` 从"Hybrid Search 返回的核心命中数量"变为"内容互不相同的核心命中数量"。`Service.Retrieve` 的公开签名和调用方式完全不变（`topK int` 参数含义在文档里明确更新，代码层面无破坏性变化）——变化只在于：以前一个内容重复的候选会占满一个 topK 名额，现在它会被去重掉，让真正内容不同的候选有机会补位，最终返回给调用方的核心命中数量在"内容确实各不相同"的正常场景下和以前完全一致，只有在存在真实重复内容时才会出现"少于 topK 个核心命中、但每一个都是独特内容"或"topK 个核心命中、且第 topK 个是原本排名更靠后但内容独特的候选"这两种此前不会发生的行为。

## 5. 修改文件

- `internal/knowledge/dedup.go`（新增，二次更新修复空正文/lone-CR 两处缺陷 + 增加抑制计数返回值）：`normalizeContentForDedup`、`dedupExactContentChunks(chunks) ([]RetrievedChunk, int)`。
- `internal/knowledge/dedup_test.go`（新增，二次更新增补 4 个专项测试）：13 个纯逻辑单测，覆盖规范化规则、去重键、字段保留、边界输入、空正文豁免、lone-CR 不折叠。
- `internal/knowledge/hybrid.go`（二次更新：`rrfFuse` 多返回一个 `coreDuplicateCount`）：`rrfFuse` 在排序后、截断到 topK 前插入一次 `dedupExactContentChunks` 调用；更新该函数的文档注释解释这个顺序的必要性。
- `internal/knowledge/hybrid_test.go`（二次更新：全部调用点适配新签名 + 新增计数专项测试）：`rc` 测试构造器补上按 ID 派生的唯一 Content（否则所有测试 chunk 的空 Content 会被新去重逻辑误判为重复，导致除去重相关测试外的几乎全部既有测试失败）；新增 `rcContent`/`fuseIDs` 构造器和 4 个 Phase 5 专属测试（含 `TestRRFFuseReturnsCoreDuplicateCount`）。
- `internal/knowledge/neighbor.go`（二次更新：`expandWithNeighbors` 多返回一个 `neighborDuplicateCount`）：`expandWithNeighbors` 在两层输出拼装完成后追加一次 `dedupExactContentChunks` 调用，作为该函数新增的规则 7；更新文件头和函数文档注释。
- `internal/knowledge/neighbor_test.go`（二次更新：全部调用点适配新签名 + 新增计数专项测试）：`anchorRC`/`neighborRC` 同样补上按 ID 派生的唯一 Content（原因同上）；新增 `anchorRCContent`/`neighborRCContent`/`expandIDs` 构造器和 4 个 Phase 5 专属测试（含 `TestExpandWithNeighborsReturnsNeighborDuplicateCount`）。
- `internal/knowledge/service.go`（二次更新：`Retrieve`/`expandWithNeighborWindow` 传递并记录抑制计数）：`Retrieve` 聚合 `rrfFuse`/`expandWithNeighborWindow` 各自返回的重复抑制计数，仅在大于 0 时打一条安全的结构化 debug 日志（`core_duplicate_count`/`neighbor_duplicate_count`，不含正文/查询文本/指纹）；`expandWithNeighborWindow` 签名改为多返回一个 `int`。文档注释同步说明 topK 新语义和"核心去重后才查邻接"的调用顺序。
- `internal/knowledge/integration_test.go`（二次更新：修正端到端测试造数缺陷 + 新增 1 个 PG-only 真实向量检索验证测试）：新增 7 个集成测试——5 个 PG-only（`setupPGOnlyIntegration`，本沙箱内真实执行并通过）+ 2 个需要 MySQL+Postgres 的 `Service.Retrieve` 全链路测试（`setupIntegration`，本沙箱因缺 MySQL 按既有约定 SKIP，已由 Codex 在真实 docker 环境验证真实 PASS，无 SKIP）。
- `README.md`：RAG 全流程一句话描述插入 Phase 5 的内容去重说明，两处（RRF 后、邻接扩展后）都提到。
- `docs/critical-paths.md`：链路 3 更名为"Hybrid Search + 邻接分块扩展 + 内容去重检索链路"，关键节点和测试覆盖两个单元格都补充 Phase 5 的内容；顶部日期行追加本次更新说明。
- `docs/eval-phase5-content-dedup-report.md`（本文件，新增）。

未修改：`internal/conversation/budget.go`（`selectEvidence`/`renderedSourceLen`/`truncateEvidenceToFit`/`ragMinSimilarityScore` 本轮零改动，去重完全在 knowledge 包内部完成）、Citation 序列化相关代码、`internal/knowledge/repository.go`、`internal/knowledge/hybrid.go` 里 `candidateK`/`sortVectorCandidatesByScoreThenID` 的实现本身、`internal/knowledge/neighbor.go` 里 `neighborIndexesFor`/`buildNeighborGroups`/`neighborLookupKey` 的实现本身、任何 SQL 文件、任何 sqlc 生成代码。

## 6. 完整验收场景与对应测试

1. **不同 ID、相同正文只保留最高排名结果。** `TestDedupExactContentChunksKeepsHighestRankedDuplicate`、`TestRRFFuseContentDedupKeepsHigherFusionRankedDuplicate`、`TestIntegrationRRFFuseDedupsExactDuplicateCoreContentAgainstRealPostgres`（真实 Postgres）、`TestIntegrationRetrieveDedupsExactDuplicateContentEndToEnd`（真实 Service.Retrieve，需 MySQL，已由 Codex 真实执行并 PASS）。
2. **CRLF/LF、首尾空白、行尾空格差异能够识别为同一份内容。** `TestNormalizeContentForDedupUnifiesCRLFAndTrimsWhitespace`。
3. **大小写、标点、内部缩进差异不能误判为重复。** `TestNormalizeContentForDedupPreservesCaseAndPunctuationAndIndentation`、`TestDedupExactContentChunksNoFalsePositiveForDifferentContent`。
4. **`A、A、B、C + topK=3` 最终得到 `A、B、C`。** `TestRRFFuseContentDedupLetsUniqueLowerRankedCandidateFillTopKSlot`、`TestIntegrationRRFFuseDedupsExactDuplicateCoreContentAgainstRealPostgres`。
5. **核心与邻接正文重复时保留核心。** `TestExpandWithNeighborsDedupPrefersCoreOverDuplicateNeighborContent`、`TestIntegrationExpandWithNeighborsDedupPrefersCoreOverNeighborAgainstRealPostgres`（真实 Postgres）、`TestIntegrationRetrieveNeighborDedupPrefersCoreOverDuplicateNeighborContentEndToEnd`（真实 Service.Retrieve，需 MySQL，已由 Codex 真实执行并 PASS）；核心去重后才查邻接额外由 `TestIntegrationExpandWithNeighborWindowNeverQueriesNeighborsOfDedupedCoreChunk`（污染探针手法：被淘汰核心块独有的邻接内容绝不出现在结果里）验证。
6. **邻接之间重复时保留输出顺序靠前者。** `TestExpandWithNeighborsDedupAmongNeighborsKeepsEarlierOutputOrder`。
7. **原始 Content、Score、Citation 元数据、NeighborOf 均不被错误修改。** `TestDedupExactContentChunksPreservesOriginalFieldsOfKeptEntry`、`TestExpandWithNeighborsDedupNeverRewritesKeptNeighborFields`。
8. **全量测试、race、vet、check-deps、diff-check 通过，数据库测试不得静默 skip。** 见下方第 7 节——所有能在 Claude 侧沙箱（只有 Postgres，没有 MySQL）里运行的数据库测试都真实执行并通过，没有一个因为环境配置被跳过；需要 MySQL 的 2 个测试在 Claude 侧沙箱因缺 MySQL 按既定约定 SKIP，Codex 已在有 MySQL+PostgreSQL 的完整 docker 环境中用 `-v` 单独重跑本阶段全部 6 个指定数据库集成测试，全部真实 PASS、无一个 SKIP。
9. **空正文不参与去重（二次更新新增）。** `TestDedupExactContentChunksNeverCollapsesEmptyContent`、`TestDedupExactContentChunksNeverCollapsesWhitespaceOnlyContent`、`TestDedupExactContentChunksEmptyContentDoesNotInterfereWithRealDuplicates`。
10. **单独 CR 不能被当作换行符（二次更新新增）。** `TestNormalizeContentForDedupNeverFoldsLoneCRIntoLF`。
11. **重复抑制计数语义正确、安全可日志化（二次更新新增）。** `TestRRFFuseReturnsCoreDuplicateCount`（核心阶段）、`TestExpandWithNeighborsReturnsNeighborDuplicateCount`（邻接阶段）；`dedup_test.go` 里每个既有测试也都补上了对第二个返回值的断言。

## 7. 真实测试结果（三次更新：Codex 第二轮复审确认全部 6 个指定集成测试真实 PASS，无 SKIP）

> 本节原记录的"本沙箱无 MySQL，2 个 `Service.Retrieve` 全链路测试 SKIP"仅反映 Claude 侧沙箱环境限制。Codex 在有完整 MySQL+PostgreSQL docker 环境的机器上，用 `-v` 单独执行了本阶段全部 6 个指定数据库集成测试（4 个 PG-only + 2 个 `Service.Retrieve` 全链路），全部真实 PASS、无一个 SKIP，详见 `.claude/CODEX_CLAUDE_HANDOFF.md`「Codex 审核结果」（第二轮，2026-08-07）。下方仍保留 Claude 侧的执行记录作为过程证据，但结论已被 Codex 侧的真实结果取代：本阶段不再有未验证的数据库集成测试。

```
gofmt -w internal/knowledge/dedup.go internal/knowledge/dedup_test.go \
         internal/knowledge/hybrid.go internal/knowledge/hybrid_test.go \
         internal/knowledge/neighbor.go internal/knowledge/neighbor_test.go \
         internal/knowledge/service.go internal/knowledge/integration_test.go
                                       # 无残留 diff
make sqlc                             # 重新生成，和已有生成代码无 diff（本轮未改任何 SQL）
go vet ./...                          # 0 diagnostics
go test -count=1 ./...                # 全部 PASS（含之前失败的
                                       #   TestIntegrationRetrieveNeighborDedupPrefersCoreOverDuplicateNeighborContentEndToEnd；
                                       #   本沙箱因无 MySQL 该测试及另一个 Service.Retrieve 全链路测试按既定约定 SKIP，
                                       #   Codex 已在有 MySQL 的环境里用 -v 单独重跑并确认真实 PASS，无 SKIP）
go test -race -count=1 ./...          # 全部 PASS，无 race
make check-deps                       # OK，无跨层/同层依赖违规
git diff --check                      # 无输出
git status --short                    # 只有本阶段应改动的文件
```

**纯逻辑单测（`dedup_test.go` 13 个，全部真实执行且 PASS，无 DB 依赖）**：

`TestNormalizeContentForDedupUnifiesCRLFAndTrimsWhitespace`、`TestNormalizeContentForDedupNeverFoldsLoneCRIntoLF`（新增，修复待修复项 2）、`TestNormalizeContentForDedupPreservesCaseAndPunctuationAndIndentation`、`TestNormalizeContentForDedupEmptyString`、`TestDedupExactContentChunksKeepsHighestRankedDuplicate`、`TestDedupExactContentChunksNoFalsePositiveForDifferentContent`、`TestDedupExactContentChunksPreservesOriginalFieldsOfKeptEntry`、`TestDedupExactContentChunksEmptyAndSingleInput`、`TestDedupExactContentChunksKeyIsContentOnlyNotIDOrScore`、`TestDedupExactContentChunksNeverCollapsesEmptyContent`（新增，修复待修复项 1）、`TestDedupExactContentChunksNeverCollapsesWhitespaceOnlyContent`（新增）、`TestDedupExactContentChunksEmptyContentDoesNotInterfereWithRealDuplicates`（新增）。上述既有测试全部同步补上了对新增第二个返回值（抑制计数）的断言。

`hybrid_test.go` 新增（4 个）：`TestRRFFuseContentDedupLetsUniqueLowerRankedCandidateFillTopKSlot`、`TestRRFFuseContentDedupKeepsHigherFusionRankedDuplicate`、`TestRRFFuseContentDedupIsDeterministicAcrossRuns`、`TestRRFFuseReturnsCoreDuplicateCount`（新增，修复待修复项 3）。既有 12 个 `rrfFuse`/`sortVectorCandidatesByScoreThenID` 测试全部重跑仍 PASS，调用点已全部适配新的二返回值签名（新增 `fuseIDs` 测试辅助函数）。

`neighbor_test.go` 新增（4 个）：`TestExpandWithNeighborsDedupPrefersCoreOverDuplicateNeighborContent`、`TestExpandWithNeighborsDedupAmongNeighborsKeepsEarlierOutputOrder`、`TestExpandWithNeighborsDedupNeverRewritesKeptNeighborFields`、`TestExpandWithNeighborsReturnsNeighborDuplicateCount`（新增，修复待修复项 3）。既有 16 个 `expandWithNeighbors`/`buildNeighborGroups`/`neighborIndexesFor` 测试全部重跑仍 PASS，调用点已全部适配新的二返回值签名（新增 `expandIDs` 测试辅助函数）。

**PostgreSQL 集成测试（真实对本机 pgvector+pg_trgm 执行，新增 5 个，全部 PASS）**：

1. `TestIntegrationRRFFuseDedupsExactDuplicateCoreContentAgainstRealPostgres` —— 两条同内容不同 ID 的 chunk（真实 `searchVectorChunks` 查出）经真实 `rrfFuse` 去重，topK=2 时保留分数更高的一条加第三条唯一候选。
2. `TestIntegrationExpandWithNeighborWindowNeverQueriesNeighborsOfDedupedCoreChunk` —— 污染探针手法：给会被去重淘汰的核心块单独种一个带独有内容的邻接块，断言这条内容绝不出现在最终结果里。
3. `TestIntegrationExpandWithNeighborsDedupPrefersCoreOverNeighborAgainstRealPostgres` —— 邻接块正文和另一个核心块正文相同时，真实 `findPublishedNeighborChunks` 查出的邻接块被去重丢弃，两个核心块都保留。
4. `TestIntegrationRealVectorSearchDrivenAnchorSelectionPrefersCoreOverDuplicateNeighborContent`（新增，修复待修复项 4）—— 不依赖 MySQL 的端到端等价测试：真实 `searchVectorChunks` 检索出全部 3 个候选（含向量分故意调弱的 `anchor-prev`）→ 真实 `rrfFuse` 核心去重/选出 topK 个核心命中 → 真实 `findPublishedNeighborChunks` 拿到邻接块 → 真实 `expandWithNeighbors` 二次去重，断言核心命中确实是 `anchor`+`core2`、`anchor-prev` 绝不残留在最终结果里，用真实 Postgres 提供修复待修复项 4 的执行证明（对应的 MySQL 版本见下方"Service.Retrieve 全链路集成测试"，已由 Codex 在有 MySQL 环境中真实执行并 PASS）。
5. 其余既有 PG-only 集成测试（`TestIntegrationHybridSearchPromotesStrongKeywordMatchIntoTopK`、`TestIntegrationHybridSearchDeduplicatesChunkHitByBothPaths`、`TestIntegrationFindPublishedNeighborChunks*` 等）全部重跑仍 PASS，未因新增去重逻辑回归。

**Service.Retrieve 全链路集成测试（2 个，需要 MySQL+Postgres）**：

`TestIntegrationRetrieveDedupsExactDuplicateContentEndToEnd`、`TestIntegrationRetrieveNeighborDedupPrefersCoreOverDuplicateNeighborContentEndToEnd`（后者是待修复项 4 修正种子数据的那一个：`anchor-prev` 的向量从 `[1,0,0]` 改成 `[0,1,0]`）——写法和本文件里所有其它 `setupIntegration`（而非 `setupPGOnlyIntegration`）测试完全一致。这两个测试在 Claude 侧沙箱（无 MySQL）运行 `go test` 时会打印跳过原因（"跳过集成测试（先 make db-up 起容器）"）后 SKIP，不计入失败；Codex 已在有完整 MySQL+PostgreSQL docker 环境的机器上用 `-v` 单独重跑这两个测试，确认真实 PASS、无 SKIP（见 `.claude/CODEX_CLAUDE_HANDOFF.md`「Codex 审核结果」第二轮）。这两个测试和上面 5 个 PG-only 测试验证的是完全相同的去重规则，区别只在于是否经过真实的 `Service.Retrieve` 公开入口（含它的 MySQL `knowledge_base` 查询和 embedding 调用）——现在两条路径都已有真实执行证明。

**回归确认**：Phase 3/4 原有的全部单测和集成测试（`hybrid_test.go`/`neighbor_test.go`/`integration_test.go` 里 Phase 3/4 部分、`conversation/budget_test.go`）在本轮改动后重新跑过，全部仍然 PASS，没有因为新增的两次内容去重调用、抑制计数返回值、或本轮的 4 个问题修复而回归。

## 8. 未验证内容与剩余风险（三次更新：Codex 第二轮复审已确认全部数据库测试真实 PASS）

- 本轮修复的 4 个问题（空正文误合并、单独 CR 越界折叠、缺少去重计数、`TestIntegrationRetrieveNeighborDedupPrefersCoreOverDuplicateNeighborContentEndToEnd` 造数缺陷）均已修复并有专门测试覆盖，详见第 0、6、7 节。
- 之前记录的限制——"本沙箱没有 MySQL，2 个 `Service.Retrieve` 全链路集成测试按设计 SKIP，需要 Codex 在有完整 docker 环境的机器上验证"——已经解除：Codex 在其 MySQL+PostgreSQL 齐备的环境中用 `-v` 单独执行了本阶段全部 6 个指定数据库集成测试（含这 2 个 `Service.Retrieve` 全链路测试），全部真实 PASS、无 SKIP，详见 `.claude/CODEX_CLAUDE_HANDOFF.md`「Codex 审核结果」（第二轮，2026-08-07）。本阶段（Phase 5）不再有未验证的数据库集成测试。
- 极端场景（融合候选列表里重复内容占比极高时 topK 补位是否总能凑够 topK 个结果）仍未单独断言，这是设计上的预期行为（返回更少但内容互不相同的结果）而非遗漏；"几乎重复但有一个内部空格或标点差异"的内容按设计不去重，如果未来发现真实业务数据里这种情况大量占用预算，需要另外评估模糊去重（本阶段明确排除在范围外）。这两点结论与上一轮相同，未变化。
- 极端场景未覆盖：如果同一个 KB 里存在大量（数十条以上）互不相同但两两之间存在真实重复内容的 chunk，`dedupExactContentChunks` 是 O(n) 的一次遍历 + map 查找，性能不是问题；但没有专门测试验证"融合候选列表里超过一半是重复内容"这种极端场景下 topK 补位是否总能找到足够多的唯一候选——这依赖 `candidateK` 本身给 RRF 融合池留出的宽度（`min(max(topK*4, topK), 100)`），如果重复比例极高、candidateK 又不够大，仍然可能出现"去重后不足 topK 个结果"的情况，这是预期行为（返回更少但都是唯一内容的结果），但没有为这个"预期但极端"的场景单独写断言。
- 去重键目前是"整个 Content 规范化后的字符串"，对于两个 chunk 只有细微的、非规范化范围内的差异（例如仅相差一个内部空格、仅相差一个标点符号，这两种情况在需求里被明确要求"不能误判"，本报告的验收场景 3 已覆盖），确实不会被去重——这是设计如此，不是遗漏，但如果未来发现真实业务数据里存在这种"几乎重复但不完全重复"的内容大量占用 RAG 预算，需要另外评估是否要引入模糊去重（本阶段明确排除在范围外）。
