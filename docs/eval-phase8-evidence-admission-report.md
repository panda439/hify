# Phase 8：检索证据准入（Evidence Admission）实施报告

对应设计：[docs/superpowers/specs/2026-08-08-rag-evidence-admission-design.md](superpowers/specs/2026-08-08-rag-evidence-admission-design.md)
对应实施计划：[docs/superpowers/plans/2026-08-08-rag-evidence-admission-plan.md](superpowers/plans/2026-08-08-rag-evidence-admission-plan.md)

日期：2026-08-08。

## 0. 一句话总结

`knowledge.Service.Retrieve` 在 RRF 排序之后、内容去重之前新增一道来源感知的证据准入：向量余弦相似度 ≥0.35 或关键词 word-similarity ≥0.45，任一路达标即通过，两路都不达标（含压根没有该路信号）则整体拒绝；全部候选都被拒绝时 `Retrieve` 返回空切片和 `nil` error，对话仍正常调用模型回答，只是不注入知识库内容、不生成知识库引用。不修改 RRF 权重/`candidateK`/topK/邻接窗口/Citation/SSE/RAG 预算。

## 1. 为什么统一 0.2 下游门槛不够

`conversation/budget.go` 的 `ragMinSimilarityScore=0.2` 在 Phase 8 之前是唯一的相关性门槛，且天然有两个问题：

1. **量尺不统一**：向量路径的 `RetrievedChunk.Score` 是 pgvector 余弦相似度，关键词路径是 pg_trgm word-similarity——两者数值分布完全不同（余弦相似度在真实语义相关文本间通常聚集在 0.3-0.9，word-similarity 对短查询词的精确匹配可以轻易到 1.0，但对语义相关但字面不同的文本几乎为 0）。用同一个 0.2 阈值去卡两种量尺，等于既没有为向量设对门槛，也没有为关键词设对门槛。
2. **过滤时机太晚**：0.2 门槛发生在 `selectEvidence`（对话侧组装最终 prompt 时），此时 `knowledge.Retrieve` 已经完成了 topK 截断和邻接窗口查询——一个低质量候选可能已经挤占了 topK 名额（让真正相关的候选没能进入结果），也已经为自己触发了一次邻接块数据库查询（纯粹浪费）。到 `selectEvidence` 才把它过滤掉，伤害已经造成。

Phase 8 把准入挪到 `knowledge` 层内部、RRF 排序之后但在 topK 截断和邻接查询之前，同时按来源分别设置门槛，两个问题一起解决。`conversation/budget.go` 的 0.2 门槛**保留不动**，作为下游防御性兜底——两层门槛的职责边界见第 5 节。

## 2. 向量 0.35 / 关键词 0.45 的准入语义

- **向量相似度 ≥ 0.35**：候选通过，不需要关键词路径也命中。
- **关键词 word-similarity ≥ 0.45**：候选通过，不需要向量路径也命中。
- **同时被两路召回**：任意一路达标即可，不要求两路都达标。
- **两路都不达标，或某一路压根没有信号**（比如向量搜索没召回这个 chunk，只有关键词召回了它）：候选被拒绝。**缺失的一路绝不能被当成"0 分"参与判断**——`admitBySourceSignal`（`internal/knowledge/admission.go`）用 `haveVector`/`haveKeyword` 两个独立布尔显式区分"这一路真的测过、分数是 X"和"这一路压根没有测过"，只有真实存在的信号才会被拿去和阈值比较，也只有真实存在但没达标的信号才会被计入 `VectorBelowAdmissionCount`/`KeywordBelowAdmissionCount`。

两个门槛都是包内常量（`vectorAdmissionThreshold`/`keywordAdmissionThreshold`），不做运行时可配置，也不用"和第一名的分差"这类相对判断代替绝对门槛——设计文档 §2.2 明确排除了动态相对分差方案：当整批结果都很差时，第一名依然是"相对最好"，不能证明它真实相关，不符合 Phase 8"宁可少召回也不注入可能无关资料"的保守目标。

**不能用 fusionScore 做判断**：RRF 的 fusionScore 是一个排名信号（典型值在 0.005-0.02 量级，`rrfK=60` 主导分母），不是 0..1 的相关度，把它拿去和 0.35/0.45 比较在数值上就是错的。`admission.go` 里的准入判断只读 `fusionEntry.vectorScore`/`fusionEntry.keywordScore`——每条候选各自路径的原始分数，从未读取 `fusionEntry.fusionScore`。

