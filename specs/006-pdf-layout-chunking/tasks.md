---

description: "Task list for 006-pdf-layout-chunking"
---

# Tasks: PDF 版面感知解析与跨页分块

**Input**: Design documents from `/specs/006-pdf-layout-chunking/`

**Prerequisites**: [spec.md](./spec.md)（US1-US5 / FR-001~022 / SC-001~008）、[plan.md](./plan.md)（含「三项拍板」）、
[research.md](./research.md)（R1~R7）、[data-model.md](./data-model.md)（实体与不变量）、
[contracts/retrieval-page-range.md](./contracts/retrieval-page-range.md)、[quickstart.md](./quickstart.md)（验收命令）

**Tests**: **需要**。本功能的两个 P1 用户故事全部靠启发式判定实现，SC-004（确定性）与 SC-005（误删率 0）
只能用纯函数单测证明；SC-001 的用例更被宪法第 VI 条要求**在改动前先跑出 FAIL**。因此测试任务不是可选项。

**Organization**: 按用户故事分阶段。Phase 2 是全部故事的共同前置（迁移 + 字段贯通），
Phase 3 起每个故事可独立验收。

## Format: `[ID] [P?] [Story] Description`

- **[P]**: 可并行（不同文件、无未完成依赖）
- **[Story]**: US1~US5，仅用户故事阶段的任务带
- 每条任务都带确切文件路径

## 本功能的三条硬规矩（每个任务都适用）

1. **每个任务改完跑 `make check-deps`**，非 0 退出即视为该任务未完成（宪法第 III 条）。
2. **`internal/db/pggen/` 是生成代码，一个字都不许手改**——只能改 `pgqueries/*.sql` 后跑 `make sqlc`。
3. **注释语言跟随所在文件**（宪法第 VII 条）：`parse.go`/`chunk.go`/新建 `layout.go` 一律**英文**；
   `model.go` 的 `RetrieveFilter` 中文段落跟随中文；新增错误的 `Message` **必须中文**；
   `fmt.Errorf` 包装链英文小写无句点。

## 三项已拍板的裁决（plan.md「三项拍板」，实现阶段不得再摇摆）

| 决策 | 结论 | 对任务的影响 |
|---|---|---|
| FR-018（部分页无文本层告知用户） | **推迟到下一期**，本期只做 FR-017 | **不生成任何 FR-018 任务**；部分扫描件只落结构化日志 + 一条断言锁定"有文本的部分正常入库"。验收报告必须写明它**未满足**，不得包装 |
| `chunks_page_range_valid` CHECK 约束 | **加** | T009 是它的独立连带任务（约 10 处测试构造点补 `PageEnd`），只补字段不改其他 |
| `writeTestPDF` 扩展 | **最小扩展，且是前置任务** | T004 独占，排在所有夹具任务之前 |
| US4（标题识别，P3） | **保留，排最后，标注可裁** | Phase 7 整体可丢弃；丢弃时报告写明"US4 已裁"而不是留白 |

---

## Phase 1: Setup（改动前基线 + 夹具地基）

**Purpose**: 把"改动前是什么样"钉死成可归档的文件。宪法第 VI 条：一个改动前就能通过的验收标准证明不了任何事。

⚠️ **T001~T007 必须在写任何实现代码之前完成**。T005 看到 FAIL 是 Phase 3 能开工的前提。

