# Research: MIRACL 中文 Mini 检索评测

## R1：公开数据源

**Decision**：使用 MIRACL v1.0 中文开发集的 topics、qrels 与 corpus。

**Reason**：MIRACL 提供中文母语查询与人工相关性判断，qrels 使用标准 TREC 结构；许可为 Apache-2.0。它比自行生成问题更可信，也避免长期维护 Hify 私有题库。

**Sources**：

- https://huggingface.co/datasets/miracl/miracl/blob/main/README.md
- https://huggingface.co/datasets/miracl/miracl-corpus/tree/main/miracl-corpus-v1.0-zh

官方统计中中文 dev 为 393 条 query、3,928 条 relevance judgment，并明确说明 judgment 同时含正负标签；完整中文 corpus 为 4,934,368 个 passage。

**Rejected**：

- BEIR SciFact：规模适中、qrels 标准，但英文为主
- C-MTEB 完整 Retrieval：有中文 corpus/query/qrels，但常见子集约 10 万篇起，且各子集标注来源不完全一致
- 自建 Hify 50 问：维护成本高且容易产生 AI 自评偏差

## R2：为什么使用 Mini 子集

**Decision**：固定 50 query、500～1000 documents，目标约 800。

**Reason**：完整 MIRACL 中文 corpus 约 493 万篇，不适合个人机器频繁重建 Embedding。Mini 子集仍使用原始 qrels，但只用于同一子集上的纵向对比。

**Boundary**：Mini corpus 缩小了检索空间，分数通常不能代表完整 MIRACL 难度；禁止与官方 leaderboard 比较。

## R3：子集选择

**Decision**：从 dev 中对 query ID 做固定 seed 的稳定排序，选择前 50 条至少含一个 relevance > 0 的 query。保留这些 query 的全部正负 judgment 并对 document union 去重；不足 800 时，从 MIRACL corpus 按固定 hash 顺序补充干扰文档并标为 unjudged。若已评审文档本身超过 1000，则准备阶段失败并要求调整 query 数，不允许静默删除 judgment。

**Reason**：不读取模型结果选题，避免为当前 `bge-m3` 定制测试集；不删除任何 judgment，保持公开正负标注完整。额外 unjudged 文档只扩大检索空间，按标准 qrels 口径处理，并在报告披露其不是人工负例。

**Alternative rejected**：随机抽样但不保存 ID；无法复现。根据当前模型最难/最易结果选 query；会污染验收集。

## R4：评分粒度

**Decision**：公开指标按 document 评分。Hify 返回 chunk，先按首次出现的 document ID 去重，再取前 K 个唯一文档。

**Reason**：MIRACL qrels 是 document 级；同一文档多个 chunk 不应重复占用排名。

**Additional diagnostics**：保存首个 chunk rank、去重前 chunk 数和去重后 document 数，用来诊断分块/邻接造成的重复，但不改变公开指标。

## R5：指标

**Decision**：K = 1/3/5/10，计算：

- Recall@K：检出的相关文档数 / 该 query 全部正相关文档数
- Precision@K：TopK 中相关文档数 / K；返回不足 K 时缺失位置按不相关计
- MRR@K：第一个相关文档倒数排名，K 内未命中为 0
- AP@K / MAP@K：每次相关命中位置的 Precision 均值，再跨 query 平均
- NDCG@K：使用 qrels relevance 等级计算 DCG，并除以理想排序 IDCG

失败或空检索以零计；没有正 qrel 的 query 在准备阶段拒绝进入数据集。

## R6：执行边界

**Decision**：新增专用 benchmark runner，复用 knowledge 模块正常入库和 `Service.Retrieve`。数据准备、入库、查询、纯重算、比较拆成可恢复阶段。

**Reason**：直接用 MTEB/BEIR 只测 Embedding，不经过 Hify 的分块、pgvector/pg_trgm、RRF、准入、去重和 Reranker；复用 `make eval` 又会不必要地引入 Agent、聊天模型和 LLM Judge。

## R7：门禁策略

**Decision**：

- 现有 fake embedding 门禁继续作为每次提交的确定性回归门禁
- MIRACL Mini 作为检索方案变化时的真实效果评测
- 第一版只产基线和 delta，不设绝对质量阈值、不进 CI

**Reason**：真实本地模型依赖服务状态和模型 digest；先得到可信重复运行证据，才能决定合理容忍区间。

## R8：许可、缓存与隐私

**Decision**：仓库记录 Apache-2.0 来源、revision、选择配置和 ID 清单；完整上游缓存、Mini 正文、向量和运行中间产物 gitignore。报告只保存公开 query、document ID、排名和指标，不保存向量。

**Reason**：控制仓库体积；避免把大数据和向量误提交。MIRACL 本身是公开数据，但仍按最小制品原则处理。