## 3. 为什么顺序是「RRF 排序 → 准入 → 内容去重 → topK → 邻接查询」

完整顺序（`hybrid.go` 的 `rrfFuse`）：

1. 向量、关键词两路各自召回扩大候选集（不变）；
2. 按 chunk ID 汇总 RRF 信号、计算 fusionScore（不变）；
3. 按 fusionScore 降序 / Score 降序 / ID 升序完成确定性排序（不变）；
4. **在完整候选集上执行来源感知准入**（新增）；
5. 对通过准入的候选执行 `dedupExactContentChunks`（Phase 5，位置从"排序后"改为"准入后"）；
6. 截断到 topK，允许后面的合格候选补位（不变，但现在补位的候选池已经是"准入后"的）；
7. 只为通过准入的核心块批量查询邻接块（`expandWithNeighborWindow` 本身不用改——见第 4 节）；
8. 邻接扩展后再做一次内容去重（不变）。

**准入不能放在 topK 截断之后**：如果先截断再准入，被拒绝的候选会在结果里留下一个空槽，后面本该补位的合格候选永远没有机会进来——`rrfFuse` 返回的切片会比 topK 短，而不是被正确的候选填满。

**准入必须发生在精确内容去重之前**：正文相同的 A、B 两条候选，如果 A 排名更高但两路原始分数都没达标，B 排名稍低但有一路达标——如果先跑内容去重（按"保留排名更高者"规则），会先保留 A、丢弃 B，随后准入层再把 A 淘汰，最终两条都不在结果里，B 的合格内容被永久性错误丢弃。正确顺序是先把不合格的 A 从候选池里删除，再在剩下的候选（此时只有 B）里执行"同正文保留排名最高者"——这样 B 自然存活。`TestRRFFuseAdmissionBeforeDedupKeepsAdmittedDuplicateOverRejectedHigherRank`（`admission_test.go`）直接构造了这个场景。

## 4. 邻接查询天然只作用于通过准入的核心块，不需要额外改动

`service.go` 的 `expandWithNeighborWindow` 接收的 `anchors` 参数，就是 `rrfFuse` 返回的、已经完成"准入 → 内容去重 → topK 截断"的核心块列表。因为准入被拒绝的候选从未进入 `anchors`，`buildNeighborRequests`（Phase 7）自然不会为它们生成任何邻接坐标——`expandWithNeighborWindow` 这个方法本身**一行代码都没有改**。全部候选被拒绝时，`anchors` 是长度为 0 的空切片，`expandWithNeighborWindow` 已有的 `len(anchors) == 0` 早退路径直接返回，不发起任何批量查询——这正是设计文档 §5 要求的"无相关证据时不执行邻接批量查询"，靠 Phase 7 已有的早退逻辑免费满足，不需要新增分支。

## 5. 无有效证据时的行为

`Service.Retrieve` 全部候选被拒绝时：

- 返回空切片（非 nil，`rrfFuse` 内部用 `make([]RetrievedChunk, 0, ...)` 构造）和 `nil` error；
- 不触发任何邻接批量查询（见第 4 节）；
- `conversation.assembleContext` 因为拿到空的检索结果，自然不会生成 `<retrieved_sources>` 消息、不添加 citation system rules、不产生知识库 Citation（这条路径本身在 Phase 8 之前就已经是"空结果 = 不注入"，Phase 8 只是让"全部候选不合格"这个新增场景也走同一条已有路径，没有引入新的分支）；
- 对话仍然正常调用模型回答——Phase 8 不做"知识库无答案"固定回复，也不阻止模型基于自身知识作答，和设计文档 §5 的范围声明一致。

## 6. 两层门槛的职责边界

