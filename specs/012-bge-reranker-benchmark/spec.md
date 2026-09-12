# Feature Specification: BGE Reranker 真实对照评测

**Feature Branch**: `012-bge-reranker-benchmark`  
**Created**: 2026-09-10  
**Status**: Approved（Phase 6 待实施）  
**Input**: 保持 011 MIRACL 中文 Mini 基线的语料、Embedding、分块和 Hybrid Search 配置不变，接入真实 `BAAI/bge-reranker-v2-m3`，验证它是否改善排序且不损害召回。

## 背景与模型选择

011 的 `Recall@10=0.976`，但 `MRR@10=0.626746`、`NDCG@10=0.696447`。50 条查询中，23 条首个相关文档排第 1，26 条排第 2～10，1 条前 10 未命中，主要问题是排序而非召回。

首选 `BAAI/bge-reranker-v2-m3`，依据是 FlagEmbedding 官方将其列为适合中文、多语言与效率部署的轻量 Cross-Encoder；RAGFlow 的本地 HuggingFace Rerank 实现默认使用该模型；模型许可为 Apache-2.0。`Qwen3-Reranker-0.6B` 仅作为以后独立实验的候选，本期不同时比较多个模型。

## User Scenarios & Testing

### User Story 1 - 运行真实本地 Rerank（Priority: P1）

开发者可以在现有 Hify Rerank 接口后运行本地 `bge-reranker-v2-m3` 服务，并确认实际请求经过模型，而不是 fake、规则排序或静默降级。

**Why this priority**: 没有真实模型调用，任何排序提升都不构成模型效果证据。

**Independent Test**: 启动模型服务并对固定 query/document 对调用 `/rerank`，响应返回完整、可验证的逐文档分数；Hify 真实运行记录模型标识、digest、调用数、耗时和降级数。

**Acceptance Scenarios**:

1. **Given** 模型已下载且服务可用，**When** 执行预检，**Then** 验证模型名、revision/digest、Apache-2.0 许可、响应契约和非恒定分数
2. **Given** 模型服务不可达、超时或响应不完整，**When** 执行评测，**Then** 本次 Rerank run 标记 incomplete，不得用 baseline 顺序冒充 Rerank 结果
3. **Given** 50 条查询执行完成，**When** 查看报告，**Then** 模型调用数与查询数、成功数、失败数、降级数能够互相核对

---

### User Story 2 - 与 011 做单变量 A/B 对照（Priority: P1）

开发者可以在完全相同的 MIRACL Mini 数据、Embedding、分块、候选召回与指标口径下，对比关闭和开启 Rerank 的结果。

**Why this priority**: 只有单变量对照才能把变化归因于 Reranker。

**Independent Test**: 使用 011 保存的兼容 baseline 与本次 Rerank 报告比较；除 Rerank 配置和代码 revision 外，其余兼容指纹完全相同。

**Acceptance Scenarios**:

1. **Given** 011 baseline 与新的 Rerank run，**When** 比较，**Then** 输出 Recall/MRR/MAP/NDCG @1/@3/@5/@10 的总体 delta 和逐 query 变化
2. **Given** 数据、qrels、语料、Embedding digest、分块、候选召回配置或指标版本不同，**When** 比较，**Then** 拒绝正式 A/B 结论
3. **Given** 仅 Rerank 配置不同，**When** 执行专用 A/B 比较，**Then** 允许比较并明确标记唯一实验变量

---

### User Story 3 - 给出采用或拒绝结论（Priority: P2）

开发者可以依据预先固定的门禁判断是否采用该模型，而不是看到结果后修改标准。

**Why this priority**: 实验的价值是支持决策，不是强行证明模型有效。

**Independent Test**: 对构造出的“排序提升”“Recall 下降”“发生降级”三类报告运行决策函数，分别得到确定结果和原因。

**Acceptance Scenarios**:

1. **Given** MRR@10 与 NDCG@10 均高于 011、Recall@10 不低于 011、50 条查询零失败零降级，**When** 判定，**Then** 输出 `ADOPT`
2. **Given** 任一主要条件未满足，**When** 判定，**Then** 输出 `DO_NOT_ADOPT` 并列出原因；实验本身仍可视为成功完成
3. **Given** 报告 incomplete 或指纹不兼容，**When** 判定，**Then** 输出 `INCONCLUSIVE`，不得给出采用结论

