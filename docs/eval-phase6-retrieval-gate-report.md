# RAG 优化第六阶段：确定性检索回归门禁（Deterministic Retrieval Eval Gate）评估报告

日期：2026-08-08（二次更新：修复 Codex 第一轮审核发现的 2 个问题，见第 0 节）。前置阶段：[docs/eval-phase3-hybrid-search-report.md](docs/eval-phase3-hybrid-search-report.md)（Hybrid Search）、[docs/eval-phase4-neighbor-window-report.md](docs/eval-phase4-neighbor-window-report.md)（邻接分块扩展）、[docs/eval-phase5-content-dedup-report.md](docs/eval-phase5-content-dedup-report.md)（内容去重）。本阶段不新增检索算法，只给 Phase 3-5 已经实现的 Hybrid Search、邻接扩展、内容去重加一套不依赖真实 LLM/Judge、可以稳定重复执行的确定性质量验收基线。

## 0. 二次更新：修复 Codex 第一轮审核发现的问题

Codex 第一轮审核（`.claude/CODEX_CLAUDE_HANDOFF.md`）确认核心门禁逻辑通过（`make eval-retrieval-gate` 真实连 MySQL+PostgreSQL、6 个 case 全部真实 PASS 无 SKIP，`go test`/`-race`/`vet`/`check-deps`/`diff-check` 全部通过），但发现 2 个问题，全部已修复：

1. **报告保存了禁止字段 Score（隐私缺陷）**：`internal/eval/retrieval/report.go` 的 `GateHit` 原来包含 `Score` 字段，`internal/knowledge/eval_gate_test.go` 也把 `c.Score` 写了进去——任务第 5 条是白名单约束（只允许 case 名、chunk/document ID、rank、NeighborOf、计数与聚合指标），`Score` 不在允许范围，代码注释此前把它错误地列为"任务允许字段"。修复：从 `GateHit` 和构造逻辑里删除 `Score`；`internal/eval/retrieval/report_test.go` 新增 `TestGateReportTypesCarryNoForbiddenFields`——用反射遍历 `GateReport` 整个类型图，断言不存在任何字段名/JSON 标签等于 content/score/query/embedding/fingerprint/vector，不再只靠人工读结构体定义。
2. **报告相对路径写错位置、污染工作区（可复现缺陷）**：`TestRetrievalGatePhase6` 原来用相对路径 `"eval/runs/phase6-retrieval-gate-latest.json"` 调 `SaveReport`，但 `go test` 执行时的工作目录是包目录 `internal/knowledge`，实际写出的文件落在 `internal/knowledge/eval/runs/...`，不在 `.gitignore` 保护范围内，`git status` 会出现未追踪的 `internal/knowledge/eval/`。修复：新增 `retrievalGateReportPath` 辅助函数——默认写进 `t.TempDir()`（测试结束自动清理，不管测试二进制的工作目录在哪都不会碰到源码树），只有设置了 `HIFY_RETRIEVAL_GATE_REPORT_PATH` 环境变量时才写到指定路径；`make eval-retrieval-gate` 现在用 `$(CURDIR)` 拼出仓库根目录的绝对路径传给这个变量，保留人类可读报告的同时，普通 `go test ./...`（包括 CI）不再产生任何工作区污染。新增 `TestRetrievalGateReportPathDefaultsAwayFromSourceTree`/`TestRetrievalGateReportPathHonorsEnvOverride` 两个纯逻辑测试（不依赖数据库）验证这两条路径分支。

清理：本轮返修过程中产生的 `internal/knowledge/eval/`（Codex 真实运行触发的那次路径错误的产物）和 `_to_delete/` 整个目录（含上一轮遗留的中转文件）均已删除，均未加入、也不会加入任何 commit。

未扩展范围：本轮只修复以上 2 项，没有改动门禁阈值、6 个 case 的检索逻辑或指标定义，`Service.Retrieve`/`internal/knowledge` 生产代码本轮零改动。

## 1. 问题背景