| | `knowledge` 层准入（Phase 8，本报告） | `conversation/budget.go` 的 `ragMinSimilarityScore=0.2` |
|---|---|---|
| 判断依据 | 每个候选**自己的**原始向量/关键词分数（来源分离） | `RetrievedChunk.Score`（向量、关键词两路取较大值，单一量尺） |
| 发生时机 | RRF 排序之后，内容去重/topK/邻接查询之前 | 最终组装 prompt 时，是流水线里最后一道过滤 |
| 目的 | 决定"这条候选有没有资格成为核心块"，避免不合格候选浪费 topK 名额和邻接查询 | 兜底：即便某条 Score 由于某种未预见的路径异常低于常理，仍有最后一道防线不把它塞进 prompt |
| 是否可能同时生效 | 是——两层不是互斥关系。0.35/0.45 通常比 0.2 更严格，所以正常情况下 0.2 这道门槛几乎不会再单独刷掉任何东西 | 保留但不删除、不提高——设计文档 §7 明确要求，且这条防线的语义（防御性兜底）与 Phase 8 的语义（准入决策）不同，混为一谈会让未来的维护者误以为两处是重复代码 |

## 7. 实现细节

### 7.1 修改/新增文件

- 新增 `internal/knowledge/admission.go`：`vectorAdmissionThreshold=0.35`/`keywordAdmissionThreshold=0.45` 两个包内常量、`admitBySourceSignal`（纯函数）、`admissionStats`（安全可记录的聚合计数类型）。
- 新增 `internal/knowledge/admission_test.go`：`admitBySourceSignal` 的 5 组纯逻辑单测（对应计划 Task 1 的 1-5 项）+ 4 个 `rrfFuse` 编排级测试（对应计划 Task 1 的 6-9 项：拒绝项不占 topK、准入先于去重两个方向的场景、准入/去重都不改写保留字段）。
- 修改 `internal/knowledge/hybrid.go`：`fusionEntry` 新增 `haveVector`/`vectorScore`/`haveKeyword`/`keywordScore` 四个字段；`rrfFuse` 的 `addPath` 新增 `isVector` 参数区分两路、分别维护这四个字段；`rrfFuse` 返回值从 `([]RetrievedChunk, int)` 改为 `([]RetrievedChunk, admissionStats)`，排序后先执行准入过滤，再对admitted 列表执行 `dedupExactContentChunks`，最后截断 topK。
- 修改 `internal/knowledge/hybrid_test.go`：更新 `fuseIDs`/`TestRRFFuseReturnsCoreDuplicateCount`（改读 `admissionStats.ContentDuplicateCount`）；把若干 pre-Phase-8 测试里低于新阈值的测试分数（`0.1`/`0.4` 等）调整到阈值以上，避免这些测试意外撞上准入拒绝（这些测试本身验证的是 RRF 融合/排序/去重，不是准入本身，见每处改动旁的注释）。
- 修改 `internal/knowledge/service.go`：`Retrieve` 改用 `admissionStats` 接收 `rrfFuse` 的第二个返回值；`expandWithNeighborWindow` 方法本身零改动（见第 4 节）；debug 日志扩展为 `candidate_count_before_admission`/`vector_below_admission_count`/`keyword_below_admission_count`/`admission_rejected_count`/`admitted_anchor_count`/`core_duplicate_count`/`neighbor_duplicate_count`/`topK`，只在有拒绝或去重发生时才打（和 Phase 5 既有约定一致），只含计数与阈值常量，不含 query/正文/embedding/逐条分数。
- 修改 `internal/knowledge/neighbor_batch_test.go`：新增 2 个 spy 测试，驱动真实 `rrfFuse -> expandWithNeighborWindow` 序列（而不是手造 anchors）——全部候选被拒绝时 0 次批量调用、混合候选时批量请求集合只包含被准入的 anchor 的坐标（用一个"被拒绝候选专属文档 ID"的探针断言它绝不出现在请求里）。
- 修改 `internal/knowledge/integration_test.go`：新增 `cosineVec`/`seedVectorOnlyFillers` 两个测试 helper 及 8 个真实 MySQL+PostgreSQL 驱动的 `Service.Retrieve` 集成测试（详见 7.3 节）；修正 `TestIntegrationRetrieveMergesAcrossModelsAndSkipsInactive` 里 `r3-weak` 的向量（`[1,4,0]` cos≈0.24 会被 Phase 8 新增的准入正确拒绝，但这个测试本意是验证跨模型合并排序，不是验证准入——改成 `[1,2,0]` cos≈0.45，继续明显弱于满分命中但保持在准入线之上）；`TestIntegrationRealVectorSearchDrivenAnchorSelectionPrefersCoreOverDuplicateNeighborContent` 更新注释和断言变量名，说明 Phase 8 之后 `anchor-prev`（cos=0）现在总是被准入层直接拒绝（而不是"可能在核心去重阶段、也可能在邻接去重阶段被捕获"），邻接探针改为总是在邻接去重阶段捕获。
- 修改 `internal/knowledge/eval_gate_test.go`：新增 3 个 KB 和 3 个门禁 case（详见 7.4 节），更新文件头部注释里的 case 数量说明。
- 修改 `README.md`：RAG 全流程段落插入 Phase 8 准入描述；Phase 6 门禁段落更新为 9 个 case、说明负样本各自独立断言；报告链接列表加入本报告。
- 修改 `docs/critical-paths.md`：链路 3 标题、关键节点、终点状态三处补充准入描述；覆盖状态一栏（第 38 行附近）补充 Phase 8 单测 + 集成测试清单；顶部生成时间注记追加 Phase 8 更新说明。
- 新增本报告 `docs/eval-phase8-evidence-admission-report.md`。

