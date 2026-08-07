# RAG 优化第三阶段：Hybrid Search 实施报告

日期：2026-08-07（初版）/ 2026-08-07 二次更新（审核修复 + 真实 Docker 环境验证结果）

## 0. 二次更新：审核修复的两个问题

Codex 最终审核发现两个问题，本次更新已修复，范围严格限定在这两点，未扩展改动：

**1. 向量候选同分时排序不稳定。** 根因有两处：`SearchVectorChunks` 的 `ORDER BY embedding <=> query_embedding` 单独存在时，余弦距离相同的行在 PostgreSQL 里返回顺序不保证；`service.go` 合并多个 embedding model 分组的候选后只按 `Score` 排序，候选顺序又依赖 Go map 遍历（随机）。而 `rrfFuse` 把候选切片的位置当成 RRF 的 rank，上游任意一处顺序不稳都会让最终融合排序跟着不稳定。修复：

- `SearchVectorChunks`（`pgqueries/chunks.sql`）改为 `ORDER BY embedding <=> query_embedding, id ASC`，重新执行 `make sqlc` 生成（未手改生成代码）。
- `service.go` 里原来的 `sort.Slice(vectorCandidates, ...Score...)` 收敛成 `internal/knowledge/hybrid.go` 新增的 `sortVectorCandidatesByScoreThenID`：Score 降序，只有 Score 完全相同时才用 chunk ID 升序兜底，和 `rrfFuse` 自己的排序规则（fusionScore desc → Score desc → ID asc）保持一致的"只在真正打平时才用 ID 兜底"的原则——不同相关度的候选永远不会被 ID 规则重排。
- 新增测试：`TestSortVectorCandidatesByScoreThenIDStableOnTiedScores`（Score 相同、输入顺序不同 → 结果都是 ID 升序）、`TestSortVectorCandidatesByScoreThenIDNeverReordersDifferentScores`（Score 不同时 ID 绝不生效）、`TestVectorCandidateBuildOrderDoesNotAffectFinalHybridResult`（模拟不同的模型分组/候选追加顺序，送入 `rrfFuse` 前先跑排序函数，最终融合结果完全一致）、`TestIntegrationSearchVectorChunksTiedScoreSortsStablyByID`（真实 Postgres 集成测试：4 个 embedding 完全相同的 chunk 乱序插入，`searchVectorChunks` 必须按 ID 升序稳定返回）。
- 原来的 `TestRRFFuseIsOrderIndependentAndDeterministicAcrossRuns` 名不副实——它只是用同一份已排好序的输入重复调用 `rrfFuse`，验证的是 `rrfFuse` 内部 map 迭代的随机性不会泄漏到输出，而不是"上游候选顺序变化，最终结果不变"。已重命名为 `TestRRFFuseInternalMapIterationDoesNotLeakIntoOutputOrder` 并改写了文档注释说明它实际覆盖的范围，真正的"上游顺序不影响最终结果"由新增的 `TestVectorCandidateBuildOrderDoesNotAffectFinalHybridResult` 覆盖。

**2. down migration 会删除共享扩展。** `CREATE EXTENSION IF NOT EXISTS pg_trgm` 证明不了这个扩展是本迁移创建的——数据库里完全可能因为其他模块早就装了它，回滚时 `DROP EXTENSION` 会把其他潜在使用方依赖的共享能力一并拆掉。修复：`000004_chunks_content_trgm.down.sql` 不再删除 `pg_trgm` 扩展本身，只回滚这条迁移自己引入的东西（`idx_chunks_content_trgm` 索引、`pg_trgm.word_similarity_threshold` 这个 GUC 的数据库级默认值）。up/down 两个文件的注释都补充说明了"pg_trgm 一旦启用就是数据库级共享能力，不由某条迁移独占管理"这个设计取舍。已手动对一个 scratch 数据库验证 up → down → up 可重复执行、无报错，索引/GUC/扩展的状态在每一步都符合预期（见第 9 节）。

## 1. 目标回顾

在不改变 `knowledge.Service.Retrieve(ctx, knowledgeBaseIDs, query, topK) ([]RetrievedChunk, error)` 对外签名的前提下，把检索从纯 pgvector 向量检索升级为：

