# Phase 13 · PDF 版面感知解析与跨页分块（006）验收报告

**规格**：[specs/006-pdf-layout-chunking/](../specs/006-pdf-layout-chunking/) ｜ **日期**：2026-09-05

> ⚠️ **先说这份报告不是什么**。本功能绝大部分结论是**机制证明**——代码路径加断言，
> 证明"这条链路现在是这样走的"。只有 SC-008 的 benchmark 属于**效果度量**，且它的口径是
> **抽取 + 分块阶段**，不是端到端入库。检索质量（召回率、答案正确率）**一个数字都没有度量**，
> 因为本仓库没有带 PDF 的评测集，硬造一个再拿它自证是自欺。两类结论不得混着读。

---

## 1. 改了什么

把 PDF 入库路径上「**页 = 语义边界**」这个前提拆掉，换成「**段落 = 语义边界，页只是它的坐标**」。

| # | 改动 | 落点 |
|---|---|---|
| 1 | 抽取阶段保留行级结构（文本 + 页码 + 字号 + Y + 渲染宽度） | `parse.go` 新增 `pdfLine` |
| 2 | 跨页重建段落流，页边界处三条判据决定是否续接 | 新建 `layout.go` |
| 3 | 剥离跨页重复的页眉页脚与纯页码行 | `layout.go` |
| 4 | 页码从**单值**变**区间**（`page_number` 收紧为起始页 + 新增 `page_end`） | pgmigration `000005`、`chunks.sql`、四处行映射、DTO、前端 |
| 5 | 元数据页码过滤从「点落区间」改为「**区间相交**」 | `chunks.sql` 两条召回查询 |
| 6 | 扫描件从"内容为空"升级为点名 OCR 的专用提示 | `errors.go` / `service.go` |
| 7 | 标题双信号交叉验证，标题路径**拼进片段正文**参与向量化（US4） | `layout.go` / `chunk.go` |
| 8 | ⭐ **范围外追加**：`rsc.io/pdf` 的 panic 被兜住，转成可行动的错误 | `parse.go` |

**依赖零新增**（R1 决策的可验证形态：`go.mod` diff 为空）。

---

## 2. 逐条成功标准

| SC | 结论 | 证据 |
|---|---|---|
| **SC-001** 跨页内容能被一次检索命中且形态完整 | ✅ | `TestChunkPDFCrossPageParagraphStaysWhole` 改动前 **FAIL**（输出见 §3）、改动后 PASS；`TestIntegrationCrossPageParagraphIsRetrievableWhole` 走真实 PG + 真实检索链路 |
| **SC-002** 页眉页脚出现次数为 0 | ✅ | `TestChunkPDFHeaderFooterNeverReachChunks`：6 页夹具，header/footer/页码行出现次数分别断言 `== 0` |
| **SC-003** 页码区间 100% 合法 | ✅ | 纯函数断言 + `chunks_page_range_valid` 数据库约束（六格真值表实测，见 §4）+ 三份真实 PDF 非法区间均为 0 |
| **SC-004** 同一 PDF 两次处理逐字节一致 | ✅ | 纯函数 200 次循环（含众数并列输入）+ 全链路 50 次 `chunkDocument` 比对 |
| **SC-005** 正文误删率 0 | ⚠️ **合格但有已知边界** | 六行穷举用例全过；真实论文上一度误删 20 行、已修（§5.2）；**病态行切分下仍会误删**（§6.2） |
| **SC-006** 非 PDF 行为完全一致 | ✅ | `make eval-retrieval-gate` 与改动前基线 **IDENTICAL**（14 条用例，逐字节，仅忽略 `ran_at`） |
| **SC-007** 扫描件提示可行动 | ✅ | `TestIntegrationScannedPDFIsDistinguishableFromAnEmptyFile`：两条文案确实不同，扫描件那条点名 OCR，且落到 `documents.error_message` |
| **SC-008** 抽取+分块耗时增幅 ≤ 50% | ✅ | 基线 11.5 ms/op → 改后 11.1 ms/op（40 页夹具，`-benchtime 20x -count 5` 取中位数）。**无可测量的回归** |

---

## 3. SC-001 的改动前 FAIL（宪法第 VI 条要求的硬证据）

