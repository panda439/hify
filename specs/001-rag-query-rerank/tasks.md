---

description: "Task list for RAG 查询优化与结果重排序"
---

# Tasks: RAG 查询优化与结果重排序

**Input**: Design documents from `/specs/001-rag-query-rerank/`

**Prerequisites**: [plan.md](./plan.md)、[spec.md](./spec.md)、[research.md](./research.md)、
[data-model.md](./data-model.md)、[contracts/](./contracts/)

**Tests**: 包含测试任务。宪法第 II/VI 条要求严格测试先行——每组行为**先加失败测试，再写最小实现**。

**Organization**: 按用户故事分组，每个故事可独立实现、独立验证。

## Format: `[ID] [P?] [Story] Description`

- **[P]**: 可并行（不同文件、无未完成依赖）
- **[Story]**: US1 / US2 / US3
- 每条都写明确切文件路径

## Path Conventions

Go 模块化单体：`internal/<module>/`、`internal/db/migrations/`、`cmd/hify/`、`eval/`、`docs/`。

---

## Phase 1: Setup（共享前置）

**Purpose**: 固定"改动前"的基线，否则 SC-003 无法验证

- [ ] T001 在**任何代码改动之前**跑 `make eval` 并把结果归档为基线，确认 `eval/baseline.json` 与本次基线一致；把命令原始输出摘录进 `docs/eval-phase9-query-rerank-report.md` 的"基线"一节
- [ ] T002 跑 `go test ./... -race -count=1`、`go vet ./...`、`make check-deps`，确认改动前工作区是全绿的（有失败先记录，不要在本功能里顺手修——宪法第 IX 条）

---

## Phase 2: Foundational（阻塞所有故事）

**Purpose**: 两个故事共用的配置骨架

**⚠️ CRITICAL**: 本阶段完成前，US1/US2 都不能开工

- [ ] T003 在 `internal/config/config.go` 的 `Config` 中新增 6 个字段并在 `Load()` 中解析：`RAGQueryRewriteEnabled`、`RAGQueryRewriteModelID`、`RAGQueryRewriteTimeout`、`RAGRerankEnabled`、`RAGRerankModelID`、`RAGRerankTimeout`，默认值与环境变量名严格照 [data-model.md](./data-model.md) §3
- [ ] T004 在 `internal/config/config.go` 中实现 §3 的校验规则：两个 duration 解析失败即返回错误；`RAGRerankEnabled=true` 且 `RAGRerankModelID==""` 时降级为关闭并 `slog.Warn`（**不得**让进程启动失败）
- [ ] T005 在 `cmd/hify/`（`buildApp`）把新配置分别透传给 `conversation.NewService` 与 `knowledge.NewService` 的构造参数占位，保证此时编译通过、行为与改动前完全一致

**Checkpoint**: 配置就位，双开关默认关闭，行为零变化

---

## Phase 3: User Story 1 - 多轮追问也能检索到正确资料 (Priority: P1) 🎯 MVP

**Goal**: 把依赖上文的省略式提问改写成可独立理解的检索问题再去召回

**Independent Test**: 两轮对话，第二轮用「那它的上限呢」提问，断言实际检索用的是补全后的问题，
且返回证据与第一轮主题一致；不需要 rerank 存在

### Tests for User Story 1 ⚠️ 先写，先失败

