---

description: "Task list for 007-document-processing-notice"
---

# Tasks: 文档处理的「成功但有提示」通道

**Input**: Design documents from `/specs/007-document-processing-notice/`

**Prerequisites**: [spec.md](./spec.md)、[plan.md](./plan.md)（含两项拍板）、[research.md](./research.md)（R1~R5）、
[data-model.md](./data-model.md)、[contracts/document-notice.md](./contracts/document-notice.md)、[quickstart.md](./quickstart.md)

**Tests**: **需要**。SC-001 被宪法第 VI 条要求**在改动前先跑出 FAIL**；US3 几乎没有实现代码，
它的全部价值就在断言里；页码编解码有一堆边界情况（空、乱序、重复、极多）只有纯函数单测能便宜地覆盖。

## Format: `[ID] [P?] [Story] Description`

- **[P]**: 可并行（不同文件、无未完成依赖）
- **[Story]**: US1~US3，仅用户故事阶段的任务带

## 本功能的三条硬规矩

1. **每个任务改完跑 `make check-deps`**，非 0 退出即视为未完成。
2. **`internal/db/gen/` 是生成代码，一个字都不许手改**——只能改 `queries/*.sql` 后跑 `make sqlc`。
3. **注释语言跟随所在文件**：`service.go`/`repository.go`/新建 `notice.go` 英文；`model.go` 中文段落跟随中文；
   migration 注释中文；**面向用户的提示文案必须中文**。

## 已拍板的裁决（plan.md，实现阶段不得再摇摆）

| 决策 | 结论 |
|---|---|
| 提示粒度（D-001） | 单列存**页码列表**，不存渲染句子，不做 `code` 字段 |
| 清除时机（D-002） | 并进 `MarkDocumentReady`，**不新增任何清除语句** |
| 提示文案 | 列表行短版 `${chunk_count} 个分片 · 有 N 页未提取文本`，完整信息走 `title` 悬浮 |
| 连续页码折叠 | **放前端**，后端 DTO 只发原始数组 |
| US3 | 不裁，但以**验收用例**为主体 |

---

## Phase 1: Setup（改动前基线）

⚠️ **T001~T004 必须在写任何实现代码之前完成**。

- [ ] T001 执行 `make app-down` 再 `make db-up`，确认 MySQL(3307) 可连；`go version` 必须报 1.26.5。⚠️ `app-down` 不能省——容器里的 asynq reconcile 会在测试跑的过程中改文档状态，正是本功能要断言的那个状态
- [ ] T002 [P] 归档确定性检索门禁基线：`make eval-retrieval-gate && cp eval/runs/phase6-retrieval-gate-latest.json /tmp/gate-baseline-007.json`。本功能**不碰检索链路**，门禁逐字节不变是它的**证明**而不是目标——变了说明改到了不该改的地方
- [ ] T003 [P] 归档改动前 `go test ./... -race -count=1` 的全绿输出到 `/tmp/tests-before-007.txt`
- [ ] T004 [US1] 在 `internal/knowledge/integration_test.go` 新增 `TestIntegrationPartialScanNoticeReachesDocumentList`（夹具 F1：5 页 PDF 第 2、4 页无文本），**走文档列表的仓储/服务路径**断言返回的文档带回缺页页码 `[2,4]`；只写用例不写实现，跑 `go test ./internal/knowledge/ -race -count=1 -run 'TestIntegrationPartialScanNoticeReachesDocumentList' -v 2>&1 | tee /tmp/sc001-before-007.txt`，⭐ **必须看到 FAIL 并归档**。⚠️ 断言必须走**列表**而不是单查——漏掉列表那处 `SELECT` 时单查用例照样绿，而用户唯一能看到提示的地方恰恰是列表

**Checkpoint**: SC-001 的 FAIL 在手。

---

## Phase 2: Foundational（阻塞全部用户故事）

⚠️ 顺序由宪法第 IV 条锁定：`migration → sqlc → notice.go → model.go → repository.go`，不得跳序。