一个改动前就能通过的验收标准证明不了任何事。改动前的输出：

```
--- FAIL: TestChunkPDFCrossPageParagraphStaysWhole
    no single chunk contains both halves of the cross-page paragraph;
    it was cut at the page boundary. chunks:
      [2] page=3 "...the retentionwindowmarker therefore applies to every archived record created before the"
      [3] page=4 "cutoff date and the quarterlyreviewmarker keeps it under review. ..."
```

chunk[2] 停在 `created before the`，chunk[3] 从 `cutoff date and` 起头。这就是 spec 问题陈述里
"两半各自缺失上下文，检索时相似度都不足以进入结果"和"模型会自行把句子补完"的实物。

---

## 4. 数据库约束的六格真值表（实测，非推演）

`chunks_page_range_valid` 是不变量 C1/C2 的强制点。**一个从未被触发过的约束和不存在的约束在证据上是等价的**，
所以六种情况各真跑一次 INSERT（全部在事务里 ROLLBACK，未写入任何数据）：

| `page_number` | `page_end` | 实测 |
|---|---|---|
| NULL | NULL | ✅ 通过 |
| 3 | 4 | ✅ 通过 |
| 3 | NULL | ❌ 拒绝 |
| **NULL** | **4** | ❌ 拒绝 ⭐ |
| 4 | 3 | ❌ 拒绝 |
| 0 | 0 | ❌ 拒绝 |

⭐ 第四格是唯一需要小心的：它靠 SQL 三值逻辑里 `FALSE AND NULL = FALSE` 成立。
**两个条件必须写在同一个 CHECK 里用 `AND` 连**——拆成两个独立约束时这一格会逃逸（`NULL` 视为通过）。

---

## 5. 变异测试：断言真的有牙齿吗

一组"测试全绿"本身不证明任何事，测试可能根本抓不到 bug。逐项注入缺陷、确认对应用例**确实失败**，
跑完全部还原（`git diff internal/db/pggen` 已确认干净）。

### 5.1 结果

| 注入的缺陷 | 结果 |
|---|---|
| 合并判定恒 `false`（退回按页切分） | ✅ 失败 |
| 合并判定恒 `true`（无条件合并所有页） | ✅ 失败 |
| 噪音判定恒 `true` | ✅ 失败 |
| 噪音三条判据从「与」改「或」 | ✅ 失败 |
| 行宽众数改回直接 `range` map | ✅ 失败（200 次循环） |
| 页码谓词加 `OR ... IS NULL`（下界侧、上界侧各一次） | ✅ 各自失败 |
| 下界谓词写回 `page_number >= min`（**只改一路**） | ❌ **逃逸** → 补测试后 ✅ |
| 四处行映射各漏掉一处 `page_end` | ❌ **逃逸 3 处** → 补测试后 ✅ |

### 5.2 两个真实盲区

**盲区 1 · 融合掩盖单路回归。** 把 `SearchVectorChunks` 的下界谓词单独改回点落语义，
`TestIntegrationPageFilterIntersectsChunkInterval` **照样通过**——它走公开的 `Retrieve`，
两路融合之后未被改坏的关键词路仍然把跨页片段带了回来。两处一起改才失败。
这是 002-metadata-filter 踩过的盲区换了个形状又出现一次：**经过融合的断言，对"只有一路出问题"是瞎的**。
补 `TestIntegrationPageFilterIntersectionAppliesToBothRecallPaths`，绕开融合直接分别打两条召回 SQL。

**盲区 2 · 四处行映射，三处无人看守。** `repository.go` 有四处要把 `page_end` 读出来，
逐处删掉那一行，**只有第一处被既有用例抓住**。逃逸的后果是静默的：那条路径的 `PageEnd` 恒为 nil，
违反 C1，前端按契约 R3 落到「—」——一个看起来正常的界面，背后是被悄悄丢掉的引用信息。
补 `TestIntegrationPageEndIsReadBackOnEveryRepositoryPath`，四条路径逐条断言。

---

## 6. 真实 PDF 验证（helper 造的夹具永远只覆盖 helper 作者想到的情形）

四份真实 PDF：两篇 arXiv 论文（`1706.03762`、`1810.04805`），
两份 macOS `cupsfilter` 生成的文档（正常散文 / 全篇无标点）。

