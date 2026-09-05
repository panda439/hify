# 契约：检索响应的页码区间与扫描件错误

**Feature**: `006-pdf-layout-chunking` | **Date**: 2026-09-04

本功能**不新增任何 HTTP 端点、不新增任何请求参数**。
但它做了两件同样属于契约变更的事，因此必须成文：

1. **改变了既有响应体的形状**——检索结果里的 chunk 多一个字段，且既有字段的**语义被收紧**
2. **改变了既有过滤入参的匹配规则**——`page_min` / `page_max` 的字面含义不变，但"什么叫匹配"变了

外加一个新的错误码。

**受影响的端点**：`POST /api/v1/knowledge-bases/{id}/retrieve`（003-retrieval-playground 的试检索）。
Citation 侧**不受影响**，见 §5。

---

## 1. 响应体：chunk 的页码字段

### 1.1 形状

```jsonc
{
  "chunks": [
    {
      "id": "...",
      "document_id": "...",
      "document_name": "handbook.pdf",
      "page_number": 3,        // 语义收紧：所在页 → 起始页
      "page_end": 4,           // 新增
      "content": "...",
      "score": 0.87,
      "is_neighbor": false,
      "neighbor_of": ""
    }
  ],
  "hit_count": 1,
  "neighbor_count": 0,
  "filter_applied": false
}
```

落点：`internal/knowledge/dto.go` 的 `chunkResult`（现状 `dto.go:106` 是
`PageNumber *int \`json:"page_number"\``），新增紧邻的 `PageEnd *int \`json:"page_end"\``。

### 1.2 契约条款

| # | 条款 |
|---|---|
| R1 | `page_number` 的语义从"该片段所在的页"收紧为"**该片段覆盖的起始页**"。改动前不可能跨页，所以这不是一次语义倒转，而是一次**细化**——对所有单页片段字面含义完全不变 |
| R2 | `page_end` 是"该片段覆盖的**结束页**" |
| R3 | ⭐ **两者同为 `null`，或同为非 `null`**。绝不出现一个有值一个为 `null` 的响应 |
| R4 | 同为非 `null` 时 `page_number <= page_end` |
| R5 | txt / md 文档的片段、以及 `000003` 迁移之前写入的存量行，两者**恒为 `null`**——不为没有页码概念的格式编造页码（FR-014） |
| R6 | 指针类型（Go 侧 `*int`）不可改成值类型：无页码时必须序列化成 JSON `null`，**不是 `0`**。`0` 是一个编造出来的页码 |
| R7 | 邻接块（`is_neighbor: true`）的 `page_number` / `page_end` 是**它自己的**真实值，不是它所属 anchor 的 |

⚠️ **R3 是前端展示逻辑的全部依据**（§2）。它在后端由 `chunks_page_range_valid` 这条数据库约束
兜底强制（见 data-model.md §6），而不是靠"应该不会出错"。

---

## 2. 前端展示契约

**现状**：`web/src/routes/knowledge-retrieval-dialog.tsx:77` 写的是

```tsx
第 {c.page_number ?? "—"} 页
```

⚠️ 注意这个现状有一个小瑕疵：`page_number` 为 `null` 时它渲染成「**第 — 页**」，
而不是干净的「—」。本功能顺带修正它（属于必要的连带改动：既然这一行要改成三分支，
就不能把一个已知的错误分支原样抄过去）。

**改后的展示规则**（三分支，无第四种情况——由 R3 保证）：

| 条件 | 展示 | 例 |
|---|---|---|
| `page_end === page_number` | 「第 N 页」 | 第 3 页 |
| `page_end !== page_number` | 「第 N-M 页」 | 第 3-4 页 |
| 两者均为 `null` | 「—」（**不带"第…页"外框**） | — |

**类型定义**：`web/src/lib/knowledge.ts:160` 的 `RetrievedChunkResult` 新增

```ts
  // 片段覆盖的结束页。与 page_number 同为 null 或同有值（后端 006 的 R3），
  // 相等时界面显示「第 N 页」，不等时显示「第 N-M 页」，均为 null 显示「—」。
  page_end: number | null;
```

**⚠️ 前端不得做的三件事**：

1. **不得**用 `page_end ?? page_number` 或 `page_number ?? 0` 一类兜底。R3 已经保证两者同步，
   兜底写法只会把一个本该被发现的后端 bug 变成一个看起来正常的界面。
2. **不得**在 `page_end` 缺失时假装它等于 `page_number`——那是在前端编造后端没给的信息。
3. **不得**把区间渲染成「第 3 页」（只取起始页）。FR-011 明确禁止"为跨页片段任选一个页码"，
   这条禁令在展示层同样成立——存了区间却只显示一端，用户看到的仍是一个不诚实的引用。

