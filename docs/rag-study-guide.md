# Hify RAG 学习与面试指南

> Hify 是 AI 辅助完成的学习项目。本指南的目标不是把项目包装成生产系统，而是帮助你沿真实代码讲清楚：RAG 怎样接入、关键节点在哪里、每次优化解决了什么问题、怎样用测试证明它有效。

## 1. 先用一句话说明 Hify 的 RAG

Hify 把上传的文档解析、分块并写入 PostgreSQL/pgvector；用户对话时，同时用向量相似度和 pg_trgm 关键词召回候选，经 RRF 融合、证据准入、精确内容去重和邻接窗口扩展后，在字符预算内把证据注入模型上下文，并返回可追溯的 Citation。

这句话包含四段：

1. **写入**：文档解析 → 分块 → Embedding → 持久化。
2. **召回**：向量检索 + 关键词检索。
3. **筛选和补全**：RRF → 准入 → 去重 → topK → 邻接块。
4. **使用证据**：预算过滤 → 注入上下文 → Citation。

## 2. 最重要的代码入口

第一次读代码不要从所有文件开始，先跟下面两个入口。

### 2.1 文档写入入口

- 主入口：`internal/knowledge/service.go` 的 `ProcessDocument`
- 文件解析：`internal/knowledge/parser.go`
- 文档分块：`internal/knowledge/chunker.go`
- Embedding 与落库编排：仍在 `ProcessDocument`
- PostgreSQL 查询：`internal/db/pgqueries/chunks.sql`

主流程可以记成：

```text
上传文档
  → 异步任务调用 ProcessDocument
  → 解析 txt / md / pdf
  → 按结构切成 chunks
  → 批量生成 embedding
  → 写入并发布当前 document_version
```

关键边界：

- Markdown 是结构感知的行级启发式解析，不是完整 CommonMark。
- PDF 依赖文字层，不做 OCR；扫描版 PDF 会失败。
- 单个结构超过大小限制时，会回退到定长切分。
- `document_version` 防止文档重新处理后检索或邻接查询混入旧版本。

### 2.2 对话检索入口

- 对话接入点：`internal/conversation/context.go` 的 `assembleContext`
- 检索总入口：`internal/knowledge/service.go` 的 `Retrieve`
- Hybrid Search/RRF：`internal/knowledge/hybrid.go` 的 `rrfFuse`
- 证据准入：`internal/knowledge/admission.go` 的 `admitBySourceSignal`
- 内容去重：`internal/knowledge/dedup.go` 的 `dedupExactContentChunks`
- 邻接扩展：`internal/knowledge/neighbor.go` 的 `expandWithNeighbors`
- 上下文预算：`internal/conversation/budget.go` 的 `selectEvidence`
- 来源注入：`internal/conversation/context.go` 的 `formatRetrievedSources`

真实调用顺序：

```text
assembleContext
  → knowledge.Service.Retrieve
  → 按 embedding model 分组生成 query vector
  → pgvector 向量召回 ┐
                        ├→ RRF 排序
  → pg_trgm 关键词召回 ┘
  → 按两路原始分数做证据准入
  → 精确正文去重
  → 截断到 topK 个核心块
  → 一次批量查询所有核心块的前后邻接块
  → 核心块优先的二次正文去重
  → selectEvidence 按预算筛选
  → 注入 <retrieved_sources>
  → 模型回答并输出 [S1] 等引用
```

## 3. Phase 1–8 分别解决了什么

### Phase 1：Eval 基线与结构感知分块

问题：没有固定评测就无法判断优化到底变好还是变坏；粗暴定长分块还可能破坏标题、段落、列表、代码块和 PDF 页码。

做法：先建立可重复的评测基线，再让 txt、Markdown、PDF 使用各自更合适的结构边界分块，并保留标题、页码等来源信息。

要点：**先建立尺子，再优化系统。**

报告：`docs/eval-phase1-baseline-report.md`

### Phase 2：修复 Chunk 长度不变量

问题：结构感知不等于可以无限长。一个超长段落、代码块或表格仍可能突破 `chunk_size`，影响 embedding、数据库和上下文预算。

