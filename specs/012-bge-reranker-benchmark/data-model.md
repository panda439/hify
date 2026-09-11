# Data Model: BGE Reranker 真实对照评测

## 1. RerankModelIdentity

- `model_name`: 固定 `BAAI/bge-reranker-v2-m3`
- `revision` / `digest`: 实际权重身份
- `license`: `Apache-2.0`
- `runtime`: Python、PyTorch、Transformers/SentenceTransformers 版本
- `endpoint_id`: 服务地址哈希，不保存敏感地址
- `ready`: 是否完成加载并通过固定样本预检

## 2. RerankPhaseStats

- `request_count` / `success_count` / `failure_count` / `degraded_count`
- `candidate_count_total`
- `cold_start_ms`
- `steady_p50_ms` / `steady_p95_ms`
- `peak_rss_bytes`
- `swap_delta_bytes`
- `stats_before` / `stats_after`: Sidecar 累计计数快照，用差值证明真实调用
- `hify_enabled_count` / `hify_applied_count` / `hify_degraded_count`：由 benchmark-only context observer 从 Hify `Service.Retrieve` 本次调用实际写入的结果聚合；不从 sidecar 计数推断。
- `hify_input_count` / `hify_duration_ms`：Hify 侧每次实际进入 rerank 的候选数和耗时聚合。
- `hify_outcome`：Hify 侧证据状态；`all_applied` 仅在每条查询 observer 都报告 applied 且无 degraded 时使用，否则为 `not_all_applied_or_degraded`。sidecar success 只证明服务端请求完成，不代表 Hify 已应用，也不伪造内部 degraded 精确数量。

约束：`success + failure = request_count`；正式 complete run 要求 50 次查询、`hify_enabled_count=50`、`hify_applied_count=50`、`hify_degraded_count=0`。sidecar 的 request/success/failure/degraded 仅是服务端请求证据，不能替代 Hify 侧计数。

## 3. ExperimentalFingerprint

继承 011 `Fingerprint` 的 dataset、revision、query/document IDs、qrels/corpus/config hash、Embedding identity、chunk config、retrieval config、K 和 metric version；新增：

- `rerank_enabled`
- `rerank_model_name`
- `rerank_model_digest`
- `rerank_candidate_limit`
- `rerank_timeout`
- `run_mode`: `deployment_gate` 或 `quality_diagnostic`

专用 A/B 只允许这些 Rerank 字段不同；其余字段不同即不可比较。

## 4. RerankComparison

- `status`: `CHANGED` / `IDENTICAL` / `NON_COMPARABLE`
- `experiment_variable`: 固定 `rerank`
- `aggregate_deltas`: 各 K 的 Recall/Precision/MRR/MAP/NDCG
- `improved_queries` / `regressed_queries` / `unchanged_queries`
- `decision`: `ADOPT` / `DO_NOT_ADOPT` / `INCONCLUSIVE`
- `reasons`: 决策原因

## 5. State Transitions

`MODEL_UNAVAILABLE → READY → RUNNING → COMPLETE`

任何身份不符、请求失败、超时、降级或报告不完整进入 `INCOMPLETE`。`COMPLETE` 后才允许进入决策；不兼容比较直接得到 `INCONCLUSIVE`。

## 6. QualityDiagnosticDecision

- `decision`: `QUALITY_PASS` / `QUALITY_FAIL` / `QUALITY_INCONCLUSIVE`
- `reasons`: 指标门禁、Hify applied/degraded 完整性或可比性原因
- `deployment_decision`: 保留 1.5 秒 run 的原结论，质量诊断不覆盖