- [x] T001 执行 `make app-down` 再 `make db-up`，确认 PostgreSQL(5433) 与 MySQL(3307) 可连；`go version` 必须报 1.26.5（低于此说明走了 brew 那份过期 formula，不是环境正常）。⚠️ `app-down` 不能省——容器里的 asynq reconcile 会在测试跑的过程中改文档状态、重发布 chunk，表现为集成测试间歇性失败且失败原因看起来毫无道理
- [x] T002 [P] 归档确定性检索门禁基线：`make eval-retrieval-gate && cp eval/runs/phase6-retrieval-gate-latest.json /tmp/gate-baseline-006.json`（SC-006 的比对基准）
- [x] T003 [P] 记录改动前 `go test ./... -race -count=1` 全绿的输出到 `/tmp/tests-before-006.txt`，作为"连带改动没有打破既有测试"的比对基准
- [x] T004 扩展 `internal/knowledge/structure_test.go` 的 `writeTestPDF`：签名从 `pages []string` 改为 `pages [][]testLine`，新增 `testLine{Text string; FontSize float64}`，每行一个 `Td`（Y 按行序递减）+ 可选 `Tf`（逐行字号），仍只支持纯 ASCII（`(`/`)`/`\` 不做 PDF 字符串转义，也没必要）；同步改掉所有既有调用点，**行为等价**（单行页的产出与改动前一致）。⚠️ 这是 F1~F8 全部夹具的共同前提，散进各测试任务会立刻互相冲突
- [x] T005 [US1] 在 `internal/knowledge/chunk_test.go` 新增 `TestChunkPDFCrossPageParagraphStaysWhole`（夹具 F1：≥5 页，第 3 页末行不以句末标点结尾、接近满行宽、不是列表/标题，第 4 页首行是它的续写），断言该段落落在**同一个** `chunkPiece` 里且 `PageNumber=3 / PageEnd=4`；**只写用例、不写实现**，跑 `go test ./internal/knowledge/ -race -count=1 -run 'TestChunkPDFCrossPageParagraphStaysWhole' -v 2>&1 | tee /tmp/sc001-before.txt`，⭐ **必须看到 FAIL 并归档**。若它改动前就绿，说明用例根本没断言到跨页这件事——**重写用例**，不是往下走
- [x] T006 [P] 在 `internal/knowledge/chunk_test.go` 新增 `BenchmarkPDFExtractAndChunk`（只覆盖**抽取 + 分块**，不含 embedding 与落库），跑 `go test ./internal/knowledge/ -run '^$' -bench 'BenchmarkPDFExtractAndChunk' -benchtime 20x -count 5 2>&1 | tee /tmp/perf-before.txt`（SC-008 基线；`-count 5` 是为了看方差，单次数字不可比）
- [x] T007 在 `specs/006-pdf-layout-chunking/` 下确认 T002/T005/T006 三份产物都在，缺任何一份不得进入 Phase 2

**Checkpoint**: 三份基线归档完毕，SC-001 的 FAIL 输出在手。

---

## Phase 2: Foundational（阻塞全部用户故事）

**Purpose**: `page_end` 这一列从数据库一路贯通到 DTO。US1/US2/US3 全都依赖它，必须先完成。

⚠️ 顺序由宪法第 IV 条锁定：`migration → sqlc → model.go → errors.go → repository.go → dto.go`，不得跳序。

- [x] T008 新建 `internal/db/pgmigrations/000005_chunk_page_end.up.sql` 与 `000005_chunk_page_end.down.sql`。up 三步**顺序不可调换**：`ALTER TABLE chunks ADD COLUMN page_end integer NULL;` → `UPDATE chunks SET page_end = page_number;`（**不加 `WHERE page_number IS NOT NULL`**——NULL 行回填成 NULL 正好满足不变量 C1）→ `ALTER TABLE chunks ADD CONSTRAINT chunks_page_range_valid CHECK ((page_number IS NULL) = (page_end IS NULL) AND (page_end IS NULL OR (page_number >= 1 AND page_number <= page_end)));`。⭐ **两个条件必须写在同一个 CHECK 里用 `AND` 连**——拆成两个独立约束时 `page_number IS NULL / page_end = 4` 这一格会从第二个约束里逃逸（SQL 里 `NULL` 视为通过）。down 为 `DROP CONSTRAINT IF EXISTS` + `DROP COLUMN`，注释里**必须写明它是不可逆的信息损失**（回滚后跨页片段只剩起始页）。up 注释里沿用 `000003` 的禁令：**禁止 `COALESCE(page_number, 0)` 一类给 NULL 页码兜底的写法**
- [x] T009 跑 `make migrate-up`，然后按 quickstart §3.2 真的验两次：(a) `SELECT count(*) FROM chunks WHERE (page_number IS NULL) <> (page_end IS NULL);` MUST 返回 **0**；(b) 手写一条 `page_number=3, page_end=NULL` 的 INSERT，MUST 被 `chunks_page_range_valid` **拒绝**。⭐ 一个从未被触发过的约束和不存在的约束在证据上是等价的，(b) 必须真的跑
- [x] T010 改 `internal/db/pgqueries/chunks.sql`：`CreateChunk` 增列 `page_end`；`SearchVectorChunks`（约 94-95 行）与 `SearchKeywordChunks`（约 143-144 行）**两处**页码谓词改为区间相交 —— `AND (sqlc.narg(filter_page_min)::int IS NULL OR page_end >= sqlc.narg(filter_page_min)::int)` 与 `AND (sqlc.narg(filter_page_max)::int IS NULL OR page_number <= sqlc.narg(filter_page_max)::int)`。⚠️ **下界作用在 `page_end` 上、上界作用在 `page_number` 上，两行是交叉的**，写反会得到一个恒不成立的条件——这是全功能最容易写错的一处。另外给 `FindPublishedNeighborChunksBatch` 的 `SELECT` 列表**加上 `page_end`**（它故意不加页码谓词，那条豁免不动，但不 SELECT 出来邻接块的 `PageEnd` 会恒为 nil）。**关键词那一处漏改 = 关键词路把范围外的片段重新带回候选池**
- [x] T011 跑 `make sqlc`，用 `git diff --stat internal/db/pggen` 确认改动只涉及 `Chunk` 模型新增 `PageEnd sql.NullInt32`、`CreateChunk` 参数新增一项、两条召回查询的谓词、邻接查询的列。**生成代码不得手改**
- [x] T012 改 `internal/knowledge/model.go`：`Chunk` 结构体紧邻 `PageNumber *int` 新增 `PageEnd *int`（约 287 行），文档注释写明不变量 C1/C2/C3 与"`page_number` 语义收紧为起始页"；同文件 `RetrieveFilter`（约 150 行）的中文注释段落把页码匹配规则从"点落区间"改写为"**区间相交**"，字段本身**一个都不改**
- [x] T013 [P] 改 `internal/knowledge/errors.go`：新增 `ErrPDFNoTextLayer`，code `knowledge.pdf_no_text_layer`，分类 `apperr.InvalidInput`（与 `ErrEmptyContent` 同类——是"这份文件本身不适合"，不是基础设施故障，重试不会有不同结果），Message 固定为中文「该 PDF 没有文本层（疑似扫描件或图片型 PDF），暂不支持自动识别，请先用 OCR 工具转换为可选中文字的 PDF 后重新上传」。⚠️ **Message 不得携带动态内容**（沿用 `errors.go` 既有约定：计数、路径一类动态细节进日志）；`ErrEmptyContent` 的文案**一个字都不改**，只是适用范围收缩为"真正的空文件"
- [x] T014 改 `internal/knowledge/repository.go`：`createChunks` 用既有 helper `intPtrToNullInt32` 写入 `page_end`；**四处行映射**（向量召回 ~501、关键词召回 ~552、邻接查询 ~604、按文档列分片 ~686）各用 `nullInt32ToIntPtr` 读出 `PageEnd`。⚠️ **一处都不能漏**——漏掉任一处的后果是那条路径返回的 chunk 的 `PageEnd` 恒为 nil，违反不变量 C1
- [x] T015 [P] 改 `internal/knowledge/dto.go`：`chunkResult`（约 106 行）在 `PageNumber *int` 旁新增 `PageEnd *int \`json:"page_end"\``。⚠️ **必须是指针**——无页码时序列化成 JSON `null` 而不是 `0`，`0` 是一个编造出来的页码（契约 R6）
- [x] T016 补齐 CHECK 约束的连带改动：全仓搜 `PageNumber: &` / `PageNumber:` 的测试数据构造点（`admission_test.go`、`hybrid_test.go`、`integration_test.go`、`neighbor*_test.go` 等约 10 处），逐处补上 `PageEnd`。⚠️ **只补这一个字段，不得顺手修改这些测试的任何其他部分**（宪法第 IX 条）
- [x] T017 跑 `go test ./... -race -count=1` 与 `/tmp/tests-before-006.txt` 比对：此时**行为尚未改变**，除 T005 的 SC-001 用例仍 FAIL 外应全绿。任何其他新失败都说明 T008~T016 里有一处写错了