Phase 3-5 各自都有充分的单测和真实数据库集成测试，但那些测试是"针对单个规则的定向验证"（比如"两条重复正文只保留一条"），不是"一次跑完、覆盖检索链路多个维度、有明确阈值、失败就报错退出"的回归门禁。`internal/eval` + `cmd/evalrunner`（`make eval`）已经有一套回归工具，但它是 agent 级的：需要真实 LLM Judge 打分、需要 `JUDGE_MODEL_ID`/`EVAL_USER_ID`，且它的 `Regressed`（决定退出码的函数）明确只看 Judge 分数，确定性指标只供人工审阅、不接入 CI 退出码（见 README「Eval」一节）。用户已明确 Hify 的 LLM 调用基本使用 mock，因此本阶段不能把真实模型/API Key 作为验收前提——需要一套完全不碰 LLM、纯粹检验"检索链路本身有没有退步"的门禁。

## 2. 设计原则（对应任务里的核心要求）

1. **必须走公开的 `knowledge.Service.Retrieve` 入口，不能用纯函数结果冒充端到端检索评测。** `internal/knowledge/eval_gate_test.go` 的 `TestRetrievalGatePhase6` 每个 case 都调用真实构造的 `Service`（`newTestService`，真实 `Repository` 接真实 MySQL+PostgreSQL/pgvector/pg_trgm，`fakeProviderService` 只替换 embedding 供应商，不改变 `Service.Retrieve` 内部任何一步：`embedQuery` → `searchVectorChunks`/`searchKeywordChunks` → `rrfFuse` → `expandWithNeighborWindow` → `expandWithNeighbors`，全部真实执行）。
2. **固定、隔离、可重复清理的数据集。** `setupEvalGate` 用独立的 `testutil.MySQL(t, "evalgate")`/`testutil.Postgres(t, "evalgate")`——`testutil` 对每个不同的 name 参数都会先 `DROP DATABASE IF EXISTS` 再重建、迁移到最新版本（见 `internal/testutil` 包注释），所以 `hify_test_evalgate` 这两个库在每次 `go test` 进程启动时都是全新的：不会和 `internal/knowledge` 包里其它测试共用的 `hify_test_knowledge` 库产生 ID 冲突，不会因为上一次运行留下的脏数据导致这次失败，也不需要任何手工清理步骤——"隔离"和"幂等"是 `testutil` 的既有机制免费提供的，本阶段没有另写清理逻辑。
3. **确定性指标：Hit@1、Hit@3、MRR、返回内容唯一率，定义写清楚，不把 Hit@K 错称为 Recall@K。** 见 `internal/eval/retrieval/gate.go`：`HitAtK`/`ReciprocalRank`/`ContentUniqueRate` 三个纯函数 + `AggregateMetrics` 聚合 + `GateMetrics` 类型。`HitAtK`/`ReciprocalRank` 的文档注释明确写了"Hit@K 是针对一个可接受文档集合的成员检查，不是多相关文档意义上的召回率"——和 `internal/eval` 里 `RAGMetrics.RecallAt1`/`RecallAt3` 已经用的同一套定义、同一句免责声明一致（见 `internal/eval/model.go`）。`ContentUniqueRate` 是 `(ResultCount - DuplicateContentCount) / ResultCount`，空结果集定义为 1.0（没有可去重的东西，不是除零错误）。
4. **回归门禁必须有明确阈值、能返回非零退出码或测试失败，不能只生成报告供人肉查看。** `EvaluateGate`（`internal/eval/retrieval/gate.go`）是一个纯函数：输入聚合指标 + `GateThresholds`，输出 `(ok bool, reasons []string)`。`TestRetrievalGatePhase6` 在真正的数据库测试里调用它，`!ok` 时 `t.Fatalf`——这就是一次真的 `go test` 失败，非零退出码，不是"生成 JSON 报告等人去看"。`EvaluateGate` 本身完全不依赖数据库，所以"健康结果通过"和"任意一项指标退化就失败"两条路径都各有独立的、跑在毫秒级的纯单测（见第 6 节），不依赖能不能连上数据库就能验证门禁逻辑本身是对的。
5. **评测产物不得保存正文、query、embedding 或指纹，只允许 case 名、chunk/document ID、rank、NeighborOf、计数与聚合指标。** `retrievalGateOutcome`（`internal/knowledge/eval_gate_test.go`）是唯一一处看到 `RetrievedChunk.Content` 的地方——它把每条结果规约成一个去重比较键（复用 Phase 5 的 `normalizeContentForDedup`），立刻算完 `DuplicateContentCount` 就丢弃，从不把 Content 本身放进返回值。`retrieval.CaseOutcome`（聚合结果）和 `retrieval.GateHit`（保存进报告的单条记录）两个类型都是结构上就放不下 Content/query 文本/embedding 的——字段只有 `Rank`/`ChunkID`/`DocumentID`/`KnowledgeBaseID`/`NeighborOf`/几个计数（`Score` **不在**允许清单里，第一轮版本曾误把它写进 `GateHit`，已在第 0 节记录的返修里删除；`internal/eval/retrieval/report_test.go` 新增了反射级测试，遍历 `GateReport` 整个类型图断言不存在名叫 content/score/query/embedding/fingerprint 的字段，防止再次回归）。报告文件默认写进 `t.TempDir()`，只有 `make eval-retrieval-gate` 通过 `HIFY_RETRIEVAL_GATE_REPORT_PATH` 环境变量显式指向仓库根目录的 `eval/runs/phase6-retrieval-gate-latest.json`（已在 `.gitignore`：`eval/runs/*.json`）时才会落在那里——详见第 0 节。
6. **数据隔离且幂等，不能因为旧数据、固定主键冲突或残留发布版本失败，不能清理其它业务/测试数据。** 见第 2 点——`testutil` 的 DROP+CREATE-每次全新 机制天然满足，且专属的 `hify_test_evalgate` 库和其它任何测试、任何业务库都没有交集，不存在"清理了别人的数据"的风险。
7. **清晰入口 + Makefile 命令，说明为什么没有破坏现有 Eval。** `make eval-retrieval-gate` 跑 `go test -v -race -count=1 -run TestRetrievalGatePhase6 ./internal/knowledge/`。选择"真实 go test，而不是新的 cmd 二进制"的原因见第 3 节。它和 `make eval`（`cmd/evalrunner`）是两套完全独立、互不调用、互不影响的工具：`make eval` 需要的 `JUDGE_MODEL_ID`/`EVAL_USER_ID`/裁判模型这套依赖，`make eval-retrieval-gate` 一个都不需要；反过来 `cmd/evalrunner`/`internal/eval` 的任何代码本阶段一行都没有改动（`go build ./...`/`go vet ./...`/`go test ./internal/eval/...` 全部照常通过，见第 7 节）。
8. **更新 README、critical-paths.md，新增本报告，如实记录真实执行与 skip 情况。** 见 README「确定性检索回归门禁（Phase 6）」小节、`docs/critical-paths.md` 链路 3 覆盖状态一栏的补充说明、本报告第 7 节。

