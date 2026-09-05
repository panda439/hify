---

description: "Task list for 008-unparseable-page-notice"
---

# Tasks: 解析失败的页面也要能被用户看见

**Prerequisites**: [spec.md](./spec.md)、[plan.md](./plan.md)、[research.md](./research.md)（R1~R5）

**Tests**: **需要**。SC-001 被宪法第 VI 条要求先跑出 FAIL；写入前移是一次行为改动，
只做一半会把 007 的缺陷固化，必须有变异测试盯着；FR-003（两类不重叠）明确以断言而非代码保证。

## Format: `[ID] [P?] [Story] Description`

## 已拍板的裁决（plan.md + research.md，实现阶段不得再摇摆）

| 决策 | 结论 |
|---|---|
| 承载形态（D-001） | 第二列 `unparseable_pages`，对称独立；**增生判据**见 research R1 |
| 写入时机（D-002） | 从 `MarkDocumentReady` **前移**到 `MarkDocumentPublishing`，且前者必须**同时停止写** |
| 展示（D-003） | 一个尾缀两段，悬浮里两段的下一步动作**必须不同** |
| 不重叠（FR-003） | **断言**，不写去重逻辑 |

## 本轮新拍板的两项（2026-09-05）

### 裁定 1 · `extractPDFPages` 用**第三个返回值**，跳过的页码挂在 `parsedContent` 上

签名改为 `([]pdfPage, []int, error)`，`parseFile` 把它放进 `parsedContent.UnreadablePages`。

**理由**：`parsedContent` 本来就是"解析这一步产出了什么"的载体（它已经装着 `Pages` 和 `Text`），
被跳过的页码正是这一步的产出之一。再包一个新结构体等于**平行造一个 `parsedContent`**，
两个类型表达同一件事，调用点还得在两者之间转换。

### 裁定 2 · 文案：短版靠"未提取/无法解析"区分，悬浮靠"OCR 有没有用"区分

- **行内短版**：`128 个分片 · 有 5 页未提取文本、2 页无法解析`
- **悬浮第一段**（无文本层）：`第 46-50 页未能提取到文本，通常是扫描图或图片型页面。
  如需检索其中内容，请用 OCR 工具把这些页转换为可选中文字后重新上传。`
- **悬浮第二段**（解析失败）：`第 1 页无法解析（页面结构已损坏或不受当前解析器支持），
  OCR 对它没有用；请用其他 PDF 工具重新导出后上传。`

⚠️ 第二段里的 **「OCR 对它没有用」是这一整期的落点**，不得删。用户刚读完第一段的"请用 OCR"，
不明说的话他会顺手对这几页也做 OCR，然后发现没用——那时这条提示就从"帮助"变成了"误导"。

---

## Phase 1: Setup（改动前基线）

- [x] T001 `make app-down` + `make db-up`；确认 MySQL(3307) 可连、`go version` 报 1.26.5
- [x] T002 [P] 归档门禁基线到 `/tmp/gate-baseline-008.json`（本功能不碰检索，逐字节不变是**证明**不是目标）
- [x] T003 [P] 归档 `go test ./... -race -count=1` 全绿输出到 `/tmp/tests-before-008.txt`
- [x] T004 [US1] 在 `internal/knowledge/integration_test.go` 新增 `TestIntegrationUnparseablePageNoticeReachesDocumentList`：夹具用**一份真实 PDF**（scratchpad 里的 arXiv 论文，第 1 页确定会解析失败）或等价的构造，**走文档列表路径**断言带回 `[1]`；只写用例不写实现，`tee /tmp/sc001-before-008.txt`，⭐ **必须看到 FAIL**。⚠️ 若手边没有能稳定触发逐页 panic 的夹具，先做 T005 造一个——**不得**为了让用例能跑就把断言弱化成"字段存在"

**Checkpoint**: SC-001 的 FAIL 在手。

> ⚠️ **实施期偏离（2026-09-05）**：与 006/007 同样的一处——T011（`model.go` 增
> `UnparseablePages` 字段）与 T005（夹具）都**提前到 Phase 1 执行**。T004 的用例
> 引用该字段、且需要那个夹具，缺任一个它都只是**编译失败**，而
> 「undefined: UnparseablePages」证明不了任何事。加一个未被读写的结构体字段
> 不改变行为，所以提前不破坏「Phase 1 不写实现代码」的意图。
>
> **夹具已验证走的是正确的机制**：`writeTestPDFWithBrokenPages` 让第 2 页的
> `/Contents` 指向不存在的对象，实测触发 `unreadable_pages=[2]` 的 WARN——
> 也就是 `pageParsePanicked` 分支，而不是 `pageUnresolvable`。这一点必须验，
> 两条分支只有前者才是 008 要覆盖的那种缺失。

---

## Phase 2: Foundational

⚠️ 顺序：`migration → sqlc → model → notice 复用 → repository`。

