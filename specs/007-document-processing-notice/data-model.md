# Phase 1 数据模型：文档处理的「成功但有提示」通道

**Feature**: `007-document-processing-notice` | **Date**: 2026-09-05

技术路线不在此重新论证，见 [research.md](./research.md)（R1~R5）。

---

## 1. 实体总览

| 实体 | 位置 | 状态 | 一句话职责 |
|---|---|---|---|
| `documents.unextracted_pages` | `internal/db/migrations/000015` | **新增列** | 未能提取文本的页码列表；`NULL` = 无缺页或不知道 |
| `Document.UnextractedPages` | `internal/knowledge/model.go` | **扩展** | 领域实体上的同一份信息，`[]int` |
| 页码列表的编解码 | `internal/knowledge/notice.go` | **新建** | `[]int` ⇄ `string` 的纯函数 |

⭐ **数据流**（信息的源头在 006 已有的代码里，本期只是不再把它丢掉）：

```
extractPDFPages → textLayerCoverage  →  []int（哪些页没有文本层）
  → ProcessDocument 一路带下去（目前到 slog.Warn 为止就丢了）
  → markDocumentReady(..., unextractedPages)
  → documents.unextracted_pages
  → 文档列表 DTO → 前端展示
```

---

## 2. `documents.unextracted_pages`（MySQL，migration `000015`）

### 2.1 up

```sql
ALTER TABLE documents ADD COLUMN unextracted_pages TEXT NULL;
```

一句话，没有回填（R4：存量文档系统并不知道它当初有没有缺页，`NULL` 是唯一诚实的值）。

**为什么不加 CHECK 约束**：与 006 的 `chunks_page_range_valid` 不同，这一列的内容是
一个**列表**，MySQL 的 CHECK 表达不了"逗号分隔的升序正整数"这种结构，硬写正则既脆弱
又难读。**不变量改由 `notice.go` 的纯函数在编码时保证，并由单测锁定**——
这不是"约束更弱"，而是这一列的不变量本来就不属于数据库能便宜表达的那一类。

**为什么不建索引**：从不按它查询、从不排序、从不过滤。它只被整行读出来展示。

### 2.2 down

```sql
ALTER TABLE documents DROP COLUMN unextracted_pages;
```

回滚会丢掉"哪些页没进去"这条信息，但**不影响任何文档的可用性与可检索性**——
这一列从来不参与检索。重新处理文档即可再次得到它。

### 2.3 列类型选择

| | `TEXT NULL`（采用） | `JSON`（否决） | `INT`（否决） |
|---|---|---|---|
| sqlc 映射 | `sql.NullString`，直白 | `json.RawMessage`/`[]byte`，多一层 | — |
| 表达力 | 够用（从不按内容查询） | 过剩 | **不够**：丢掉页码，用户不知道 OCR 哪几页 |
| 引诱错误用法 | 无 | 会引诱"顺便再塞个 code 字段"，正是 FR-011 禁的 | — |

---

## 3. 编码格式与不变量（`notice.go`）

**格式**：升序、逗号分隔、无空格、1-indexed。例：`46,47,48,49,50`。
空列表编码为 **`NULL`**，不是空字符串——"没有缺页"和"有一个长度为 0 的列表"
在语义上是同一件事，只用一种表示，避免两种空值。

| # | 不变量 | 谁保证 |
|---|---|---|
| N1 | 编码结果要么是 `NULL`，要么至少含一个页码 | `encodeUnextractedPages` |
| N2 | 页码**升序**且**去重** | 编码时显式排序去重，**不依赖来源顺序的偶然性**（宪法第 V 条） |
| N3 | 每个页码 `>= 1` | 编码时过滤非正值；页码 1-indexed 是全仓库既有约定 |
| N4 | 解码是编码的逆：`decode(encode(x)) == 规范化后的 x` | 往返单测 |
| N5 | 解码遇到无法解析的历史值 MUST 返回**空列表**而不是报错 | 一列展示用的数据不该让整个文档列表接口失败 |

