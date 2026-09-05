# Implementation Plan: PDF 版面感知解析与跨页分块

**Branch**: `006-pdf-layout-chunking` | **Date**: 2026-09-04 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/006-pdf-layout-chunking/spec.md`

## Summary

把 PDF 入库路径上"**页 = 语义边界**"这个错误前提拆掉，换成"**段落 = 语义边界，页只是它的坐标**"。

具体是四件事：

1. **抽取阶段保留行级结构线索**——`extractPDFPages` 目前把每页拍平成一个 string，`rsc.io/pdf`
   给出的 `FontSize` 和 X/Y/W 全被丢弃。改为先产出**行级结构**（文本 + 页码 + 字号 + Y + 渲染宽度），
   拍平成 string 变成下游的一步而不是抽取的输出。
2. **剥离版面噪音**——跨页重复的页眉页脚、纯页码行，用「位置 ∧ 重复率 ∧ 长度」三条判据取交集识别，
   方向**宁可漏剥**（SC-005 要求正文误删率为 0）。
3. **跨页重建段落流**——页边界处用「上页末行不以句末标点结尾 ∧ 不是列表/标题 ∧ 接近满行宽」
   三条判据决定是否续接，方向**倾向合并**（错误切断无法在下游任何环节补救，错误合并还有准入与重排兜底）。
   段落流才是分块的输入，`pendingOverlap` 因此天然可以跨页携带。
4. **页码从单值变区间**——`chunks` 表新增 `page_end`，`page_number` 语义收紧为**起始页**；
   元数据页码过滤从"点落区间"改为"**区间相交**"。

外加一件成本极低的事：**扫描件从"内容为空"升级成可行动的专用提示**（FR-017），
但**本期不实现 OCR 或视觉检索**（FR-019）。

技术路线由 [research.md](./research.md) 定死，本文件不重新论证：
Go 进程内排版启发式、不引入外部解析器（R1）；`page_end` 新列 + 区间相交过滤（R2）；
三条合并判据（R3）、三条噪音判据 + 纯页码行独立规则（R4）、标题双信号交叉验证（R5）、
扫描件按"有文本页数 / 总页数"分三档（R6）、存量数据只回填不重建（R7）。

⚠️ **能力上限必须写进验收报告，不得包装**：本方案的标题识别质量与复杂排版适应性**明显低于**
业界基于视觉版面检测模型的方案（Docling / MinerU 一类）。这是一个知情取舍——为两个不需要模型的
P1 能力引入一整条 Python 运行时依赖、破坏单二进制约束，收益与代价不成比例。**不是"做到了同等效果"。**

## Technical Context

**Language/Version**: Go 1.26.5（`GOTOOLCHAIN` 管理，brew formula 版本落后不作数）
+ React 19 / Vite / TS（本次前端**有**改动，见下方 Project Structure）

**Primary Dependencies**: Gin、`rsc.io/pdf v0.1.1`（既有，本期**不换库、不升级**）、
sqlc（PG 查询代码生成）、pgvector、pg_trgm。**本期不新增任何依赖**——
这条本身就是 R1 决策的可验证形态：`go.mod` 的 diff 必须为空。

**Storage**: PostgreSQL + pgvector 存 `chunks`（**新增一列 `page_end`**，pgmigration `000005`）；
MySQL 8.x 存业务数据 `documents`（**本期零改动**——FR-018 已按「三项拍板」决策 1 推迟到下一期）；
Redis 无变化。

**Testing**: `go test ./... -race -count=1`、`go vet ./...`、`make check-deps`、
启发式判定的纯函数单测（不依赖 DB、不依赖 PDF 库）、真实 PostgreSQL 的入库/检索集成测试
（**禁止 skip**，宪法第 VI 条）、确定性检索门禁 `make eval-retrieval-gate`（必须与改动前**逐字节一致**）。

**Target Platform**: 单进程 Go 二进制（Linux/macOS），前端经 `web/embed.go` 的 `go:embed` 内嵌，
单实例部署。

**Project Type**: 模块化单体的 Web 服务（Go + Gin 后端 + React SPA），改动集中在文档**入库**阶段。

**Performance Goals**: SC-008——单份 PDF 的入库处理时间相比改动前**增加不超过 50%**。
⚠️ 口径必须说清：本功能只改动**抽取 + 分块**这一段，而端到端入库耗时的大头是 embedding 调用与落库；
按端到端算这条几乎必然满足，等于没约束。**因此度量口径固定为"抽取 + 分块阶段"**（纯 CPU、无网络、
可用 Go benchmark 直接测），这是更严格也是唯一有意义的口径。噪音识别需要跨页统计，
必须是 O(总行数) 而不是 O(页数²)。

**Constraints**:
非 PDF 路径（txt/md）行为**逐字节不变**（FR-020 / SC-006，由 `make eval-retrieval-gate` 证明）；
存量已入库文档保持可检索且页码有效（FR-022，由 R2 的等价性 + 一次回填保证）；
片段长度上限与单文档片段数上限**不放宽**（FR-004）；
相同 PDF 输入产出相同片段序列与页码归属（FR-021 / SC-004）；
正文误删率为 0（SC-005）；不得为无页码概念的格式编造页码（FR-014）。

**Scale/Scope**: 触及 **2 个模块**——`internal/knowledge`（第 2 层，本功能全部业务逻辑）
与 `internal/db`（第 0 层，1 个 pgmigration + 1 个查询文件 + sqlc 重新生成）；
前端 2 个文件（类型 + 展示）；**0 个新增跨模块依赖边**。
不触及 `agent`、`conversation`、`workflow`、`mcp`、`auth`、`user`、`provider`。

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| 原则 | 关卡 | 判定 | 理由 |
|---|---|---|---|
| **I** 如实标注 AI 归属 | 阶段报告与任何对外材料不得写成"亲手设计/实现" | **PASS** | 报告沿用既有 `docs/eval-phase*` 体例，只陈述技术事实与实测输出；能力上限一节明写"低于业界视觉版面方案" |
| **II** 规格先行 | spec → plan → tasks → 实现 | **PASS** | `spec.md` 已接受、`research.md` 已定 R1~R7，本文件即 plan；**实现代码在 `tasks.md` 产出前不得开写** |
| **III** 模块分层 | 是否新增跨模块依赖边 | **PASS** | 改动落在 `internal/knowledge`（第 2 层）与 `internal/db/pggen`（第 0 层）。knowledge→db/pggen 是既有合法方向，**零新增依赖边**。每个任务后跑 `make check-deps`，非 0 退出即视为未完成 |
| **IV** 模块内实现顺序固定 | 涉及数据结构变化时 migration → sqlc 先行 | **PASS** | 顺序为 `pgmigration(000005) → make sqlc → model.go → errors.go → parse.go/chunk.go/layout.go → repository.go → service.go → dto.go → 前端`。⭐ `parse.go`/`chunk.go`/`layout.go` 不是分层里的一层，它们是 knowledge 模块的**内部实现**（纯函数，不碰 DB、不碰其他模块）；约束是"在 `model.go` 之后（它们产出 model 声明的类型）、在 `service.go` 之前（service 是它们唯一的消费者）"。`handler.go`/`wire.go` 本期无改动（不新增端点） |
| **V** 确定性优先（复现与 tie-break） | 相同输入产出相同顺序，不依赖 map 迭代顺序 | **PASS** | FR-021/SC-004 是硬要求。行排序沿用 `sort.SliceStable` 的 Y 降序→X 升序并补最终 tie-break；**⚠️ 噪音识别的"跨页重复率统计"与"正文行宽众数"天然想用 map，必须在遍历前把 key 排序后再迭代，绝不允许直接 `range` 一个 map 决定判定结果或输出顺序**——这是本功能最容易踩的确定性坑 |
| **V** 确定性优先（纯函数可测） | 判定逻辑可抽成不依赖数据库的纯函数并被单测覆盖 | **PASS** | R7 已把它定成约束：**所有启发式判定（合并、噪音、纯页码行、标题、扫描件比值）必须是不依赖数据库、不依赖 `rsc.io/pdf` 的纯函数**，输入是 `[]pdfLine` 或更小的值类型，输出是判定结果。落点 `layout.go` + `layout_test.go`。这也是 SC-005（误删率 0）唯一能被便宜地反复验证的形态 |
| **V** 确定性优先（LLM 降级/超时/开关） | 引入 LLM 参与检索链路时必须同时定义失败降级、超时上限、关闭开关 | **N/A** | **本功能不引入任何模型调用，因此这半条没有作用对象。** 三处都被显式否决过：R1 否决外部解析器（含其视觉版面检测模型）、R3 否决"用语言模型判断段落是否连续"、FR-019 明确排除 OCR 与视觉检索（ColPali 一类）。`ProcessDocument` 里既有的 embedding 调用是**改动前就存在**的，本功能既不新增也不改变它的失败处理路径。⚠️ 判 N/A 的依据是"没有引入"，不是"引入了但不需要降级"——一旦后续任务里冒出任何模型调用，这条立刻从 N/A 变成必须满足 |
| **VI** 证据式验收 | 有真实命令输出才算完成 | **PASS** | 验收清单见 [quickstart.md](./quickstart.md)。⭐ **SC-001 的用例必须在改动前先跑一次并看到 FAIL**，且 FAIL 输出要归档——一个改动前就能通过的验收标准证明不了任何事。数据库测试**禁止 skip** |
| **VII** 按读者选择语言 | 用户文案中文、注释跟随所在文件 | **PASS** | `chunk.go`/`parse.go` 现有注释通篇**英文** → 这两个文件的新增注释与新建的 `layout.go` 一律英文；`model.go` 的 `RetrieveFilter` 段落是中文 → 那一段的增补跟随中文；新增的扫描件错误 `Message` **必须中文**；`internal/fmt.Errorf` 包装链保持英文小写无句点；`specs/` 下一切中文（本文件即是） |
| **VIII** 提交时机由所有者决定 | 不擅自 commit/push | **PASS** | 本阶段只落 `specs/` 下的设计文档，不写实现代码、不提交 |
| **IX** 最小范围 | 不夹带无关改动 | **PASS**（⚠️ 见下方「已知范围边界」） | 明确不做：**多栏排版阅读顺序重建、跨页表格合并、OCR、视觉检索（ColPali 一类页面级多向量嵌入）**。另外不做：换 PDF 库、升级依赖、重建存量文档的分块、改动检索/融合/准入/重排/去重/邻接的任何逻辑 |

### 已知范围边界（非违规，但必须明示）

1. **`page_end` 的 CHECK 约束会连带改到约 10 处测试数据构造**。
   data-model.md §6 建议在 `000005` 里加 `chunks_page_range_valid` 约束（`page_number` 与 `page_end`
   必须同为 NULL 或同有值）。代价是：现有测试里所有 `Chunk{PageNumber: &p}` 而不设 `PageEnd` 的
   构造点会被数据库**响亮拒绝**。这是**必要的连带改动**——但除补 `PageEnd` 外，
   **不得顺手修改这些测试的其他部分**。
   如果所有者认为这个代价不值，替代方案是不加约束、只在 Go 侧断言（data-model.md §6 有对比）。
2. **噪音审计日志会记录被剥离行的文本**，这与 002-metadata-filter 确立的"日志不记片段正文"口径
   方向相反。理由与边界见 data-model.md §8——简短说：**FR-008 要求"可事后核查误删"，
   不记文本就无法核查**；页眉页脚按定义是每页重复的版面元素而非用户正文，且宪法第 VII 条的
   日志禁令针对的是凭据类敏感信息（密码/token/API Key），本场景无此类数据。
   缓解：只记归一化后的文本、截断到上限、每份文档记录条数设上限。
3. **US4（标题识别，P3 / SHOULD）允许被整体裁掉**。R5 已论证：现状下 PDF 从不产出 `section_title`，
   所以裁掉它**不破坏任何既有行为**。裁与不裁在 `tasks.md` 阶段决定，不在本文件锁死。
4. ~~**FR-018（部分扫描件告知用户）目前没有承载字段**~~ → **已裁决：整条推迟到下一期**，
   见下方「三项拍板」决策 1。本期"部分页无文本层"只落结构化日志、用户界面看不到提示，
   这是一个**被明示的未满足项**，验收报告里不得写成"已满足 FR-018"。

## Project Structure

### Documentation (this feature)

```text
specs/006-pdf-layout-chunking/
├── spec.md              # 已接受（US1-US5、FR-001~022、SC-001~008）
├── plan.md              # 本文件
├── research.md          # Phase 0：R1~R7 决策与被否决方案
├── data-model.md        # Phase 1：实体、字段、不变量、SQL 改写等价性论证
├── contracts/
│   └── retrieval-page-range.md   # 既有响应体的形状变更 + 新错误码（本期不新增端点）
├── quickstart.md        # Phase 1：可执行验证指南
├── checklists/
│   └── requirements.md  # 规格质量检查（已完成）
└── tasks.md             # Phase 2 产出（/speckit-tasks）
```

`contracts/` 下只有一份文件且**不是 OpenAPI 风格的端点契约**——本功能不新增任何 HTTP 端点或请求参数，
但它**改变了既有响应体的形状与既有过滤入参的匹配规则**，那同样是契约变更，必须成文。

### Source Code (repository root)

```text
internal/
├── db/
│   ├── pgmigrations/
│   │   ├── 000005_chunk_page_end.up.sql     # 新增：ADD COLUMN page_end + 回填 + CHECK 约束
│   │   └── 000005_chunk_page_end.down.sql   # 新增：DROP CONSTRAINT + DROP COLUMN（⚠️ 不可逆丢失跨页信息）
│   ├── pgqueries/chunks.sql                 # 改：CreateChunk 增列；两处页码过滤谓词改为区间相交
│   └── pggen/                               # make sqlc 重新生成，**不得手改**
└── knowledge/
    ├── model.go            # 改：Chunk 增 PageEnd *int；RetrieveFilter 的页码语义注释改写为区间相交
    ├── errors.go           # 改：新增扫描件专用错误（Message 中文），与 ErrEmptyContent 区分
    ├── parse.go            # 改：extractPDFPages 产出行级结构（pdfLine），保留 FontSize / Y / 渲染宽度
    ├── layout.go           # 新建：噪音判定、纯页码行判定、跨页合并判定、标题判定、扫描件比值
    │                       #        —— 全部纯函数，不碰 DB、不碰 rsc.io/pdf（宪法第 V 条）
    ├── chunk.go            # 改：chunkPDFPages 由"逐页独立分块"改为"消费段落流"，产出带页码区间的 chunkPiece
    ├── repository.go       # 改：createChunks 写入 page_end；四处行映射读出 page_end
    ├── service.go          # 改：ProcessDocument 分派扫描件错误；噪音审计日志
    ├── dto.go              # 改：chunkResult 新增 page_end
    ├── neighbor.go         # 确认：邻接块的 page_end 与其他来源元数据一并继承（预期零代码改动，需断言锁定）
    ├── layout_test.go      # 新建：全部启发式判定的纯函数单测（含 SC-005 的误删断言）
    ├── structure_test.go   # 改：扩展 writeTestPDF（多行 / 多字号 / 逐行定位），新增跨页与噪音夹具
    ├── chunk_test.go       # 改：跨页合并、区间归属、长度上限仍生效
    ├── integration_test.go # 改：真实 PG 的端到端入库 + 区间相交过滤 + 跨页片段召回
    ├── filter_test.go      # 改：页码过滤的区间相交语义
    └── eval_gate_test.go   # 改：确认既有用例**逐字节不变**（SC-006 的证据）