### 6.1 ⭐ 最重要的发现：`rsc.io/pdf` 在真实 PDF 上 panic

**两篇论文都直接 panic**：

```
panic: malformed PDF: reading at offset 0: stream not present
  rsc.io/pdf.Page.Content(...)  →  knowledge.extractPDFPages
```

**这是既有缺陷，不是 006 引入的**——改动前同一个 `page.Content()` 调用一样裸着，
`internal/knowledge` 全包没有一处 `recover()`。后果是：用户上传一份普通的学术 PDF，
文档处理不是"失败并给出提示"，而是 panic。**006 修的是跨页切断，可这些文档根本进不到分块那一步。**

**已修（范围外追加，所有者明确批准）**：`safeNewPDFReader` / `safePageText` 把 panic 收进
新的 `ErrPDFUnreadable`（中文文案点名"重新导出"）。recover 刻意只包住 `rsc.io/pdf` 的两个调用，
**不得扩大到我们自己的代码**——那里的 nil 解引用是真 bug，应该继续响亮地崩。
逐页兜底，所以单页解析失败不会连累整份文档。

修完之后两篇论文都能正常入库：

| 文档 | 页数 | 无法解析 | 跨页合并 | 剥离 | 非法区间 |
|---|---|---|---|---|---|
| Attention Is All You Need | 15 | 第 1 页（arXiv 封面页） | 5 处 | 13 行 | 0 |
| BERT | 16 | 第 1 页 | 3 处 | 0 行 | 0 |
| 散文（页眉+页脚） | 6 | — | 0 处 | 12 行（6 页眉 + 6 页码行） | 0 |

⚠️ **这不代表那些 PDF"能用了"**：`rsc.io/pdf` 对真实世界 PDF 的覆盖度才是根因，本次一个字没动。
换解析器正是 research.md 的 R1 权衡后**否决**的路线。**这个改动是一层围栏，报告里必须按围栏来读。**

### 6.2 第二个发现：纯页码行规则误删正文

第一次跑真实论文时，**20 行被当成纯页码行删掉了**——学术 PDF 里满是"只有一个数字"的行：
抽取器单独成行的上下标、脚注标记、公式编号。它们是正文。这是**真实输入上的 SC-005 违反**。

**已修**：只有数字的行现在需要额外佐证——既要是该页**最外侧**那一行，数值也要落在文档**真实页数范围**内。
无歧义的形态（`第 3 页` / `Page 3 of 12` / `3 / 12`）不受此限。修后同一篇论文剥离 13 行，
正是第 2-14 页各自的页码，**零误删**。用例：`TestStripLayoutNoiseBareNumberNeedsCorroboration`。

---

## 7. 能力上限与已知缺陷（不得包装）

### 7.1 本方案明显弱于业界视觉版面方案

标题识别质量与复杂排版适应性**明显低于** Docling / MinerU 一类基于**视觉版面检测模型**的方案。
这是一个知情取舍（R1）：为两个不需要模型的 P1 能力引入一整条 Python 运行时依赖、破坏单二进制约束，
收益与代价不成比例。**不是"做到了同等效果"。**

实测佐证：Attention 论文 110 个片段全部识别出标题；BERT 论文 **0 个**——它的标题字号与正文差距
不足 `headingFontSizeRatio`，双信号交叉验证直接判为"识别不出"，按 FR-016 留空。
这是设计要的行为（宁可不给，不可编造），但也说明**US4 的召回率在真实文档上可以是 0**。

### 7.2 行切分的魔数没修，且会传导

`extractPDFPages` 的换行阈值 `lastY - t.Y > 2` 仍是个魔数，字号异常大或行距很密的文档上会切错。
**这个误差会直接传导到噪音判定**：实测中，`cupsfilter` 把长单词硬断成 `...was cr` / `eated`，
6 字符的 `"reated"` 在 91% 的页上重复、且短、且靠近页边——三条判据全中，**27 行正文被删**。
本次**没有解决**这个缺陷，不能因为周边变好了就不提。

### 7.3 ~~无句末标点的文档，页码区间会退化~~ → **已修（2026-09-05 后补）**

