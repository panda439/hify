# Hify

[![CI](https://github.com/panda439/hify/actions/workflows/ci.yml/badge.svg)](https://github.com/panda439/hify/actions/workflows/ci.yml)

一个 AI 辅助完成的简化版 LLM 应用开发平台学习项目（对标 Dify 的核心能力），Go + React。项目用于理解和验证 LLM 供应商抽象、RAG 流水线、Agent 工具调用循环与工作流引擎等系统的实现取舍；代码实现、测试和前端均有 AI 工具协助，不将其表述为个人独立完成的生产平台。

我在项目中的学习重点是：能基于代码和提交记录解释关键数据流、参与方案判断，审查 AI 辅助实现的边界，并通过测试和真实接口验证结果。项目介绍与面试提纲见 [docs/portfolio-interview.md](docs/portfolio-interview.md)。

## 核心能力

- **LLM Provider 抽象层**：统一 `Client` 接口（Chat / ChatStream / Embed），OpenAI 兼容协议适配多家供应商；API Key AES-256-GCM 加密落库。
- **弹性调用装饰器**：per-provider 熔断（gobreaker）、并发限流 + Redis 令牌桶、指数退避重试（流式场景只重试首连，断流绝不重试以避免向客户端重复推送）、空闲超时。
- **RAG 全流程**：结构感知文档解析分块（md 保留标题/段落/列表/代码块/表格并把标题带入 embedding 内容、txt 按段落/句子边界切、pdf 逐字形位置重建后按页切分保留页码；单个结构超限统一回退定长切分；PDF 无 OCR，扫描版/无文字层直接报错，Markdown 解析是行级启发式而非完整 CommonMark）→ 批量 embedding →  **Hybrid Search**：**PostgreSQL + pgvector** 余弦向量检索 + **pg_trgm** 字符级 trigram/word-similarity 关键词检索（不是 BM25，中英文一视同仁）并行召回，各自在库内打分/排序取宽候选窗口（`candidateK`），**Reciprocal Rank Fusion** 按 chunk ID 去重融合、**内容去重（Exact Content Dedup）**后再截断全局 topK 核心命中（规范化后完全相同的正文只保留排名最高的一条，去重发生在截断到 topK 之前，让内容不同的候选可以补位；只做保守的 CRLF/首尾空白/行尾空格归一化，不做语义或模糊相似去重；无维度声明 vector 列支撑混合维度知识库；关键词一路不依赖 embedding，向量一路的 embedding 服务失败时仍可退化返回关键词结果）→ **邻接分块扩展（Neighbor Window Retrieval）**：每个核心命中块（已经过内容去重，被淘汰的重复核心块不再查询邻接窗口）best-effort 补上同文档、同 `document_version`、已发布的前一个/后一个 chunk（不参与排名；输出布局是全部核心块在前、全部邻接块整体在后的两层结构，绝不逐个核心块交替，配合对话侧按输入顺序贪心消耗预算的既有逻辑，保证预算不足时邻接块绝不挤出排名更低的核心块，也不需要提高 RAG 字符预算；绝不跨文档重处理版本混入邻接内容）——**批量邻接查询（Batch Neighbor Lookup）**：不管一次检索涉及多少个核心命中块、多少个不同的文档/版本，正常路径只发生一次数据库往返（把所有核心块需要的 `document_id + document_version + chunk_index` 坐标去重展平后一次批量取回，取代了早期按文档版本分组循环查询的 N+1 模式），批量查询失败或 KB 重新处理导致旧版本被删除时静默降级为只返回核心块 → 邻接扩展后再做一次内容去重（核心块优先于邻接块，邻接块之间保留输出顺序靠前者）→ 检索结果注入对话上下文并通过 SSE 暴露调试信息。详见 [docs/eval-phase3-hybrid-search-report.md](docs/eval-phase3-hybrid-search-report.md)、[docs/eval-phase4-neighbor-window-report.md](docs/eval-phase4-neighbor-window-report.md)、[docs/eval-phase5-content-dedup-report.md](docs/eval-phase5-content-dedup-report.md)、[docs/eval-phase6-retrieval-gate-report.md](docs/eval-phase6-retrieval-gate-report.md)、[docs/eval-phase7-batch-neighbor-report.md](docs/eval-phase7-batch-neighbor-report.md)。
- **Agent 工具调用循环**：OpenAI 风格 function calling，流式 tool_calls 按 Index 合并分片，MCP（stdio + SSE）工具发现/同步/调用，最大迭代保护。
- **SSE 流式架构**：断线时部分内容仍落库；goroutine / 信号量泄漏防护（trySend + context 联动）。
- **简化工作流引擎**：DAG 校验（无环/可达性）、条件分支（expr-lang）、模板变量渲染、执行轨迹持久化——一个迷你版工作流编排器。
- **JWT + RBAC**、asynq 异步任务队列、sqlc 生成的类型安全 SQL。

## 技术栈

Go 1.26 / Gin / MySQL 8 / PostgreSQL 17 + pgvector / Redis / asynq / sqlc · React / TypeScript / Vite / @xyflow/react

## 快速开始

```bash
cp .env.example .env        # 按需修改
make db-up                  # docker compose 起 MySQL/PG/Redis
go run ./cmd/hify migrate up
make dev                    # 后端（air 热重载）
make web-dev                # 前端
```

## 测试

```bash
make test        # vet + 全部测试（容器没起时集成测试自动 skip）
make test-race   # 加 race detector
```

测试策略围绕 [docs/critical-paths.md](docs/critical-paths.md)——9 条"改造时容易出问题"的核心链路清单：纯逻辑用 characterization test 锁行为（弹性装饰器的故障注入、tool_calls 分片合并等），跨库契约用真实 MySQL + pgvector 集成测试验证（`internal/testutil` 每包独立测试库，支持并行）。CI 里用 service containers 跑全量。

## Eval

`internal/eval` + `cmd/evalrunner`（`make eval`）是开发者自用的 agent 回归工具：跑 `eval/testset.yaml` 里固定的一组 prompt，记录每个 case 的执行轨迹，产出结构化报告，和上一次的基线对比。不进数据库、不对外暴露，结果只写本地文件（`eval/runs/*.json`、`eval/baseline.json`）。

```bash
go run ./cmd/evalrunner --judge-model-id <UUID> --user-id <UUID>
# 或
make eval JUDGE_MODEL_ID=<UUID> EVAL_USER_ID=<UUID>
```

**两类指标，职责分开**：

- **确定性指标**（`internal/eval/metrics.go`，代码算出来的，不经过任何 LLM）：`RetrievalHit`（期望文档是否至少命中一个）、`MRR`（期望文档集合里排名最靠前的那次命中的排名倒数，`expected_document_ids` 数组顺序不影响结果）、`RecallAt1`/`RecallAt3`（期望文档是否落在检索结果的前 1/前 3 名内，即 Hit@K——不是多相关文档意义上的召回率，`RetrievalHit` 已经覆盖"整批检索结果里有没有命中"，这两个指标补的是排名靠不靠前的分辨率）、`ExpectedDocumentCited`（最终引用是否指向期望文档）、`CitationRequirementMet`（`require_citation=true` 时是否真的带了引用）。每个指标都是 `{evaluated, value}` 的形状——`evaluated=false` 表示这条 case 没配置对应字段（比如没写 `expected_document_ids`），不是"评估了但是 false"，报告里显示成"—"。
- **LLM Judge 指标**：1-5 分的整体质量分，负责 `expected_facts`/`forbidden_facts` 有没有被正确表达、回答相关性、表达质量这类需要语义理解的判断。Judge 看不到、也不再声称能看到已脱敏的 Trace 原文（检索到的文档内容、用户原始问题、工具调用参数/结果）——这些在 Citation/Trace 隐私改造后已经从 Trace 里移除，Judge 只能看到 span 的状态/耗时元数据。V1 不做严格意义的 Faithfulness（逐字比对检索原文与回答）评分。

`TestCase` 的 `expected_document_ids`/`require_citation`/`expected_facts`/`forbidden_facts` 都是可选字段，见 `eval/testset.yaml` 的注释和示例。`turns`（阶段一新增）让一个 case 在同一个 conversation 里依次发送多条 prompt，只有最后一轮的回复/检索/引用参与打分，用来测多轮指代（后一轮用代词/省略指代前一轮，不重复说出关键信息）。`Compare` 输出的 Markdown 报告里，Judge 分数和这六个确定性指标都会显示"本次 / 基线 / 变化"；`Regressed`（决定 `evalrunner` 退出码的函数）目前只看 Judge 分数——确定性指标可能因为知识库内容变化等非代码原因波动，暂不接入 CI 退出码，只供人工审阅。旧版（改造前）跑出来的 `baseline.json` 缺这些新字段也能正常加载对比，缺的部分统一显示"—"。

### 确定性检索回归门禁（Phase 6）

`internal/eval/retrieval`（纯逻辑：`Hit@1`/`Hit@3`/`MRR`/内容唯一率 + 阈值判定）+ `internal/knowledge/eval_gate_test.go`（`TestRetrievalGatePhase6`，`make eval-retrieval-gate`）是和上面 agent 级 Eval 完全独立的第二套回归工具，专门盯 Phase 3-5 的检索质量，不依赖 LLM/Judge：

```bash
make eval-retrieval-gate
```

这套门禁走真实 MySQL + PostgreSQL/pgvector/pg_trgm + fake embedding，直接调用公开的 `knowledge.Service.Retrieve`（不是绕过公开入口直接测纯函数），固定的六个 case 覆盖语义向量命中、强关键词命中进 topK、同一 chunk 被向量/关键词同时召回不重复、不同 ID 相同正文只保留最高排名者唯一候选补位、核心块优先于重复邻接块、空结果场景不制造命中。判定失败就是真的 `go test` 失败（非零退出码），不是只生成报告等人看——`internal/eval/retrieval` 的 `EvaluateGate` 是纯函数，"健康结果通过""任意一项指标退化则失败"两条路径都各有独立单测（不碰数据库），保证门禁本身不会形同虚设。数据库不可达时和其它集成测试一样打印原因后 SKIP（`internal/testutil` 既定约定），不是静默跳过。`go test` 的工作目录是包目录而不是仓库根，所以评测报告默认写进 `t.TempDir()`（自动清理，普通 `go test ./...`/CI 不会弄脏工作区）；`make eval-retrieval-gate` 通过 `HIFY_RETRIEVAL_GATE_REPORT_PATH` 环境变量显式指定仓库根目录路径，才会在 `eval/runs/phase6-retrieval-gate-latest.json`（已在 `.gitignore`）留下人类可读报告，只含 case 名、chunk/document ID、rank、`NeighborOf`、计数与聚合指标，不含检索到的正文、查询原文、embedding 或分数。详见 [docs/eval-phase6-retrieval-gate-report.md](docs/eval-phase6-retrieval-gate-report.md)。

## 架构决策记录

关键设计取舍（为什么 pgvector 而不是专用向量库、为什么无维度声明的 vector 列、跨库删除为什么靠顺序而不是分布式事务、流式重试的边界在哪）散落在各模块的包注释和 `docs/` 里，整理中。

## 已知限制

- 这是学习型模块化单体，不是生产级 Dify 替代品；当前文档存储使用本地磁盘，不适合多实例共享。
- Eval V1 不做严格的 citation faithfulness（逐字验证回答是否被检索原文支持）；确定性指标会出现在报告中，但当前回归退出码只依据 LLM Judge 分数，仍需人工审阅。
- Trace 会脱敏检索文档、原始问题和工具参数/结果；这降低了观测数据的泄露风险，也意味着 Judge 不能据此审查原文内容。
- `make eval` 需要已有的裁判模型和用户配置，会产生本地评测报告；它是开发回归工具，不是对外在线评测服务。