### 7.2 未修改

- `internal/knowledge/dedup.go`/`neighbor.go`：`dedupExactContentChunks`/`expandWithNeighbors`/`buildNeighborRequests` 逻辑零改动——准入是在调用它们之前先过滤好输入，它们自己不需要知道"准入"这个概念存在。
- `internal/knowledge/service.go` 的 `expandWithNeighborWindow` 方法体：零改动（见第 4 节）。
- RRF 权重（`vectorWeight`/`keywordWeight`）、`rrfK`、`candidateK` 公式、`maxTopK`/`defaultTopK`、邻接窗口大小（仍然只取 immediate previous/next）、批量查询架构（Phase 7 的一次批量调用）：零改动。
- Citation 协议、SSE 协议、前端、`conversation/budget.go` 的 RAG 字符预算、`ragMinSimilarityScore` 常量本身：零改动。
- `internal/eval`（agent 级 Eval，`cmd/evalrunner` 用的那套）、`eval/baseline.json`、`eval/testset.yaml`：零改动。

### 7.3 真实数据库集成测试（8 个，对应计划 Task 4 的 1-8 项）

全部驱动公开的 `Service.Retrieve`（不是绕过公开入口直接测 `rrfFuse`），使用真实 pgvector 余弦相似度和真实 pg_trgm word-similarity，不是手工构造的分数：

1. `TestIntegrationRetrieveAdmissionReturnsEmptyForIrrelevantQueryAgainstNonEmptyKB`——非空 KB + 正交向量 + 无关键词命中，返回空。
2. `TestIntegrationRetrieveAdmissionVectorThresholdBoundaryAgainstRealPgvector`——真实 pgvector 算出的余弦相似度在 0.34（拒绝）/0.36（通过）两侧正确判定；用 `cosineVec(c)` helper（`[c, sqrt(1-c²), 0]`，和 fake provider 固定返回的查询向量 `[1,0,0]` 的点积恰好等于 `c`）精确构造分数，而不是近似凑数。**未在 0.35 这个精确浮点值上测试**——float32 的 pgvector 列存储 + 真实 SQL 余弦计算的舍入可能让一个数学期望等于 0.35 的目标值落在 0.35 的任一侧，浮点精确相等的边界由 `admission_test.go` 的纯 float64 单测（`TestAdmitBySourceSignalVectorThreshold`）负责，不依赖真实 SQL 的舍入行为。
3. `TestIntegrationRetrieveAdmissionKeywordThresholdBoundaryAgainstRealPgTrgm`——真实 pg_trgm 算出的 word-similarity 边界：`"abcdefghij"` 分别对 `"xx abcd yy"`（实测 ≈0.3636，拒绝）和 `"xx abcde yy"`（实测 ≈0.4545，通过）——两个数字都是用 `docker exec ... psql -c "SELECT word_similarity(...)"` 直接查出来的真实值，不是手动估算。
4. `TestIntegrationRetrieveAdmissionEitherStrongPathAdmitsBothWeakRejects`——两路都弱的候选被拒绝，纯向量强命中和纯关键词强命中都被通过。
5. `TestIntegrationRetrieveAdmissionRejectedTopCandidateLetsLowerRankedAdmittedCandidateBackfillTopK`——topK 前部拒绝项被删除、后续合格候选补位。用 `seedVectorOnlyFillers` 播种 8 条向量填充块把 `candidateK(topK=2)=8` 的向量检索窗口占满，让两条只靠关键词达标的候选彻底不出现在向量候选池里，从而能构造出"fusionScore 最高但被准入拒绝的候选，ranked 高于两条只靠关键词准入的候选"这个真实场景（如果不这样隔离，任何单一路径内部排名和分数是单调一致的，无法在真实 DB 分数下构造"高排名但不合格"的候选，详见测试内联注释和下面 8.1 节）。
6. `TestIntegrationRetrieveAdmissionRejectedCandidateDoesNotInterfereWithDuplicateResolutionAmongAdmittedSurvivors`——见 8.1 节，这是对计划 Task 4 第 6 项的可实现改写。
7. `TestIntegrationRetrieveAdmissionRejectedCandidateNeverTriggersNeighborLookup`——被拒绝候选自己文档下的"污染探针"邻接块绝不出现在最终结果里。
8. `TestIntegrationRetrieveAdmissionCorrectAcrossKnowledgeBasesAndEmbeddingModels`——两个 KB（`m3` 3 维 / `m2` 2 维两个不同 embedding 模型）各自一条通过一条拒绝，准入结果和 KB/模型无关。