全篇无句末标点时，每个页边界的三条合并判据都成立，实测 10 页合并成 1 个单元，
34 个片段全部标成「第 1-10 页」。长度上限确实生效（FR-004 未破），但**引用价值归零**。
spec 的 Edge Case 只要求"不得合并成一个超长段落"，没要求"区间不得退化"——这条边界画得不够。

**修法**：给合并加一条跨度上限 `maxMergedPageSpan = 3`。合并的偏向本来由
"错误切断无法补救"这条不对称性支撑，但**不对称性是有限度的**：一次合并已经跨了
三页还没遇到任何一个句末标点时，证据的含义变了——这不再是"一个恰好很长的段落"，
而是一份根本没有标点的文档，它每个页边界满足三条判据的原因与段落无关。

**实测**：同一份病态文档从 **1 个跨 1-10 页的单元**变成 **4 个（各 ≤3 页）**；
两份真实论文的段落流数字**一个没变**（143 单元/5 处合并、24 单元/0 处合并），
上限对正常文档完全不可见。用例：`TestBuildParagraphStreamCapsMergeSpan` 与
`...CapDoesNotAffectRealParagraphs`。

⚠️ 这是给引用质量加了个下限，**不是把合并做得更准**：这类文档里的内容仍然会跨
两个页边界合并，仍然可能被标得略宽。

### 7.4 overlap 计入页码导致区间有意偏宽

片段开头的 overlap 种子来自上一个片段的尾部；如果那段文字来自第 3 页而本片段正文从第 4 页开始，
区间标为「3-4」。**这是决策不是 bug**：FR-010 要的是"实际覆盖"的页码，把区间做宽不是编造，
做窄反而会让用户拿着"第 4 页"去找一句印在第 3 页的话。同理，一个超长合并单元被回退切分时，
每个子片段继承整个单元的区间。

### 7.5 FR-018 本期**未满足** → 已由 007 补上（2026-09-05）

「部分页面无文本层时告知用户」整条推迟到下一期（所有者裁决，plan.md「三项拍板」决策 1）。
006 交付时的实际行为：有文本的页正常入库 + 一条结构化日志，**用户界面看不到任何提示**。
这是一个**被明示的未满足项**，不是"已满足"。

**后续**：007-document-processing-notice 补上了这条通道（`documents.unextracted_pages`
+ 文档列表提示），见 [eval-phase14-document-processing-notice-report.md](./eval-phase14-document-processing-notice-report.md)。
⚠️ 但 007 的报告 §6.3 记了一个本报告 §6.1 直接相关的口子：**只有"无文本层"的页会产生提示，
被 `safePageText` 的 recover 逐页跳过的"解析失败"页不会**——实测一篇 15 页 arXiv 论文
第 1 页解析失败被跳过，用户看不到任何提示。那是 008 的范围。

### 7.6 明确不做

多栏排版阅读顺序重建、跨页表格合并、OCR、视觉检索（ColPali 一类）。

---

## 8. 与既有功能的交叉影响

| 既有功能 | 影响 | 验证 |
|---|---|---|
| 002 页码过滤 | 语义改为区间相交 | 契约 §3.3 九格对照表逐格断言；单页片段九格行为与改动前**完全一致** |
| 存量已入库文档 | 一次 `page_end = page_number` 回填 | 实测 dev 库 C1 违反行数 = 0；等价性论证见 data-model §7.1 |
| 邻接窗口 | `page_end` 随来源元数据继承 | `TestExpandWithNeighborsKeepsNeighborsOwnPageInterval` 断言邻接块报的是**它自己**的区间 |
| Citation V1 | 无影响 | `message_citations` 不存页码 |
| 确定性检索门禁 | **逐字节不变** | IDENTICAL，14 → 14 条 |
| `make eval` | **全程未使用** | 它每条用例都调真实对话与裁判模型，同一份代码跑两次都不一致，用它证明"行为未变"得到的是噪音不是证据 |

---

## 9. 复现

```bash
make app-down && make db-up
make migrate-up
go test ./... -race -count=1
make eval-retrieval-gate
go test ./internal/knowledge/ -run '^$' -bench 'BenchmarkPDFExtractAndChunk' -benchtime 20x -count 5
```
