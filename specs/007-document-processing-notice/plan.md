# Implementation Plan: 文档处理的「成功但有提示」通道

**Branch**: `007-document-processing-notice` | **Date**: 2026-09-05 | **Spec**: [spec.md](./spec.md)

## Summary

系统当前只能表达两种处理结局——成功和失败——而第三种真实存在：**处理完成了、文档可用、
但有一部分内容没能进去**。006 已经实现了「部分页面无文本层时处理有文本的部分」，
但那件事**用户看不见**，只有一条给开发者看的结构化日志。

本期加一条**平行于失败的通道**，四件事：

1. `documents` 新增一列 `unextracted_pages`，存未能提取文本的页码列表（`NULL` = 无缺页）。
2. 写入并进 `MarkDocumentReady` 这条既有语句——它已经是"一个版本成为 ready"的唯一入口，
   已经带着 `version + status='publishing'` 的并发保护，也已经在无条件清 `error_message`。
   **FR-004（无缺页时清除）因此是免费得到的。**
3. 文档列表把提示显示出来，与"失败"在视觉上明确区分，且文档仍呈现为可用。
4. 提示带页码，让用户知道该对哪几页做 OCR——不只是"有内容没进去"。

技术路线由 [research.md](./research.md) 定死：单列存页码列表、不存渲染句子、不做 code 字段（R1）；
写入并进 `MarkDocumentReady`（R2）；失败路径一字不改（R3）；存量不回填（R4）。

⚠️ **本期不实现 OCR，也不让文档变得更完整。** 它只是把一件已经发生的事说出来。
验收报告不得把"用户现在知道有 5 页没进去"写成"处理质量提升"。

## Technical Context

**Language/Version**: Go 1.26.5（`GOTOOLCHAIN` 管理）+ React 19 / Vite / TS

**Primary Dependencies**: 无新增。`go.mod` 的 diff 必须为空。

**Storage**: MySQL 8.x 的 `documents` 表新增一列（migration `000015`）。
PostgreSQL / pgvector **零改动**——本功能不触碰 `chunks`、不触碰检索链路。
Redis 无变化。

**Testing**: `go test ./... -race -count=1`、`go vet ./...`、`make check-deps`、
真实 MySQL 的集成测试（**禁止 skip**，宪法第 VI 条）、
确定性检索门禁 `make eval-retrieval-gate`（必须与改动前**逐字节一致**——
本功能不碰检索，这是它的证明而不是它的目标）。

**Project Type**: 模块化单体的 Web 服务，改动集中在文档**入库结果的表达**。

**Performance Goals**: 无。本功能不新增任何查询、不新增任何循环；
写入是并进一条既有 UPDATE 的一个字段。

**Constraints**：
失败路径行为**完全不变**（FR-005 / SC-006）；
无提示时文档呈现与改动前**逐字节一致**（FR-010 / SC-004）；
非 PDF 与未重新处理的存量文档**零提示**（FR-014 / FR-015 / SC-007）；
纯扫描件继续作为**失败**处理（FR-013 / SC-006）。

**Scale/Scope**: 触及 **2 个模块**——`internal/knowledge`（第 2 层）与 `internal/db`（第 0 层）；
前端 2 个文件；**0 个新增跨模块依赖边**。
不触及 `agent`、`conversation`、`workflow`、`mcp`、`auth`、`user`、`provider`。

## Constitution Check

| 原则 | 判定 | 理由 |
|---|---|---|
| **I** 如实标注 AI 归属 | **PASS** | 报告只陈述技术事实；明写本功能**不改善处理质量**，只是把已发生的事说出来 |
| **II** 规格先行 | **PASS** | spec → research → 本文件 → tasks → 实现。**实现代码在 `tasks.md` 产出前不得开写** |
| **III** 模块分层 | **PASS** | 改动落在 `internal/knowledge`(2) 与 `internal/db/gen`(0)，既有合法方向，**零新增依赖边** |
| **IV** 模块内实现顺序固定 | **PASS** | `migration(000015) → make sqlc → model.go → repository.go → service.go → dto.go → 前端`。`errors.go`/`handler.go`/`wire.go` 本期无改动（不新增端点、不新增错误） |
| **V** 确定性优先 | **PASS** | 页码列表由抽取阶段按页序产出，天然有序；**序列化时必须显式保持升序**，不得依赖来源顺序的偶然性。无 map 迭代、无排序歧义 |
| **V** 确定性（LLM 降级/超时/开关） | **N/A** | 本功能**不引入任何模型调用**。判 N/A 的依据是"没有引入"——一旦后续任务里冒出任何模型调用，这条立刻变成必须满足 |
| **VI** 证据式验收 | **PASS** | 验收清单见 [quickstart.md](./quickstart.md)。⭐ **SC-001 的用例必须在改动前先跑一次并看到 FAIL**。数据库测试**禁止 skip** |
| **VII** 按读者选择语言 | **PASS** | 提示文案面向用户 → **中文**；`service.go`/`repository.go` 现有注释为英文 → 新增跟随英文；`model.go` 的中文段落跟随中文；migration 注释中文（沿用 `internal/db/migrations/` 既有体例）；`specs/` 全中文 |
| **VIII** 提交时机由所有者决定 | **PASS** | 本阶段只落 `specs/`，不写实现代码、不提交 |
| **IX** 最小范围 | **PASS**（⚠️ 见下） | 明确不做：OCR、视觉检索、通用文档处理警告框架、通知中心、提示历史 |