## 3. 架构选择：为什么不扩展 `cmd/evalrunner`，也不新建一个 cmd 二进制

`cmd/evalrunner`/`internal/eval`（`runner.go`）是围绕 `conversation.Service` + LLM Judge 构建的：`Run` 需要一个真实（或可流式回复的）会话服务和一个裁判模型客户端。本阶段任务明确禁止把 LLM/Judge 依赖引入这个门禁，所以复用 `cmd/evalrunner` 的执行路径行不通。

另一个选项是新建一个独立的 `cmd/retrievalgate` 二进制，直接连生产风格的 MySQL/PostgreSQL DSN（像 `cmd/evalrunner/main.go` 那样用 `config.Load()` + `platform.NewMySQLPool`/`NewPostgresPool`）。这条路径的问题是：本阶段任务要求数据集"固定、隔离、可重复清理"——`internal/testutil` 已经完整实现了这个能力（每个包一个独立的、每次运行前 DROP+CREATE 的 `hify_test_<name>` 库），但它是围绕 `*testing.T` 设计的（用 `t.Skipf` 表达"容器没起"，而不是返回一个错误）。要在一个普通 `main()` 里复用同样的隔离保证，要么重新实现一遍 DROP+CREATE+migrate 的逻辑（第二份要维护的代码，容易和 `testutil` 慢慢跑偏），要么修改 `testutil` 让它同时支持 `*testing.T` 和非测试调用者（扩大一个专门为测试设计的包的职责边界，且没有生产环境需要这个能力）。