做法：所有结构最终都要满足统一长度上限；单个结构过长时回退定长切分。

要点：**结构质量不能破坏系统硬约束。**

报告：`docs/eval-phase2-chunk-size-fix-report.md`

### Phase 3：Hybrid Search

问题：纯向量检索擅长语义，但可能漏掉编号、专有名词和精确字符串；纯关键词检索又不理解语义。

做法：

- pgvector 负责语义召回。
- pg_trgm `word_similarity` 负责字符级关键词召回。
- 两路各取 `candidateK` 个宽候选，再通过 RRF 按排名融合。

为什么用 RRF：向量 cosine similarity 和关键词 word similarity 不是同一量尺，直接把原始分数相加没有可靠含义；RRF 只利用各自排名，更容易组合异构检索器。

注意：RRF 的 `fusionScore` 只负责排序，不代表真实相关度；对外 `RetrievedChunk.Score` 保留原始相关度。

报告：`docs/eval-phase3-hybrid-search-report.md`

### Phase 4：Neighbor Window Retrieval

问题：命中的 chunk 可能只有一句结论，定义、条件或上下文在前后块。

做法：核心命中确定后，再补充同一文档、同一版本的前后块。

关键设计：输出不是 `核心1、邻居1、核心2`，而是“所有核心块在前，所有邻接块在后”。因为对话层按输入顺序消耗预算，这样预算紧张时，邻接内容不会挤掉排名较低但仍然重要的核心命中。

报告：`docs/eval-phase4-neighbor-window-report.md`

### Phase 5：Exact Content Dedup

问题：不同 chunk ID 可能包含相同正文，重复注入会浪费预算，也会产生重复引用。

做法：对正文做保守归一化后精确去重，仅处理 CRLF、首尾空白和行尾空格；不做模糊或语义去重，避免误删只有细微但重要差异的内容。

顺序：核心候选要先去重再 topK，才能让后面的不同内容补位；邻接扩展后再做一次去重，并始终让核心块胜过重复邻接块。

报告：`docs/eval-phase5-content-dedup-report.md`

### Phase 6：确定性检索门禁

问题：普通单测只能证明局部函数，不能证明完整 `Service.Retrieve` 在真实数据库下没有退化。

做法：使用真实 MySQL、PostgreSQL、pgvector、pg_trgm 和 fake embedding，固定测试数据与问题，计算 Hit@1、Hit@3、MRR 和 ContentUniqueRate；任一指标退化就让测试失败。

要点：这里的 Hit@K 是“可接受结果是否出现在前 K”，不能包装成多相关文档语义下的 Recall@K。

报告：`docs/eval-phase6-retrieval-gate-report.md`

### Phase 7：批量邻接查询

问题：如果每个文档版本分别查询邻接块，一次检索会产生 N+1 数据库往返。

做法：把所有核心块需要的 `(document_id, document_version, chunk_index)` 坐标展开、去重后，一次批量查询。

失败策略：批量查询失败时返回核心块，不因为补充上下文失败而让整个对话失败。

报告：`docs/eval-phase7-batch-neighbor-report.md`

### Phase 8：Evidence Admission

问题：RRF 只能给候选排序。即使所有候选都不相关，也一定有第一名；如果直接取 topK，就会把“坏结果里相对最好的一条”注入模型。

做法：在 RRF 排序后，使用候选各自来源路径的原始分数进行准入：

- vector cosine similarity ≥ `0.35`，通过；
- keyword word similarity ≥ `0.45`，通过；
- 两路任意一路通过即可；
- 两路已有的信号都未通过，则拒绝。

顺序必须是：

```text
RRF 排序 → 准入 → 正文去重 → topK → 邻接查询
```

这样被拒绝的候选不会占 topK，也不会触发无意义的邻接查询。全部候选被拒绝时，`Retrieve` 返回空结果；对话仍可调用模型正常回答，但不注入知识库内容，也不生成知识库引用。

报告：`docs/eval-phase8-evidence-admission-report.md`

## 4. 最容易讲错的几个概念

### 4.1 RRF 分数不是相关度