---

## 3. 页码过滤入参：语义不变，匹配规则变

### 3.1 入参形状（**完全不变**）

请求体里的 `page_min` / `page_max`（Go 侧 `RetrieveFilter.PageMin` / `PageMax`）：

| 属性 | 是否变化 |
|---|---|
| 字段名、JSON 形状 | 不变 |
| 1-indexed 闭区间 | 不变 |
| 任一端可省略 = 那一侧不设限 | 不变 |
| 校验规则（必须为正整数、min ≤ max，违反返回 `knowledge.invalid_page_range`） | 不变 |
| 与文档级过滤是「与」关系、同一条件多取值是「或」关系 | 不变 |
| 邻接块豁免页码过滤（002 的 FR-011） | 不变 |

### 3.2 匹配规则（**变了**）

| | 改动前 | 改动后 |
|---|---|---|
| 判定 | chunk 的**那一页**落在 `[page_min, page_max]` 内 | chunk 的**区间** `[page_number, page_end]` 与 `[page_min, page_max]` **有交集** |

### 3.3 前后行为对照表

取一个**跨第 3-4 页**的片段（`page_number=3, page_end=4`）与一个**单页第 3 页**的片段
（`page_number=3, page_end=3`）作对照：

| 过滤 | 跨页片段(3-4) 改动前 | 跨页片段(3-4) 改动后 | 单页片段(3) 改动前 | 单页片段(3) 改动后 |
|---|---|---|---|---|
| `min=3, max=3` | 命中 | 命中 | 命中 | 命中 |
| `min=4, max=4` | **不命中** | ⭐ **命中** | 不命中 | 不命中 |
| `min=4, max=9` | **不命中** | ⭐ **命中** | 不命中 | 不命中 |
| 只给 `min=4` | **不命中** | ⭐ **命中** | 不命中 | 不命中 |
| `min=1, max=2` | 不命中 | 不命中 | 不命中 | 不命中 |
| `min=5, max=9` | 不命中 | 不命中 | 不命中 | 不命中 |
| `min=1, max=10` | 命中 | 命中 | 命中 | 命中 |
| 只给 `max=3` | 命中 | 命中 | 命中 | 命中 |
| 不设两端 | 命中 | 命中 | 命中 | 命中 |

⭐ **三个变化格全部集中在跨页片段上，且全部是"原本漏掉的现在命中了"。
不存在任何一格是"原本命中的现在不命中"。** 这是 FR-022 与 SC-006 的契约层表述——
调用方不需要为这次改动调整任何已有的过滤条件。

**跨页片段的例子**（对应上表第 2 行）：一段说明横跨第 3 页末尾与第 4 页开头，
入库后是**一个**片段、标注为第 3-4 页。用户在试检索里把范围限定为「第 4 页」：

- 改动前：这段内容根本不存在于任何"第 4 页"的片段里（它被硬切成两半，前半标第 3 页、
  后半标第 4 页且缺失上文），限定第 4 页只能拿到不完整的后半句
- 改动后：这个片段被完整召回，因为它**确实包含第 4 页的内容**

### 3.4 一条永不放宽的规则

**`page_number` 为 `null` 的 chunk，只要设了 `page_min` / `page_max` 中任意一端，就不匹配。**

这依赖 SQL 三值逻辑（`FALSE OR NULL` = `NULL`，而 `WHERE` 只接受 `TRUE`），
在改动前后完全一样——因为 `page_end` 与 `page_number` 同为 NULL（data-model.md 的不变量 C1）。

⚠️ **禁止**用 `COALESCE(page_number, 0)` / `COALESCE(page_end, 0)` 一类写法"修复"它。
那等于给一个本来没有页码的片段编造出第 0 页，与 `000003_chunk_source_metadata.up.sql` 里
写明的既有禁令正面冲突。`page_end` 完全适用这条禁令。

---

## 4. 扫描件错误码

### 4.1 现有的（不变，但适用范围缩小）

| 错误码 | Message | 落点 |
|---|---|---|
| `knowledge.empty_content` | 文档内容为空或无法提取到文本 | `internal/knowledge/errors.go:18` 的 `ErrEmptyContent`；`service.go:482` 在 `pieces` 为空时经 `failDocument` 返回 |

⭐ **本功能之后，这个错误的适用范围收缩为"真正的空文件"**——零字节、只有空白字符、
或解析后确实一个字都没有的文档。它的 Message 由此从"一句含糊的兜底"变成一句准确的描述。
**Message 文案本身不改**（改了会牵动既有测试与用户认知，属于范围外）。

### 4.2 新增的