因此本阶段选择：**门禁本体是一个真实的 `go test`**（`internal/knowledge/eval_gate_test.go` 的 `TestRetrievalGatePhase6`），和这个包里所有其它 Phase 3/4/5 集成测试用的是完全相同的机制（`testutil.MySQL`/`testutil.Postgres`、跳过时打印原因而不是静默跳过）；**纯逻辑部分独立成一个新的叶子包** `internal/eval/retrieval`（不导入 `internal/eval`、不导入 `internal/conversation`、不导入 `internal/knowledge`），单独可测，也避免了一个具体的 import cycle 问题：`internal/eval`（`runner.go`）导入 `internal/conversation`，而 `internal/conversation` 导入 `internal/knowledge`——如果 `internal/knowledge/eval_gate_test.go` 直接导入 `internal/eval`，会形成 `knowledge -> eval -> conversation -> knowledge` 的环，编译不过。`internal/eval/retrieval` 是 `internal/eval` 的同级包而不是子复用，但概念上延续了 `internal/eval` 已经在用的 `{Evaluated, Value}` 指标形状（`BoolMetric`/`FloatMetric`，字段名和语义都和 `internal/eval/model.go` 里的同名类型一致）和 Hit@K 而非 Recall@K 的定义——这是"复用做法"而不是"复用代码"，但达到的效果一致：两套 Eval 工具的指标语义不会互相矛盾。

`Service.Retrieve` 的公开签名和 `internal/knowledge` 包内任何生产代码本阶段零改动——`eval_gate_test.go` 是一个纯粹的新增测试文件，`internal/eval/retrieval` 是一个纯粹的新增包，不涉及对 Phase 3/4/5 已有断言的任何削弱。

## 4. 数据集：6 个 case，覆盖任务要求的 6 类场景

每个 case 用独立的知识库（`kb-gate-*`）和文档 ID，互不干扰；下表的"命中数学"是可以脱离数据库手算的部分——已经在开发过程中用一次性 PG-only 沙箱测试against 真实 Postgres 验证过完全吻合（结果见第 7 节），不是没有跑过的猜测。

| Case | 场景 | 设计要点 |
|---|---|---|
| `vector_semantic_hit` | 语义向量命中 | 3 条候选里只有 `gv-hit` 余弦非零（1.0），另两条正交（0）；查询词刻意不在任何正文里出现，纯向量路径决定排序 |
| `keyword_strong_hit` | 强关键词命中进入 topK | `gk-hit` 向量分最弱（cos=0，向量单路会掉到第 4 被 topK=3 淘汰），但正文含查询里的精确关键词——RRF 融合后 `fusionScore = 0.65/(60+4) + 0.35/(60+1) ≈ 0.015894`，比向量单路排名第一的 `gk-v1`（`0.65/(60+1) ≈ 0.010656`）还高，融合结果里排第一，把 `gk-v3` 挤出 topK |
| `content_dedup_topk_fill` | 不同 ID 完全相同正文只保留最高排名者，唯一候选补位 | 复刻 Phase 5 报告的 `dup-high`/`dup-low`/`unique` 场景，`topK=2` 时结果必须精确等于 `[dup-high, unique]` |
| `core_over_duplicate_neighbor` | 核心块优先于正文重复的邻接块 | 复刻 Phase 5 待修复项 4 修复后的 `anchor`/`anchor-prev`/`core2` fixture（`anchor-prev` 向量与查询正交，只能靠 chunk_index 邻接被捞回，不能凭向量分单独赢核心命中名额） |
| `hybrid_dedup_same_chunk_both_paths` | 同一 chunk 被向量/关键词同时召回时不重复 | `gh-both` 正文既含精确关键词又是最强向量命中，结果里必须只出现一次 |
| `no_results_negative` | 不相关查询/空结果场景不制造命中 | `kb-gate-empty` 零 chunk，`Retrieve` 必须老实返回空结果 |