RRF 根据“第几名”计算融合分。它回答的是“综合两路排名后谁应该在前面”，不回答“这个候选是否真的相关”。因此不能拿 RRF 分数和 `0.35` 这样的向量阈值比较。

### 4.2 `RetrievedChunk.Score` 也不是所有场景下同一种含义

- 核心块：来自向量或关键词路径的真实相关度；双路命中时取较强信号。
- 邻接块：没有独立检索分数，继承所属核心块的 Score，只用于维持预算优先级。

不要把邻接块的 Score 说成它自己的 cosine similarity。

### 4.3 pg_trgm 不是 BM25

Hify 的关键词检索是 PostgreSQL `pg_trgm` 字符 trigram/word similarity，不是基于词频、逆文档频率的 BM25。它部署简单、中英文都能做字符匹配，但长文本相关性和传统全文检索能力不等同于 Elasticsearch/BM25。

### 4.4 Hit@K 不是这里的 Recall@K

当前门禁每个问题配置一个可接受结果集合，Hit@K 只判断前 K 是否出现其中之一。真正的 Recall@K 通常要求知道该查询的全部相关文档，并计算召回比例。

### 4.5 固定阈值不是经过真实业务校准的最优值

`0.35/0.45` 是学习项目里的固定设计假设，测试证明代码正确执行了门槛，不证明它们适合所有业务数据。生产环境需要标注真实正负样本，观察 precision/recall 后校准。

## 5. 故障降级与系统边界

| 故障 | Hify 的处理 | 原因 |
|---|---|---|
| 某个 KB ID 无效或已删除 | 跳过该 KB | 单个错误不应中断对话 |
| 某个 embedding model 调用失败 | 跳过对应向量路径，关键词路径继续 | Hybrid Search 的两路故障隔离 |
| 向量查询失败 | 关键词候选仍可返回 | 保留部分检索能力 |
| 关键词查询失败 | 向量候选仍可返回 | 同上 |
| 邻接批量查询失败 | 降级为只返回核心块 | 邻接块只是补充上下文 |
| 全部候选低于准入门槛 | 返回空证据，模型正常回答 | 宁可少召回，不注入不相关知识 |
| context 取消或超时 | 立即向上返回 | 不能把用户取消误装成“没有结果” |

这里最值得学习的是：**可选增强失败时降级，主请求取消时传播。** 两者不能混为一谈。

## 6. 测试如何证明这套优化有效

测试分为四层：

1. **纯函数单测**：RRF、准入、去重、邻接组装和指标计算，不依赖数据库。
2. **编排 spy 测试**：确认邻接查询正常路径只调用一次、空输入零调用、错误时降级。
3. **真实数据库集成测试**：验证 pgvector、pg_trgm、版本隔离、发布状态和批量 SQL。
4. **端到端检索门禁**：从公开的 `Service.Retrieve` 进入，固定九个 case，防止后续改动让关键指标退化。

常用验收命令：

```bash
go test -count=1 ./...
go test -race -count=1 ./...
make eval-retrieval-gate
go vet ./...
make check-deps
```

面试中不要只说“测试覆盖率很高”，应说清楚每一层证明什么、证明不了什么。

## 7. 面试时可以直接使用的项目讲解

### 7.1 两分钟版本

> Hify 是我用 AI 编程工具辅助完成的 Go + React 学习项目，我主要通过它理解 RAG 的完整工程链路，并结合测试和代码审核逐阶段学习。文档进入系统后，会按 txt、Markdown、PDF 的结构进行解析和分块，批量生成 embedding 后存入 PostgreSQL/pgvector。检索时并行走 pgvector 语义检索和 pg_trgm 关键词检索，再用 RRF 融合两路排名。
>
> 我重点学习和审核了几个容易被忽略的问题。第一，RRF 只能排序，不能证明候选真的相关，所以融合后还要根据两路原始分数做证据准入；没有合格证据时不向模型注入知识库。第二，核心块可能缺少上下文，所以会补同文档同版本的前后块，但所有核心块必须排在邻接块前面，避免上下文预算不足时邻接块挤掉核心命中。第三，对相同正文在 topK 前和邻接扩展后分别去重，减少重复内容与 Citation。最后，把邻接查询从按文档版本循环改成单次批量查询，消除 N+1。
>
> 验收上除了纯函数单测和真实数据库集成测试，还建立了从公开 Retrieve 接口进入的确定性检索门禁，覆盖语义命中、关键词命中、去重、无关查询返回空、准入后 topK 补位等九个场景。这个项目不是我独立手写的生产平台，但我能沿代码说明关键设计、失败降级和测试证据。

