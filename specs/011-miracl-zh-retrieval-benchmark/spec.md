# Feature Specification: MIRACL 中文真实检索评测

**Feature Branch**: `011-miracl-zh-retrieval-benchmark`

**Created**: 2026-09-10

**Status**: Draft（待所有者接受后实施）

**Input**: 在不自行长期维护题库的前提下，引入带公开人工相关性标注的中文检索数据，使用 Hify 当前真实 `bge-m3` 检索链路计算 Recall、Precision、MRR、MAP 和 NDCG，用于比较 Embedding、分块、融合、准入与重排调整的真实效果。

## 问题陈述

Hify 现有 `make eval-retrieval-gate` 使用受控向量守代码回归，稳定但不能衡量真实 Embedding 效果；`make eval` 虽使用真实 `bge-m3`，只有两份文档和 14 条问题，且“命中任意可接受文档”不能计算严格的多相关文档 Recall@K。

本期不再自建一套需要长期人工维护的 Hify 题库，而是从 MIRACL v1.0 中文开发集确定性选取 50 条查询，并从其公开 qrels 涉及的文档构建约 800 篇的 Mini 子集。原始标注不改写，结果只称为“MIRACL 中文 Mini 子集表现”，不得与完整 MIRACL 排行榜横向比较。

## User Scenarios & Testing

### User Story 1 - 可复现地准备公开中文评测集 (Priority: P1)

开发者执行准备命令后，得到同一版本、同一查询和同一候选文档集合；来源、许可、选择规则和哈希可核对。

**Why this priority**: 数据集不稳定，后面的任何指标都不可比较。

**Independent Test**: 清空本地生成目录后连续准备两次，manifest、query IDs、document IDs、qrels 与内容哈希完全一致。

**Acceptance Scenarios**:

1. **Given** MIRACL v1.0 中文 topics/qrels/corpus 可访问，**When** 准备 Mini 子集，**Then** 选出固定 50 条 query，保留这些 query 的全部已评审 qrels 文档，并生成约 800 篇候选文档
2. **Given** 同一数据版本与选择参数，**When** 重复准备，**Then** 除生成时间外所有数据与哈希一致
3. **Given** 下载不完整、哈希不符、qrels 指向缺失文档或某条 query 没有正例，**When** 准备数据，**Then** 命令失败且不留下可被误用为完整数据的 manifest

---

### User Story 2 - 用真实 Hify 检索链路计算标准指标 (Priority: P1)

开发者使用本地真实 `bge-m3` 将 Mini 语料入库，通过 `knowledge.Service.Retrieve` 执行查询，得到文档级标准检索指标和逐条结果。

**Why this priority**: 只直接跑 MTEB/BEIR 不能证明 Hify 自己的分块、数据库召回、融合、准入和重排行为。

**Independent Test**: 完整运行 50 条 query，报告能由保存的 run 结果和 qrels 离线重算，且两次重算逐字段一致。

**Acceptance Scenarios**:

1. **Given** Mini 数据和可用的 `bge-m3`，**When** 运行 benchmark，**Then** 文档使用真实 Embedding 入库，查询走公开 `Retrieve` 入口，不调用聊天模型或 LLM Judge
2. **Given** Hify 返回同一文档的多个 chunk，**When** 计分，**Then** 按首次出现顺序去重为 document ranking，同一文档不得重复占用 K 个名额
3. **Given** qrels 含多个相关文档或分级相关性，**When** 计分，**Then** 输出 Recall@K、Precision@K、MRR@K、MAP@K、NDCG@K，并保存 TP/相关文档数等可审计原始计数
4. **Given** Embedding 请求、数据库、入库或查询失败，**When** 汇总，**Then** 失败 query 留在分母并记录错误；整次报告不得伪装为通过

---

### User Story 3 - 可比较两次检索方案 (Priority: P2)

开发者可以把当前 run 与同数据、同模型版本、同语料和同评分口径的 baseline 比较，明确哪些指标上升、下降或不可比较。

**Why this priority**: 优化的价值来自同条件对照，而不是孤立分数。

**Independent Test**: 用相同 run 自比得到 IDENTICAL；修改任一结果排名后能稳定报告对应指标变化。

**Acceptance Scenarios**:

1. **Given** 两份兼容报告，**When** 比较，**Then** 输出各项 delta 和逐 query 退化/改善列表
2. **Given** 数据版本、query/doc 集合、qrels、模型 digest、维度或评分口径不同，**When** 比较，**Then** 明确拒绝比较，而不是输出误导性 delta
3. **Given** 尚未跑出可靠基线，**When** 执行 benchmark，**Then** 只报告当前表现，不预设绝对通过阈值

### Edge Cases

- 公开 qrels 引用的正例必须全部进入 Mini corpus；容量冲突时减少负例，不得删除正例
- 未被 qrels 标注的文档按 TREC/BEIR 口径视为不相关，但报告必须披露“未标注不等于已人工判错”
- 文档切成多个 chunk 后，只以 document ID 参与公开 qrels 评分；另报原始 chunk 数和文档去重数
- 某条 query 的全部检索结果被准入阈值过滤时，Recall/MRR/MAP/NDCG 均按零计，不得排除
- 本地缓存可删除后重建；仓库不提交 MIRACL 大体积语料