每个 case 除了参与聚合指标，还各自有针对该场景特有正确性的直接断言（比如 `content_dedup_topk_fill` 直接断言最终两条结果的 ID 精确等于 `[gd-dup-high, gd-unique]`，`hybrid_dedup_same_chunk_both_paths` 直接数 `gh-both` 出现次数必须等于 1）——聚合指标门禁和单 case 断言是两层独立的保护，任何一层失败都是真的 `go test` 失败。

## 5. 修改/新增文件

- 新增 `internal/eval/retrieval/gate.go`：`BoolMetric`/`FloatMetric`/`CaseOutcome`/`GateMetrics`/`GateThresholds` 类型，`HitAtK`/`ReciprocalRank`/`ContentUniqueRate`/`AggregateMetrics`/`EvaluateGate` 纯函数。零数据库依赖，见第 3 节 import cycle 说明。
- 新增 `internal/eval/retrieval/gate_test.go`：14 个纯单测，覆盖 rank1/rank3/miss/空结果/重复正文/未配置期望值（任务最低验收明确列出的 6 类）+ 聚合指标排除未配置 case + 门禁通过/失败（4 种失败原因分别独立测）。
- 新增 `internal/eval/retrieval/report.go`：`GateHit`/`GateCaseReport`/`GateReport` 类型 + `SaveReport`（JSON 落盘，镜像 `internal/eval/report.go` 的 `Save`）。**二次更新（修复待修复项 1）**：`GateHit` 删除了 `Score` 字段。
- 新增 `internal/eval/retrieval/report_test.go`（**二次更新，修复待修复项 1**）：`TestGateReportTypesCarryNoForbiddenFields`，反射遍历 `GateReport` 类型图断言不存在 content/score/query/embedding/fingerprint/vector 字段。
- 新增 `internal/knowledge/eval_gate_test.go`：`TestRetrievalGatePhase6`，真实数据库驱动整个数据集 + 门禁判定 + 报告落盘。`setupEvalGate`/`retrievalGateOutcome`/`docSet`/`retrievalGateThresholds` 辅助函数。复用本包已有的 `seedKB`/`seedChunkWithContent`/`seedNeighborChunkBatch`/`newFakeProvider`/`newTestService`/`normalizeContentForDedup`。**二次更新（修复待修复项 1、2）**：`retrievalGateOutcome` 构造 `GateHit` 时不再传 `Score`；新增 `retrievalGateReportPath`/`retrievalGateReportPathEnv`（默认 `t.TempDir()`，`HIFY_RETRIEVAL_GATE_REPORT_PATH` 可覆盖）及两个纯逻辑回归测试 `TestRetrievalGateReportPathDefaultsAwayFromSourceTree`/`TestRetrievalGateReportPathHonorsEnvOverride`。
- 修改 `Makefile`（**二次更新，修复待修复项 2**）：`eval-retrieval-gate` target 通过 `HIFY_RETRIEVAL_GATE_REPORT_PATH=$(CURDIR)/eval/runs/phase6-retrieval-gate-latest.json` 传绝对路径，不再依赖脆弱的相对路径。
- 修改 `README.md`：「Eval」一节下新增「确定性检索回归门禁（Phase 6）」小节；核心能力一节的报告链接列表加上本报告。
- 修改 `docs/critical-paths.md`：顶部生成时间注记追加 Phase 6 说明（门禁是开发工具而非新的生产数据流，不新增为第 10 条链路，和 `cmd/evalrunner`/`internal/eval` 在这份清单里的既有处理方式一致）。
- 新增本报告 `docs/eval-phase6-retrieval-gate-report.md`。