### 7.2 如果面试官问“为什么不用 Reranker”

> 这个项目的目标是先把无额外模型依赖的检索链路做完整，因此用了 Hybrid Search、RRF 和固定证据准入，没有引入 Cross-Encoder 或 LLM Reranker。好处是行为确定、成本低、容易离线测试；缺点是复杂语义排序能力有限。如果有真实业务数据和延迟预算，我会先用当前门禁建立基线，再评估只对准入后的少量候选做 Rerank，而不是一开始就增加模型调用。

### 7.3 如果面试官问“阈值为什么是 0.35/0.45”

> 这两个值是学习项目中的固定初始门槛，向量和 pg_trgm 使用不同阈值，因为它们不是同一量尺。当前测试证明边界逻辑和过滤顺序正确，但没有真实业务标注集，所以我不会声称它们是最优值。生产环境会收集正负查询样本，以 precision 优先或业务目标为约束，分别校准两路门槛。

### 7.4 如果面试官问“没有检索结果怎么办”

> Retrieve 返回空结果和 nil error，对话仍然调用模型，但不添加 retrieved sources 和 Citation 规则。这样既不会因为知识库没有答案让整个对话失败，也不会把低相关内容当事实注入。真正的请求取消或超时则必须传播，不能降级为空结果。

## 8. 你需要重点读懂的代码

建议按下面顺序学习，每次只读一个主题：

1. `internal/knowledge/service.go`：只读 `Retrieve`，画出主流程。
2. `internal/knowledge/hybrid.go`：理解 RRF 排名与原始 Score 为什么分开。
3. `internal/knowledge/admission.go`：理解“任一路达标”和缺失信号的区别。
4. `internal/knowledge/dedup.go`：理解为何准入在去重前、去重在 topK 前。
5. `internal/knowledge/neighbor.go`：理解核心块优先的两层输出。
6. `internal/conversation/budget.go`：理解证据如何真正进入上下文预算。
7. `internal/conversation/context.go`：理解证据注入和 Citation 的边界。
8. `internal/knowledge/eval_gate_test.go`：从测试反向理解完整验收标准。

每读完一个文件，至少能回答三个问题：

- 这个模块接收什么、返回什么？
- 它解决哪个失败场景？
- 哪条测试能证明它没有破坏主链路？

## 9. 学完后的自测题

1. 为什么向量分数和关键词分数不能直接相加？
2. 为什么 RRF 第一名也可能不相关？
3. 为什么准入必须发生在内容去重之前？
4. 为什么内容去重要发生在 topK 之前？
5. 为什么核心块必须整体排在邻接块之前？
6. 邻接块的 Score 到底代表什么？
7. 为什么邻接查询失败可以降级，但 context 取消不可以？
8. pg_trgm 和 BM25 的区别是什么？
9. 当前 Hit@K 为什么不能称为严格的 Recall@K？
10. 真实生产环境应怎样校准证据准入阈值？

如果这十题都能结合 Hify 的函数和测试回答出来，就已经不是“知道 RAG 名词”，而是能讲清楚一条可运行、可降级、可验收的 RAG 工程链路。

## 10. 项目边界与诚实表述

- Hify 是 AI 辅助完成的学习项目，不声称由个人独立设计并手写全部实现。
- 当前 LLM/Embedding 的大量测试使用 fake/mock，不能声称验证了真实模型效果或生产成本。
- 固定测试集证明的是确定性回归和代码行为，不代表真实业务上的全局检索质量。
- 没有生产流量、真实标注数据和长期运行指标，因此不要虚构 QPS、准确率提升百分比或延迟收益。
- 可以诚实强调：你参与了需求拆分、方案取舍、AI 协作、代码审核、测试验收，并能沿真实代码解释实现。
