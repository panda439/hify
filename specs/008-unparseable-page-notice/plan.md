# Implementation Plan: 解析失败的页面也要能被用户看见

**Branch**: `008-unparseable-page-notice` | **Date**: 2026-09-05 | **Spec**: [spec.md](./spec.md)

## Summary

007 建立了「成功但有提示」这条通道，但它**只覆盖了两种缺失里的一种**。
页面进不了知识库有两条路：没有文本层（扫描图），和**解析失败被逐页跳过**（006 加的 recover 兜底）。
后者现在完全不可见——实测一篇 15 页 arXiv 论文第 1 页解析失败，用户看到一份「就绪、无提示」
的文档，而第 1 页根本不在里面。

三件事：

1. **新增第二列** `unparseable_pages`，与 `unextracted_pages` 对称独立。
   两种原因的下一步动作不同（OCR vs 换工具重新导出），一条消息服务不了两种——
   这是不并进现有那一列的全部理由，也是本期存在的全部理由。
2. **把两类的写入从 `MarkDocumentReady` 前移到 `MarkDocumentPublishing`**。
   后者的注释自己写着"这一步锁定了活儿已经干完，只差发布"，而缺页列表正是那个活儿的结果。
   顺带修掉 007 报告 §6.2：恢复流程完成的文档不再丢提示。
3. **展示合成一个尾缀**：`128 个分片 · 有 5 页未提取文本、2 页无法解析`，
   悬浮里两段各自给出**不同的**下一步动作。

技术路线由 [research.md](./research.md) 定死：第二列 + 增生判据（R1）；写入前移（R2）；
一个尾缀两段（R3）；不重叠靠断言不靠去重（R4）；存量不回填（R5）。

⚠️ **本功能不修复任何页面**，不让任何无法解析的页变得能解析。它只是把第二种已经发生的缺失说出来。

## Technical Context

**Language/Version**: Go 1.26.5 + React 19 / Vite / TS。**无新增依赖。**

**Storage**: MySQL `documents` 新增一列（migration `000016`）。PostgreSQL **零改动**。

**Testing**: `go test ./... -race -count=1`、`go vet`、`make check-deps`、
真实 MySQL 集成测试（**禁止 skip**）、`make eval-retrieval-gate` 逐字节不变
（本功能不碰检索，这是**证明**不是目标）。

**Constraints**：
006 的两个整份失败错误（无法解析该 PDF / 没有文本层）**一字不改**（FR-013 / SC-007）；
两类都不存在时呈现与改动前**逐字节一致**（FR-009 / SC-006）；
非 PDF 与存量文档**零提示**（FR-014 / FR-015 / SC-008）；
两类列表**重叠率 0**（FR-003 / SC-004）。

**Scale/Scope**: `internal/knowledge`(2) + `internal/db`(0)，前端 2 个文件，**0 个新增依赖边**。

## Constitution Check

| 原则 | 判定 | 理由 |
|---|---|---|
| **I** 如实标注 | **PASS** | 报告明写本功能不修复任何页面，只是把第二种缺失说出来 |
| **II** 规格先行 | **PASS** | spec → research → 本文件 → tasks → 实现 |
| **III** 模块分层 | **PASS** | 零新增依赖边 |
| **IV** 实现顺序 | **PASS** | `migration(000016) → make sqlc → notice.go → model → repository → service → dto → 前端` |
| **V** 确定性 | **PASS** | 复用 007 的 `notice.go` 编解码（已显式排序去重），新列走同一套 |
| **V** LLM 降级 | **N/A** | 不引入任何模型调用 |
| **VI** 证据式验收 | **PASS** | ⭐ SC-001 必须在改动前先 FAIL；数据库测试禁止 skip |
| **VII** 语言 | **PASS** | 提示文案中文；`notice.go`/`service.go` 英文；migration 注释中文 |
| **VIII** 提交时机 | **PASS** | 本阶段只落 `specs/` |
| **IX** 最小范围 | **PASS**（⚠️ 见下） | 不做 OCR、不做视觉检索、不做通用警告框架、不尝试修复无法解析的页 |

### 已知范围边界

1. **两列的重复是刻意接受的代价**。编解码、写入、读出、展示各有两份近似的代码。
   合并它们需要一个"原因"维度，而那正是 FR-011 禁止的东西。**在只有两种原因时，
   重复远比一个为想象中的第 N 种原因设计出来的结构便宜**——增生判据见 research.md R1。
