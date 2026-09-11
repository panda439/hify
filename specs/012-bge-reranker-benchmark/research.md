# Research: BGE Reranker 真实对照评测

## R1：模型选择

**Decision**: 首轮固定 `BAAI/bge-reranker-v2-m3`。

**Rationale**: FlagEmbedding 官方将其列为适合中文、多语言和效率部署的轻量 Cross-Encoder；RAGFlow 本地 HuggingFace Rerank 默认模型也是它；Apache-2.0 许可适合本地学习与工程验证。Qwen3-Reranker-0.6B 更新、综合基准更强，但 Qwen 官方表中中文 CMTEB-R 为 71.31，BGE 为 72.16，且 Qwen 属 decoder-style/instruction-aware 路径，首轮接入变量更多。

**Sources**:

- https://github.com/FlagOpen/FlagEmbedding/blob/master/examples/inference/reranker/README.md
- https://github.com/infiniflow/ragflow/blob/main/rag/llm/rerank_model.py
- https://huggingface.co/BAAI/bge-reranker-v2-m3
- https://github.com/QwenLM/Qwen3-Embedding#reranker

## R2：服务形态

**Decision**: 使用独立 Python sidecar，通过 `/rerank` 服务给 Hify 调用。

**Rationale**: Dify、RAGFlow 都采用独立 Rerank provider/HTTP 服务模式；Hify 已实现同类 `/rerank` 契约。M1 Mac 不适合把 vLLM 作为首选，Ollama 也没有 Hify 需要的标准 Rerank endpoint。`sentence-transformers`/Transformers 可直接加载该 Cross-Encoder，最少适配即可返回完整 index/score。

**Alternatives rejected**:

- 把 Python 嵌入 Go 进程：扩大生产依赖并破坏模块边界。
- 直接在 benchmark 内调用模型：绕过 Hify provider/Retrieve，无法证明真实链路。
- vLLM/TEI：更适合 Linux/GPU 服务，增加本机验证成本。

## R3：Python 运行时

**Decision**: 使用 `uv` 创建独立 Python 3.12 环境，锁定依赖；不用系统 Python 3.14。

**Rationale**: 当前机器已有 `uv`，系统 Python 为 3.14；PyTorch/Transformers 在较新的 Python 上可能存在轮子和兼容风险。隔离的 3.12 环境更可复现，不污染 Hify Go 运行时。

## R4：A/B 可比性

**Decision**: 新增“允许 `rerank_config` 为唯一差异”的实验比较模式；普通 compare 仍严格拒绝所有口径差异。

**Rationale**: 011 compare 正确地将 retrieval config 差异视为不兼容，但 012 的实验变量正是 Rerank。专用模式必须验证其余 dataset/query/doc/qrels/corpus/embedding/chunk/hybrid/K/metric 指纹完全相同，再允许输出 delta。

## R5：实验与采用分离

**Decision**: 完成实验不等于采用。结果分为 `ADOPT`、`DO_NOT_ADOPT`、`INCONCLUSIVE`。

**Rationale**: 模型可能让排序变差、Recall 下降或在 M1 上超时。只要证据完整，负结果仍有学习价值；不应修改标准追求“必须提升”。

## R6：MIRACL 标注边界

**Decision**: 正式指标继续按原 passage qrels；额外输出标题重复/unjudged 命中诊断，但不参与决策分数。

**Rationale**: 已观察到部分排名靠前的 passage 标题和语义合理，却不是 qrels 指定 passage。改写 qrels 会破坏公开数据口径，忽略该现象又会误解模型退化原因。

## R7：资源与超时

**Decision**: 模型与 Ollama 同时运行；预检磁盘/可用内存，记录冷启动、稳态 p50/p95、峰值内存和 swap。真实 run 沿用 Hify Rerank 超时/整体降级机制。

**Rationale**: 32GB 内存足以尝试约 0.6B 模型，但“能加载”不代表延迟适合真实 RAG；必须以本机实测决定。

## R8：质量诊断方式

**Decision**: 保留 1.5 秒部署门禁，另做固定 30 秒的 `quality_diagnostic`。

**Rationale**: 实测 p95 约 11.7 秒，30 秒可覆盖当前本机推理，同时不把慢推理冒充成可生产部署。该诊断只回答“排序质量是否提升”。

**Alternatives rejected**:

- 把生产默认超时改成 30 秒：会隐藏已证明的延迟不兼容。
- 减少 candidate limit：同时改变排序输入，无法单独判断当前模型效果。
- 立即更换其他模型：在当前模型质量尚未测出前扩大变量。