- [ ] T005 新建 `internal/db/migrations/000015_document_unextracted_pages.up.sql`（`ALTER TABLE documents ADD COLUMN unextracted_pages TEXT NULL;`）与对应 `.down.sql`（`DROP COLUMN`）。**不做任何回填**——系统并不知道存量文档当初有没有缺页，凭空标记就是编造，`NULL` 是唯一诚实的值（FR-015 / R4）。注释里写明：不加 CHECK 是因为「逗号分隔的升序正整数」不是 MySQL 能便宜表达的结构，不变量改由 `notice.go` 保证并被单测锁定；不建索引是因为从不按它查询
- [ ] T006 跑 `make migrate-up`，然后验一次：`SELECT count(*) FROM documents WHERE unextracted_pages IS NOT NULL;` MUST 返回 **0**（FR-015 / SC-007）
- [ ] T007 改 `internal/db/queries/documents.sql`：`MarkDocumentReady` 的 `SET` 里**增加一个赋值** `unextracted_pages = ?`，紧挨着既有的 `error_message = NULL`；**三处**读取文档的查询（按 ID 取、按知识库列表、reconciliation 扫描）的 `SELECT` 列表都加上新列。⚠️ **漏掉列表那一处 = 功能完全不生效但单查用例照样绿**。⚠️ **禁止**为清除提示单开一条语句——两条语句之间没有事务就有窗口，窗口里状态和提示不一致，而这种不一致没有任何报错
- [ ] T008 跑 `make sqlc`，用 `git diff --stat internal/db/gen` 确认改动只涉及 `Document` 模型新增字段、`MarkDocumentReady` 参数新增一项、三处查询的列。**生成代码不得手改**
- [ ] T009 [P] 新建 `internal/knowledge/notice.go`（英文注释）：`encodeUnextractedPages([]int) sql.NullString` 与 `decodeUnextractedPages(sql.NullString) []int`。格式为升序、逗号分隔、无空格、1-indexed；空列表编码为 **`NULL` 而不是空字符串**（"没有缺页"和"长度为 0 的列表"是同一件事，只用一种表示）。⭐ 编码时**显式排序去重、过滤非正值**——`textLayerCoverage` 目前恰好按页序产出，但那是它的实现细节，依赖它就是把顺序从值的性质变成调用链的性质（宪法第 V 条）。解码遇到无法解析的历史值 MUST 返回**空列表而不是报错**：一列展示用的数据不该让整个文档列表接口失败
- [ ] T010 [P] 新建 `internal/knowledge/notice_test.go`：往返用例（`decode(encode(x))` 等于规范化后的 x）、空列表 ⇄ `NULL`、单页、**乱序输入必须编码成升序**、重复页码去重、非正值被过滤、大量页码（模拟千页文档）、坏值解码降级为空列表（N1~N5）
- [ ] T011 改 `internal/knowledge/model.go`：`Document` 新增 `UnextractedPages []int`，中文注释写明——1-indexed 升序；`nil` 同时表示"这次没缺页"和"这份文档从未被本功能上线后的版本处理过"，**系统不区分因为它确实不知道**；⚠️ 它**不是错误**，携带它的文档 `status` 仍是 `ready`，失败原因走 `ErrorMessage`，两条通道完全独立（FR-002）
- [ ] T012 改 `internal/knowledge/repository.go`：`markDocumentReady` 增加 `unextractedPages []int` 参数并用 `encodeUnextractedPages` 写入；**三处**文档行映射用 `decodeUnextractedPages` 读出
- [ ] T013 跑 `go test ./... -race -count=1` 与 `/tmp/tests-before-007.txt` 比对：此时**行为尚未改变**，除 T004 的 SC-001 用例仍 FAIL 外应全绿

**Checkpoint**: 列贯通、编解码可测、约束验过。用户故事可以开工。

---

## Phase 3: User Story 1 - 部分内容没进去时，用户能看见 (P1) 🎯 MVP

**Goal**: 部分页无文本层的文档正常入库，**同时**文档列表上有一条明确的提示。

**Independent Test**: T004 的用例转绿；外加走 HTTP 文档列表端点断言 `unextracted_pages` 出现在响应里。

- [ ] T014 [US1] 改 `internal/knowledge/service.go` 的 `ProcessDocument`：006 的 `textLayerCoverage` 已经算出缺页页码，目前只落 `slog.Warn` 就丢掉了——把它**一路带到** `markDocumentReady` 的新参数。⚠️ 全部页无文本时仍走 `ErrPDFNoTextLayer` **失败**路径，**不得**改判为「成功但有提示」（FR-013）；非 PDF 恒传 nil（FR-014）
- [ ] T015 [US1] 改 `internal/knowledge/dto.go`：文档响应新增 `unextracted_pages []int \`json:"unextracted_pages"\``。⚠️ 后端**只发 `null` 或非空数组**，绝不发空数组（契约 C2）
- [ ] T016 [P] [US1] 改 `web/src/lib/knowledge.ts`：`KnowledgeDocument` 新增 `unextracted_pages: number[] | null`，注释写明它不是错误、`status` 仍为 `ready`、`null` 与 `[]` 都当作"无提示"
- [ ] T017 [US1] 改 `web/src/routes/knowledge-documents-dialog.tsx`（约 103 行）：`status === "ready"` 且有缺页时，在 `${chunk_count} 个分片` 后追加短版提示 `· 有 N 页未提取文本`，完整信息（折叠后的页码区间 + OCR 指引）走 `title` 悬浮。⭐ **连续页码折叠成区间在前端做**（`46,47,48,49,50` → `第 46-50 页`），后端只发原始数组。⚠️ 短版里的 **N 必须是真实总数**，不是被截断后的列举数量——截断的是列举，不是事实
- [ ] T018 [US1] 跑 T004 的用例，⭐ **它现在必须 PASS**；与 `/tmp/sc001-before-007.txt` 的 FAIL 一起留作阶段报告的硬证据
- [ ] T019 [US1] 在 `internal/knowledge/retrieve_handler_test.go` 或等价的 HTTP 层测试里，走**真实的文档列表端点**断言 `unextracted_pages` 出现在 JSON 响应中且值正确（契约 §1）
- [ ] T020 [US1] 补断言 SC-002：提示的完整文案里**同时**含真实数量、页码、以及 OCR 指引——三者缺一，用户就只拿到焦虑而不是下一步动作（FR-008 / FR-009）