2. **⚠️ 写入前移是一次行为改动，不只是搬家**。`MarkDocumentReady` 必须**同时**停止写这两列，
   否则恢复流程传 nil 会把 publishing 阶段刚写对的值清空——那正是本期要修的缺陷，反被固化。
   这是本次最容易只做一半的地方，tasks 里要单列一条任务并配变异测试。
3. **提示仍然只在文档列表可见**，不进对话链路。用户在对话里问到缺失内容时**仍然只会得到
   "检索不到"**——沿用 007 的边界，本期不扩大。报告里必须写明。

## Project Structure

```text
internal/
├── db/
│   ├── migrations/000016_document_unparseable_pages.{up,down}.sql   # 新增列
│   ├── queries/documents.sql   # 改：写入从 MarkDocumentReady 移到 MarkDocumentPublishing；
│   │                           #     五处 SELECT 带出新列
│   └── gen/                    # make sqlc 重新生成，不得手改
└── knowledge/
    ├── model.go         # 改：Document 增 UnparseablePages []int
    ├── notice.go        # 改：复用既有编解码（新列走同一套，不复制一份）
    ├── repository.go    # 改：markDocumentPublishing 增两个参数；markDocumentReady 去掉参数
    ├── service.go       # 改：extractPDFPages 的 unreadable 列表一路带下去
    ├── parse.go         # 改：把逐页跳过的页码**返回**出来（目前只落 slog.Warn 就丢了）
    ├── dto.go           # 改：文档响应增字段
    └── integration_test.go / notice_test.go   # 改/增

web/src/{lib/knowledge.ts, routes/knowledge-documents-dialog.tsx}   # 改：两段尾缀
docs/eval-phase15-unparseable-page-notice-report.md                 # 新建
```

## 数据流

```
extractPDFPages
  └─ unreadable []int（逐页 recover 跳过的页）—— 目前只落 slog.Warn 就丢了   ← 信息源头
       ↓ 返回出来
ProcessDocument
  ├─ 全部页解析失败 → ErrPDFUnreadable（失败）        ← 不变（FR-013）
  ├─ 全部页无文本   → ErrPDFNoTextLayer（失败）       ← 不变（FR-013）
  └─ 部分缺失 → 两个列表一路带到 ⬇
markDocumentPublishing(id, version, lease, unextractedPages, unparseablePages)  ← ⭐ 写入点前移
markDocumentReady(id, version, chunkCount)                                     ← ⭐ 不再触碰两列
```

## 降级与失败矩阵

| 情形 | 行为 | 依据 |
|---|---|---|
| txt / md | 两类恒 NULL | FR-014 |
| PDF 全部正常 | 两类恒 NULL，呈现与改动前逐字节一致 | FR-009 / SC-006 |
| 部分页无文本层 | 只写 `unextracted_pages`，只出一段 | FR-008 |
| 部分页解析失败 | 只写 `unparseable_pages`，只出一段 | **SC-001** |
| 两种都有 | 两列都写，尾缀出两段，悬浮给两个**不同**的下一步动作 | **SC-003** |
| 全部页解析失败 | 仍走 `ErrPDFUnreadable` **失败** | FR-013 / SC-007 |
| 全部页无文本层 | 仍走 `ErrPDFNoTextLayer` **失败** | FR-013 / SC-007 |
| 恢复流程完成发布 | 两列保留 publishing 阶段写下的值 | **FR-005**（修 007 §6.2） |
| 重新处理后不再缺失 | `MarkDocumentPublishing` 整体覆盖为 NULL | 007 FR-004 延续 |
| 存量文档 | NULL，不回填 | FR-015 |

## Complexity Tracking

**无违规。** 三条范围边界已明示。

## 进入 `/speckit-tasks` 前待拍板

1. **`parse.go` 的返回形态**：`extractPDFPages` 目前返回 `([]pdfPage, error)`。
   要把 unreadable 列表带出来，是加第三个返回值，还是把 `[]pdfPage` 包成一个带
   `Unreadable []int` 的结构体？后者调用点改动更小但引入一个新类型。
2. **两段尾缀的文案措辞**：需要在一行内既短又能让人分清两类，且悬浮里两段的
   下一步动作必须明显不同。