## Requirements

### Functional Requirements

- **FR-001**: MUST 固定数据源为 MIRACL v1.0 中文开发集，并记录来源 URL、许可、上游版本或 revision
- **FR-002**: MUST 用确定性规则选择 50 条含至少一个正 qrel 的查询；选择规则与 seed 写入配置
- **FR-003**: MUST 保留选中 query 的全部已评审文档（relevance > 0 正例与 relevance = 0 负例）；不足目标时按固定规则加入 MIRACL corpus 干扰文档并显式标记 unjudged，最终文档数 MUST 在 500～1000 之间，目标 800。任何已评审项不得因容量被删除
- **FR-004**: MUST 保存 query/document/qrels 清单及 SHA-256；缺失引用、重复 ID 冲突或哈希不一致 MUST 失败
- **FR-005**: MUST 将完整下载缓存和生成语料放入 gitignored 路径；仓库只保存配置、manifest schema、固定 ID 清单、脚本/代码和小型报告
- **FR-006**: MUST 使用真实 `bge-m3:567m` 或显式指定的真实 Embedding 模型；报告保存模型名、digest、维度、服务地址脱敏标识
- **FR-007**: MUST 通过 Hify 的正常文档解析/分块/Embedding/发布能力建立隔离知识库，不得直接把上游向量塞入 pgvector
- **FR-008**: MUST 通过 `knowledge.Service.Retrieve` 运行每条 query；不得直接调用第三方向量库绕过 Hify
- **FR-009**: MUST 将 chunk ranking 稳定折叠为 document ranking，保留首次出现的文档并记录来源 chunk/rank
- **FR-010**: MUST 计算 Recall、Precision、MRR、MAP、NDCG，K 固定为 1、3、5、10；实现须有不依赖数据库和模型的纯函数测试
- **FR-011**: MUST 保存逐 query 排名、qrels、指标、错误及汇总；报告不得保存 Embedding 向量
- **FR-012**: MUST 支持从已保存 run 纯重算指标，纯重算不得调用模型或数据库
- **FR-013**: MUST 支持兼容性检查和 baseline 比较；不兼容报告 MUST 拒绝输出 delta
- **FR-014**: MUST 将失败 query 保留在指标分母；任何未处理 query 都使 run 标记 incomplete
- **FR-015**: MUST 保留现有 `make eval-retrieval-gate` 且其报告除 `ran_at` 外逐字段不变
- **FR-016**: MUST 新增独立命令入口；准备、入库、运行、重算和比较步骤可分别恢复，不因最后一步失败重做全部 Embedding
- **FR-017**: MUST 记录准备、Embedding、入库、查询各阶段耗时、文档数、chunk 数和查询数；本地模型成本标记 not_applicable，不写 0 美元
- **FR-018**: MUST NOT 调用聊天模型、生成答案或 LLM Judge
- **FR-019**: MUST NOT 声称 Mini 分数等同完整 MIRACL 或生产中文 RAG 表现
- **FR-020**: MUST NOT 在首版加入 CI、Web UI、定时任务、自动参数搜索或模型微调

### Key Entities

- **Benchmark Dataset Manifest**：上游版本、选择规则、query/doc 清单、qrels 和哈希
- **Benchmark Corpus Document**：保留 MIRACL document ID 与正文，映射到 Hify document ID
- **Qrel**：query ID、document ID、relevance
- **Retrieval Run**：配置快照、每条 query 的 document ranking、错误和阶段耗时
- **Metric Report**：逐 query 与总体的标准指标
- **Comparison Report**：两份兼容 run 的指标变化和退化/改善 query

## Success Criteria

- **SC-001**: 同一上游版本与配置重复准备，固定字段和全部内容哈希完全一致
- **SC-002**: Mini 集含 50 条中文 query、目标 800 篇且始终在 500～1000 范围内的文档，所有正负 qrel 均能解析到 corpus 文档，额外干扰文档明确标为 unjudged
- **SC-003**: 50 条 query 全部通过真实 `bge-m3` 与 Hify `Retrieve` 执行；失败和未处理数量均明确报告
- **SC-004**: Recall/Precision/MRR/MAP/NDCG 的构造样本单测覆盖单正例、多正例、分级相关、重复文档、空结果和失败 query
- **SC-005**: 同一保存 run 连续重算两次，除生成时间外逐字段一致
- **SC-006**: 相同 run 自比输出 IDENTICAL；数据/模型/口径不兼容时比较命令非零退出
- **SC-007**: 现有确定性检索门禁 14 条 case 无 skip，四项指标保持 1.0，报告除 `ran_at` 外 IDENTICAL
- **SC-008**: 阶段报告明确区分机制验证、Mini 数据真实效果与完整 MIRACL/生产边界

## Assumptions

- 本机 M1 Pro 32GB、约 440GB 可用空间，已安装 `bge-m3:567m`；800 篇规模适合本地学习
- 首次构建允许下载较大的 MIRACL 中文 corpus 并扫描所需 document ID，最终 Mini 缓存远小于完整 corpus
- MIRACL 中文 qrels 由母语标注者产生；Hify 不修改这些标签
- 第一版先建立可信基线，不拍脑袋设“必须达到多少”的绝对质量阈值
