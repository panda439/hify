# Tasks: BGE Reranker 真实对照评测

**Input**: `spec.md`、`research.md`、`plan.md`、`data-model.md`、`quickstart.md`、`contracts/`

**实施约束**: Luna subagent 负责实施；它不是仓库中唯一工作的 agent，不得回滚他人修改，不得 commit/push。严格 TDD：先运行目标测试并看到预期失败，再写最小实现，最后由主 agent 独立验收。

## Phase 1：基线与环境门禁

- [x] T001 记录当前 branch/commit/status、011 manifest/raw run/report 哈希、Ollama `bge-m3:567m` digest、数据库/Redis 状态和本机内存/swap/磁盘；011 制品缺失或漂移立即停止
- [ ] T002 归档本轮前 `make eval-retrieval-gate` 和 011 raw run 纯重算结果；数据库测试 skip 明确记为未验证
- [x] T003 验证 `uv` 可用并解析 Python 3.12；系统 Python 3.14 不作为模型环境；确认模型与虚拟环境缓存落入 gitignored 路径
- [x] T004 在 `internal/eval/retrievalbench/model.go` 先补仅承载数据的 Rerank identity/stats/fingerprint/decision 字段，使后续验收测试可编译；不得在此任务实现决策行为

## Phase 2：User Story 1——真实本地 Rerank

**Goal**: 本地固定模型通过 Hify 真实 Rerank 路径完成 50 次调用，身份、契约、错误、耗时和资源可核对。

- [x] T005 [P] [US1] 在 `eval/rerank-service/test_app.py` 先写 `/health` 未就绪/就绪、身份脱敏、模型名/revision/license/runtime 契约失败测试
- [x] T006 [P] [US1] 在 `eval/rerank-service/test_app.py` 先写 `/rerank` 完整 index、输入顺序映射、有限分数、空文档/超限和内部异常失败测试
- [x] T007 [P] [US1] 在 `eval/rerank-service/test_app.py` 先写 `/stats` 无正文累计计数、成功/失败/候选数、延迟和 RSS 失败测试
- [x] T008 [US1] 新增 `eval/rerank-service/pyproject.toml` 与 `uv.lock`，固定 Python 3.12、FastAPI/Uvicorn、SentenceTransformers/Transformers/PyTorch 与 pytest 兼容版本
- [x] T009 [US1] 在 `eval/rerank-service/app.py` 实现延迟加载的固定 `BAAI/bge-reranker-v2-m3` sidecar、三端点和稳定 index/score 映射，不记录 query/passage
- [x] T010 [US1] 运行 Python 测试 GREEN，并用变异证明删除 index 完整性或写入恒定分数时已有测试失败
- [x] T011 [US1] 使用固定 revision 下载/加载真实模型，核对 Apache-2.0、权重 digest 和 runtime；失败不得换模型或 mock
- [x] T012 [US1] 对固定中文相关/无关样本真实调用 `/rerank`，确认相关分更高；记录冷启动、RSS、swap 与原始响应结构
- [x] T013 [P] [US1] 在 `cmd/retrievalbench/main_test.go` 先写 Rerank precheck、sidecar stats 前后差值、幂等 provider/model setup 和敏感值不落报告的失败测试
- [x] T014 [US1] 扩展 `cmd/retrievalbench`，通过 provider.Service 幂等创建/复用本地无鉴权 provider 与 rerank model，checkpoint 仅存 gitignored ID
- [x] T015 [US1] 将固定模型 ID、现有 1.5s timeout 与 enabled=true 注入 benchmark 的 knowledge service；普通 Hify 默认配置保持不变
- [x] T016 [US1] 在真实 run 前后读取 `/stats`，将请求/成功/失败/候选、延迟与资源差值写入 run；任何超时或降级使 run incomplete

## Phase 3：User Story 2——单变量 A/B

**Goal**: 只允许 Rerank 相关指纹不同，并输出可信的指标 delta 与逐 query 分类。

