# Phase 15 · 解析失败的页面也要能被用户看见（008）验收报告

**规格**：[specs/008-unparseable-page-notice/](../specs/008-unparseable-page-notice/) ｜ **日期**：2026-09-05

> ⚠️ **本功能不修复任何页面。** 它不让任何无法解析的页变得能解析、不让任何内容进入知识库。
> 它只做一件事：把**第二种**已经发生的缺失说出来。

---

## 1. 为什么会有这一期

007 建立了「成功但有提示」通道，但它**只覆盖了两种缺失里的一种**：

| 缺失方式 | 007 之后 | 用户的下一步 |
|---|---|---|
| 页面无文本层（扫描图） | ✅ 有提示 | 做 OCR 后重传 |
| 页面**解析失败**被跳过 | ❌ **完全不可见** | **OCR 没用**，换工具重新导出 |

第二种来自 006 给 `rsc.io/pdf` 加的逐页 recover：那个库遇到损坏或不受支持的页面结构会 panic，
兜住之后这一页被跳过——**于是它在统计"有没有文本层"之前就已经消失了**。

⭐ **这一期不是规划出来的，是 007 的 FR-011 拦出来的。** 第一反应是把解析失败的页也塞进
`unextracted_pages`（列名字面上覆盖得住）。FR-011 挡住了这个动作，逼出一个问题：
它们真的是同一件事吗？不是——两种原因的**下一步动作完全不同**，一条消息服务不了两种；
要区分就得记"哪些页是哪种原因"，那正是 FR-011 禁掉的通用 `code` 字段。
**一条反抽象约束在一天之内拦下了一次真实的搭便车。**

---

## 2. 逐条成功标准

| SC | 结论 | 证据 |
|---|---|---|
| **SC-001** 解析失败的页能被看见 | ✅ | `TestIntegrationUnparseablePageNoticeReachesDocumentList` 改动前 **FAIL**（§3）、改动后 PASS；**真实 arXiv 论文**实测 `unparseable=[1]`（§4） |
| **SC-002** 下一步动作不是 OCR | ✅ | 悬浮第二段明写「**OCR 对它没有用**；请用其他 PDF 工具重新导出后上传」 |
| **SC-003** 两类不被混为一谈 | ✅ | `TestIntegrationBothFailureKindsStaySeparate`：一份两类都有的文档，分别断言 `[2]` 与 `[4]` |
| **SC-004** 两类重叠率 0 | ✅ | 同上用例逐页断言 |
| **SC-005** 恢复路径提示不丢 | ✅ | `TestIntegrationNoticeSurvivesReconciliationRecovery`——**这条在 007 的实现上是 FAIL 的** |
| **SC-006** 无缺失时呈现逐字节一致 | ✅ | 两类均空时渲染分支不进入 |
| **SC-007** 整份失败仍是失败 | ✅ | 006 的 `ErrPDFUnreadable` / `ErrPDFNoTextLayer` 用例未改动仍绿；变异测试确认改判会被抓住 |
| **SC-008** 非 PDF 与存量 0 条 | ✅ | `TestIntegrationTxtAndMarkdownNeverCarryNotice`（**已扩展为断言两类**，见 §5）；dev 库存量计数为 0 |

**其它**：`go test ./... -race -count=1` 全绿 **0 SKIP**；`go vet`/`check-deps`/`tsc` 干净；
门禁与基线 **IDENTICAL**（本功能不碰检索，这是**证明**不是目标）。

---

## 3. SC-001 的改动前 FAIL

```
WARN knowledge: some pdf pages could not be parsed and were skipped
     unreadable_pages=[2] readable_pages=2

--- FAIL: TestIntegrationUnparseablePageNoticeReachesDocumentList
    解析失败的页码 = []，应当是 [2]——第 2 页根本没进知识库，而用户在列表上看不到任何迹象
    got=&{... Status:ready ChunkCount:2 UnextractedPages:[] UnparseablePages:[] ...}
```

日志知道第 2 页没进去；用户看到「就绪、2 个分片、两个提示列表都空」。

---

## 4. 真实 PDF 实测

`1706.03762`（Attention Is All You Need，15 页，第 1 页是 arXiv 封面、解析失败）：

```
status=ready chunks=142 unextracted=[] unparseable=[1]
```