未修改：`internal/knowledge` 包除新增的 `eval_gate_test.go` 外零改动（`dedup.go`/`hybrid.go`/`neighbor.go`/`service.go` 等生产代码本阶段一行未动）；`internal/eval`（agent 级 Eval，`cmd/evalrunner` 用的那套）零改动；Citation 协议、`conversation/budget.go` 的 RAG 预算逻辑零改动。

## 6. 完整验收场景与对应测试

1. **指标函数纯单测覆盖 rank 1、rank 3、miss、空结果、重复正文、未配置期望值。** `TestHitAtKRank1`、`TestHitAtKRank3`、`TestHitAtKMiss`、`TestHitAtKUnconfiguredExpectedValue`、`TestContentUniqueRateEmptyResult`、`TestContentUniqueRateDuplicateContent`、`TestContentUniqueRateAllUnique`（`internal/eval/retrieval/gate_test.go`）。
2. **聚合指标正确排除未配置期望文档的 case，不当成 miss 拖累 Hit@K/MRR，但仍计入内容唯一率。** `TestAggregateMetricsExcludesUnconfiguredFromHitAndMRR`、`TestAggregateMetricsAllUnconfiguredYieldsUnevaluatedHitAndMRR`。
3. **门禁失败路径：健康结果通过，四种退化方式（低命中率、低 MRR、重复内容、指标未评估）各自独立导致失败，都有非空的失败原因。** `TestEvaluateGatePassesOnHealthyRun`、`TestEvaluateGateFailsOnLowHitRate`、`TestEvaluateGateFailsOnLowMRR`、`TestEvaluateGateFailsOnDuplicateContent`、`TestEvaluateGateFailsOnUnevaluatedMetric`。
4. **真实 `Service.Retrieve` 数据库评测完整执行，所有规定 case 均 PASS，用 `-v` 证明没有静默 skip。** `TestRetrievalGatePhase6`（`internal/knowledge/eval_gate_test.go`），六个 `t.Run` 子测试 + 门禁聚合断言。数据库不可达时打印 `testutil` 既定的跳过原因（"跳过集成测试（先 make db-up 起容器）"），不是静默跳过——见第 7 节本沙箱的真实运行记录。
5. **语义向量命中、强关键词命中进 topK、同一 chunk 双路召回不重复、内容去重 topK 补位、核心优先于重复邻接块、空结果不制造命中——6 类场景全部覆盖。** 见第 4 节表格逐条对应的 `t.Run` 子测试。
6. **评测产物不含正文/query/embedding/指纹。** 结构性保证 + 自动化反射测试——见第 2 节第 5 点，`CaseOutcome`/`GateHit`/`GateReport` 没有任何字段能装下这些内容（`Score` 已在本轮返修中从 `GateHit` 删除，见第 0 节），`TestGateReportTypesCarryNoForbiddenFields` 用反射遍历整个类型图断言不存在禁止字段名。落盘路径默认不写进源码树（`t.TempDir()`），`make eval-retrieval-gate` 显式指定仓库根目录路径时才产生人类可读报告，见第 7 节。
7. **不新增 Reranker/Query Rewrite/Multi Query/HyDE/LLM Judge/外部模型依赖，不修改排序权重/topK/RAG 预算/Citation 协议/前端。** 结构性保证——本阶段新增的两个文件（`eval_gate_test.go`、`internal/eval/retrieval/*`）都只读取 `Service.Retrieve` 的返回值，不修改任何生产代码路径；`hybrid.go`/`neighbor.go`/`dedup.go`/`service.go`/`conversation/budget.go` 全部零改动（见第 5 节"未修改"）。
8. **不为了让指标通过而弱化已有 Phase 3-5 断言，不把 `eval/baseline.json` 里旧的 5 条结果伪装成当前完整基线。** 本阶段完全没有碰 `eval/baseline.json`/`eval/testset.yaml`/`internal/eval`（agent 级）的任何文件；Phase 3/4/5 的所有已有测试原样保留、原样通过（见第 7 节）。
9. **全量测试、race、vet、check-deps、diff-check 通过，数据库测试不得静默跳过。** 见第 7 节。

