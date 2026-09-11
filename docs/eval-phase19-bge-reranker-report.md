# Phase 19：BGE Reranker 真实对照评测

状态：部署门禁 `INCONCLUSIVE`，质量诊断 `QUALITY_PASS`。真实模型已加载；固定 1.5s 部署不兼容，但固定 30s benchmark 质量诊断完成。这两个结论分别回答部署可用性和排序质量，不互相覆盖。

## 门禁快照

- checkout：`012-bge-reranker-benchmark`，基线 commit：`0492f32`；实施开始前 `git status --short --branch` 仅显示该分支和未跟踪的 012 规格目录，当前改动均为本任务文件
- 011 manifest/raw run/report SHA256：`737cfb8dfdecdc3a33830e66f24931b85c42c9776ace4762a187e75dce1e9c30` / `10c66e64326d9a63233308147926fe8c828e70343ce20cbcce2ee39c338e6830` / `35d2c666ea587aa42b8508d073977de2bd3d29fce72913b3023cc7dd94d929b6`
- 011 gate baseline archive SHA256：`c3e588acdc2cac5a981abe63c9326901f866ccb9d2274d639211aeccd06605f7`
- Ollama `bge-m3:567m` digest：`7907646426070047a77226ac3e684fbbe8410524f7b4a74d02837e43f2146bab`
- 数据库/Redis：`hify-mysql-1`、`hify-postgres-1` healthy，`hify-redis-1` running
- 主机资源：32 GiB RAM；`memory_pressure -Q` 报告系统 free 74%；swap 14,336 MiB（used 13,088.19 MiB，free 1,247.81 MiB）；磁盘可用约 440 GiB

上述快照未发现 011 制品漂移；本轮未覆盖或改写 011 raw run/report。

## 已固定的实验身份

- 模型：`BAAI/bge-reranker-v2-m3`
- Hugging Face revision：`953dc6f6f85a1b2dbfca4c34a2796e7dde08d41e`
- 许可证声明：`Apache-2.0`
- Python 运行时：`3.12.2`（通过 `uv` 解析）
- 模型缓存：`eval/cache/rerank-models/`（已加入 `.gitignore`）
- 本地虚拟环境：`eval/rerank-service/.venv/`（已加入 `.gitignore`）

## TDD 与工程验证

已完成 sidecar `/health`、`/rerank`、`/stats` 契约测试，以及 Go 侧 precheck、stats 差值、provider/model 幂等配置、rerank compare、决策和 benchmark observer 单元测试。Python 测试结果为 `10 passed`；串行 `internal/knowledge` 全包和全量 Go race 均通过。对 `/rerank` 响应临时删除 `index` 字段后，既有完整性测试实际失败（`KeyError: 'index'`），恢复实现后全量 Python 测试重新通过，证明该边界测试能捕获响应索引突变；新增 observer 测试证明 adapter 能捕获 applied/degraded。

## 真实模型与样本预检

设置 `HF_HUB_DISABLE_XET=1` 后，固定 revision 通过官方标准 HTTP 完整下载并在离线模式加载成功。runtime 为 Python `3.12.2`、PyTorch `2.6.0`、Transformers `4.48.3`。固定中文样本真实响应：相关 passage 分数 `0.9999529123`，无关 passage 分数 `0.0000418080`；sidecar `/health` 为 ready，Hify provider `/rerank` 真实响应相关分数 `0.9986981153`，无关分数 `0.0000204140`。

模型预检的 sidecar `stats_before.cold_start_ms=4294`；质量 run 的 `stats_before` 同样记录冷启动 `4294ms`，该冷启动发生在 50-query run 之前，因此 run 内冷启动增量为 `0`，不能解读为没有模型加载成本。

## 真实 Hify 50-query run

通过真实 MySQL/PostgreSQL/Redis、Hify provider 和 `knowledge.Service.Retrieve` 执行 50 条查询，固定 `HIFY_RAG_RERANK_TIMEOUT=1500ms`。raw run 为 `eval/runs/miracl-zh-rerank-candidate.json`，报告为 `eval/runs/miracl-zh-rerank-candidate-report.json`。