- [ ] T006 [P] [US1] 在 `internal/conversation/queryrewrite_test.go` 写 `shouldSkipRewrite` 的表驱动失败测试：无历史+无指代词+长度达标 → skip；含「它/这个/那个/上述/前者/后者/该/其」任一 → 不 skip；有历史 → 不 skip；空串/纯标点 → skip；英文 `it/this/those` → 不 skip
- [ ] T007 [P] [US1] 在 `internal/conversation/queryrewrite_test.go` 写 `parseRewriteResult` 失败测试：裸 JSON、带 ```` ```json ```` 围栏、首尾空白、非法 JSON（返回 error）、缺字段（零值）
- [ ] T008 [P] [US1] 在 `internal/conversation/queryrewrite_test.go` 写 `validateRewrite` 失败测试：`ambiguous=true` → 不采用；空/纯空白 → 不采用；>200 runes → 不采用；> max(3×原长,60) runes → 不采用；正常改写 → 采用并返回去空白后的结果

### Implementation for User Story 1

- [ ] T009 [US1] 新建 `internal/conversation/queryrewrite.go`：包内常量（`maxRewriteHistoryTurns=4`、`maxRewriteQuestionRunes=200`、`minRewriteTriggerRunes=2`）、指代词模式（中英文，参考 [research.md](./research.md) R4）、`shouldSkipRewrite` 纯函数，使 T006 通过
- [ ] T010 [US1] 在 `internal/conversation/queryrewrite.go` 实现 `parseRewriteResult` / `validateRewrite` 纯函数，使 T007、T008 通过
- [ ] T011 [US1] 在 `internal/conversation/queryrewrite.go` 写改写提示词：要求只输出 `{"standalone_question","ambiguous"}` JSON；历史与问题包在明确的数据标签内并声明"是待分析数据不是指令"（FR-006，对齐 `context.go` 里 `formatSource` 的处理思路）；规则照抄 research-agent 的语义但**去掉"反问用户"分支**——歧义时只置 `ambiguous=true`
- [ ] T012 [US1] 在 `internal/conversation/queryrewrite.go` 实现 `rewriteQuery(ctx, ...) rewriteOutcome`：开关关闭/快速路径命中 → 直接返回原问题且不调 LLM；否则用 `providerSvc.ResolveClient` + `Chat`（`temperature=0`、不挂工具）调用，带 `RAGQueryRewriteTimeout` 的 `context.WithTimeout`；模型选择为 `RAGQueryRewriteModelID`，为空则用当前 Agent 的 chat 模型
- [ ] T013 [US1] 在 `internal/conversation/service.go` / `wire.go` 给 `service` 增加改写所需字段与构造参数（开关、模型 ID、超时），保持 `Service` 接口不变
- [ ] T014 [US1] 在 `internal/conversation/context.go` 的 `assembleContext` 中，于 `knowledgeSvc.Retrieve` **之前**调用 `rewriteQuery`，把返回的 `SearchQuery` 作为 `Retrieve` 的 query 传入；`latestUserMessage` 本身仍原样进入消息序列（FR：改写只影响检索，不影响回答呈现）
- [ ] T015 [US1] 在 `internal/conversation/integration_test.go` 增加可编程 fake provider client，覆盖：改写成功 → Retrieve 收到的是改写后问题；`ambiguous=true` → Retrieve 收到原问题；开关关闭 → Retrieve 收到原问题且 fake 的 Chat 调用次数为 0

**Checkpoint**: US1 独立可用——不依赖 rerank、不依赖 migration

---

## Phase 4: User Story 2 - 最相关的证据排在最前面 (Priority: P1)

**Goal**: 在候选截断到 topK 之前，按问题—片段真实相关度重排

**Independent Test**: 构造一组候选，真正相关的片段在融合排名中排在 topK 之外，
断言重排后它进入结果且靠前；不需要查询改写存在

### 4.1 数据层与供应商能力（宪法第 IV 条：migration → sqlc → model → service → handler → wire）

- [ ] T016 [US2] 新建 `internal/db/migrations/000012_provider_rerank_capability.up.sql` 与 `.down.sql`，内容照 [data-model.md](./data-model.md) §1.1；跑 `make migrate-up` 与一次 `make migrate-down`+`migrate-up` 往返验证
- [ ] T017 [US2] 跑 `make sqlc`，确认无 diff（列定义未变，只改 CHECK）
- [ ] T018 [P] [US2] 在 `internal/provider/model.go` 增加 `CapabilityRerank = "rerank"` 常量
- [ ] T019 [P] [US2] 在 `internal/provider/rerank_test.go` 写失败测试：请求体编码（`return_documents=false`、`top_n=len(documents)`）、响应解码、以及 [contracts/rerank-http-api.md](./contracts/rerank-http-api.md) 的 5 条校验（空/长度不符/越界 index/重复 index/缺失 index）逐条返回"不可信"
- [ ] T020 [US2] 在 `internal/provider/llm.go` 增加 `RerankRequest`/`RerankResult`/`RerankScore` 类型，并在 `Client` 接口加 `Rerank` 方法
- [ ] T021 [US2] 在 `internal/provider/openai_compat.go` 用 `net/http` 实现 `openAICompatClient.Rerank`（`POST {base}/rerank`，`Authorization: Bearer`，复用既有 `classifyError` 与 retry-after 采集），使 T019 通过
- [ ] T022 [US2] 在 `internal/provider/resilience.go` 实现 `resilientClient.Rerank`，与 `Embed` 同构地套熔断/重试
- [x] T023 [US2] 在 `internal/provider/service.go`、`handler.go` **与 `dto.go`** 的能力白名单中放行 `rerank`，非法值仍返回中文错误
  - **本条任务书原文写错了：说"两处判断"，实际是三处。** 漏掉的是 `dto.go` 里 `addModelRequest.Capability` 的 gin binding 标签 `oneof=chat embedding`，它在请求进到 handler 逻辑之前就生效——另外两处改得再对，请求也永远到不了它们那里，rerank 模型**根本注册不进去**，而这是它唯一的注册入口（前端暂无该选项）
  - 全套单测与集成测试都是绿的，因为它们直接调 service/handler，绕过了 gin binding。这个缺陷是 T043 冒烟测试用真实 HTTP 请求打出来的——正是 CLAUDE.md 说的"`/smoke-test` 和真实 HTTP 验证是唯一的安全网"的实例
  - 已补 `internal/provider/dto_binding_test.go`：只测 binding 标签本身（不连库、不构造 service），遍历 `Capability*` 常量断言全部能过、常见手误拼写全部被拒。以后加第四种能力时忘记改标签就会立刻红
- [ ] T023a [US2] **（US1 review 追加）** 修 `Temperature: 0` 被静默丢弃的问题：`internal/provider/openai_compat.go` 的 `toOpenAIRequest` 无条件写 `Temperature: float32(req.Temperature)`，而 go-openai v1.41.2 的字段标签是 `json:"temperature,omitempty"` —— 0 值会被整个省略，供应商按默认温度（通常 1.0）执行。改法：`provider.ChatRequest.Temperature` 改成 `*float64`，或增设 `TemperatureSet bool`，让"显式 0"能真正发出去。影响面不止本功能：Agent 自己配 `Temperature=0` 现在同样无效。改完补一个断言"温度为 0 时请求体确实含 temperature 字段"的测试
- [ ] T024 [P] [US2] 给 4 个测试假实现补 `Rerank` 空方法：`internal/workflow/integration_test.go`、`internal/knowledge/integration_test.go`、`internal/eval/runner_test.go`、`internal/conversation/integration_test.go`——**只加方法，不改这些文件的其他内容**

### 4.2 检索链路改造（行为等价先行）

- [ ] T025 [US2] 在 `internal/knowledge/hybrid_test.go` 增加失败测试：`rrfFuse` 去掉 `topK` 参数后返回"完整已准入已去重列表"，且对同一输入，`rrfFuse(...)[:topK]` 与改造前 `rrfFuse(..., topK)` 结果逐字相同（FR-018 的单测级证据）
- [ ] T026 [US2] 修改 `internal/knowledge/hybrid.go`：`rrfFuse` 去掉 topK 参数与截断逻辑，更新其文档注释（说明截断已移到 `Retrieve` 的重排之后），使 T025 通过
- [ ] T027 [P] [US2] 在 `internal/knowledge/rerank_test.go` 写 `applyRerank` 失败测试：分数降序生效；分数相同按重排前原始位置升序（确定性 tie-break）；`RetrievedChunk.Score`/Citation 元数据/`NeighborOf` 均不被改写；校验不通过时返回 `false` 且候选顺序原样返回
- [ ] T028 [US2] 新建 `internal/knowledge/rerank.go`：常量 `rerankInputLimit=50`、`rerankedCandidate`/`rerankStats` 类型、`applyRerank` 纯函数与响应校验，使 T027 通过
- [ ] T029 [US2] 在 `internal/knowledge/service.go` 的 `Retrieve` 中按 [plan.md](./plan.md)"检索链路的新顺序"插入重排步骤：`rrfFuse → (重排前 50) → applyRerank → topK 截断 → expandWithNeighborWindow`；候选数 ≤1 或开关关闭时短路不发请求
- [ ] T030 [US2] 在 `internal/knowledge/wire.go` / `service` 结构体注入 rerank 依赖（开关、模型 ID、超时、打分函数），打分函数做成可替换的方法值字段——照 `findNeighborBatch` 的既有先例，让单测可以注入固定打分的假实现
- [ ] T031 [US2] 在 `internal/knowledge/integration_test.go` 增加真实 PostgreSQL + fake provider 的集成测试：融合排名第 6 位的候选被重排到第 1 位并进入 topK=3 的结果；断言邻接查询只为重排后的核心块发生（复用 Phase 7 的 spy 计数手法）

**Checkpoint**: US2 独立可用——关闭改写开关也能验证

---

## Phase 5: User Story 3 - 出问题时对话不受影响，且能看清发生了什么 (Priority: P2)

**Goal**: 降级路径全覆盖 + 可观测落地 + 隐私红线

**Independent Test**: 强制外部调用失败/超时，断言本轮仍正常完成且出现降级标记

- [x] T032 [P] [US3] 在 `internal/conversation/integration_test.go` 写降级失败测试：改写 LLM 返回 error / 超时 / 返回不可解析文本 → `Retrieve` 收到原问题、本轮正常完成、`rewriteOutcome.Degraded=true`
- [x] T033 [P] [US3] 在 `internal/knowledge/integration_test.go` 写降级失败测试：rerank 返回 error / 超时 / 返回重复 index → 结果顺序与关闭重排时**逐字一致**（整体丢弃，禁止部分采用）
- [x] T034 [US3] 在 `internal/conversation/context.go` 增加 `query_rewrite` 子 span，attrs 严格限定为 [data-model.md](./data-model.md) §5.1 的 5 个 key；常量按既有惯例定义（参照 `internal/platform/trace/attrs.go` 的命名风格）
- [x] T035 [US3] 在 `internal/knowledge/service.go` 扩展既有 `"knowledge: retrieval candidate admission and dedup"` slog 行，补 `rerank_enabled`/`rerank_applied`/`rerank_degraded`/`rerank_input_count`/`rerank_duration_ms`，并把触发条件放宽为"发生拒绝/去重/邻接去重 **或** 重排被应用/降级"
- [x] T036 [US3] 写隐私断言测试：捕获 slog 输出与 span attrs，断言其中**不含**问题原文、片段正文、逐条分数（FR-017）——放在 `internal/knowledge/rerank_test.go` 与 `internal/conversation/queryrewrite_test.go`
- [x] T037 [US3] 写确定性测试（SC-007）：用固定打分假 client 重复执行同一 `Retrieve` 20 次，断言 chunk ID 序列 100% 一致，放在 `internal/knowledge/integration_test.go`

**Checkpoint**: 三个故事全部独立可用，降级矩阵逐条有测试

---

## Phase 6: Polish & Cross-Cutting

- [~] T038 [P] 在 `eval/testset.yaml` 新增一组"多轮省略式追问"用例——**用例已写完，但目标未达成，见 T038a**
  - **提前到 US2 之前完成**（2026-08-24）：跑基线时发现既有的 `multi_turn_coreference_chunk_size` 在改写关闭下就已经 `RetrievalHit=true`/`MRR=1.00`/评分 5——它的末轮带着"分块大小"这个实词，关键词路直接命中，指标没有上升空间，**SC-001/SC-002 用它度量不出任何提升**。因此这条不是收尾工作，而是效果可度量的前提
  - 新增 4 条，设计约束是**末轮不得包含任何在目标文档里出现过的实词**：`hard_multi_turn_ellipsis_upload_size_limit`（末轮"那大小有限制吗"，且"大小"在文档里只属于"分块大小"这个**别的**话题，专门制造关键词路被带偏）、`hard_multi_turn_ellipsis_locked_params`（末轮"除了它，还有别的一起被固定住的吗"）、`hard_multi_turn_colloquial_ellipsis_rate_limit`（末轮"多久能好"，口语+省略叠加）、`ambiguous_reference_must_not_guess`（FR-003 守门：末轮"它"有两个合理指代对象，**不允许**猜，故意不配 `expected_document_ids`）
  - 所有 `expected_facts` 逐字对应知识库里两篇真实文档的原文，无编造
  - **实测结果：设计目标没达成。** 关闭改写跑，三条"硬"用例 `RetrievalHit` 全为 `true`、`MRR` 全为 `1.00`，分数 5/5/4。原因不是问题不够难——**评测知识库总共只有 4 个 chunk、2 篇文档，而 `retrievalTopK = 5`**（`internal/conversation/context.go:39`），每次检索都把整个语料全量返回，Hit@1/MRR 在结构上不可能低于 1.00。任何问题在这个语料下都无法"检索失败"

- [x] T038a **【SC-001/SC-002 的度量落点】** —— **已完成（2026-08-25）**，在 `internal/knowledge/eval_gate_test.go` 的确定性门禁数据集上新增 3 个受控 case
  - **SC-001**（成对看）：`rewrite_before_elliptical_query_misses` + `rewrite_after_standalone_query_hits`。同一个知识库、**同一条目标片段**、同一条流水线，唯一变量是 query 字符串——这正是查询改写在生产里做的唯一一件事。数据是量过的而非拍脑袋：对目标正文 `word_similarity('它的上限呢', ...) = 0`、`word_similarity('分块大小上限', ...) = 0.5714286`，而 `keywordAdmissionThreshold = 0.45`；目标片段向量与查询向量正交（cos=0 < `vectorAdmissionThreshold=0.35`），两条召回路都堵死，除"改写后的关键词"外没有别的解释路径
  - **SC-002**：`rerank_promotes_out_of_topk_candidate`。5 条候选靠正文长度差异拉开关键词相似度，目标稳定排在融合第 4；**case 内先跑一次不重排的 baseline 断言目标确实不在 topK 里**（前置条件不成立就直接 fail，避免"本来就在里面"的假通过），再用注入的固定打分把它提到第 1
  - **变异验证**：把"改写前"的 query 换成补全后的字符串，该 case 立即以 `ResultCount = 1, want 0` 失败——证明断言真的会咬人，不是永远绿
  - **边界（必须随报告一起说）**：这组 case 证明的是**机制成立**（改写确实改变了检索结果、重排确实改变了顺序），**不是真实效果幅度**。禁止在任何对外材料里表述成"提升了 N 个百分点"

- [x] T038b 修复开发环境 PostgreSQL 落后的迁移版本 —— **已完成（2026-08-24）**
  - 问题：`schema_migrations.version = 3`，`pgmigrations/000004_chunks_content_trgm`（`pg_trgm` 扩展 + trigram 索引 + 库级 `word_similarity_threshold`）从未在开发库执行，关键词路每次调用都失败降级（`function word_similarity(unknown, text) does not exist`），**Hybrid Search 在开发环境一直是纯向量单路**
  - 代码没有错：`Retrieve` 的 best-effort 降级按设计工作；集成测试一直是绿的，因为 `testutil` 每次建全新测试库并跑完整迁移。只有长期存在的开发库落在了 version 3
  - 修复后核验：`version = 4`、`pg_trgm` 已装、`idx_chunks_content_trgm` 已建、`word_similarity()` 可调用
  - 已知连带：2026-08-24 之前的所有 `make eval` 结果（含 `eval/baseline.json`）都是纯向量跑出来的

- [ ] T039 双开关关闭下跑 `make eval-retrieval-gate`（**确定性门禁，不是 `make eval`**），断言除 `ran_at` 外与改动前逐字节一致、四项 metrics 不变、`pass: true`（SC-003）。不一致必须先修 T026 的等价性再继续
  - 已于 2026-08-24 在 US1 完成后跑过一次并通过（与 2026-08-08 的报告逐字节相同）；US2 改完 `rrfFuse` 之后**必须重跑**
  - **不要用 `make eval` 验证这一条**：它每条用例都调真实对话模型 + 裁判模型，同一份代码跑两次都不会一致
- [ ] T040 双开关打开下跑 T038a 的受控门禁用例，记录 SC-001/SC-002 的机制断言结果；报告中**必须写明**这是机制证明而非真实效果幅度（依据见 spec.md 的"度量方式修正"）
- [ ] T041 [P] 更新 `README.md` 与 `docs/critical-paths.md`：新的检索链路顺序、6 个配置项、rerank 模型注册方式（前端暂无入口）
- [ ] T042 [P] 写 `docs/eval-phase9-query-rerank-report.md`，沿用 Phase 1-8 报告结构，含未用真实 rerank 服务验证的项的如实说明
- [x] T043 **（2026-08-25 完成）** 按 [quickstart.md](./quickstart.md) 逐条走验收清单——冒烟测试抓到 T023 的第三处白名单遗漏（见上），修复并补测试后重跑通过；配置校验在真实启动路径上按设计降级（`HIFY_RAG_RERANK_ENABLED=true` 但未配模型 ID 时打 Warn 并关闭，不让进程启动失败）。原清单项：`make migrate-up`/`make sqlc`/`make check-deps`/`go test ./... -race -count=1`（无 skip）/`go vet ./...`/`/smoke-test`
- [x] T044 ~~更新 `.claude/CODEX_CLAUDE_HANDOFF.md`~~ —— **作废（2026-08-25）**：所有者已停用 Codex，该交接文件已删除，宪法第 VIII 条同步改写为「提交时机由所有者决定」（v1.2.0）。本阶段的实施结果、真实测试输出与未验证项改为落在 `docs/eval-phase9-query-rerank-report.md`，不再需要单独的交接文件

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: 无依赖，必须最先做（T001 基线一旦错过就补不回来）
- **Foundational (Phase 2)**: 依赖 Setup，阻塞所有故事
- **US1 (Phase 3)**: 依赖 Phase 2；与 US2 无相互依赖
- **US2 (Phase 4)**: 依赖 Phase 2；4.1 必须在 4.2 之前（`Client` 接口先稳定）
- **US3 (Phase 5)**: 依赖 US1 与 US2 的实现存在（它测的是这两条路径的降级）
- **Polish (Phase 6)**: 依赖前面全部

### Within Each User Story

- 测试先写、先失败，再写最小实现
- migration → sqlc → model → service → handler → wire（US2 的 4.1 严格按此顺序）
- 纯函数先于接线，接线先于集成测试

### Parallel Opportunities

- T006/T007/T008 三组纯函数测试可并行（同一文件不同函数，建议顺序写以免冲突）
- T018/T019 可与 T016/T017 并行（不同文件）
- T024 的 4 个假实现补丁可并行
- T032/T033 可并行；T038/T041/T042 可并行
- US1 与 US2 可由不同人并行推进

---

## Implementation Strategy

### MVP（只做 US1）

1. Phase 1 Setup → Phase 2 Foundational → Phase 3 US1
2. **停下来验证**：两轮对话人工验证 + `make eval` 看 SC-001
3. US1 单独就能显著改善多轮体验，且零 migration、零供应商改动，可先上

### 增量交付

1. Setup + Foundational → 地基
2. + US1 → 独立验证 → 可上（MVP）
3. + US2 → 独立验证 → 可上
4. + US3 → 降级与可观测补齐 → 生产可开
5. Polish → 评测数字与文档

---

## Notes

- `[P]` = 不同文件、无依赖
- 每个任务完成后跑一次相关的定向测试，不要攒到最后
- `make check-deps` 在 Phase 4 结束后必须跑一次（新增了跨模块调用点）
- Claude 不执行 `git commit`/`git push`；提交时机由所有者决定