## 7. 真实测试结果（二次更新：返修轮次的完整复测）

```
gofmt -l internal/eval/retrieval/*.go internal/knowledge/eval_gate_test.go Makefile
                                       # 无残留 diff
go build ./...                        # 0 error
go vet ./...                          # 0 diagnostics
go test -count=1 ./...                # 全部 PASS（含 TestRetrievalGatePhase6，
                                       #   本沙箱因无 MySQL 按既定约定打印原因后 SKIP，
                                       #   不计入失败——和 Phase 3/4/5 所有
                                       #   setupIntegration 测试的既定行为完全一致；
                                       #   含新增的 TestRetrievalGateReportPath* 两个
                                       #   纯逻辑测试，不依赖数据库，真实 PASS）
go test -count=1 -v ./internal/eval/retrieval/...
                                       # 15 个纯单测全部真实执行并 PASS（14 个原有 +
                                       #   1 个新增的 TestGateReportTypesCarryNoForbiddenFields），
                                       #   无 DB 依赖
go test -race -count=1 ./...          # 全部 PASS，无 race
make check-deps                       # OK，无跨层/同层依赖违规
git diff --check                      # 无输出
git status --short                    # 只有本阶段应改动的文件，internal/knowledge/eval/
                                       #   和 _to_delete/ 均已清理，不再出现
```

**纯逻辑单测（`internal/eval/retrieval/gate_test.go` + `report_test.go`，共 15 个，全部真实执行且 PASS，无 DB 依赖）**：

`TestHitAtKRank1`、`TestHitAtKRank3`、`TestHitAtKMiss`、`TestHitAtKUnconfiguredExpectedValue`、`TestContentUniqueRateEmptyResult`、`TestContentUniqueRateDuplicateContent`、`TestContentUniqueRateAllUnique`、`TestAggregateMetricsExcludesUnconfiguredFromHitAndMRR`、`TestAggregateMetricsAllUnconfiguredYieldsUnevaluatedHitAndMRR`、`TestEvaluateGatePassesOnHealthyRun`、`TestEvaluateGateFailsOnLowHitRate`、`TestEvaluateGateFailsOnLowMRR`、`TestEvaluateGateFailsOnDuplicateContent`、`TestEvaluateGateFailsOnUnevaluatedMetric`、`TestGateReportTypesCarryNoForbiddenFields`（新增，修复待修复项 1）。

**`internal/knowledge` 包内的纯逻辑回归测试（新增，修复待修复项 2，不依赖数据库）**：

`TestRetrievalGateReportPathDefaultsAwayFromSourceTree`（断言默认路径是绝对路径且不在测试工作目录前缀下）、`TestRetrievalGateReportPathHonorsEnvOverride`（断言设置 `HIFY_RETRIEVAL_GATE_REPORT_PATH` 后返回值精确等于该值）——本沙箱真实执行并 PASS。

**真实数据库门禁测试（`TestRetrievalGatePhase6`，本沙箱只有 Postgres，按既定约定 SKIP，需 Codex 在有 MySQL+PostgreSQL 的完整 docker 环境里补跑确认真的 PASS）**：

本沙箱在开发过程中额外用一次性的 PG-only 沙箱测试（不提交，仅用于开发时验证，已删除）复刻了本阶段全部 6 个 case 的核心检索/融合/邻接逻辑（`repo.searchVectorChunks`/`repo.searchKeywordChunks`/`rrfFuse`/`buildNeighborGroups`/`repo.findPublishedNeighborChunks`/`expandWithNeighbors`，即 `Service.Retrieve` 内部实际调用的同一批函数，只是不经过需要 MySQL 的 `knowledge_base` 查询这一层），针对真实本机 Postgres 逐一验证：