**Checkpoint**: `page_end` 从表到 DTO 全线贯通、约束在拦、生成代码干净。用户故事可以开工。

---

## Phase 3: User Story 1 - 跨页内容能被检索到 (P1) 🎯 MVP

**Goal**: 横跨页边界的段落落在**同一个**片段里，并如实标注为页码区间。

**Independent Test**: `TestChunkPDFCrossPageParagraphStaysWhole`（T005 已写，此刻仍 FAIL）转绿；
外加真实 PG 的集成测试：F1 入库 → 就该段落检索 → 断言存在一个包含**完整**段落的片段。

- [x] T018 [US1] 改 `internal/knowledge/parse.go`：新增 `pdfLine{Text string; Page int; FontSize float64; Y float64; Width float64}`，`extractPDFPages` 改为产出 `[]pdfLine`——`FontSize` 取该行各字形 `Text.FontSize` 的**众数**（取不到为 `0`），`Y` 取该行首字形的 `Text.Y`，`Width` = `max(X+W) - min(X)`。"拍平成 string"降级为下游的一步而不是抽取的输出。保持既有的 X 间隙补空格逻辑与 `sort.SliceStable`（Y 降序 → X 升序）不动，并补一个最终 tie-break 保证同 Y 同 X 时顺序稳定。⚠️ **`FontSize == 0` 表示"未知"，不是"字号很小"**——一切依赖字号的判据在它为 0 时一律返回"不成立"，绝不猜测
- [x] T019 [US1] 新建 `internal/knowledge/layout.go`（英文注释），先落**阈值常量**与 `buildParagraphStream`/`shouldMergeAcrossPage`：`paragraphUnit{Content string; PageStart int; PageEnd int}`；合并判据三条**同时**成立才合并 —— (1) 上页末行不以句末标点结尾（`。．！？；：.!?;:` 及全半角变体）；(2) 上页末行不是列表项或标题（不匹配 `^\s*\d+[.、)]`、`^\s*[-*·]`、`^第.{1,3}[章节条]`，且 `FontSize` 未显著大于正文众数）；(3) 上页末行接近满行宽（`Width >= 该页正文行宽众数 × 阈值比例`）。取舍方向**倾向合并**（错误切断无法在下游任何环节补救，错误合并还有准入与重排兜底）。⭐ 全部是纯函数：不碰 DB、不碰 `rsc.io/pdf`，输入 `[]pdfLine`，输出值类型切片
- [x] T020 [US1] 在 `layout.go` 实现**行宽众数**的确定性统计：(a) 分桶粒度固定为包内常量（按 1pt 取整为桶键）；(b) 统计后把桶键**排序后**再遍历取最大计数；(c) 并列时取**桶键较大者**。⚠️ 直接 `range` 一个 map 取最大值会让同一份 PDF 两次处理产出不同结果，**直接违反 SC-004**，而且这种 bug 在单测里可能几十次才复现一次——这是本功能最危险的确定性坑
- [x] T021 [US1] 改 `internal/knowledge/chunk.go`：`chunkPiece` 新增 `PageEnd *int`（`PageNumber` 语义收紧为**起始页**），文档注释更新；`chunkPDFPages` 由"逐页独立分块"改为消费 `[]paragraphUnit`（相应重命名为 `chunkPDFStream` 或保留原名并改签名），`pendingOverlap` 因此天然跨页携带。⭐ **overlap 种子的页码计入**：`PageNumber` 取"包括 overlap 种子在内、片段中实际出现的最小页码"——把区间做宽不是编造，做窄反而会让用户拿着"第 4 页"去找一句印在第 3 页的话。⚠️ `prependOverlap` **不改**，FR-004（合并后仍服从长度上限）因此是免费得到的：单个合并段落超限时照旧回退 `chunkText` 定长切分
- [x] T022 [US1] 确认 `chunkDocument` 的 **txt / md 两条分支一行不改**，并在 `chunk_test.go` 补断言锁定：这两条分支产出的 `chunkPiece` 的 `PageNumber` 与 `PageEnd` **恒为 nil**（不变量 C4 / FR-014 / FR-020）
- [x] T023 [P] [US1] 新建 `internal/knowledge/layout_test.go`，用手写 `[]pdfLine` 字面量覆盖合并判据：三条同时成立 → 合并；每条**各失效一次** → 不合并；页内自然结束的段落（以句末标点收尾）→ 不粘连；不变量 P1~P5（区间单调、`PageStart <= PageEnd`、合并只发生在相邻页边界）
- [x] T024 [P] [US1] 在 `layout_test.go` 新增 **SC-004 确定性用例**：对**同一个** `[]pdfLine` 输入连续调用判定函数 **20 次以上**断言结果完全相同，且**专门覆盖行宽众数存在并列的输入**（两个桶计数相同）——那正是 tie-break 唯一会被触发的地方，也是"排序后迭代"这条规则唯一真正起作用的场景
- [x] T025 [US1] 在 `chunk_test.go` 补：夹具 F6（≥4 页，每行「1. 某项」，**全篇无句号**，跨页连续）**不得**被合并成一个超长段落，`chunk_size` 硬上限仍生效（判据第 2 条的列表项规则挡住它）；夹具 F3（单页）产出与改动前一致
- [x] T026 [US1] 跑 `go test ./internal/knowledge/ -race -count=1 -run 'TestChunkPDFCrossPageParagraphStaysWhole' -v`，⭐ **它现在必须 PASS**；把它与 `/tmp/sc001-before.txt` 的 FAIL 一起留作阶段报告的硬证据
- [x] T027 [US1] 在 `internal/knowledge/integration_test.go` 新增真实 PG 的端到端用例：F1 入库 → 就跨页段落检索 → 断言召回的片段包含**完整**段落且 `PageNumber=3 / PageEnd=4`。**禁止 skip**（宪法第 VI 条）
- [x] T028 [US1] 在 `integration_test.go` 新增 **SC-004 全链路用例**：同一份 F1 连跑两次 `chunkDocument`，断言片段序列与页码归属**逐字节相同**