⚠️ **N2 的"不依赖来源顺序"是刻意的**：`textLayerCoverage` 目前恰好按页序产出，
但那是它的实现细节。编码时显式排序，让顺序成为**值的性质**而不是调用链的性质，
否则某天上游改了遍历方式，产出就会静默变得不确定（宪法第 V 条 / 006 踩过同类坑）。

---

## 4. `Document`（扩展，`internal/knowledge/model.go`）

现有字段旁新增：

```go
// UnextractedPages 是这次处理中**没能提取到文本**的页码（1-indexed、升序）。
// nil 表示两件事之一：这次处理没有缺页，或者这份文档从未被本功能上线后的
// 版本处理过（存量行）。两者都渲染为"无提示"，系统不区分——因为它确实不知道。
//
// ⚠️ 它不是错误：携带它的文档 status 仍然是 ready，可以正常检索。
// 失败原因走 ErrorMessage，两条通道完全独立（FR-002）。
UnextractedPages []int
```

**不变量**：与 §3 的 N1~N3 相同。领域层拿到的永远是已规范化的值。

---

## 5. `MarkDocumentReady` 的改写（⭐ 本功能最关键的一处）

现状：

```sql
UPDATE documents
SET status = 'ready', error_message = NULL, chunk_count = ?, lease_expires_at = NULL
WHERE id = ? AND version = ? AND status = 'publishing';
```

改后**只多一个赋值**：

```sql
SET status = 'ready', error_message = NULL, chunk_count = ?,
    unextracted_pages = ?, lease_expires_at = NULL
```

**为什么这一处如此关键**（R2）：

1. 它是"一个版本成为 ready"的**唯一**入口。
2. `WHERE version = ? AND status = 'publishing'` 已经保证写进去的提示属于
   **最终生效的那一次**处理，而不是被淘汰的那次。本功能**一行并发代码都不用写**。
3. 它已经在**无条件清 `error_message`**。把 `unextracted_pages` 加在旁边，语义完全对称：
   **每次成功整体覆盖，没有缺页就写 NULL**。⭐ **FR-004（无缺页时清除陈旧提示）
   由此是免费得到的**，不需要任何额外语句，也不引入任何新的时序问题。
4. "0 行受影响 = 别的 runner 抢先完成了，良性幂等竞争"这条既有含义**一字不变**。

⚠️ **禁止**为清除提示单开一条语句。两条语句之间没有事务，就有一个窗口，
窗口里文档的状态和提示是不一致的——而这种不一致没有任何报错，只会让用户看到一条过期的提示。

---

## 6. 三处 `SELECT` 必须带出新列

`documents.sql` 里读取文档的查询（按 ID 取、按知识库列表、reconciliation 扫描）
都要把 `unextracted_pages` 带出来，对应的行映射也要读。

⚠️ **漏掉列表那一处 = 本功能完全不生效但测试可能全绿**：单查一份文档的用例会通过，
而用户唯一能看到提示的地方恰恰是**列表**。验收必须有一条走列表接口的断言。

---

## 7. DTO 与展示契约

见 [contracts/document-notice.md](./contracts/document-notice.md)。

---

## 8. 不变量总表（回归断言的目标）

| # | 不变量 | 层 | 断言在哪 |
|---|---|---|---|
| N1-N5 | 编码格式、升序去重、往返、坏值降级 | 纯函数 | `notice_test.go` |
| D1 | 有缺页 → 写入；无缺页 → 写 `NULL` | 服务 | 真实 MySQL 集成测试 |
| D2 | 重新处理后不再缺页 → 提示消失 | 服务 | 集成测试（SC-005） |
| D3 | 提示属于赢下发布的那一次处理 | 服务 | 集成测试（并发/版本竞争） |
| D4 | 纯扫描件仍走**失败**，不写提示 | 服务 | 集成测试（SC-006） |
| D5 | txt/md 恒 `NULL` | 服务 | 集成测试（SC-007） |
| D6 | **列表接口**带出该字段 | 仓储/HTTP | 走列表接口的断言（见 §6 的 ⚠️） |
| D7 | 失败文档显示失败原因，不显示旧提示 | 展示 | 前端 + DTO 断言（FR-005） |
| D8 | 无提示时呈现与改动前逐字节一致 | 展示 | SC-004 |