**Checkpoint**: US1 独立可验收，MVP 达成——用户上传部分扫描的 PDF 后能看见有内容没进去。

---

## Phase 4: User Story 2 - 提示与失败不会被混淆 (P1)

**Goal**: 用户一眼分清「处理失败，现在不可用」和「可用，但有一部分没进去」。

**Independent Test**: 同一列表里同时放失败文档与带提示的就绪文档，断言两者呈现不同且后者仍呈现为可用。

⚠️ 这个故事失败的方式是**沉默的**：做得像失败会让用户去删文档，做得太弱会被忽略，两种情况下 US1 都白做且**没人会来报 bug**。

- [ ] T021 [US2] 在 `web/src/routes/knowledge-documents-dialog.tsx` 确认提示的视觉呈现**明确区别于失败**（失败走既有的失败样式，提示走一个明显更弱、但不至于被忽略的样式），且带提示的文档**仍呈现为可用**（不加任何"需要修复"的暗示）
- [ ] T022 [US2] 在同一文件确认：`status !== "ready"` 时**绝不展示** `unextracted_pages`（契约 C5）。⚠️ 字段本身可能有值（上一次成功时写的），**判断依据是 `status`，不是字段有没有值**（FR-005）
- [ ] T023 [US2] 确认失败文档的呈现与改动前**逐字节一致**：`d.status === "failed" ? d.error_message` 这一支一字不改（契约 C7 / R3）
- [ ] T024 [US2] 确认没有把提示做成 `status` 的第五种取值：`status` 的取值集合由数据库 CHECK 约束固定，且它表达的是**文档可不可用**——带提示的文档是可用的，提示是可用文档的附加说明，不是一种新状态（契约 §2.2）
- [ ] T025 [P] [US2] 在 `internal/knowledge/integration_test.go` 新增断言：一份**失败**的文档，其响应里即使 `unextracted_pages` 有历史值也不得被前端消费——用 DTO 层断言 `status` 与该字段的组合，锁定 D7

**Checkpoint**: US1 + US2 完成，两个 P1 全部可独立验收。

---

## Phase 5: User Story 3 - 提示随重新处理而更新 (P2)

**Goal**: 提示与文档的**当前**状态 100% 一致，不存在陈旧提示。

⚠️ 本阶段**几乎没有实现代码**——FR-004 由 R2 的「并进 `MarkDocumentReady`」免费得到。
但**一个免费得到的性质如果没有断言锁定，下一次有人拆分那条 SQL 时它就会无声消失**。

- [ ] T026 [US3] 在 `internal/knowledge/integration_test.go` 新增 SC-005 用例（夹具 F5）：同一份文档先处理一个缺页版本（断言提示存在），再处理一个不缺页的版本，**断言提示消失**（`unextracted_pages` 变回 `NULL`）
- [ ] T027 [US3] 新增反向用例：一份原本无提示的文档，被替换成有扫描页的版本后重新处理，**断言提示出现**
- [ ] T028 [US3] 新增版本竞争用例（夹具 F6）：同一文档两次处理竞争 publishing，断言最终的提示属于**赢下发布的那一次**——这条由 `WHERE version = ? AND status = 'publishing'` 保证，用例是它的证据
- [ ] T029 [US3] 新增用例：处理**失败**时，显示的是失败原因而不是上一次成功留下的旧提示（FR-005）

**Checkpoint**: 提示不会变成一条没人相信的陈旧信息。

---

## Phase 6: Polish & 验收