web/src/
├── lib/knowledge.ts                     # 改：RetrievedChunkResult 增 page_end: number | null
└── routes/knowledge-retrieval-dialog.tsx # 改：页码展示改为「第 N 页」/「第 N-M 页」/「—」

docs/eval-phase13-pdf-layout-chunking-report.md   # 新建：阶段报告（编号沿用既有 eval-phase* 体例）
```

**明确不改动**：`internal/knowledge/{handler,wire,hybrid,admission,dedup,rerank,tasks}.go`、
`internal/{agent,conversation,workflow,mcp,auth,user,provider,config}` 全部、
`internal/db/queries/`（MySQL 侧，除非 FR-018 的拍板结果要求，见下）、
`internal/db/pgqueries/chunks.sql` 的邻接查询（`FindPublishedNeighborChunksBatch` 故意不加页码过滤，
沿用 002 的 FR-011 豁免，本期不动）。

**Structure Decision**: 沿用仓库既有的模块化单体布局，**不引入新目录、不引入新模块**。
本功能全部业务逻辑落在既有的 `internal/knowledge` 包内；唯一的新建源文件是 `layout.go` / `layout_test.go`，
理由是 `chunk.go` 已逾 700 行，把五组互不相干的启发式判定再塞进去会让"纯函数、易测、易审"这条
（宪法第 V 条的落点）在阅读上失效。`layout.go` 新建时按宪法第 VII 条选定**英文注释**，
与相邻的 `parse.go` / `chunk.go` 一致。

## 入库链路：改动插在哪几步

```
ProcessDocument
  │
  ├─ parseFile → extractPDFPages
  │     └─ 改：产出 []pdfLine（文本 + 页码 + 字号 + Y + 渲染宽度）     ← 改动 1
  │
  ├─ 新增：layout.go 噪音识别 → 剥离页眉页脚 / 纯页码行 + 审计日志      ← 改动 2
  ├─ 新增：layout.go 跨页合并判定 → 重建段落流 []paragraphUnit          ← 改动 3
  │
  ├─ chunkDocument → chunkPDFPages
  │     └─ 改：消费段落流而不是逐页 string；overlap 跨页携带；
  │            每个 chunkPiece 带 PageNumber(起) + PageEnd(止)          ← 改动 4
  │
  ├─ 新增：扫描件比值判定 → 0 则返回专用错误；(0,1) 则继续 + 告知        ← 改动 5
  │
  ├─ len(pieces)==0 → ErrEmptyContent          ← 不变（现在只剩"真的空文件"会走到这）
  ├─ len(pieces)>maxChunksPerDocument → ErrTooManyChunks   ← 完全不变
  ├─ Embed 批量调用                                        ← 完全不变
  └─ createChunks 落库（多写一列 page_end）                 ← 改动 6
