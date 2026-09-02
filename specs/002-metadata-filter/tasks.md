---

description: "Task list for 检索元数据过滤（Metadata Filtering）"
---

# Tasks: 检索元数据过滤（Metadata Filtering）

**Input**: Design documents from `/specs/002-metadata-filter/`

**Prerequisites**: [plan.md](./plan.md)、[spec.md](./spec.md)、[research.md](./research.md)、
[data-model.md](./data-model.md)、[contracts/](./contracts/)

**Tests**: 包含测试任务。宪法第 II/VI 条要求严格测试先行——每组行为**先加失败测试，再写最小实现**。

**Organization**: 按用户故事分组，每个故事可独立实现、独立验证。

## Format: `[ID] [P?] [Story] Description`

- **[P]**: 可并行（不同文件、无未完成依赖）
- **[Story]**: US1 / US2 / US3 / US4
- 每条都写明确切文件路径

## Path Conventions

Go 模块化单体：`internal/<module>/`、`internal/db/pgqueries/`、`internal/db/gen/`、`docs/`。
**本期无 `internal/db/pgmigrations/` 改动**（见 plan.md：过滤所需的列已存在）。

---

## Phase 1: Setup（共享前置）

**Purpose**: 固定"改动前"的基线，否则 SC-003 无法验证

- [x] T001 在**任何代码改动之前**跑 `make eval-retrieval-gate`，把
  `eval/runs/phase6-retrieval-gate-latest.json` 归档为基线。
  注意：比对时必须忽略 `ran_at` 字段（每次运行都不同），其余逐字节比对。
  （001 期的 T001 因为先实现后补跑而失败过，这次必须先跑）
- [x] T002 跑 `go test ./... -race -count=1`、`go vet ./...`、`make check-deps`，
  确认改动前工作区全绿（有失败先记录，**不要**在本功能里顺手修——宪法第 IX 条）

**Checkpoint**: 基线已归档，可以开始改动

> **关于"测试先行"的如实记录（宪法第 II/VI 条）**：Phase 2 的 T003/T004（`IsEmpty`/`Validate`
> 的失败测试）实际是在 T005/T006 的实现之后写的，不是严格的先红后绿。US1-US4 的集成测试与
> 门禁用例则是在对应实现之后补的。为弥补这一点，改用**变异测试**验证断言确有效力：
> 分别注入"过滤不下推"和"NULL 页码视为通过"两种缺陷，确认用例真的会失败（详见报告第 6.4 节）。
> 其中单侧页码变异一轮**暴露出原用例的盲区并已修正**——这个收获恰恰说明"测试通过"本身
> 不能替代"测试有牙齿"。

---

## Phase 2: Foundational（阻塞所有故事）

**Purpose**: 过滤器类型、校验、开关、SQL 下推——四个故事全部依赖它

**⚠️ CRITICAL**: 本阶段完成前，US1/US2/US3/US4 都不能开工

### 2.1 类型与纯函数（先测试）

- [x] T003 [P] 新建 `internal/knowledge/filter_test.go`，为尚不存在的
  `RetrieveFilter.IsEmpty()` 写失败测试：三字段全空 → true；任一非空 → false；
  `DocumentIDs` 为长度 0 的非 nil 切片 → 仍然 true（FR-006 的空过滤器判定）
- [x] T004 [P] 在同一文件为 `RetrieveFilter.Validate()` 写失败测试：
  文档数 > `maxFilterDocumentIDs`(50) → `ErrTooManyFilterDocuments`；
  `PageMin`/`PageMax` ≤ 0 → `ErrInvalidPageRange`；`PageMin > PageMax` → `ErrInvalidPageRange`；
  只给一端合法；空过滤器 → nil。**断言超限时返回错误而非截断**（FR-015）
- [x] T005 在 `internal/knowledge/model.go` 新增 `RetrieveFilter`、`RetrieveOptions`、
  常量 `maxFilterDocumentIDs = 50`，并实现 `IsEmpty()`/`Validate()`，按
  [data-model.md](./data-model.md) §3。注释延续 `model.go` 既有语言