```
用户问题
  ├─ Vector Search（pgvector 余弦相似度，不变）
  ├─ Keyword Search（pg_trgm trigram/word-similarity，新增）
  └─ RRF 融合、去重、全局 TopK
```

## 2. 为什么是 pg_trgm，不是 BM25

PostgreSQL 默认的全文检索（`tsvector`/`to_tsvector('english', ...)`）依赖分词 + 词干化，`english` 配置对中文完全无效——没有内置中文分词配置（如 `zhparser`），中文文本会被当成一个不可分割的 token，检索价值接近于零。真正的 BM25 需要分词、词频（TF）、逆文档频率（IDF）统计和相关性打分模型，这次改造没有引入任何分词器或 TF-IDF 统计表。

`pg_trgm` 提供的是**纯字符级 trigram 相似度**：把字符串切成连续 3 字符的 n-gram 集合，用 Jaccard 相似度的变体衡量两个字符串"像不像"。它天然对中英文一视同仁（不需要分词），代价是不做真正的词频/相关性排序——两个字符序列相似不代表语义相关，也不代表关键词密度高。

因此仓库里所有相关代码、SQL、注释、测试名一律称它 **trigram keyword search** 或 **lexical search**，没有任何地方声称是 BM25 或全文检索相关性排序。

## 3. Vector Search 与 Keyword Search 的入口

- **Vector Search**：`internal/knowledge/repository.go` 的 `searchVectorChunks`（原 `searchChunks` 改名），对应 `internal/db/pgqueries/chunks.sql` 的 `SearchVectorChunks`。语义完全未变：`embedding <=> query_embedding` 走 pgvector 余弦距离，`1 - 距离` 为相似度；`knowledge_base_id`/`embedding_dimension`/`is_published` 三个 WHERE 条件都保留。
- **Keyword Search**：`internal/knowledge/repository.go` 的新方法 `searchKeywordChunks`，对应 `SearchKeywordChunks` 查询。用 `sqlc.arg(query_text) <% content` 做候选过滤（pg_trgm 的可索引 word-similarity 算子），`ORDER BY word_similarity(...) DESC, id ASC` 排序取候选。不含 `embedding_dimension` 过滤——这一路完全不依赖 embedding。
- **调用编排**：`internal/knowledge/service.go` 的 `Retrieve` 方法。先按 embedding 模型分组跑向量检索（每组独立 `embedQuery` + `searchVectorChunks`，失败只跳过该模型分组），再对所有 active 知识库跑**一次**全局关键词检索（`searchKeywordChunks`，与 embedding 模型分组无关），最后把两路候选交给 `hybrid.go` 的 `rrfFuse` 融合。

## 4. 候选扩大（candidateK）

`internal/knowledge/hybrid.go` 的 `candidateK(topK) = min(max(topK*4, topK), 100)`：

- 两路各自拿 `candidateK` 条候选（不是 `topK`），给 RRF 融合留排序空间——如果两路各自只取 `topK`，融合空间几乎不存在。
- 硬上限 100：避免大 `topK`（`clampTopK` 已把 `topK` 限制在 `maxTopK=50`）把候选窗口无限放大，`50*4=200` 会被砍到 100。
- 小知识库（chunk 总数远小于 `candidateK`）正常退化：SQL 的 `LIMIT` 只是上限，实际返回条数由数据量决定。
- 最终仍然只返回 `topK` 条——`rrfFuse` 融合完成后按 `topK` 截断。

## 5. RRF 融合、去重与排序规则

`internal/knowledge/hybrid.go` 的 `rrfFuse(vectorChunks, keywordChunks, topK)` 是一个纯函数（无 DB 依赖）：

```
rrfScore(chunk) = Σ_path  weight[path] / (rrfK + rank[path](chunk))
```