**Checkpoint**: US1 独立可验收。此时已经是可交付的 MVP——跨页召回这个最高频失效方式被修好了。

---

## Phase 4: User Story 2 - 页眉页脚不进入正文 (P1)

**Goal**: 跨页重复的页眉页脚与纯页码行不出现在任何片段里，且**一行正文都不误删**。

**Independent Test**: F2 入库后遍历全部产出片段，断言页眉文字与页脚文字出现次数**都是 0**（不是"很少"）。

⚠️ 与 US1 相互独立——即使不做跨页合并，单独剥离噪音也能带来可度量的收益。

- [x] T029 [US2] 在 `layout.go` 实现 `stripLayoutNoise`（纯函数）：一行判为噪音需**同时**满足 (1) 位置在页面顶部/底部 Y 带内；(2) 归一化（抹掉数字）后的文本在文档中出现的**页数占比 ≥ 阈值**；(3) 长度显著短于该页正文行宽。外加一条**独立规则**：纯页码行（整行只有数字，或匹配「第 X 页 / 共 Y 页」「X / Y」「Page X of Y」一类模式）直接判为噪音，不要求满足上述三条。方向**宁可漏剥**（SC-005 要求正文误删率为 0）
- [x] T030 [US2] 在 `stripLayoutNoise` 里实现 FR-009 的**最小页数门槛**：文档页数低于门槛时**跳过基于重复率的判定**，不做剥离（两三页统计不出可信的重复率）。纯页码行那条独立规则是否同样受门槛约束，在代码注释里写死并被单测锁定，不要留给读者猜
- [x] T031 [US2] 跨页重复率统计的**确定性**：与 T020 同一个坑——统计用的 map 必须把 key **排序后**再迭代，绝不允许直接 `range` 决定判定结果或输出顺序。复杂度必须是 **O(总行数)** 而不是 O(页数²)
- [x] T032 [US2] 给 `layout.go` 每个阈值常量补文档注释，**写明取值理由和它在防什么**（与 `admission.go`、`toolloop.go` 既有风格一致）。⚠️ **全部是包内常量，不做运行时可配置**——它们是启发式的实现细节，暴露成配置只会把调参责任推给不可能知道怎么调的用户
- [x] T033 [US2] 改 `internal/knowledge/service.go`：`ProcessDocument` 在抽取后、分块前调用噪音剥离，并落**结构化审计日志**（英文，字段见 data-model §8）：`document_id` / `page` / `reason`（枚举 `repeated_header` / `repeated_footer` / `page_number_line`）/ `line_length` / `repeat_page_ratio` / `text`（**归一化后**、截断到包内常量上限）/ `stripped_total`（每份文档一条汇总）。⚠️ **三条缓解措施都必须落地**：只记归一化文本、截断、每文档逐行记录条数设上限（超出只累加 `stripped_total`），防止一份千页文档刷爆日志
- [x] T034 [P] [US2] 在 `layout_test.go` 新增 **SC-005 的六行穷举用例**（纯函数，穷举判据的每个维度各失效一次）：① 位置在页顶但只出现在单页 → **不剥离**；② 跨页高度重复但位于页面中部 → **不剥离**；③ 位置在页顶且跨页重复但长度接近正文行宽 → **不剥离**；④ 三条同时成立 → 剥离；⑤ 整行只有数字 / 匹配「第 X 页」「Page X of Y」→ 剥离；⑥ 文档页数低于门槛 → **不剥离**（无论前三条是否成立）。前三行是 SC-005 的真正内容：**任何单条判据成立都不足以剥离**（FR-007）
- [x] T035 [P] [US2] 在 `layout_test.go` 补重复率统计存在**并列**时的确定性用例（连跑 20 次以上结果一致）
- [x] T036 [US2] 在 `structure_test.go` / `chunk_test.go` 新增夹具 F2（≥5 页，**必须高于最小页数门槛**；每页顶部同一章节名、底部「第 X 页 / 共 Y 页」，正文各页不同），断言 **SC-002**：遍历全部产出片段，页眉文字与页脚文字出现次数**都是 0**
- [x] T037 [US2] 新增夹具 F7（**仅两页**，两页都带相同页眉），断言页眉**不**被剥离（FR-009：宁可漏剥，不可误删）
- [x] T038 [US2] 在 `integration_test.go` 或 `structure_test.go` 断言审计日志确实被写出且含剥离原因（FR-008：不记文本就无法核查误删，这是有意的、已在 plan.md「已知范围边界」第 2 条登记的口径偏离）