### User Story 4 - 分离验证模型质量（Priority: P1）

开发者可以在不修改生产默认超时的前提下，使用固定 30 秒评测超时完成同一模型的质量诊断，区分“模型排序效果”和“当前本地部署延迟”。

**Independent Test**: 同一 50-query 数据、模型、候选数和检索配置下，仅把 benchmark 超时设为 30 秒；证明 Hify `applied=50`、`degraded=0` 后才输出质量指标。

**Acceptance Scenarios**:

1. **Given** 30 秒评测模式，**When** 执行 50 条查询，**Then** 生产默认 1.5 秒不变，指纹明确标记 `quality_diagnostic`
2. **Given** 50 条均实际应用 Rerank，**When** 与 011 比较，**Then** 输出真实 delta 和 `QUALITY_PASS` / `QUALITY_FAIL`
3. **Given** 任何超时、失败或降级，**When** 判定质量，**Then** 输出 `QUALITY_INCONCLUSIVE`
4. **Given** 质量诊断通过，**When** 生成报告，**Then** 仍不输出 `ADOPT`，只说明模型排序效果值得继续做部署优化

### Edge Cases

- Reranker 返回乱序 index、重复 index、缺 index、非数值或非有限分数时，整次调用降级并在评测中记为失败，不接受部分结果。
- 模型下载不完整、revision 漂移或服务实际加载了别的模型时，预检失败。
- 查询的候选少于 10 条时，只对实际候选重排，Precision@K 仍使用固定 K 分母。
- MIRACL qrels 是 passage 级且不完备；标题相同或语义合理但 unjudged 的 passage 仍按原标准计分，可单独诊断但不得修改正式 qrels。
- 首次加载模型的冷启动耗时与后续请求分开记录，不能混成一次查询延迟。

## Requirements

### Functional Requirements

- **FR-001**: MUST 固定实验模型为 `BAAI/bge-reranker-v2-m3`，保存官方来源、Apache-2.0 许可、revision/digest 与运行时版本
- **FR-002**: MUST 以本地独立服务暴露 Hify 已支持的 `/rerank` 契约，不把 Python/模型运行时嵌入 Go 主进程
- **FR-003**: MUST 使用 Hify 现有 provider 与 `knowledge.Service.Retrieve` 的真实 Rerank 路径，不新增绕过业务链路的评分捷径
- **FR-004**: MUST 使用 011 的同一 50 query、800 documents、qrels/corpus hash、`bge-m3:567m` digest、1024 维、chunk 500/overlap 50、Hybrid Search 和 K 集合
- **FR-005**: MUST 保持 RRF 权重、准入阈值、candidateK、去重、邻接扩展及 metadata filter 配置不变
- **FR-006**: MUST 在下载或运行前验证 M1 Pro/32GB 可用内存和磁盘；模型服务与 Ollama 同时运行时不得造成系统内存压力或 swap 异常增长
- **FR-007**: MUST 预检 `/rerank` 的请求/响应契约、模型身份和固定样本的非恒定相关性分数
- **FR-008**: MUST 对 50 条查询全部运行真实 Rerank；保存成功、失败、超时、降级、候选数和耗时，但不保存敏感配置
- **FR-009**: MUST 将模型加载冷启动与稳态请求延迟分开报告，并计算稳态 p50/p95
- **FR-010**: MUST 扩展 011 比较逻辑，支持声明 `rerank` 为唯一允许差异；其他影响口径的指纹差异仍须拒绝
- **FR-011**: MUST 输出各 K 的 Recall、Precision、MRR、MAP、NDCG delta 及逐 query 改善/退化/不变列表
- **FR-012**: MUST 使用预先固定的决策规则：仅当 MRR@10、NDCG@10 均提升，Recall@10 不下降，且 50 query 零失败零降级时才 `ADOPT`
- **FR-013**: MUST 把真实效果、机制测试、延迟/资源表现和 qrels 不完备边界分开报告
- **FR-014**: MUST 保留 011 baseline 原文件和原始 run，不覆盖、不重写、不重新生成后冒充历史基线
- **FR-015**: MUST 保证关闭 Rerank 时，现有 `make eval-retrieval-gate` 14 条 case 和 011 重算结果不变
- **FR-016**: MUST NOT 自动调参、修改人工 qrels、引入 LLM Judge、生成答案、微调模型或同时比较 Qwen/Cohere/Jina/Voyage
- **FR-017**: MUST NOT 因实验失败默认开启生产 Rerank；是否采用由报告门禁决定
- **FR-018**: MUST 将模型权重、缓存、虚拟环境与生成 run 放入 gitignored 路径；仓库只保留配置、代码、测试和小型报告
- **FR-019**: MUST 将 30 秒超时限定为 benchmark `quality_diagnostic`，不修改 Hify 生产默认值
- **FR-020**: MUST 将 1.5 秒部署结论与 30 秒质量结论分开保存、分开表达
- **FR-021**: MUST 仅在 `hify_applied_count=50`、`hify_degraded_count=0`、零失败时计算 `QUALITY_PASS` / `QUALITY_FAIL`
- **FR-022**: MUST 保持模型、candidate limit、数据、Embedding、分块、Hybrid Search 和指标口径不变