- `rrfK = 60`（RRF 论文的标准默认值，也是业界最常见的默认值）。
- `vectorWeight = 0.65`，`keywordWeight = 0.35`——偏向语义召回（这是 RAG 管道而不是搜索引擎），但关键词专属命中依然有实质影响力，不会被向量结果挤出去。
- 去重：按 `chunk.ID` 建 `map[string]*fusionEntry` 累加两路贡献，同一个 chunk 无论被几路命中，最终只出现一次。
- 排序（三级，完全确定）：`fusionScore` 降序 → `Score`（相关度）降序 → chunk `ID` 升序。第三级只在前两级都打平时才生效，绝不用来重排不同相关度的候选——`hybrid_test.go` 的 `TestRRFFuseTiedFusionScoreBreaksByIDWhenScoreAlsoTies` 验证了这一点。
- 全局融合：多 embedding 模型的向量候选先在 `service.go` 里用 `sortVectorCandidatesByScoreThenID`（Score 降序、Score 相同时 ID 升序兜底）重新排序、截断到 `candidateK`（保证跨模型的向量 rank 是全局的，不是组内的），关键词候选本来就是跨知识库的单次全局查询——两者一起送进 `rrfFuse`，不先按某个知识库单独截断。这条排序规则和 SQL 层 `SearchVectorChunks`（`ORDER BY embedding <=> query_embedding, id ASC`）、`rrfFuse` 自己的三级排序，三处用的是同一个"只在真正打平时才用 ID 兜底"的约定——见第 0 节的审核修复说明。

## 6. `Score` 与 `fusionScore` 的区别（本阶段最重要的兼容性约束）

`internal/conversation/budget.go` 的 `selectEvidence` 会把 `Score < ragMinSimilarityScore(0.2)` 的候选直接丢弃。RRF 的原始融合分数量级在 `~0.005~0.02` 之间（`weight/(rrfK+rank)`，rank 越靠后越小），如果把它直接塞进 `RetrievedChunk.Score`，几乎所有结果都会被这道下限过滤掉——这是整个 Hybrid Search 阶段最容易踩的坑，也是 spec 里反复强调的约束。

处理方式：

- `fusionScore` 只存在于 `hybrid.go` 内部的 `fusionEntry` 结构体里，从未写入 `RetrievedChunk`，也从未被导出。
- `RetrievedChunk.Score` 保持语义不变：
  - 只被向量一路命中 → 原始余弦相似度。
  - 只被关键词一路命中 → 原始 word-similarity。
  - 两路都命中 → 取两个原始相关度的**较大值**（不是相加、不是平均、更不是 fusionScore）。
- 最终顺序完全由 `fusionScore` 决定，返回之后不再按 `Score` 重排——`service.go` 的 `Retrieve` 直接返回 `rrfFuse` 的结果，中间没有任何二次排序。
- `model.go` 里 `RetrievedChunk` 的文档注释显式记录了这条不变量，防止未来改动无意破坏它。
- SSE 调试信息、Citation 行为均未改动字段形状——`RetrievedChunk` 仍然只多了一个 `Score float64`，`Retrieve` 方法签名分毫未变，`conversation` 层不需要任何改动就能继续用同一套 `ragMinSimilarityScore` 过滤逻辑。

## 7. embedding 失败时如何降级到关键词检索

`service.go` 的 `Retrieve` 把向量检索和关键词检索拆成两个独立的失败域：

- 向量一路按 embedding 模型分组，`embedQuery` 或 `searchVectorChunks` 对某个模型失败，只跳过该模型对应的知识库（`slog.Warn` 记录后 `continue`），不影响其他模型分组，也不影响关键词一路。
- 关键词一路在向量分组循环**之外**单独跑一次，只要 `activeKBIDs` 非空就会执行，完全不关心上面的向量分组是否全部失败——这就是"embedding 服务全挂时，关键词检索仍有机会返回结果"的机制来源。
- 两路都失败或都没有命中：`rrfFuse(nil, nil, topK)` 返回空切片，`Retrieve` 返回 `(nil-ish empty, nil)`，不让对话主链路报错——和 Phase 3 之前的行为一致。
- 例外：**context 取消/超时永远不会被当成"这一路失败，跳过"处理**。`classifyRetrieveErr(ctx, err)` 在每个失败分支之前先检查 `ctx.Err()`，非空就直接把这个 error 返回给调用方，不吞掉、不继续跑下一个知识库。这是因为 context 被取消意味着调用方（对话主链路）已经不再关心这次调用的结果，继续 best-effort 重试没有意义，而且会掩盖真实的超时/取消语义。
- 日志：所有失败日志只记录 `err`、`embedding_model_id` 之类的元数据，从不记录用户的完整问题原文或 chunk 正文。

## 8. 修改文件

新增：

