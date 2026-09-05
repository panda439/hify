# Phase 14 · 文档处理的「成功但有提示」通道（007）验收报告

**规格**：[specs/007-document-processing-notice/](../specs/007-document-processing-notice/) ｜ **日期**：2026-09-05

> ⚠️ **本功能不改善任何处理质量。** 它不让任何一页变得能提取、不让任何内容进入知识库。
> 它只做一件事：**把一件已经发生的事说出来**。任何把它写成"处理质量提升"的说法都是错的。

---

## 1. 改了什么

系统此前只能表达两种处理结局——**成功**和**失败**——而第三种真实存在：
处理完成了、文档可用、**但有一部分内容没能进去**。

006 已经实现了「部分页面无文本层时处理有文本的部分」，但那件事**用户看不见**：
一份 50 页合同后 5 页是扫描签字页时，前 45 页正常入库、状态显示「就绪」，
而「有 5 页没进去」只存在于一条给开发者看的 `slog.Warn` 里。

| # | 改动 | 落点 |
|---|---|---|
| 1 | `documents` 新增一列 `unextracted_pages`（页码列表，`NULL` = 无缺页或不知道） | MySQL migration `000015` |
| 2 | 写入并进 `MarkDocumentReady` 这条既有语句 | `queries/documents.sql` |
| 3 | 页码列表的编解码（升序、去重、坏值降级） | 新建 `notice.go` / `notice_test.go` |
| 4 | `ProcessDocument` 把 006 已算出的缺页页码一路带下去 | `service.go` |
| 5 | 文档列表展示提示：短版在行内、完整信息走悬浮 | `dto.go` + 前端 2 个文件 |

**依赖零新增**，PostgreSQL 侧**零改动**，检索链路**一行未改**。

---

## 2. 逐条成功标准

| SC | 结论 | 证据 |
|---|---|---|
| **SC-001** 无需查日志即可知道有内容没进去 | ✅ | `TestIntegrationPartialScanNoticeReachesDocumentList` 改动前 **FAIL**（见 §3）、改动后 PASS；`TestListDocumentsEndpointCarriesUnextractedPages` 走真实 HTTP handler |
| **SC-002** 用户能说出下一步动作 | ✅ | 悬浮文案含**页码区间 + 原因 + OCR 指引**三者；行内短版含真实总数 |
| **SC-003** 提示不被误判为失败 | ✅ | 前端 amber 样式与 destructive 失败样式分离；文档仍呈现为可用；`status` 未新增取值 |
| **SC-004** 无提示时呈现逐字节一致 | ✅ | 无缺页时 `unextracted_pages` 序列化为 `null`，渲染分支不进入，`${chunk_count} 个分片` 一字未改 |
| **SC-005** 提示与当前状态 100% 一致 | ✅ | `TestIntegrationNoticeDisappearsWhenPagesNoLongerMissing` / `...AppearsWhenPagesBecomeMissing` 双向断言 |
| **SC-006** 纯扫描件仍作为失败呈现 | ✅ | `TestIntegrationScannedPDFIsDistinguishableFromAnEmptyFile` 未改动仍绿；变异测试确认改判会被抓住 |
| **SC-007** 非 PDF 与存量文档 0 条提示 | ✅ | `TestIntegrationTxtAndMarkdownNeverCarryNotice`；dev 库 `unextracted_pages IS NOT NULL` 计数为 **0** |

**其它验证**：`go test ./... -race -count=1` 全绿 **0 SKIP**；`go vet` / `make check-deps` / `tsc --noEmit` 干净；
`make eval-retrieval-gate` 与基线 **IDENTICAL**（本功能不碰检索，这是**证明**而非目标）；
`/smoke-test` 全部 200、启动 0 ERROR。

---

## 3. SC-001 的改动前 FAIL

```
WARN knowledge: pdf pages without a text layer were skipped
     document_id=doc-notice pages_without_text="[2 4]" pages_with_text=3

--- FAIL: TestIntegrationPartialScanNoticeReachesDocumentList
    文档列表没有带回缺页信息——用户无法知道有 2 页没进去，
    这条信息目前只存在于一条给开发者看的日志里。
    got=&{... Status:ready ErrorMessage: ChunkCount:3 UnextractedPages:[] ...}
```

⭐ 这段输出本身就是问题陈述：**日志已经知道第 2、4 页没进去，而用户拿到的那份数据里
`Status:ready`、`ChunkCount:3`，再无别的。** 两者之间隔的就是本功能。

---

## 4. 设计上值得记的两处

### 4.1 清除是免费的，因为写入并进了 `MarkDocumentReady`

`MarkDocumentReady` 已经是"一个版本成为 ready"的唯一入口，且已经带着本功能需要的全部保证：

```sql
SET status = 'ready', error_message = NULL, chunk_count = ?,
    unextracted_pages = ?, lease_expires_at = NULL
WHERE id = ? AND version = ? AND status = 'publishing';
```

- `WHERE version = ? AND status = 'publishing'` 保证提示属于**赢下发布的那一次**处理——
  本功能**一行并发代码都没写**。