- [x] T006 在 `internal/knowledge/errors.go` 新增 `ErrTooManyFilterDocuments`、
  `ErrInvalidPageRange`、`ErrMetadataFilterDisabled`，Message 为中文（宪法第 VII 条），
  文案照 data-model.md §3.3
- [x] T007 跑 T003/T004 的测试，确认由红转绿

### 2.2 SQL 下推（无 migration，从 sqlc 起步）

- [x] T008 在 `internal/db/pgqueries/chunks.sql` 的 `SearchVectorChunks` 与
  `SearchKeywordChunks` 各加三行可空谓词，写法严格照 [research.md](./research.md) R1。
  中文注释必须写明：(a) 为什么用恒真短路而不是拼 SQL（FR-016）；
  (b) 全 NULL 时为什么结果与排序逐字不变（`ORDER BY ... id ASC` 兜底）；
  (c) NULL 页码为什么靠三值逻辑天然排除、**禁止**改成 `COALESCE`（research.md R2）
- [x] T009 更正 `chunks.sql` 中 `CreateChunk` 那条声称"page_number 一律传 NULL"的过期注释
  （plan.md 范围边界 3）。**只改注释，不改 SQL 语义**
- [x] T010 跑 `make sqlc`，确认 `internal/db/gen` 只有两条召回查询的参数结构体发生变化，
  邻接查询与写路径的生成代码**未被触及**。生成代码不得手改
- [x] T011 在 `internal/knowledge/repository.go` 给 `searchVectorChunks`/`searchKeywordChunks`
  各加 `filter RetrieveFilter` 参数并翻译成 sqlc 可空参数（[contracts](./contracts/internal-contracts.md) C3）。
  **repository 层不做任何校验**（宪法第 IV 条）

### 2.3 配置开关与 Service 接线

- [x] T012 [P] 在 `internal/config/config.go` 新增 `RAGMetadataFilterEnabled`
  与 `HIFY_RAG_METADATA_FILTER_ENABLED`（默认 `false`），解析失败返回错误
- [x] T013 在 `internal/knowledge/service.go` 把 `Retrieve` 签名改为带
  `opts RetrieveOptions`，接口声明（`Service` interface）同步修改；
  在函数最前面加入校验与开关判定：开关关闭 + 非空过滤器 → `ErrMetadataFilterDisabled` 且
  **不发起任何数据库调用**（research.md R4）；校验失败 → 对应错误且不检索
- [x] T014 把 `opts.Filter` 透传到两路召回调用；**第 2 步及之后（rrfFuse/重排/截断/邻接）
  一行都不改**（plan.md「检索链路：过滤插在哪一步」）
- [x] T015 在 `internal/knowledge/wire.go` / `cmd/hify` 的 `buildApp` 把新配置注入
  `knowledge.NewService`
- [x] T016 [P] 修 `internal/conversation/context.go` 与 `internal/workflow/executor.go`
  的调用点，传 `knowledge.RetrieveOptions{}`
- [x] T017 [P] 修 `conversation`/`workflow`/`eval` 下假 knowledge service 的签名。
  **除补签名外不得修改这些测试的任何其他部分**（plan.md 范围边界 1）
- [x] T018 跑 `go build ./...`、`make check-deps`、`go vet ./...`，确认编译通过、无违规

**Checkpoint**: 过滤能力就位，开关默认关闭，空过滤器行为零变化

---

## Phase 3: User Story 1 - 把检索限定到指定文档 (Priority: P1) 🎯 MVP

**Goal**: 调用方能指定一个或多个 `document_id`，两路召回都受此约束

**Independent Test**: 同一问题，不带过滤时结果含 A/B/C 三份文档的片段；限定到 A 后
结果中不含任何 B/C 的片段，且 A 的片段在 A 内部的相对顺序与不带过滤时一致