- [x] T017 [P] [US2] 在 `internal/eval/retrievalbench/compare_test.go` 先写唯一 Rerank 差异可比较、非 Rerank 差异拒绝、普通 compare 仍严格拒绝的失败测试
- [x] T018 [P] [US2] 在 `internal/eval/retrievalbench/report_test.go` 先写 Rerank identity/stats 完整性、非有限延迟/计数矛盾、incomplete 传播失败测试
- [x] T019 [US2] 实现实验指纹校验和 `CompareRerankExperiment`；普通 `CompareReports` 行为不变
- [x] T020 [US2] 扩展 CLI/Makefile 的 Rerank precheck、setup、run、score、experiment-compare 薄入口和错误码测试
- [x] T021 [US2] 对同一 raw run 连续 score 两次并逐字段比较；构造单排名变化验证 Recall/MRR/MAP/NDCG delta 与 query 分类
- [x] T022 [US2] 用非 Rerank 指纹突变验证比较非零退出，不得用 diagnostic override 生成正式 delta

## Phase 4：User Story 3——决策与报告

**Goal**: 按预先固定标准给出 ADOPT、DO_NOT_ADOPT 或 INCONCLUSIVE。

- [x] T023 [P] [US3] 在 `internal/eval/retrievalbench/decision_test.go` 先写三种决策、边界相等、Recall 下降、失败/降级和不兼容的失败测试
- [x] T024 [US3] 在 `internal/eval/retrievalbench/decision.go` 实现纯决策函数和稳定原因列表；不得在真实结果产生后修改阈值
- [ ] T025 [US3] 运行 50-query 真实 Rerank benchmark；确认 sidecar stats 增量与 Hify 查询数相符，保存 candidate、失败/降级和资源/延迟证据
- [ ] T026 [US3] 与 011 baseline 做实验 compare，输出各 K delta、逐 query 改善/退化/不变和最终决策
- [x] T027 [US3] 对低分 query 做 qrels passage/title/unjudged 诊断，单独记录但不得修改 qrels 或正式指标
- [x] T028 [US3] 产出 `docs/eval-phase19-bge-reranker-report.md`，分开写机制证明、真实效果、资源/延迟、采用决策和 MIRACL 边界

## Phase 5：完整验收

- [x] T029 关闭 Rerank 后重跑 `make eval-retrieval-gate`，14 条 case 无 skip，并确认 011 raw run 纯重算结果逐字段一致
- [x] T030 运行 Python 全部测试、`go test -race -count=1 ./...`、`go vet ./...`、`make check-deps`、`git diff --check`；任何 skip/失败如实列出
- [x] T031 对照 FR-001～018、SC-001～008、contract 和 quickstart 逐条验收，更新本任务清单；T025/T026 因真实 run incomplete 保持未完成
- [x] T032 主 agent 独立检查真实 diff、模型身份、原始报告、资源证据和全部验证输出；结论为工程链路通过、正式 A/B 仍 `INCONCLUSIVE`

## Dependencies

`Phase 1 → Phase 2 → Phase 3 → Phase 4 → Phase 5`。T005～T007 可并行，T013 可在 Python sidecar 单测期间并行；真实模型预检 T011/T012 未通过时不得执行 T025，也不得用 fake 结果继续。

## Phase 6：30 秒质量诊断（待实施）

- [x] T033 [US4] 先写失败测试：`quality_diagnostic` 固定 30 秒、不得改变生产 1.5 秒默认值，并写入指纹
- [x] T034 [US4] 实现 benchmark-only 模式与独立输出文件；不改 Hify 普通启动配置
- [x] T035 [US4] 先写失败测试并实现 `QUALITY_PASS` / `QUALITY_FAIL` / `QUALITY_INCONCLUSIVE` 纯函数决策
- [x] T036 [US4] 重跑 50-query 真实诊断，确认 Hify applied/degraded、sidecar 请求数、延迟和资源证据完整
- [x] T037 [US4] 与 011 baseline 比较，输出各 K delta、逐 query 分类和质量决策；不覆盖 1.5 秒制品
- [x] T038 [US4] 更新报告，明确区分质量结论与部署结论
- [x] T039 复跑 Python/Go/race/vet/deps/diff-check、14 条检索门禁和 011 纯重算
- [x] T040 主 agent 独立验收新增 diff、真实质量制品和默认配置不变证据；独立复跑全量门禁、011 重算及 comparison/decision 确定性均通过

Phase 6 仅在 T033～T035 的测试边界固定后进入真实 run；任何降级均使质量结论为 `QUALITY_INCONCLUSIVE`。