**Checkpoint**: US1 + US2 完成，两个 P1 全部可独立验收。

---

## Phase 5: User Story 3 - 引用信息保持诚实 (P2)

**Goal**: 页码要么准确要么明确缺失，绝不出现编造的页码；跨页片段展示为区间。

**Independent Test**: 遍历所有产出片段断言 `1 <= PageNumber <= PageEnd <= 文档总页数`；
前端单页显示「第 N 页」、跨页显示「第 N-M 页」、无页码显示「—」。

- [x] T039 [US3] 在 `layout.go` / `chunk.go` 产出处断言不变量 **C3 的上界**（`PageEnd <= 文档总页数`）—— ⚠️ 这一条**数据库检查不到**（DB 不知道文档有几页），只能由纯函数在产出时保证并被单测锁定
- [x] T040 [P] [US3] 扩展 `internal/knowledge/filter_test.go` 的 `TestFilterPageRangeExcludesNullPageChunks`，给 `page_end` 侧补上对称用例，且 **单侧各试一次**：只给下界 / 只给上界 / 闭区间。⚠️ 002 的经验是只测闭区间时单侧的错误会被另一侧未改动的谓词掩盖掉，用例照样通过
- [x] T041 [US3] 在 `integration_test.go` 新增跨页片段的**区间相交**用例，按 contracts §3.3 的表**逐行**断言（`min=4,max=4` / `min=4,max=9` / 只给 `min=4` 三格 ⭐ 从"不命中"变"命中"；其余六格前后一致）。⚠️ 不存在任何一格是"原本命中的现在不命中"——若测出这样一格，说明 T010 的谓词写反了
- [x] T042 [US3] 在 `neighbor_test.go` 或 `neighbor_batch_test.go` 补断言（N1）：邻接块的 `PageEnd` 是**它自己**的值，不是 anchor 的、也不是 nil。`neighbor.go` 预期**零代码改动**——但"预期零改动"不是证据，必须有断言锁定
- [x] T043 [P] [US3] 改 `web/src/lib/knowledge.ts`（约 160 行）：`RetrievedChunkResult` 新增 `page_end: number | null`，注释写明它与 `page_number` 同为 null 或同有值
- [x] T044 [US3] 改 `web/src/routes/knowledge-retrieval-dialog.tsx`（约 77 行）：页码展示改为三分支——相等「第 N 页」/ 不等「第 N-M 页」/ 均为 null「**—**」（**不带"第…页"外框**，顺带修掉现状 `第 {c.page_number ?? "—"} 页` 渲染出「第 — 页」的瑕疵）。⚠️ **三件不得做的事**：不得用 `page_end ?? page_number` 或 `?? 0` 一类兜底（那会把一个本该被发现的后端 bug 变成看起来正常的界面）；不得在 `page_end` 缺失时假装它等于 `page_number`；不得把区间只渲染成起始页（FR-011 的禁令在展示层同样成立）