- [x] T019 [US1] 在 `internal/knowledge/integration_test.go` 新增
  `TestIntegrationRetrieveFilterByDocument`（真实 PostgreSQL，**禁止 skip**）：
  三份文档的知识库，先不带过滤检索记录结果，再限定到 A，断言
  (a) 结果中无 B/C 片段；(b) A 的片段相对顺序不变
- [x] T020 [US1] 新增 `TestIntegrationRetrieveFilterUnknownDocument`：
  过滤器引用不存在的、或属于**另一个知识库**的 `document_id` → 返回空结果、
  **不报错**、**不静默忽略该条件**（FR-010 + Edge Cases 第 1 条）
- [x] T021 [US1] 新增 `TestIntegrationRetrieveFilterNoAutoRelax`：
  过滤后候选数远低于 `candidateK` 时，断言结果**仍然只含过滤范围内的片段**，
  且准入阈值照常执行（不因候选少而跳过）——FR-009 + Edge Cases 最后一条
- [x] T022 [US1] 跑上述三条用例，确认由红转绿

**Checkpoint**: US1 可独立验收

---

## Phase 4: User Story 2 - PDF 页码可被过滤 (Priority: P1)

**Goal**: 按页码范围缩小检索范围；无页码的 chunk 视为不匹配

**Independent Test**: 目标片段在第 N 页，范围含 N 时召回、不含 N 时不召回

- [x] T023 [P] [US2] 在 `internal/knowledge/structure_test.go` 新增 FR-001 修订要求的
  **回归断言**：`chunkPDFPages` 产出的每个 piece 都带正确的 1-indexed 页码。
  这条不是新功能，是防止后续改动把已有的产出链路退化掉
- [x] T024 [US2] 在 `integration_test.go` 新增 `TestIntegrationRetrieveFilterByPageRange`
  （真实 PG，禁止 skip）：目标片段在第 N 页，`[N-2, N+2]` 命中、`[1, 5]`（不含 N）不命中
- [x] T025 [US2] 新增 `TestFilterPageRangeExcludesNullPageChunks`：
  `page_number IS NULL` 的 chunk（非 PDF / 存量行）在页码过滤下**不被召回**、
  在**无过滤**下**正常被召回**（SC-005 修订 / research.md R2）。
  这条同时锁定"绝不允许改成 `COALESCE(page_number, 0)`"
- [x] T026 [US2] 跑上述用例，确认由红转绿

**Checkpoint**: US2 可独立验收

---

## Phase 5: User Story 3 - 关掉过滤时行为逐字不变 (Priority: P1)

**Goal**: 本功能不成为既有检索质量的风险来源

**Independent Test**: 空过滤器 / 开关关闭时，固定输入集的检索输出与上线前逐字一致

- [x] T027 [US3] 在 `internal/knowledge/filter_test.go` 新增 `TestRetrieveFilterDisabled`：
  开关关闭 + 非空过滤器 → `ErrMetadataFilterDisabled`，且断言
  **没有发生任何数据库调用**（用假 repository 计数）——research.md R4
- [~] T028 [US3] **按规格执行但结论与原文不同，如实记录**：原文要求"补一条门禁用例证明空过滤器
  路径进入门禁覆盖范围"。实际发现**不需要新增用例**——门禁原有的 12 个 case 全部以
  `RetrieveOptions{}`（空过滤器）调用 `Retrieve`，本身就是空过滤器路径的覆盖，而它们改动后
  逐字段不变这件事本身就是断言。为此新增一个 case 反而会稀释这条断言
  - 改为按 SC-001/SC-002 的要求新增两条**过滤生效**的门禁用例
    （`filter_scopes_to_document`、`filter_scopes_to_page_range`），见 T033 之外的实际产出