| 错误码 | Message（中文，宪法第 VII 条） | 触发条件 |
|---|---|---|
| `knowledge.pdf_no_text_layer` | 该 PDF 没有文本层（疑似扫描件或图片型 PDF），暂不支持自动识别，请先用 OCR 工具转换为可选中文字的 PDF 后重新上传 | PDF 文档的**全部**页面都无可提取文本（R6：有文本页数 / 总页数 == 0） |

**分类**：`apperr.InvalidInput`，与 `ErrEmptyContent` 同类——它是"这份文件本身不适合"，
不是基础设施故障，重试不会有不同结果。

**呈现路径**：与所有文档处理失败一致——`service.go` 的 `failDocument` →
`markDocumentFailed` 写入 `documents.error_message` → `GET /api/v1/knowledge-bases/{id}/documents`
的 `error_message` 字段 → 前端文档列表。**不需要新端点、不需要新字段。**

### 4.3 两个错误必须能被用户区分（SC-007）

| 用户实际遇到的情况 | 改动前看到 | 改动后看到 | 用户能否判断下一步 |
|---|---|---|---|
| 传了一个空文件 | 文档内容为空或无法提取到文本 | 文档内容为空或无法提取到文本 | 改动前后都能——检查文件 |
| 传了一份纯扫描件 | 文档内容为空或无法提取到文本 | **该 PDF 没有文本层……请先用 OCR 工具转换** | ⭐ 改动前**不能**（无法区分自己是哪种情况），改动后能 |

SC-007 的原话是"用户看到的提示能够让其判断下一步动作（需要 OCR），**无需查阅日志**"。
上表第 2 行就是它的验收形态。

⚠️ **Message 不得携带动态内容**（沿用 `errors.go` 里既有的注释约定：
"Message 是固定的，任何动态细节——计数、文件路径——进日志"）。
"有 3 页无法提取"这类数字进结构化日志，不进 `Message`。

### 4.4 FR-018（部分扫描件）的承载方式：**本契约暂不定义**

R6 规定：有文本页数占比介于 0 与 1 之间时，**正常处理有文本的部分**，并告知有页面未能提取。

前半句是明确的（本契约的 R1-R7 与 §3 全部适用于成功入库的那部分片段）。
**后半句没有承载字段**——`documents` 表只有 `error_message`，而 `MarkDocumentReady` 的 SQL
硬编码 `error_message = NULL`，一份成功入库的文档没有任何地方能挂一条提示。

因此本契约**不定义** FR-018 的响应形状，它取决于 plan.md「进入 tasks 前待拍板」第 1 条的结果。
⚠️ 在拍板之前，**不得**在实现里临时选一种——契约层没定的东西被实现层默默定下来，
正是宪法第 II 条要防的漂移。

---

## 5. 不受影响的契约（逐条确认，不是遗漏）

| 契约 | 为什么不受影响 |
|---|---|
| **Citation V1 的持久化** | `message_citations` 表（MySQL `000011`）**不存页码**；`internal/conversation/dto.go:68` 的 `toCitationResponse` 把 `PageNumber` 固定写成 `nil`。页码从单值变区间对它完全没有作用面 |
| **SSE / 对话流的 Evidence** | 同上，不携带页码 |
| **邻接窗口的过滤豁免** | `FindPublishedNeighborChunksBatch` 故意不带页码谓词（002 的 FR-011），本功能不动它。只是它 `SELECT` 的列要加 `page_end`，否则邻接块的 `page_end` 恒为 `null`，违反 R3/R7 |
| **`RetrievedChunk.Score` 的语义** | 过滤是布尔的范围缩小，不参与打分（002 的 FR-012）。本功能不碰融合、准入、去重、重排、截断 |
| **`is_neighbor` / `neighbor_of`** | 不变 |
| **文档级过滤 `document_ids`** | 不变 |
| **txt / md 文档的一切行为** | `chunkDocument` 的这两条分支一行不改（FR-020），由 `make eval-retrieval-gate` 逐字节证明（SC-006） |

---

## 6. 错误响应的固定形状

所有错误沿用宪法「技术栈与工程约束」定下的形状，本功能不引入例外：

```json
{"error": {"code": "knowledge.pdf_no_text_layer", "message": "该 PDF 没有文本层（疑似扫描件或图片型 PDF），暂不支持自动识别，请先用 OCR 工具转换为可选中文字的 PDF 后重新上传"}}
```

- 成功响应**不包壳**，直接返回资源 JSON，HTTP 状态码承载语义
- `code` 是稳定的机器可读标识（`knowledge.` 前缀），`message` 是**中文**用户文案
- 内部的 `fmt.Errorf` 包装链保持英文、小写开头、无句末标点——它的读者是日志和开发者，
  与 `message` 的读者不是同一批人，不得混用