```

**检索侧只有一处改动**：两条召回查询的页码过滤谓词（点落 → 区间相交）。
融合、准入、去重、重排、截断、邻接扩展**一行不改**——这是"本次改动全部发生在入库阶段"这句话的
可验证含义（spec 的 Assumptions 最后一条）。

## 降级与失败矩阵

| 情形 | 行为 | 依据 |
|---|---|---|
| txt / md 文档 | 与改动前**逐字节一致**（`chunkDocument` 的这两条分支一行不改） | FR-020 / SC-006 |
| 单页 PDF | 无跨页合并可做；噪音判定因页数不足门槛被跳过 → 与改动前一致 | Edge Cases / FR-009 |
| 仅两页 PDF | **跳过**基于重复率的噪音判定，页眉**不**剥离（宁可漏剥） | FR-009 / R4 |
| 每页都以句末标点结束 | 三条合并判据第 1 条即不成立 → 不合并，产出与按页切分基本一致 | FR-003 / R3 |
| 整篇无句末标点（列表/表格型） | 合并成立但长度上限仍生效，超限回退 `chunkText` 定长切分 | FR-004 / Edge Cases |
| 单段落跨越三页以上 | 同上——合并不豁免长度上限 | FR-004 |
| 页眉文字与某页正文完全相同 | 三条判据取交集 + 只在跨页重复时成立 → 宁可漏剥 | SC-005 / R4 |
| 字号信号取不到（`FontSize` 为 0/缺失） | 字号信号一律判为**不成立**，不猜 | FR-016 / R5 |
| 标题无法可靠识别 | `section_title` 保持 NULL（与现状一致） | FR-016 / R5 |
| 全部页无文本层（纯扫描件） | 新的专用错误 + 中文提示指向 OCR，**不再是** `ErrEmptyContent` | FR-017 / SC-007 |
| 部分页无文本层 | 有文本的部分**正常入库**，并告知有页面未提取（承载方式待拍板） | FR-018 |
| 真正的空文件 / 零字节 | 仍走 `ErrEmptyContent`（这条路径的语义由此变得准确） | FR-017 |
| `page_number` 为 NULL 的存量行遇到页码过滤 | 仍不匹配（SQL 三值逻辑，`page_end` 同为 NULL） | FR-014 / R2 |
| 存量已入库的 PDF 文档 | 一次 `page_end = page_number` 回填，检索与过滤行为**逐字节不变**；不重建分块 | FR-022 / R7 |

## Complexity Tracking

> **Fill ONLY if Constitution Check has violations that must be justified**

**无违规。** Constitution Check 九条全部 PASS 或 N/A（第 V 条的"LLM 降级"半条判 N/A，
理由已在表内写明：本功能不引入任何模型调用，该半条没有作用对象）。四条**范围边界**已在上方
「已知范围边界」明示——它们是需要被看见的取舍，不是对宪法的偏离，因此不进本表。

| Violation | Why Needed | Simpler Alternative Rejected Because |
|-----------|------------|-------------------------------------|
| — | — | — |

## Post-Design Constitution Re-Check

Phase 1 设计（data-model.md / contracts / quickstart.md）完成后重新对照，结论不变：

- **第 III 条**：`page_end` 全程在 `knowledge`(2) 与 `db/pggen`(0) 之间流动，零新增依赖边；
  前端改动不进依赖图。`make check-deps` 每个任务后跑。
- **第 IV 条**：`000005` migration 与 `make sqlc` 是第一批任务，任何 Go 代码都排在其后；
  `handler.go`/`wire.go` 本期无改动，不构成"跳层"。
- **第 V 条**：五组判定全部落在 `layout.go` 的纯函数里并有单测；重复率统计与行宽众数的 map
  必须排序后迭代；`page_end` 不参与任何排序，因此不影响既有 tie-break。
- **第 VI 条**：改动前基线必须先跑并归档（门禁 JSON + SC-001 的 FAIL 输出 + 分块阶段 benchmark），
  见 quickstart.md 步骤 0。
- **第 VII 条**：新错误 `Message` 中文；`layout.go` 英文注释；`specs/` 全中文。
- **第 IX 条**：四条范围边界已明示，其中「US4 允许被裁」与「FR-018 承载方式待拍板」
  必须在 `tasks.md` 里有明确落点，不能悬着进实现。

## 进入 `/speckit-tasks` 前的三项拍板（已决，2026-09-04）

原「待拍板」三项由所有者逐条裁定如下。**这三条是 `tasks.md` 拆分的直接输入，实现阶段不得再摇摆**。

### 决策 1 · FR-018「部分页面无文本层时告知用户」→ **推迟到下一期，本期只做 FR-017**

**裁定**：本期只实现 FR-017（**整份**无文本层的扫描件 → 专用中文错误提示，与 `ErrEmptyContent`
区分）。FR-018（部分页无文本层时把提示带给用户）**整条推出本期范围**。

**理由**：US5 的价值几乎全部集中在纯扫描件——那是用户真的被"内容为空"这条提示卡住、
无法判断下一步的场景。而 FR-018 需要 `documents` 表新增一列"成功但有提示"的承载字段
（现状 `MarkDocumentReady` 硬编码 `error_message = NULL`，一份成功入库的文档没有任何地方
能挂提示），连带 MySQL migration + repository + service + DTO + 前端文档列表展示，
是本功能唯一会溢出到 MySQL 与前端文档列表的地方，与它带来的边际价值不成比例。

**本期对"部分页无文本层"的实际行为**（必须在 `tasks.md` 与验收报告里如实写明，不得包装成"已满足 FR-018"）：
- 有文本的页面**正常入库**，不因部分页缺文本而整体失败；
- 缺文本的页码落一条**结构化日志**（`slog.Warn`，含 document_id 与缺文本页码列表）；
- 用户界面**不会**看到这条提示——这是一个**已知的、被明示的未满足项**。

**spec.md 的处置**：FR-018 保留原文不改（它仍然是这个功能长期该有的样子），但在其后补一行
`Deferred` 标注，指向本决策。`tasks.md` 不为它生成任何任务。

### 决策 2 · `chunks_page_range_valid` CHECK 约束 → **加**

**裁定**：`000005` migration 中加上 `CHECK ((page_number IS NULL) = (page_end IS NULL))`
（具体表达式以 data-model.md §6 为准）。

**理由**：不加的失败方式是**无声的**——一行 `page_number` 有值而 `page_end` 为 NULL 的脏数据，
会让新的区间相交过滤 SQL 静默漏召回，而漏召回在这个系统里没有任何下游环节能发现。
R2 的等价性论证需要这条硬保险才成立。

**已知连带代价**（`tasks.md` 必须给它一个独立任务，不得散落）：约 10 处只设 `PageNumber`
不设 `PageEnd` 的测试数据构造点会被数据库响亮拒绝，需要补 `PageEnd`。
**只补 `PageEnd` 这一处，不得顺手修改这些测试的任何其他部分**（宪法第 IX 条）。

### 决策 3 · `writeTestPDF` 扩展 → **最小扩展，且必须是前置任务**

**裁定**：扩展到「**每行一个 `Td` + 可选 `Tf`**」为止，不再多做。它在 `tasks.md` 里是一个
**独立的前置任务**，排在所有需要 PDF 夹具的测试任务之前。

**理由**：现状 `writeTestPDF` 把整页文本作为单个 `Tj` 放在固定坐标 `72 700 Td`、固定字号 12，
所有字形共享同一个 Y 和同一个字号。这意味着它**造不出本功能需要的任何一份夹具**：
造不出多行 → 造不出页眉页脚（位置判据）；造不出不同字号 → 测不了标题判定；
造不出不同行宽 → 测不了"接近满行宽"这条合并判据。
不把它提成前置任务，后面每个测试任务都会各自去改它，改法还会互相打架。

**边界**：不实现多栏、不实现旋转、不实现字体嵌入——这些本功能都用不到（多栏排版已被
spec 明确排除在范围外）。

### US4（标题识别，P3 / SHOULD）的处置

plan.md「已知范围边界」第 3 条要求它在 `tasks.md` 阶段落定。**裁定：保留，但排在最后一批任务，
且标注为可裁**。R5 已论证现状下 PDF 从不产出 `section_title`，裁掉不破坏任何既有行为——
因此它是本功能唯一一块"做不完可以安全丢弃"的部分，前三项 P1/P2 能力不依赖它。