- [~] T029 [US3] **比对方式已修正，如实记录**：原文要求"忽略 `ran_at` 后逐字节比对整份报告"。
  该做法与 SC-001/SC-002 要求的"门禁中存在过滤用例"直接冲突——新增用例必然让 `cases` 数组变长，
  整份比对必然 DIFFERS
  - 改为 `scripts/compare-retrieval-gate.py`：基线里每个 case 必须存在且**逐字段相同**、
    `metrics` 与 `pass` 必须相同、**允许新增 case**。SC-003 要断言的是"既有行为一个字没变"，
    不是"报告文件永不增长"
  - 实际结果：`IDENTICAL（12 个既有用例逐字段一致，metrics/pass 未变）`，新增 2 条用例
- [x] T030 [US3] 跑 `go test ./... -race -count=1` 全量，确认既有
  `hybrid_test`/`admission_test`/`dedup_test`/`neighbor_test`/`rerank_test` 全绿
  （contracts C4 的不变量清单）

**Checkpoint**: 回归风险已被断言锁定

---

## Phase 6: User Story 4 - 过滤生效情况可观测 (Priority: P2)

**Goal**: 能分清"过滤没生效"与"过滤生效了但该范围里确实没答案"

- [x] T031 [US4] 在 `service.go` 的既有 slog 行
  （`knowledge: retrieval candidate admission and dedup`）中并入
  [data-model.md](./data-model.md) §5 的 6 个字段，触发条件放宽为
  原条件 **或** `filter_applied`。**不新开一行**
- [x] T032 [US4] 断言 FR-018 脱敏：日志**不含** document_id 取值、页码数值、
  query 原文、片段正文。只记种类与数量

**Checkpoint**: US4 可独立验收

---

## Phase 7: 下推证明与验收（跨故事）

- [x] T033 新增 `TestIntegrationRetrieveFilterPushedDownToRecall`（SC-004，**本功能最关键的用例**）：
  构造目标文档的片段全部排在全库相似度榜 `candidateK` 名之外的知识库；
  断言限定到该文档后**仍能拿到该文档内的 topK 个候选**，而不是"全库 topK 里恰好属于它的那几条"。
  这是过滤下推与应用层筛选唯一能被外部观察到的区别
- [x] T034 新增 `TestIntegrationRetrieveFilterExemptsNeighborsFromPageFilter`（FR-011）：
  页码过滤命中某 anchor 时，其落在范围外的邻接块**仍在结果中**（`NeighborOf != ""`）；
  同时断言**不存在**来自过滤范围外文档的邻接块（文档级仍生效）
- [x] T035 按 [quickstart.md](./quickstart.md) 逐条执行验收清单，每条都要有真实输出
  （新增一步：变异测试，见报告第 6.4 节）
- [x] T036 跑 `/smoke-test`，确认签名变更没有破坏 `buildApp` 装配与既有对话链路
- [x] T037 产出 `docs/eval-phase10-metadata-filter-report.md`，体例照 `docs/eval-phase1~9-*.md`。
  **必须区分「代码验证过」与「效果验证过」**：本期没有带范围标注的真实语料，
  测不出效果幅度，全部结论都是**机制证明**，不得写成效果数字（research.md R8）。
  报告还必须如实写明 plan.md 范围边界 2：**本期不提供设置过滤器的入口，功能上线后无人可用**，
  价值兑现依赖下一期

---

## Dependencies

```
Phase 1 (T001-T002)  ← 必须最先，且在任何代码改动之前
   ↓
Phase 2 (T003-T018)  ← 阻塞全部故事
   ↓
   ├─ Phase 3 US1 (T019-T022)
   ├─ Phase 4 US2 (T023-T026)
   ├─ Phase 5 US3 (T027-T030)
   └─ Phase 6 US4 (T031-T032)
        ↓
Phase 7 (T033-T037)  ← 需要全部故事就位
```

Phase 2 内部顺序（宪法第 IV 条）：
`model.go(T005) → errors.go(T006) → chunks.sql(T008/T009) → sqlc(T010) → repository.go(T011) → service.go(T013/T014)`。
本期无 migration、无 `dto.go`/`handler.go` 改动（本期不提供 HTTP 入口）。