- [x] T005 造一个**能稳定触发逐页解析失败**的测试夹具：在 `structure_test.go` 扩展 `writeTestPDF`，让指定页的 `/Contents` 指向一个不存在的对象（`rsc.io/pdf` 会在 `Page.Content()` 上 panic，正是 006 的 `safePageText` 兜住的那条路径）。⚠️ 这是本期唯一的新夹具能力，必须**先于**所有需要它的测试任务
- [x] T006 新建 `internal/db/migrations/000016_document_unparseable_pages.{up,down}.sql`：`ALTER TABLE documents ADD COLUMN unparseable_pages TEXT NULL;` / `DROP COLUMN`。**不回填**（R5）。注释写明它与 `unextracted_pages` 平行独立、两者下一步动作不同、以及 research R1 的**增生判据**（列数到 3 是重新考虑形态的信号）
- [x] T007 跑 `make migrate-up`，验 `SELECT count(*) FROM documents WHERE unparseable_pages IS NOT NULL;` MUST 返回 **0**
- [x] T008 改 `internal/db/queries/documents.sql`：⭐ 把 `unextracted_pages = ?` **从 `MarkDocumentReady` 移到 `MarkDocumentPublishing`**，并在后者同时新增 `unparseable_pages = ?`；**五处** SELECT 都加上新列。注释里写明前移的理由（`MarkDocumentPublishing` 自己的注释已说"这一步锁定了活儿已经干完"，缺页列表正是那个活儿的结果）
- [x] T009 ⚠️ **确认 `MarkDocumentReady` 里两列的赋值都已删除**。留着的话恢复流程传 nil 会把 publishing 阶段刚写对的值清空——**那正是本期要修的缺陷，反被固化**。这条单列一个任务，因为它是本次最容易只做一半的地方
- [x] T010 跑 `make sqlc`，`git diff --stat internal/db/gen` 确认改动符合预期。生成代码不得手改
- [x] T011 改 `internal/knowledge/model.go`：`Document` 新增 `UnparseablePages []int`，注释写明它与 `UnextractedPages` **平行但语义不同**——两者的下一步动作不同（OCR vs 换工具重新导出），这是它们分开的全部理由；两者**互不重叠**
- [x] T012 [P] 改 `internal/knowledge/notice.go`：新列**复用**既有的 `encodeUnextractedPages`/`decodeUnextractedPages`，把它们改名为与列无关的通用名（如 `encodePageList`/`decodePageList`）。⚠️ **不得复制一份**——编解码是同一件事，两份实现早晚会分叉
- [x] T013 改 `internal/knowledge/repository.go`：`markDocumentPublishing` 增两个参数并写入；`markDocumentReady` **去掉** `unextractedPages` 参数；行映射读出新列
- [x] T014 跑 `go test ./... -race -count=1`：此时除 T004 的 SC-001 仍 FAIL 外应全绿

---

## Phase 3: User Story 1 - 解析失败的页也能被看见 (P1) 🎯 MVP

- [x] T015 [US1] 改 `internal/knowledge/parse.go`：`extractPDFPages` 签名改为 `([]pdfPage, []int, error)`，把逐页 recover 跳过的页码**返回出来**（目前只落 `slog.Warn` 就丢了）；`parseFile` 放进 `parsedContent.UnreadablePages`
- [x] T016 [US1] 改 `internal/knowledge/service.go`：把 `parsed.UnreadablePages` 一路带到 `markDocumentPublishing`。⚠️ 全部页解析失败仍走 `ErrPDFUnreadable` **失败**路径，全部页无文本仍走 `ErrPDFNoTextLayer`，**都不得改判**（FR-013）；非 PDF 两类恒 nil
- [x] T017 [US1] 改 `internal/knowledge/dto.go`：文档响应新增 `unparseable_pages`，与 `unextracted_pages` 同规则（`null` 或非空数组，绝不空数组）
- [x] T018 [P] [US1] 改 `web/src/lib/knowledge.ts`：新增 `unparseable_pages: number[] | null`，注释写明与 `unextracted_pages` 的语义差别
- [x] T019 [US1] 跑 T004 的用例，⭐ **必须 PASS**；与 `/tmp/sc001-before-008.txt` 一起归档
- [x] T020 [US1] 走**真实 HTTP 文档列表端点**断言新字段出现在响应里（沿用 007 的 `doListDocuments`）

---

## Phase 4: User Story 2 - 两种缺失原因不会被混为一谈 (P1)

⚠️ 这个故事**是本功能不并进现有那一列的全部理由**。如果最终两类仍被渲染成一句笼统的
"有 N 页没进去"，这一整期的设计成本就白付了——还不如当初直接塞进去。

- [x] T021 [US2] 改 `web/src/routes/knowledge-documents-dialog.tsx`：尾缀改为按需出现的两段
      （`· 有 5 页未提取文本、2 页无法解析`），悬浮里两段各自完整。只有一类时**只出那一段**，
      不留空位（FR-008）
- [x] T022 [US2] 悬浮第二段**必须明写「OCR 对它没有用」**。用户刚读完第一段的"请用 OCR"，
      不明说他会顺手对这几页也做 OCR，然后发现没用——那时提示就从"帮助"变成了"误导"