- `internal/db/pgmigrations/000004_chunks_content_trgm.up.sql` / `.down.sql`
- `internal/knowledge/hybrid.go`（RRF 融合，纯逻辑，无 DB 依赖）
- `internal/knowledge/hybrid_test.go`（10 个纯逻辑单测）

修改：

- `internal/db/pgqueries/chunks.sql`（`SearchChunks` 改名 `SearchVectorChunks`；新增 `SearchKeywordChunks`）
- `internal/db/pggen/chunks.sql.go`、`internal/db/pggen/querier.go`（`make sqlc` 真实生成，未手写）
- `internal/knowledge/repository.go`（`searchChunks` 改名 `searchVectorChunks`；新增 `searchKeywordChunks`）
- `internal/knowledge/service.go`（`Retrieve` 改为双路检索 + RRF 融合 + `classifyRetrieveErr` 失败隔离；二次更新把内联排序改成调用 `sortVectorCandidatesByScoreThenID`，移除了不再用到的 `sort` import）
- `internal/knowledge/hybrid.go`（二次更新新增 `sortVectorCandidatesByScoreThenID`）
- `internal/knowledge/model.go`（`RetrievedChunk` 文档注释补充 Score/fusionScore 的语义边界）
- `internal/knowledge/integration_test.go`（原 `searchChunks(` 调用点全部改为 `searchVectorChunks(`；新增 `setupPGOnlyIntegration` 等辅助函数和 Phase 3 集成测试；二次更新新增 `TestIntegrationSearchVectorChunksTiedScoreSortsStablyByID`，新增 `reflect` import）
- `internal/knowledge/hybrid_test.go`（二次更新重命名 `TestRRFFuseIsOrderIndependentAndDeterministicAcrossRuns` → `TestRRFFuseInternalMapIterationDoesNotLeakIntoOutputOrder` 并改写文档注释；新增 3 个覆盖上游排序的测试）
- `internal/db/pgmigrations/000004_chunks_content_trgm.up.sql` / `.down.sql`（二次更新：down 不再 `DROP EXTENSION pg_trgm`，up/down 注释都补充了共享扩展归属的说明）
- `README.md`、`docs/critical-paths.md`（见下方"实际执行过的测试"之外的文档同步）

## 9. 实际执行过的测试

**当前状态（二次更新）：Codex 已在本机真实 Docker 环境（MySQL + PostgreSQL + Redis 全部起容器）完整执行过一遍本阶段全部代码，结果：**

```
go test -count=1 ./...        # 通过，真实连接 MySQL/PostgreSQL，knowledge 包全部集成测试（含既有的、
                               #   依赖 setupIntegration/MySQL 的用例）真实执行，非 SKIP
go test -race -count=1 ./...  # 通过，无 race
go vet ./...                  # 通过
make check-deps               # 通过，无跨层/同层依赖违规
新增 Hybrid Search PostgreSQL 集成测试（见下方清单）  # 全部通过
```

这意味着本阶段此前"沙箱没有 MySQL，既有集成测试全部 SKIP"的限制，在有完整 Docker 环境的机器上不存在——`internal/knowledge/integration_test.go` 里所有依赖 `setupIntegration`（MySQL + Postgres）的既有测试，以及本阶段新增的全部测试，都在真实数据库上跑通了。

**本轮修复的验证（本次会话，沙箱环境）**：本会话运行在没有现成 Docker 服务的沙箱里，为了验证新代码不是空口承诺，沿用了之前搭建的本机 PostgreSQL（从源码编译的 pgvector 0.8.0 + PostgreSQL 自带的 pg_trgm，端口 5433，DSN 与 `internal/testutil` 常量一致），未额外起 MySQL。在这个环境下重新执行并确认：

```
go vet ./...                    # 通过，0 diagnostics
go test -count=1 ./...          # 全部 PASS（knowledge 包新增/改写的单测与 PostgreSQL-only 集成测试真实执行；
                                 #   依赖 MySQL 的既有集成测试在本沙箱按设计 SKIP，与本节开头 Codex 在
                                 #   完整 Docker 环境下的通过结果互补，不矛盾）
go test -race -count=1 ./...    # 全部 PASS，无 race
make sqlc                       # 重新生成，diff 与手写预期一致，未手改生成代码
./scripts/check-deps.sh (make check-deps)  # OK，无跨层/同层依赖违规
gofmt -l <本次修改的 Go 文件>      # 无输出（已全部格式化）
git diff --check                # 无输出（无空白符问题）
```