### Key Entities

- **Rerank Model Identity**: 模型名、revision/digest、许可、运行时与脱敏服务地址
- **Rerank Run**: 011 兼容指纹、唯一实验变量、逐 query 候选/排名/错误/耗时和完整性
- **A/B Comparison**: 两组指标 delta、逐 query 分类、兼容性和采用结论
- **Runtime Evidence**: 冷启动、p50/p95、内存、swap、成功/失败/降级计数

## Success Criteria

### Measurable Outcomes

- **SC-001**: 本地服务对固定样本返回完整且可复现的 index/score，并能证明加载的是指定模型 revision
- **SC-002**: 50/50 查询经过真实 Rerank，失败、超时和降级均为 0；否则报告明确为 incomplete
- **SC-003**: A/B 比较除 Rerank 外所有结果口径指纹一致，不兼容输入 100% 被拒绝
- **SC-004**: 决策规则对 `ADOPT`、`DO_NOT_ADOPT`、`INCONCLUSIVE` 构造样本均有纯函数测试
- **SC-005**: 仅对完整的 1.5 秒部署门禁，若结果达到 MRR@10 > 0.626746、NDCG@10 > 0.696447、Recall@10 >= 0.976，则输出 `ADOPT`；否则如实输出 `DO_NOT_ADOPT`
- **SC-006**: 报告记录模型冷启动、稳态 p50/p95、峰值内存和 swap 变化，不以“能运行”代替资源证据
- **SC-007**: 关闭 Rerank 后原有确定性检索门禁 14/14 无 skip，011 保存 run 的纯重算指标逐字段一致
- **SC-008**: 最终报告明确指出 Mini qrels 不完备，禁止把单次 Mini 提升描述成完整 MIRACL 或生产收益
- **SC-009**: 30 秒诊断 run 完成 50/50 真实 Rerank，或如实输出 `QUALITY_INCONCLUSIVE`
- **SC-010**: 质量报告给出 `QUALITY_PASS`、`QUALITY_FAIL` 或 `QUALITY_INCONCLUSIVE`，且不覆盖 1.5 秒的 `INCONCLUSIVE`

## Assumptions

- 当前分支继承 011 benchmark 能力及其保存的本地 baseline；012 不重新定义数据集。
- M1 Pro 32GB 理论上能运行约 0.6B 的 Cross-Encoder，但是否达到可接受延迟必须以本机实测为准。
- Hify 已具备 Rerank provider、超时、整体降级和开关能力；本期优先复用，只有验收暴露真实缺口时才做最小修补。
- 完成实验不等于必须采用模型；`DO_NOT_ADOPT` 也是有效、可信的实验结论。

## Out of Scope

- 完整 MIRACL 评测或与公开排行榜横向比较
- Qwen3、Cohere、Jina、Voyage 等第二模型对照
- 自动权重/阈值搜索、微调和自建人工 qrels
- Web UI、生产部署、CI、定时运行和默认启用 Rerank
- 根据质量诊断自动修改生产超时、candidate limit 或模型