**Checkpoint**: 引用诚实性端到端闭环（DB 约束 → Go 断言 → SQL 过滤 → DTO → 界面）。

---

## Phase 6: User Story 5 - 扫描件给出可行动的提示 (P2)

**Goal**: 纯扫描件的提示明确指向 OCR，与"空文件"可区分。

**Independent Test**: 上传纯图片 PDF，断言错误码是 `knowledge.pdf_no_text_layer` 而**不是** `knowledge.empty_content`，两条文案确实不同。

⚠️ **本期不实现 OCR 或视觉检索**（FR-019）。FR-018（部分扫描件告知用户）已按「三项拍板」决策 1 **整条推迟**。

- [x] T045 [US5] 在 `layout.go` 新增纯函数：按 R6 统计"有可提取文本的页数 / 总页数"，返回三档（0 / (0,1) / 1）
- [x] T046 [US5] 改 `service.go` 的 `ProcessDocument`：比值为 **0** 且文件类型是 PDF → 经 `failDocument` 返回 `ErrPDFNoTextLayer`；比值介于 0 与 1 之间 → **正常处理有文本的部分**（不整体失败）+ 一条结构化日志（含缺文本的页码列表，英文，动态细节只进日志不进 `Message`）；比值为 1 → 无提示。⚠️ `len(pieces)==0` 的 `ErrEmptyContent` 分支保留不动——它的适用范围由此收缩为"真正的空文件"，语义反而变准确了
- [x] T047 [P] [US5] 新增夹具 F4（所有页都无可提取文本，现有 helper 传空串即可造，`structure_test.go:738` 已有先例），断言 **SC-007**：错误码为 `knowledge.pdf_no_text_layer`、Message 中文且与 `ErrEmptyContent` 的文案**确实不同**；再入库一个真正的空文件，断言它**仍走** `knowledge.empty_content`
- [x] T048 [P] [US5] 新增夹具 F5（5 页里 2 页无文本、3 页有文本），断言有文本的 3 页**正常入库**、页码归属仍指向**真实页号**（跳过的页不导致页码错位）。⚠️ **不断言任何"告知用户"的行为**——FR-018 已推迟，这里只锁定"不整体失败 + 页码不错位"

**Checkpoint**: FR-017 完成。FR-018 明确未做，报告里如实写。

---

## Phase 7: User Story 4 - 章节标题作为上下文保留 (P3，⚠️ 允许整体裁掉)

**Goal**: PDF 标题层级被识别并**拼进片段正文**参与向量化。

**Independent Test**: 断言标题下方片段的**正文内容本身**（而非仅元数据字段）包含标题路径。

⚠️ **本阶段可以整体丢弃**。R5 已论证：现状下 PDF 从不产出 `section_title`，裁掉不破坏任何既有行为。
裁掉时**必须在阶段报告里写明"US4 已裁"**，不是留白。