新增/涉及的具体测试：

**纯逻辑单测（`hybrid_test.go`，13 个，全部真实执行且 PASS，无 DB 依赖）**：
`TestRRFFusePromotesChunkHitByBothPaths`、`TestRRFFuseDedupesChunkPresentInBothPaths`、`TestRRFFuseVectorOnly`、`TestRRFFuseKeywordOnly`、`TestRRFFuseTiedFusionScoreBreaksByIDWhenScoreAlsoTies`（用 `(vectorRank=70, keywordRank=10)` 这对数值验证过的真实浮点数相等构造出的精确平局，不是近似相等）、`TestRRFFuseTruncatesToTopK`、`TestRRFFuseScoreIsRelevanceNotRawFusionScore`、`TestRRFFuseScoreForBothPathsHitIsMaxNotSum`、`TestRRFFuseInternalMapIterationDoesNotLeakIntoOutputOrder`（原名 `TestRRFFuseIsOrderIndependentAndDeterministicAcrossRuns`，本轮重命名并改写了文档注释，见第 0 节）、`TestRRFFusePreservesChunkMetadata`、`TestCandidateKBounds`，以及本轮新增的 `TestSortVectorCandidatesByScoreThenIDStableOnTiedScores`、`TestSortVectorCandidatesByScoreThenIDNeverReordersDifferentScores`、`TestVectorCandidateBuildOrderDoesNotAffectFinalHybridResult`。

**PostgreSQL 集成测试（真实对本机 pgvector+pg_trgm 执行，全部 PASS）**：

1. `TestIntegrationSearchKeywordChunksFindsExactChineseKeyword` — 精确中文关键词命中，断言具体 chunk ID。
2. `TestIntegrationSearchKeywordChunksFindsExactEnglishKeyword` — 英文关键词命中。
3. `TestIntegrationSearchKeywordChunksExcludesUnpublished` — 未发布 chunk 不命中。
4. `TestIntegrationSearchKeywordChunksScopedToKnowledgeBase` — 跨知识库不命中（正反两面都断言）。
5. `TestIntegrationSearchKeywordChunksIgnoresEmbeddingDimension` — 3 维/2 维 chunk 同时命中。
6. `TestIntegrationSearchVectorChunksOrderingAndDimensionFilterPGOnly` — 向量检索余弦排序 + 维度过滤。
7. `TestIntegrationSearchVectorChunksTiedScoreSortsStablyByID`（本轮新增）— 4 个 embedding 完全相同的 chunk 乱序插入，`searchVectorChunks` 必须按 ID 升序稳定返回，直接验证 SQL 层的修复。
8. `TestIntegrationHybridSearchPromotesStrongKeywordMatchIntoTopK` — 用真实 DB 数据证明纯向量 top4 会漏掉的强关键词命中，融合后进入 topK。
9. `TestIntegrationHybridSearchDeduplicatesChunkHitByBothPaths` — 同一 chunk 被两路真实命中后融合结果只出现一次。
10. `TestIntegrationHybridSearchPreservesCitationMetadata` — `document_name`/`page_number`/`section_title` 在向量路径、关键词路径、融合之后均完整保留。
11. `TestIntegrationSearchKeywordChunksEmptyQueryOrKBsReturnsEmpty` — 空 query、nil/空知识库列表均返回空。

迁移回滚验证（本轮重新执行，因为 down migration 内容变了）：手动对一个 scratch 数据库依次执行 000001→000004 的 up 迁移，确认 `pg_trgm.word_similarity_threshold=0.3`、`idx_chunks_content_trgm` GIN 索引、`pg_trgm` 扩展均存在；执行 000004 的 down，确认索引被删除、GUC 被 RESET，**但 `pg_trgm` 扩展仍然存在**（`SELECT extname FROM pg_extension WHERE extname='pg_trgm'` 仍返回一行）；再执行一次 up，确认索引/GUC 重新生效、扩展的 `CREATE EXTENSION IF NOT EXISTS` 因扩展已存在而跳过、无报错——完整验证了 up → down → up 可重复执行。

## 10. Eval 对比

**未运行真实 Eval。** 原因：