### 已知范围边界（非违规，但必须明示）

1. **`unextracted_pages` 这个列名是有意窄的**（R1）。第二种警告想复用它会立刻显得荒谬——
   这正是 FR-011 要的效果。**代价**：将来真出现第二种警告时，需要一次真正的设计讨论
   （很可能是重构成别的形状），而不是加个枚举值就完事。这是知情取舍：
   在只有一个用例时提前抽象，代价是后面每一种警告都要迁就一个凭空设计的形状。
2. **`documents` 的行会变宽一点**。这张表在文档列表接口里被整行读出，多一列 TEXT
   会略微增加传输量。评估：缺页是少数情况，绝大多数行是 `NULL`，影响可忽略。
   **不为此引入延迟加载**——那是为一个不存在的问题增加复杂度。
3. **提示只在文档列表可见**，不进对话链路、不进引用。用户在对话里问到缺失内容时，
   **仍然只会得到"检索不到"**——本功能不改善那个体验，它只让用户在上传后能预先知道。
   这一条必须写进验收报告，别让人以为静默缺失被彻底解决了。

## Project Structure

```text
internal/
├── db/
│   ├── migrations/
│   │   ├── 000015_document_unextracted_pages.up.sql    # 新增：ADD COLUMN
│   │   └── 000015_document_unextracted_pages.down.sql  # 新增：DROP COLUMN
│   ├── queries/documents.sql   # 改：MarkDocumentReady 增一个赋值；三处 SELECT 带出新列
│   └── gen/                    # make sqlc 重新生成，**不得手改**
└── knowledge/
    ├── model.go         # 改：Document 增 UnextractedPages []int（中文注释段落跟随中文）
    ├── repository.go    # 改：markDocumentReady 增参数；行映射读出新列 + 解析/序列化
    ├── service.go       # 改：ProcessDocument 把 006 已算出的缺页页码带到 markDocumentReady
    ├── dto.go           # 改：文档响应增字段
    ├── notice.go        # 新建：页码列表的序列化/反序列化（纯函数）
    ├── notice_test.go   # 新建：纯函数单测（含空值、单页、乱序、大量页）
    └── integration_test.go  # 改：真实 MySQL 的写入/清除/并发归属用例

web/src/
├── lib/knowledge.ts                    # 改：KnowledgeDocument 增 unextracted_pages
└── routes/knowledge-documents-dialog.tsx  # 改：ready 且有缺页时展示提示

docs/eval-phase14-document-processing-notice-report.md   # 新建：阶段报告
```

**明确不改动**：`internal/knowledge/{errors,handler,wire,chunk,parse,layout,hybrid,admission,dedup,rerank,neighbor,tasks}.go`、
`internal/db/pgqueries/` 与 `internal/db/pgmigrations/` 全部（PG 侧零改动）、
`internal/{agent,conversation,workflow,mcp,auth,user,provider,config}` 全部。

**Structure Decision**: 唯一的新建源文件是 `notice.go` / `notice_test.go`，
理由是页码列表的序列化是纯函数、有边界情况（空、乱序、极多），值得单独可测；
塞进 `repository.go` 会让它只能通过数据库来测。

## 数据流：提示从哪来，到哪去