- [ ] T030 `make check-deps` 输出 OK；`go vet ./...` 无输出；`cd web && npx tsc --noEmit` 通过
- [ ] T031 `go test ./... -race -count=1` 全绿，**无 skip**（数据库测试禁止 skip = 未验证）
- [ ] T032 `make eval-retrieval-gate` 与 `/tmp/gate-baseline-007.json` 比对，MUST 输出 **`IDENTICAL`**。本功能不碰检索链路，这是它的**证明**——若它变了，说明改到了不该改的地方
- [ ] T033 ⚠️ 全程**未使用** `make eval` 作为证据：它每条用例都调真实对话与裁判模型，同一份代码跑两次都不一致
- [ ] T034 [P] SC-004 断言：全页有文本的文档，其列表呈现与改动前**逐字符一致**——不产生任何新的视觉噪音
- [ ] T035 [P] SC-006 断言：纯扫描件仍作为**失败**呈现，错误码与文案与 006 上线时**完全一致**
- [ ] T036 [P] SC-007 断言：txt / md 恒 `null`；迁移后存量行统计为 0
- [ ] T037 **变异测试**（跑完务必还原）：逐项注入并确认对应用例**确实失败** —— `MarkDocumentReady` 不写新列 → SC-001；写入但无缺页时不清除 → SC-005；**只在单查那处 `SELECT` 带出新列、列表那处漏掉** → SC-001（这正是它必须走列表接口的原因）；编码时不排序 → 乱序往返单测；纯扫描件改判为「成功但有提示」 → SC-006；前端在 `status !== "ready"` 时也展示该字段 → D7
- [ ] T038 跑 `/smoke-test`
- [ ] T039 ⚠️ **手动验证一份真实的、部分页是扫描图的 PDF**：上传后在文档列表里确认提示可见、可读、且不像一个错误。helper 造的 PDF 永远只覆盖 helper 作者想到的情形——006 就是在这一步撞上了 `rsc.io/pdf` 在真实 PDF 上 panic 的既有缺陷
- [ ] T040 产出 `docs/eval-phase14-document-processing-notice-report.md`，**必须如实包含三件事**：① 本功能**不改善处理质量**，不让任何一页变得能提取，只是把一件已经发生的事说出来，不得写成"处理质量提升"；② **提示只在文档列表可见**，用户在对话里问到缺失内容时**仍然只会得到"检索不到"**——本功能让他能在上传后预先知道，不改善那个体验；③ **存量文档不追溯标记**，`NULL` 同时表示"没缺页"和"不知道"，系统不区分
- [ ] T041 ⚠️ **不擅自 commit/push**（宪法第 VIII 条）——验收跑完向所有者汇报，由所有者决定提交时机

---

## Dependencies & Execution Order

```
Phase 1 (基线)  ← T004 的 FAIL 是 Phase 3 开工的前提
      ↓
Phase 2 (Foundational：列 + 编解码 + 贯通)  ← 阻塞全部故事
      ↓
Phase 3 (US1, P1) 🎯 MVP
      ↓
Phase 4 (US2, P1)  ← 依赖 US1 的展示产出
      ↓
Phase 5 (US3, P2)  ← 依赖 US1 的写入产出
      ↓
Phase 6 (验收)
```

### 故事间依赖

- **US1 是其余两个的前提**：US2 验收的是 US1 产出的提示怎么呈现，US3 验收的是它怎么更新。这与 006 那种"故事互相独立"的形态不同——本功能小且线性，硬拆成独立故事是假的。
- **US2 虽然依赖 US1，但仍是 P1**：它失败的方式是沉默的（见 Phase 4 的 ⚠️），降为 P2 会让它在实现阶段被顺手砍掉。

### 可并行

- Phase 1：T002 / T003
- Phase 2：T009 / T010（notice.go 与它的单测）可与 T005~T008 并行编写，但落地顺序仍受宪法第 IV 条约束
- Phase 3：T016（前端类型）可与 T014/T015（后端）并行
- Phase 6：T034 / T035 / T036

---

## Implementation Strategy

### MVP = Phase 1 + 2 + 3（US1）

用户上传部分扫描的 PDF 后**能看见有内容没进去**——这是本功能存在的全部理由，
其余每一条都是为了让这一条成立。

### 增量交付

1. **Phase 1+2+3** → 用户能看见（SC-001 从 FAIL 转 PASS）
2. **+ Phase 4** → 看见的东西不会被误解成失败
3. **+ Phase 5** → 看见的东西不会过期
4. **Phase 6** → 证据归档 + 阶段报告

### 每个任务完成的判据

改完代码 → `make check-deps` OK → `go vet` 无输出 → 相关测试绿。三条任一不过，该任务**未完成**。