007 上同一份文档得到的是 `unparseable` 这个概念根本不存在、`unextracted=[]`——
即**一份"就绪、无提示"的文档，而第 1 页不在里面**。

---

## 5. 设计上的两处要点

### 5.1 写入前移到 `MarkDocumentPublishing`（同时修 007 §6.2）

007 把写入放在 `MarkDocumentReady`，理由是"那是一个版本成为 ready 的唯一入口"。
对，但它留了个洞：`ReconcileStuckDocuments` 恢复一份卡在 publishing 的文档时调的也是这一条，
而它**没有重新解析过文件**，只能传 nil——于是恢复路径完成的文档提示被清空。

008 把写入前移一步。理由写在 `MarkDocumentPublishing` 自己的注释里：

> 「这一步"锁定"了"活儿已经干完，只差发布"」

**缺页列表正是那个活儿的结果**，该和"活儿干完了"一起被锁定，而不是等到发布确认——
因为发布确认可能由一个不知情的恢复流程来做。这一步同样带 `version + status='processing'` 的
CAS，所以"提示属于赢下这轮的那次尝试"一点没丢；"每次整体覆盖、没有就写 NULL"也依然成立，
所以 007 的「重新处理后提示消失」仍然免费。

⚠️ **这次改动最危险的是只做一半**：前移写入却不删掉 `MarkDocumentReady` 的旧赋值，
恢复流程传 nil 就会把 publishing 阶段刚写对的值清空——**比改动前更糟，且完全静默**。
变异测试专门注入了这一项（§6 第 1 条）。

### 5.2 两列的重复是刻意接受的代价，以及**增生的判据**

编解码已经共用一份（`encodePageList`/`decodePageList`，与列名无关），但写入、读出、展示
各有两份近似代码。合并它们需要一个"原因"维度，那正是 FR-011 禁止的东西。

⭐ **判据必须复述在这里，否则下一个人会直接加第三列**：

> 新原因给用户的「下一步动作」与某个已有列**相同** → 并进那一列；**不同** → 才有资格
> 开自己的列，且**必须先写规格**。
> **列数到 3 是重新考虑形态的信号，不是继续加列的许可**——但在到达 3 之前不要提前抽象，
> 两列的重复远比一个为想象中的第 N 种原因设计出来的结构便宜。

---

## 6. 变异测试

| 注入的缺陷 | 结果 |
|---|---|
| ⭐ **只做一半**：`MarkDocumentReady` 把赋值加回来 | ✅ 失败（SC-005） |
| `MarkDocumentPublishing` 不写新列 | ✅ 失败 |
| 两类写反 | ✅ 失败 |
| 列表那处 SELECT 漏掉新列 | ✅ 失败 |
| `extractPDFPages` 不把跳过的页返回出来 | ✅ 失败 |
| 非 PDF 也写提示 | ❌ **逃逸** → 补断言后 ✅ |

**逃逸的那一条**：007 的非 PDF 用例只断言了 `UnextractedPages`，新列被误写照样绿。
已把它扩展为**两类都断言**。这是同一个教训第二次出现——**加一列时，既有的"这里应该是空"
类断言必须一并扩展**，否则新列天生没有看守。

---

## 7. 已知缺陷与边界（不得包装）

### 7.1 ⚠️ 提示仍然只在文档列表可见

用户在对话里问那几页的内容时，**检索仍然什么都召不回来，模型仍然可能拿着不完整的上下文作答**。
本功能（和 007 一样）让他能在**上传后预先知道**，**对话链路上的静默缺失依然没有解决**。

### 7.2 本功能不修复任何页面

`rsc.io/pdf` 对真实世界 PDF 的覆盖度才是根因，006 只加了一层围栏，008 只是把围栏内发生的事
说出来。换解析器仍是 006 research.md R1 权衡后**否决**的路线。

### 7.3 两类之外还有没有第三种缺失？

目前已知就这两种。若出现第三种，走 §5.2 的判据，**不要**直接加第四列。

### 7.4 明确不做

OCR、视觉检索、通用文档处理警告框架、修复无法解析的页。

---

## 8. 复现

```bash
make app-down && make db-up && make migrate-up
go test ./... -race -count=1
make eval-retrieval-gate
cd web && npx tsc --noEmit
```