- `query_count=50`，查询本身无错误；benchmark-only context observer 记录 Hify 侧 `hify_enabled_count=50`、`hify_applied_count=0`、`hify_degraded_count=50`、`hify_input_count=1759`、`hify_duration_ms=7621`，因此 run 的 `hify_outcome=not_all_applied_or_degraded` 且 `complete=false`。这些字段来自每次 `Service.Retrieve` 的实际 rerank 分支，不由 sidecar 计数推断。
- sidecar 在 run 前后统计增量为 `request_count=5`、`success_count=5`、`failure_count=0`、`degraded_count=0`、`candidate_count_total=189`；统计到的 steady p50/p95 为约 `7840.29/11653.86ms`，峰值 RSS `688128000` bytes，swap 增量 `0`。sidecar success 只证明最终完成了 5 个 HTTP 推理请求，不代表 Hify 接受并应用了这些结果。
- Hify 日志实际记录多次 `context deadline exceeded`，随后出现 `circuit breaker is open`，其余查询保持 baseline 顺序。候选排名与 011 baseline 完全一致；这只能证明降级保持原序，不能证明 reranker 没有提升，也不能据此评价模型质量。

未使用 fake provider、替代模型、规则分数或伪造结果。

## 30 秒质量诊断（独立制品）

使用同一 50 query、800 documents、qrels/corpus、`bge-m3:567m`、candidate limit、分块、Hybrid Search 和指标口径，仅将 benchmark-only `run_mode=quality_diagnostic` 的 Hify rerank timeout 固定为 `30000ms`。生产默认配置和 1.5s deployment-gate 制品未改写。

- raw run：`eval/runs/miracl-zh-rerank-quality.json`；报告：`eval/runs/miracl-zh-rerank-quality-report.json`；comparison：`eval/runs/miracl-zh-rerank-quality-comparison.json`；决策：`eval/runs/miracl-zh-rerank-quality-decision.json`
- 制品 SHA256：raw `3a1eddb3575794a69cfa7075ff1cfe2c2dd528120afd595049f817ad58b77cf7`；report `542055bd81a163bf8887c9671e4a1e7a0a847b1770c6ad686e6f459725412ad4`；comparison `ea973d479aedbf9080d5f188a8f96f716675d98a130663a9dc6367442fef1887`；decision `6b4da81735de76dd09fe2d2e3b355e84ec04d99d5bac8800aad6963bb73433d2`
- Hify observer：`enabled=50`、`applied=50`、`degraded=0`、query failures `0`；sidecar：`request_count=50`、`success_count=50`、`failure_count=0`、`candidate_count_total=1759`
- Hify rerank input 总数 `1759`，耗时总计 `152565ms`；sidecar steady p50/p95 `3072.78/4330.46ms`，峰值 RSS `810631168` bytes，swap 增量 `0`
- sidecar `stats_before.cold_start_ms=4294ms`；冷启动发生在 50-query run 前的模型预检阶段，所以 run 内 `cold_start_ms` 增量为 `0`
- @10 quality metrics：Recall `0.985333`（011 `0.976000`，delta `+0.009333`），MRR `0.761556`（delta `+0.134810`），NDCG `0.790638`（delta `+0.094190`）
- quality compare：`CHANGED`，@1/@3/@5/@10 各 K delta 已写入独立制品；决策主口径固定为 K=10，逐 query 互斥分类为 improved `28`、regressed `14`、unchanged `8`，合计 `50`；quality decision：`QUALITY_PASS`

`QUALITY_PASS` 仅表示在 30s 诊断条件下模型排序质量通过固定门禁；不表示当前 1.5s 部署可采用，也不产生 `ADOPT`。部署结论仍为 `INCONCLUSIVE`（超时/熔断不兼容），不是 `bge-reranker-v2-m3` 质量差。

## 任务边界

真实模型 T011/T012、Hify 路径和 stats 传播已完成；1.5s 50-query deployment run 不完整，部署决策制品为 `INCONCLUSIVE`（`candidate run is incomplete`）。Phase 6 的 30s quality run 完整且质量决策为 `QUALITY_PASS`，但不覆盖部署结论。主 agent 已独立审核 diff、模型权重 SHA256、真实 run 计数和决策制品，并复跑 Python 测试、Go race 全量测试、`go vet`、依赖检查、14 条检索门禁及 011 raw run 纯重算；全部通过，011 baseline 制品未修改。T025/T026 继续保持未完成，不把降级后的零 delta 当作模型效果。