- `make eval` 需要 `JUDGE_MODEL_ID`/`EVAL_USER_ID` 两个环境变量指向一个已配置好 API Key 的真实 LLM 供应商模型和一个已存在的用户，本沙箱环境没有任何 LLM Provider 凭证。
- `internal/eval`/`cmd/evalrunner` 的整条链路要经过 `conversation`/`agent` 模块，最终落到 MySQL 的 `knowledge_bases`/`documents`/`conversations` 等表——本沙箱没有跑起来的 MySQL 实例（见上一节）。注意这与"Codex 在真实 Docker 环境跑通了 `go test` 全套集成测试"是两回事：`go test` 覆盖的是代码行为的正确性（含 `Service.Retrieve` 端到端集成测试），`make eval` 覆盖的是"检索到的内容对真实业务问答质量的影响"，需要 LLM Judge 打分，Codex 那次运行没有涉及 `JUDGE_MODEL_ID`/`make eval`，所以即便在有完整 Docker 环境的机器上，本阶段目前也仍然没有一次真正跑过 `make eval`。
- 没有 `eval/baseline.json` 之外可比对的新基线数据，凭空编造 `RetrievalHit`/`MRR`/`Recall@1`/`Recall@3`/`Citation Precision`/`Citation Coverage` 数字没有意义，也违反明确要求"不能编造结果"。

作为部分替代，第 9 节的 PostgreSQL 集成测试直接验证了 Eval 关心的底层确定性行为（能否命中期望 chunk、排序是否正确、Hybrid Search 是否让强关键词命中进入 topK），但这不等价于跑一遍 `eval/testset.yaml` 的端到端指标对比。

## 11. 未验证内容与剩余风险

- **未跑通真实端到端 Eval**：上一节已说明原因；`vectorWeight=0.65`/`keywordWeight=0.35`/`rrfK=60`/候选窗口公式都是基于 RRF 通用实践和本次集成测试里的具体场景选的合理默认值，没有用 `eval/testset.yaml` 的真实数据集做过量化调优或 A/B 对比，存在需要根据真实语料重新校准的可能。
- **`pg_trgm.word_similarity_threshold=0.3` 是数据库级全局默认值**：写在迁移里、对整个数据库生效（`ALTER DATABASE ... SET`），不支持按请求/按知识库动态调整（这也是"不允许动态调整权重"限制范围内的设计取舍）。如果未来出现关键词检索"过于宽松"或"过于严格"的真实反馈，需要改这个迁移里的常量并重新评估，而不是在运行时调参。
- **中文 trigram 边界效应未被真实语料验证**：`pg_trgm` 的字符 n-gram 对中文标点/断句的处理和英文空格分词不完全对等，本次测试用的是精心构造的短句，没有覆盖长文档、术语密集、标点稀疏等真实场景下 word_similarity 的表现。
- **本会话（沙箱）没有 MySQL 环境**，所以本会话里依赖 `setupIntegration`（需要同时起 MySQL + Postgres）的**既有**测试、以及 `Service.Retrieve` 层面的端到端集成测试（`TestIntegrationRetrieveMergesAcrossModelsAndSkipsInactive` 等）按设计 SKIP，本会话没有亲自跑通"多知识库跨 embedding 模型分组 + Hybrid Search"这个完整路径在 `Service.Retrieve` 层面的行为——这部分在本会话里只有代码审查和 `rrfFuse` 纯逻辑单测覆盖。**但**如第 9 节所述，Codex 已经在有完整 Docker 环境（MySQL + PostgreSQL + Redis）的机器上跑过 `go test -count=1 ./...`，其中就包含了这些原本会 SKIP 的既有集成测试、以及 `Service.Retrieve` 层面的端到端用例，结果是全部通过——这条风险在那台机器上已经被真实验证覆盖了；这里保留记录是因为验证是 Codex 在另一台机器上做的，不是本会话亲自跑的，如果换一个环境重新执行，仍然建议用 `make db-up` 起齐三个依赖后再跑一遍 `go test -count=1 ./...` 交叉确认。
- **性能未压测**：`candidateK` 上限 100、GIN 索引过滤 + 运行时 `word_similarity` 精排的组合在小规模测试数据下表现正常，但没有在 `maxChunksPerKnowledgeBase=5000` 级别的真实数据量上做过延迟测量。