### 7.4 Phase 6 门禁扩展（9 个 case）

保留原有 6 个 case（全部继续 PASS，`make eval-retrieval-gate` 验证），新增 3 个：

- `nonempty_kb_irrelevant_query`：非空 KB + 无关查询返回空，负样本，独立断言 `ResultCount==0`。
- `vector_below_admission`：唯一候选的余弦相似度 0.2（低于 0.35）且无关键词信号，返回空，负样本，独立断言 `ResultCount==0`。
- `admitted_candidate_backfills_topk`：正样本，topK=2 时最终结果必须精确是 `[ga-kw-1st, ga-kw-2nd]`，`ga-rej-top`（fusionScore 最高但不合格）绝不出现。

两个负样本 case 的 `ExpectedConfigured=false`，不参与 `Hit@1`/`Hit@3`/`MRR` 聚合平均（这是 `retrieval.AggregateMetrics` 既有的设计，Phase 6 就有），但每个都在自己的 `t.Run` 里有独立的 `t.Fatalf` 断言——不会出现"聚合指标筛掉负样本、门禁形同虚设"的情况，满足计划 Task 5 的"负样本必须有直接断言"要求。9 个 case 全部真实执行，`go test -v` 输出确认无一个被跳过；`make eval-retrieval-gate` 的四项聚合指标（Hit@1/Hit@3/MRR/ContentUniqueRate）仍然全部是 1.0。

## 8. 关于真实数据库测试构造的一个数学限制（诚实记录）

### 8.1 「正文完全相同、排名反转、准入结果相反」的场景无法用真实 DB 分数构造

计划 Task 4 第 6 项原文是"高排名不合格重复项不能让低排名合格重复项丢失"，字面理解是构造两条**正文完全相同**的候选，其中排名（fusionScore）更高的一条不合格、排名更低的一条合格。这个场景在 `admission_test.go`（`TestRRFFuseAdmissionBeforeDedupKeepsAdmittedDuplicateOverRejectedHigherRank`）里用直接注入的合成分数完整覆盖了，但在真实数据库驱动的集成测试里，这个精确组合在数学上不可构造，原因：

- 两条正文完全相同的行，关键词路径的 `word_similarity` 是对 `content` 字段算出来的，正文相同意味着这两行的关键词原始分数**必然相等**——要么同时通过关键词准入，要么同时不通过，不可能一条通过一条不通过。
- 于是唯一能让"一条合格一条不合格"成立的路径是向量分数不同——但向量搜索的排序（进而 RRF 里那一路的 rank）本身就是按余弦相似度降序排列的：分数更高的那条，在向量路径里的 rank 必然不低于（通常严格高于）分数更低的那条。一条更高分（因而更可能通过准入）的候选，不可能在同一个按分数排序、`LIMIT candidateK` 截断的真实查询结果里排在另一条更低分（因而更可能被拒绝）候选的后面。

也就是说，"正文相同 + 排名反转 + 准入结果相反"这个精确组合，只有在测试直接控制"list 位置"和"分数"这两个独立变量时才能构造出来（`rc()`/`rcContent()` helper 正是为此设计），而真实 SQL 的排序把这两个变量绑死成单调关系，天然无法制造这个组合。