- [x] T049 [US4] 在 `layout.go` 实现标题判定（纯函数）：**双信号交叉验证**，两个信号都命中才判为标题 —— (1) 字号显著大于全文正文字号众数；(2) 匹配常见标题编号模式（`1.` / `1.1` / `第三章` / 全大写短行）。⚠️ `FontSize == 0` 时字号信号一律**不成立**，不猜（FR-016：识别不出就留空，不编造）
- [x] T050 [US4] 在 `chunk.go` 的 PDF 分支按既有 `chunkMarkdown` 的标题栈逻辑，把标题路径**拼接进片段正文**随正文一起参与向量化。⚠️ **仅写入 `SectionTitle` 元数据字段不满足 FR-015**——向量化只作用于片段正文。长度冲突时按 FR-015a **优先保全正文**，由标题路径缩短或丢弃（与既有 Markdown 路径同一规则）
- [x] T051 [P] [US4] 新增夹具 F8（≥3 页，「1. 」「1.1 」编号 + 字号显著大于正文的标题行），断言标题下方片段的**正文**含标题路径；无可辨识标题结构时 `SectionTitle` **留空而非编造**

---

## Phase 8: Polish & 验收（跨切面）

**Purpose**: 把"跑过并看到输出"变成归档证据（宪法第 VI 条）。

- [x] T052 `make check-deps` MUST 输出 `OK - no cross-layer or same-layer violations`；`go vet ./...` MUST 无输出
- [x] T053 `go test ./... -race -count=1` 全绿，**无 skip**（数据库测试禁止 skip = 未验证）
- [x] T054 ⭐ `make eval-retrieval-gate` 后跑 `python3 scripts/compare-retrieval-gate.py /tmp/gate-baseline-006.json eval/runs/phase6-retrieval-gate-latest.json`，MUST 输出 **`IDENTICAL`**（SC-006 的唯一证据）。⚠️ **本功能不给门禁新增用例**——它的新行为全部在 PDF 路径上而门禁语料是 txt/md，所以这次比对用**最严格形态：cases 数组长度都不许变**。若它变了，说明有人在不相关的地方动了门禁，那本身就是要查的事
- [x] T055 ⚠️ 全程**未使用** `make eval` 作为"行为未变"的证据——它每条用例都调真实对话与裁判模型，同一份代码跑两次都不会一致，用一个自己都不可复现的东西证明"行为未变"得到的不是证据而是噪音
- [x] T056 跑 T006 的 benchmark 出 `/tmp/perf-after.txt` 并与 `/tmp/perf-before.txt` 比对（有 `benchstat` 用它，没有就人工比中位数）：抽取 + 分块阶段增幅 **≤ 50%**，且**不随页数恶化**。⚠️ 若增幅随页数增长而恶化，那不是"稍微慢了点"，是噪音识别写成了 O(页数²)，要回头改实现而不是调阈值
- [x] T057 **变异测试**（确认断言真的有牙齿，跑完务必还原）：逐项注入下列缺陷并确认**对应用例确实失败** —— 合并判定恒 `false` → SC-001 用例；合并判定恒 `true` → F6 + 长度上限用例；噪音判定恒 `true` → SC-005 全部单测；噪音三条判据从「与」改「或」→ SC-005 前三行；行宽众数改回直接 `range` map → SC-004 用例（⚠️ 若跑 20 次仍不失败，说明次数不够，加到 200 次）；过滤 SQL 下界谓词写回 `page_number >= min` → 区间相交用例的三个 ⭐ 格；页码谓词加 `OR page_end IS NULL` → NULL 页码排除用例（**两侧各试一次**，002 实际踩过"只测闭区间掩盖单侧错误"这个盲区）；四处行映射漏掉一处 `page_end` → 对应路径的断言（尤其邻接块那处）
- [x] T058 跑 `/smoke-test`（本功能不新增端点，冒烟的作用是确认迁移、装配与既有对话链路没被破坏）
- [x] T059 ⚠️ **手动验证一份真实的多页 PDF**（不是 helper 造的）：上传 → 在试检索界面看页码是否显示成「第 N-M 页」。helper 造的 PDF 永远只覆盖 helper 作者想到的情形；真实 PDF 的字形定位、字号、行距分布跟手搓的完全不是一回事——`extractPDFPages` 的换行阈值（`lastY - t.Y > 2`）是个魔数，行切分不可靠会**直接传导到噪音判定上**。这是唯一能碰到这类问题的一步
- [x] T060 产出 `docs/eval-phase13-pdf-layout-chunking-report.md`（沿用既有 `docs/eval-phase*` 体例），**必须如实包含五件事**：① **能力上限**——本方案的标题识别质量与复杂排版适应性**明显低于**业界基于视觉版面检测模型的方案（Docling / MinerU 一类），这是知情取舍，**不是"做到了同等效果"**；② 哪些结论是**机制证明**（代码路径 + 断言，本功能绝大部分属此）、哪些是**真实效果度量**（只有 SC-008 的 benchmark，且口径是抽取 + 分块而非端到端），两者不得混淆；③ `extractPDFPages` 的换行阈值仍是**未修复的魔数**，行切分不可靠会传导到噪音判定——这是本次**没有解决**的已知缺陷，不能因为周边变好了就不提；④ **FR-018 未满足**（部分扫描件不告知用户，只落日志），不得写成"已满足"；⑤ overlap 种子计入页码导致区间**有意偏宽**（本可标「第 4 页」的片段标成「第 3-4 页」），这是决策不是 bug；US4 若被裁则明写"US4 已裁"
- [x] T061 ⚠️ **不擅自 commit/push**（宪法第 VIII 条）——全部验收项跑完后向所有者汇报，由所有者决定提交时机