- `vector_semantic_hit`：`fused topK=3 = [gv-hit gv-decoy-a gv-decoy-b]`，`gv-hit` 排第一，与设计一致。
- `keyword_strong_hit`：`fused topK=3 = [gk-hit gk-v1 gk-v2]`，`gk-hit` 排第一、`gk-v3` 被挤出，和第 4 节表格里手算的 RRF 分数完全吻合。
- `content_dedup_topk_fill`：`fused topK=2 = [gd-dup-high gd-unique]`，和 Phase 5 报告的场景 4 断言完全一致。
- `core_over_duplicate_neighbor`：`anchors topK=2 = [gn-anchor gn-core2]`（`coreDup=1`，内容去重已经在核心融合阶段就捕获了 `gn-anchor-prev` 和 `gn-core2` 的重复——因为这个 KB 只有 3 条 chunk、候选池覆盖全部，这是 Phase 5 报告里同一现象在本阶段数据集上的重现，不是新问题），最终 `expandWithNeighbors` 结果 `= [gn-anchor gn-core2]`（`nbDup=1`，邻接阶段的二次去重再次确认 `gn-anchor-prev` 不会漏网），和设计一致：两个核心块都保留，重复的邻接块被丢弃。
- `hybrid_dedup_same_chunk_both_paths`：`fused topK=2 = [gh-both gh-other]`，`gh-both` 恰好出现 1 次。

这 5 组结果和 `eval_gate_test.go` 里每个 `t.Run` 的断言逐一核对完全吻合，`no_results_negative`（零 chunk 的 KB 必然返回空）不需要额外验证。这为"该测试本身因为需要 MySQL、在本沙箱只能 SKIP"这件事提供了尽可能强的间接证据：驱动最终结果的核心逻辑（向量检索、关键词检索、RRF 融合、邻接扩展、两次内容去重）在真实 Postgres 上确实按设计工作，`Service.Retrieve` 只是在这些结果外面包了一层 MySQL 的 `knowledge_base` 查询和 embedding 路由，这一层在 Phase 3/4/5 已有的 `TestIntegrationRetrieveMergesAcrossModelsAndSkipsInactive` 等测试里已经反复验证过。

**回归确认**：`internal/eval`（agent 级 Eval）、`cmd/evalrunner`、`internal/knowledge` 除 `eval_gate_test.go` 外的全部既有测试（Phase 3/4/5 单测 + 集成测试）在本轮改动后重新跑过，全部仍然 PASS，没有因为新增的门禁文件产生任何编译或行为影响。

## 8. 未验证内容与剩余风险（二次更新：返修轮次后的状态）

- Codex 第一轮审核实际已经用真实 MySQL+PostgreSQL 完整跑过 `make eval-retrieval-gate`，6 个 case 全部真实 PASS、无 SKIP——这部分已经不是"未验证"。本轮返修（删除 `Score`、修复报告路径）范围很窄，没有改动 6 个 case 的检索逻辑或数据/断言本身，但改动本身（`GateHit` 少一个字段、报告落盘路径逻辑）在本沙箱因为同样缺 MySQL，`TestRetrievalGatePhase6` 依然只能 SKIP，需要 Codex 在下一轮复审里针对这两处改动重新跑一遍确认没有引入新问题（尤其是确认 `make eval-retrieval-gate` 真实产生的报告文件这次落在 `eval/runs/`，`git status` 干净）。
- 数据集固定为 6 个 case、每类场景 1 个，不是穷举——如果未来 Phase 3/4/5 的实现方式发生大改动（比如 RRF 常数、`candidateK` 公式变化），`keyword_strong_hit` 这类依赖具体分数运算得出"必须排第一"结论的 case 可能需要重新校验其数学假设是否还成立；这是"用具体数字验证具体行为"这种测试方法的固有特点，不是本阶段独有的缺口。
- 阈值全部设为 1.0（即"零容忍"）是因为这批 6 个 case 都被设计成在健康实现下必然 100% 命中——这是数据集设计的产物，不是"所有检索场景都应该要求 100% 命中率"这种普遍结论；如果未来数据集扩展到包含天然更难的 case（比如语义相近但非精确的查询），阈值需要重新评估，不能照搬 1.0。