- [x] T023 [US2] 新增夹具：一份**两种缺失都有**的 PDF（若干页无文本层 + 若干页解析失败），
      断言两类页码分别正确、且**互不重叠**（FR-003 / SC-004）
- [x] T024 [P] [US2] 断言只有一类缺失时另一类为 `null`，不出现空的另一段（FR-008）

---

## Phase 5: User Story 3 - 提示能熬过发布交接 (P2)

⚠️ 本阶段修的是 **007 报告 §6.2 记录的已知缺陷**。

- [x] T025 [US3] 新增用例：一份有缺失的文档在发布阶段被中断、由 `ReconcileStuckDocuments` 完成，
      断言两类提示**与由原流程完成时一致**（FR-005 / SC-005）。这条在 007 上是 FAIL 的
- [x] T026 [US3] 反向用例：无缺失的文档走同样的恢复路径，断言两类都为空——
      没有它，一个"恢复时永远保留旧值"的实现也能让上面那条通过

---

## Phase 6: Polish & 验收

- [x] T027 `make check-deps` OK；`go vet ./...` 无输出；`tsc --noEmit` 通过
- [x] T028 `go test ./... -race -count=1` 全绿，**无 skip**
- [x] T029 门禁与 `/tmp/gate-baseline-008.json` 比对 MUST `IDENTICAL`；⚠️ 全程未用 `make eval` 当证据
- [x] T030 [P] SC-006：两类都不存在时呈现与改动前**逐字符一致**
- [x] T031 [P] SC-007：整份解析失败 / 整份无文本层仍作为**失败**，两条错误文案与 006 上线时完全一致
- [x] T032 [P] SC-008：非 PDF 与存量文档两类均为 0
- [x] T033 **变异测试**（跑完还原）：⭐ **`MarkDocumentReady` 保留旧的 `unextracted_pages` 赋值** → US3 用例（这是本期最容易只做一半的地方）；`MarkDocumentPublishing` 不写新列 → SC-001；两类写反 → SC-003；只在单查带出新列、列表漏掉 → SC-001；悬浮第二段抄第一段的文案 → T022 的断言；非 PDF 也写 → SC-008
- [x] T034 `/smoke-test`
- [x] T035 ⚠️ 用 scratchpad 里的**真实 arXiv 论文**手动验证：它第 1 页确定解析失败，上传后文档列表应当出现"1 页无法解析"，且悬浮里明说 OCR 没用
- [x] T036 产出 `docs/eval-phase15-unparseable-page-notice-report.md`，**必须如实包含**：① 本功能**不修复任何页面**，只是把第二种已经发生的缺失说出来；② **提示仍然只在文档列表可见**，对话里问到缺失内容仍然只会得到"检索不到"；③ 两列的重复是刻意接受的代价，**增生判据**（research R1）必须复述进报告，否则下一个人会直接加第三列
- [ ] T037 ⚠️ **不擅自 commit/push**——验收跑完向所有者汇报

---

## Dependencies

```
Phase 1 → Phase 2 → Phase 3 (US1, MVP) → Phase 4 (US2) → Phase 5 (US3) → Phase 6
```

- **T005（夹具）阻塞几乎所有测试任务**，必须最先做。
- **T009 是独立任务不是 T008 的一部分**：前移写入和停止旧写入是两个动作，只做前一个
  会让恢复流程清空刚写对的值，比现状更糟。
- US2 依赖 US1 的产出，但仍是 P1（理由见 Phase 4 的 ⚠️）。

## Implementation Strategy

**MVP = Phase 1+2+3**：解析失败的页终于能被用户看见，007 留下的那半个洞补上。
Phase 4 让它不至于和另一类混为一谈（否则这一期的设计成本白付），Phase 5 修 007 §6.2。


---

## 实施期的偏离与发现

- **T011 与 T005 提前到 Phase 1**（见 Phase 1 的偏离说明）。这是连续第三期撞同一个顺序问题
  ——「先写会失败的验收用例」与「字段在 Foundational 阶段加」这两条本身冲突。
  **下次写 tasks 时应直接把「用例引用的类型字段」划进 Phase 1**。
- **夹具必须验机制**：`writeTestPDFWithBrokenPages` 造出来之后，先用探针确认它触发的是
  `pageParsePanicked` 而不是 `pageUnresolvable`。两条分支只有前者是 008 要覆盖的那种缺失，
  不验的话整期验收会建立在错的前提上，而且全程没有任何迹象。
- **变异测试逃逸一项**：007 的非 PDF 用例只断言了旧的那一列，新列被误写照样绿。
  教训是**加一列时，既有的「这里应该是空」类断言必须一并扩展**，否则新列天生没有看守。
- **顺带修正了一段会误导人的注释**：007 在 `MarkDocumentReady` 上写的"把写入并进了这一条"
  在写入前移之后已经是错的，留着会让下一个人把赋值加回来。已改写成明确的禁令。