```
ProcessDocument
  │
  ├─ parseFile → extractPDFPages
  │     └─ 006 已经算出「哪些页没有文本层」（textLayerCoverage），
  │        目前只落 slog.Warn 就丢掉了                          ← 信息的源头
  │
  ├─ 全部页无文本 → ErrPDFNoTextLayer（失败）                    ← 不变（FR-013）
  ├─ 部分页无文本 → **把页码列表一路带下去**                      ← 改动 1
  │
  ├─ 分块 / Embed / createChunks / markDocumentPublishing        ← 完全不变
  │
  └─ markDocumentReady(id, version, chunkCount, unextractedPages) ← 改动 2
        └─ SQL 里 unextracted_pages 与 error_message = NULL 并排，
           每次成功整体覆盖 —— 无缺页时写 NULL，FR-004 免费得到
```

**读取侧只有一处改动**：文档列表接口多带一个字段。检索、对话、引用**一行不改**。

## 降级与失败矩阵

| 情形 | 行为 | 依据 |
|---|---|---|
| txt / md 文档 | 无"页"概念，恒 `NULL`，呈现与改动前一致 | FR-014 / SC-007 |
| PDF 全部页都有文本 | 写 `NULL`，无提示 | FR-004 / SC-004 |
| PDF 部分页无文本 | 写页码列表，文档 `ready`，列表显示提示 | FR-003 / SC-001 |
| PDF 全部页无文本（纯扫描件） | 仍走 `ErrPDFNoTextLayer` **失败**路径，不写提示 | FR-013 / SC-006 |
| 处理失败 | 显示失败原因；提示**不显示**（status 不是 ready） | FR-005 |
| 重新处理后不再缺页 | `MarkDocumentReady` 写 `NULL`，提示消失 | FR-004 / SC-005 |
| 并发处理（重试与原任务竞争） | `version + status='publishing'` 保证提示属于赢下发布的那一次 | Edge Cases |
| 存量已入库文档 | `NULL`，无提示，呈现与改动前一致；重新处理后才有 | FR-015 / SC-007 |
| 缺页数量极多 | 存全量、**界面折叠展示**（列前几页 + "等 N 页"），数量是真实总数 | R5 |
| 文档 / 知识库被删除 | 提示不构成任何阻碍——它只是一列数据 | Edge Cases |

## Complexity Tracking

**无违规。** 三条范围边界已在上方明示，它们是需要被看见的取舍，不进本表。

| Violation | Why Needed | Simpler Alternative Rejected Because |
|-----------|------------|-------------------------------------|
| — | — | — |

## 进入 `/speckit-tasks` 前的两项拍板（已决，2026-09-05）

### 决策 1 · 提示文案：列表行给短版，完整信息走悬浮

**裁定**：文档列表那一行显示 `${chunk_count} 个分片 · 有 N 页未提取文本`，
完整信息（页码列举 + OCR 指引）走该元素的 `title` 悬浮提示。

**理由**：FR-008（能判断下一步动作）与 FR-009（说明有多少）都要满足，但列表行的宽度
是硬约束——把「有 5 页未能提取文本（第 46-50 页），这些页可能是扫描图，如需检索其中
内容请用 OCR 工具转换后重新上传」整句塞进一行，要么撑爆布局，要么被 CSS 截断成一句
残句，那比不显示更糟。**短版负责让用户注意到（含真实数量），悬浮负责让他知道做什么。**

⚠️ **短版里的 N 必须是真实总数**，不是被截断后的列举数量。截断的是列举，不是事实。

### 决策 2 · 连续页码折叠成区间 → **放前端**

**裁定**：`46,47,48,49,50` → `第 46-50 页` 的折叠**在前端做**。
后端 DTO 只发原始的页码数组，不发渲染好的字符串。

**理由**：与 R1 「不存渲染好的句子」是同一条理由的延续——DTO 返回渲染结果等于把展示层
烧进 API 契约，折叠规则或文案改一次就要动后端。**后端负责事实，前端负责呈现**，
这条边界在本功能里不该破例。

**代价**（如实记）：将来若出现第二个消费方（比如导出、通知邮件），折叠逻辑会被复制一份。
接受这个代价——现在只有一个消费方，为一个不存在的第二方提前统一，是 FR-011 反对的
那类提前抽象的另一种形态。

### US3 的处置

US3（提示随重新处理而更新，P2）**不裁**，但它几乎不需要额外实现代码：
FR-004 由 R2 的「并进 `MarkDocumentReady`」免费得到。它的任务因此**以验收用例为主**，
这不是偷懒——**一个免费得到的性质如果没有断言锁定，下一次有人拆分那条 SQL 时它就会
无声消失**。