---

## Dependencies & Execution Order

### 阶段依赖

```
Phase 1 (Setup / 基线)      ← 必须最先，T005 的 FAIL 是 Phase 3 开工的前提
        ↓
Phase 2 (Foundational)      ← 阻塞全部用户故事；内部顺序由宪法第 IV 条锁死
        ↓
   ┌────┴────┬──────────┬──────────┐
Phase 3    Phase 4    Phase 6    (Phase 5 依赖 3+4 的产出)
 US1(P1)    US2(P1)    US5(P2)
   └────┬────┴──────────┘
        ↓
Phase 5 (US3, P2)  ← 展示与过滤的验收依附于 US1/US2 产出的跨页片段
        ↓
Phase 7 (US4, P3, 可裁)
        ↓
Phase 8 (Polish / 验收)
```

### 故事间依赖

- **US1 与 US2 相互独立**：即使只做其中一个，也各自能带来可度量的收益。两者都只依赖 Phase 2。
- **US3 依附 US1**：它验收的是 US1 产出的跨页区间，无法脱离 US1 单独交付价值（spec 已定为 P2）。
- **US5 完全独立**：只依赖 Phase 2 的 `errors.go`，可以与 US1/US2 并行做。
- **US4 依赖 US1**（复用 `layout.go` 的字号众数与行结构），且**允许整体裁掉**。

### 可并行的任务组

- Phase 1：T002 / T003 / T006 三条互不相干（T006 需要 T004 先落地）
- Phase 2：T013（errors.go）与 T015（dto.go）可与 T012/T014 并行；T010→T011 必须串行
- Phase 3：T023 / T024 两个纯函数测试文件内的用例可并行编写
- Phase 4：T034 / T035 可并行
- Phase 5：T040 / T043 可并行（一个 Go 测试、一个前端类型）
- Phase 6：T047 / T048 可并行
- **跨故事并行**：Phase 2 完成后，US5（Phase 6）可与 US1/US2 同时推进——它们没有共享文件之外的依赖

---

## Implementation Strategy

### MVP = Phase 1 + Phase 2 + Phase 3（US1）

跨页召回是当前实现**最直接、最高频**的失效方式，也是本次改动唯一无法用其它手段绕开的问题
（邻接窗口补不回被截断的句子）。US1 单独交付就已经修好了它。

### 增量交付顺序

1. **Phase 1+2+3** → 跨页内容能被检索到（SC-001 从 FAIL 转 PASS，这是硬证据）
2. **+ Phase 4** → 噪音不再污染向量、不再制造虚假相似（SC-002 归零）
3. **+ Phase 5** → 引用展示为区间，端到端闭环
4. **+ Phase 6** → 扫描件从"内容为空"变成可行动的提示（成本极低、收益明确）
5. **+ Phase 7** → 标题上下文（可裁；裁掉要在报告里写明）
6. **Phase 8** → 证据归档 + 阶段报告

### 每个任务完成的判据

改完代码 → `make check-deps` OK → `go vet ./...` 无输出 → 相关测试绿。
三条任一不过，该任务**未完成**，不得往下走。


---

## 实施期间的范围外追加（所有者 2026-09-05 明确批准）

- [x] T062 兜住 `rsc.io/pdf` 的 panic：`parse.go` 新增 `safeNewPDFReader` / `safePageText`，
  逐页 recover，全部页面都解析失败时返回新错误 `ErrPDFUnreadable`（中文文案）。
  ⚠️ recover 刻意只包住 `rsc.io/pdf` 的两个调用，**不得扩大到本仓库自己的代码**——
  那里的 nil 解引用是真 bug，应当继续响亮地崩，而不是被洗成"这份 PDF 读不了"。
  **发现于 T059**（真实 PDF 验证），是既有缺陷而非 006 引入；不修的话 006 的头号能力
  在真实语料上根本碰不到。详见报告 §6.1。
- [x] T063 收紧纯页码行规则：只有数字的行需要额外佐证（该页最外侧那一行 + 数值落在文档真实页数内），
  无歧义形态（`第 3 页`/`Page 3 of 12`/`3 / 12`）不受此限。
  **发现于 T059**：一篇 15 页论文里 20 行上下标/脚注标记被当成页码删掉，是真实输入上的 SC-005 违反。
  详见报告 §6.2。
- [x] T064 补两条被变异测试逼出来的用例：
  `TestIntegrationPageFilterIntersectionAppliesToBothRecallPaths`（融合掩盖单路回归）与
  `TestIntegrationPageEndIsReadBackOnEveryRepositoryPath`（四处行映射三处无人看守）。详见报告 §5.2。