- 它本来就在无条件清 `error_message`，多一个字段语义完全对称：**每次成功整体覆盖，
  无缺页就写 NULL**。「重新处理后提示消失」由此免费得到。

⚠️ 正因为它是免费的，它**特别容易在将来被无声弄坏**：任何人把这条 SQL 拆开、
或给提示单开一条写入语句，清除就没了，表现是用户看到一条过期提示——没有报错、没有日志。
`TestIntegrationNoticeDisappearsWhenPagesNoLongerMissing` 就是拦这个的。

### 4.2 列名刻意窄

否决了「JSON 列 + `code` 字段」的方案：一个 `code` 字段的存在本身就在邀请第二种、
第三种警告搭便车，而它们从来没被设计过——那正是 FR-011 禁止的通用警告框架的种子。
`unextracted_pages` 只能装一件事，第二种警告想复用它会立刻显得荒谬，只能自己开一列，
而开列这个动作会强制一次真正的设计讨论。

**代价如实记**：将来真出现第二种警告时，需要一次真正的重构，而不是加个枚举值。

---

## 5. 变异测试

七项注入，**全部被对应用例抓住**（跑完全部还原，`git diff internal/db/gen` 确认干净）：

| 注入的缺陷 | 结果 |
|---|---|
| `MarkDocumentReady` 沿用旧 SQL（不写新列） | ✅ 失败 |
| **只在单查带出新列，列表那处漏掉** | ✅ 失败 |
| 写入但无缺页时不清除（保留旧值） | ✅ 失败 |
| 编码时不排序（依赖来源顺序） | ✅ 失败 |
| 纯扫描件改判为「成功但有提示」 | ✅ 失败 |
| DTO 漏掉字段拷贝 | ✅ 失败 |
| 非 PDF 也写提示 | ✅ 失败 |

⭐ 第二项是本功能最容易「测试全绿但功能没生效」的地方：`documents.sql` 有**五处** `SELECT`
（规格里估的是三处，实际清点是五处），漏掉「列表」那一处的话单查用例照样绿，
而**用户唯一能看到提示的地方恰恰是列表**。SC-001 的断言因此被要求必须走列表路径。

---

## 6. 已知缺陷与边界（不得包装）

### 6.1 ⚠️ 提示只在文档列表可见，对话里仍然只会得到"检索不到"

用户在对话里问那 5 页的内容时，**检索仍然什么都召不回来，模型仍然可能拿着不完整的
上下文作答**。本功能让他能在**上传后预先知道**，不改善那个体验。
静默缺失在**对话链路上并没有被解决**。

### 6.2 ⚠️ 被 reconciliation 恢复的文档拿不到提示

`ReconcileStuckDocuments` 接手的是一份卡在 `publishing` 的文档，**它没有重新解析过文件**，
因此不知道这次处理有没有缺页，只能写 `NULL`。

这在语义上**不是撒谎**——`NULL` 的定义本来就是"没有缺页**或者**不知道"——
但结果是：一份实际缺页、却由 reconciliation 完成发布的文档，用户看不到提示。
要消除它得让缺页页码在 `markDocumentPublishing` 阶段就落库，那是另一次写入和另一个
覆盖时机，超出本期范围。

### 6.3 ⚠️ 「页面解析失败」不产生提示，只有「无文本层」会

一个页面进不了知识库有**两种**方式：无文本层（扫描图），以及**解析时让 `rsc.io/pdf` panic**
（006 已把 panic 兜住，逐页跳过）。**只有前者会产生提示。**

实测：一篇 15 页的 arXiv 论文，第 1 页解析失败被跳过，`textLayerCoverage` 看到的是
剩下 14 页且全部有文本 → `missing` 为空 → **无提示**。用户不知道第 1 页没进去。

这是本期范围划分留下的口子，不是实现错误——但它意味着 **SC-001 覆盖的是两种缺失里的一种**。

### 6.4 存量文档不追溯标记

`NULL` 同时表示"这次没缺页"和"从未被本功能上线后的版本处理过"，**系统不区分**——
因为它确实不知道：一份从未重新处理过的文档当初有没有缺页，那个信息在当时的抽取阶段
就没被记录，凭空给它任何值都是编造。用户对某份文档存疑时，重新处理它即可。

### 6.5 失败后字段会残留旧值（已被前端正确处理，但值得知道）

集成测试实测确认：一份曾经成功、带提示的文档在重新处理**失败**后，
`unextracted_pages` 里**仍然留着上一次的值**（`[2]`）——这次处理根本没走到 `MarkDocumentReady`。

前端因此**必须按 `status` 判断要不要展示，而不是按字段有没有值**（契约 C5）。
这不是理论风险，是实测出来的行为，已被 `TestIntegrationFailedReprocessDoesNotShowStaleNotice` 记录。

### 6.6 明确不做

OCR、视觉检索、通用文档处理警告框架、通知中心、提示历史。

---

## 7. 复现

```bash
make app-down && make db-up
make migrate-up
go test ./... -race -count=1
make eval-retrieval-gate
cd web && npx tsc --noEmit
```