`TestIntegrationRetrieveAdmissionRejectedCandidateDoesNotInterfereWithDuplicateResolutionAmongAdmittedSurvivors`（真实数据库测试第 6 项）因此改为验证这个不变量在真实场景下唯一可能出现的形态：一个**内容不相关**、排名靠前的候选被准入拒绝，不会干扰排名靠后的一对**正文相同**候选之间正常的 Phase 5 去重（保留其中排名更高的一条）——最终结果既不含被拒绝的候选，也不含重复正文的两条，只有去重后幸存的那一条。这个测试和 `admission_test.go` 的纯逻辑测试合起来，仍然完整覆盖了设计文档 §3 要求的"先准入、再去重"顺序不变量：纯逻辑测试证明顺序本身正确，真实 DB 测试证明这个顺序在真实查询结果上不会引入意外交互。

## 9. 真实测试结果

本机环境：docker-compose 起的 `hify-mysql-1`（MySQL 8）+ `hify-postgres-1`（pgvector/pg17，含 `pg_trgm` 扩展）+ `hify-redis-1`，均可用——不同于此前若干轮次的云端沙箱（只有 Postgres、没有 MySQL），本轮所有需要 MySQL 的集成测试（含 `Service.Retrieve` 全链路）都是真实执行，没有 SKIP。

```
gofmt -l .                          # 无输出（既有的 internal/workflow/integration_test.go 未格式化问题与本阶段无关，未处理）
go build ./...                      # 通过
go vet ./...                        # 通过
go test -count=1 ./...              # 全部 PASS，无 SKIP
go test -race -count=1 ./...        # 全部 PASS，无 race
go test -count=1 -v ./internal/knowledge -run 'TestAdmitBySourceSignal'          # 5/5 PASS
go test -count=1 -v ./internal/knowledge -run 'TestRRFFuseAdmission'             # 4/4 PASS（编排级）
go test -count=1 -v ./internal/knowledge -run 'TestExpandWithNeighborWindow'     # 8/8 PASS（含 2 个新增 Phase 8 spy 测试）
go test -count=1 -v ./internal/knowledge -run 'TestIntegrationRetrieveAdmission' # 8/8 PASS，真实 MySQL+PostgreSQL，无 SKIP
go test -count=1 -v ./internal/knowledge -run 'TestRetrievalGatePhase6'          # 9/9 sub-case PASS，无 SKIP
make eval-retrieval-gate                                                        # PASS，Hit@1/Hit@3/MRR/ContentUniqueRate 均为 1.0
make check-deps                     # OK - no cross-layer or same-layer violations
git diff --check                    # 无输出
git status --short                  # 只有本阶段正式改动 + .claude/CODEX_CLAUDE_HANDOFF.md（不提交）+ eval/runs/（已 gitignore）
```

`internal/eval/retrieval` 和 6 个原有 Phase 6 门禁 case 的指标定义、阈值判定逻辑本阶段零改动（`GateThresholds`/`EvaluateGate`/`AggregateMetrics` 未触碰），只是新增了 3 个使用同一套既有类型的新 case。

## 10. 未验证 / 剩余风险

- 没有做任何性能基准测试——准入本身是纯内存操作（一次遍历 + map 查找），不引入新的外部调用，预期开销可忽略，但没有实测数字，任务本身也未要求。
- 门禁阈值（`retrievalGateThresholds()` 里的 `MinHitAt1=MinHitAt3=MinMRR=MinContentUniqueRate=1.0`）延续 Phase 6 已有的结论：这 9 个 case 都被设计成健康实现下必然 100% 命中/唯一，1.0 是这批固定 case 的零容忍阈值，不是"所有检索场景在生产环境都应该 100% 命中"的通用结论。
- 0.35/0.45 两个阈值本身是设计阶段基于对两种相似度量尺特性的定性判断固定下来的（设计文档 §2.3），本阶段的测试只验证"实现是否忠实执行了这两个数字"，不对"这两个数字本身是否是全局最优阈值"做统计意义上的验证——设计文档已经声明这是本阶段的既定假设，未来只有真实评测证据支持时才应该单独立项调整，不在本阶段范围内。
