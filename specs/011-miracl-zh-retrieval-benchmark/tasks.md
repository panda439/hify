# Tasks: MIRACL 中文真实检索评测

**Input**: `spec.md`、`research.md`、`plan.md`、`data-model.md`、`quickstart.md`

**实施者**: Luna subagent。实施者不是独自在仓库工作，不得回滚他人改动；发现重叠修改时适配当前状态并报告。严格 TDD：先看到针对目标行为的失败，再写最小实现。

## Phase 1：基线与失败测试

- [x] T001 记录 `git status`、commit、模型 digest、数据库状态；归档 `make eval-retrieval-gate` 报告作为 011 前基线，任何 skip 记为未验证
- [x] T002 [P] 在 `internal/eval/retrievalbench/metrics_test.go` 用手算样本写 Recall/Precision/MRR/AP/NDCG @K 失败测试，覆盖单正例、多正例、分级相关、空结果和 query 失败
- [x] T003 [P] 在 `internal/eval/retrievalbench/ranking_test.go` 写 chunk→document 首次出现去重、稳定顺序、邻接重复和 K 截断测试
- [x] T004 [P] 在 `internal/eval/retrievalbench/dataset_test.go` 写稳定选择、全部正负 judgment 保留、unjudged 干扰文档补足、缺 qrel 文档、重复冲突和原子发布测试
- [x] T005 [P] 在 `internal/eval/retrievalbench/compare_test.go` 写自比 IDENTICAL、排名突变、兼容指纹拒绝和诊断 override 标记测试

## Phase 2：纯模型、指标与报告

- [x] T006 在 `internal/eval/retrievalbench/model.go` 实现 manifest/query/document/qrel/run/report 类型及验证，不引入数据库依赖
- [x] T007 在 `internal/eval/retrievalbench/ranking.go` 实现 chunk ranking 到唯一 document ranking 的纯转换
- [x] T008 在 `internal/eval/retrievalbench/metrics.go` 实现 @1/@3/@5/@10 的逐 query 与总体指标；失败 query 留在分母
- [x] T009 在 `internal/eval/retrievalbench/report.go` 实现稳定 JSON/JSONL 保存、SHA-256 和纯重算
- [x] T010 在 `internal/eval/retrievalbench/compare.go` 实现指纹兼容检查、总体 delta 与逐 query 分类
- [x] T011 运行该包全部测试并补 mutation 证明：删除 document 去重或把失败 query 排除分母时已有测试必须失败

## Phase 3：公开数据准备

- [x] T012 新增 `eval/benchmarks/miracl-zh-mini.yaml`，固定 MIRACL v1.0 zh/dev、seed、50 query、500～1000 docs、目标 800、K 与本地路径
- [x] T013 在 `internal/eval/retrievalbench/dataset.go` 实现 topics/qrels/corpus 读取、稳定选择、正负 judgment 完整性、unjudged 干扰文档补足与内容哈希
- [x] T014 实现临时目录写入后原子发布；网络/压缩包/格式/hash/缺文档错误不得留下正式 manifest
- [x] T015 更新 `.gitignore`，完整缓存、Mini 正文、向量和中间 run 不得进入 git；固定配置、小 manifest/ID 清单和报告可提交
- [x] T016 连续 prepare 两次并比较固定字段与全部 SHA-256；记录上游 revision、许可、下载规模、最终 query/doc/qrel 数

## Phase 4：真实 Hify 入库与查询

- [x] T017 先写 `internal/knowledge` benchmark adapter 集成失败测试：隔离 KB、上游 document ID 映射、正常 Upload/Process、ready 恢复、失败阻止 complete
- [x] T018 实现最小 benchmark adapter，复用正常解析/分块/Embedding/发布路径；不得新增生产 HTTP API 或直接写上游向量
- [x] T019 验证真实 `bge-m3:567m` 名称、digest 与 1024 维，不可用时立即停止，不降级 fake
- [x] T020 完成 500～1000 篇 Mini corpus 入库，记录成功/失败文档、chunk、Embedding 批次和各阶段耗时；支持断点恢复
- [x] T021 先写查询运行器测试，再对 50 query 调用 `knowledge.Service.Retrieve(topK=10)`，保存原始 chunk ranking、唯一 document ranking、错误和耗时
- [x] T022 确认任一入库/query 失败都会令 run `complete=false`，失败 query 指标为零，重跑只补未完成阶段

## Phase 5：CLI、Makefile 与对照

- [x] T023 为 `cmd/retrievalbench` 的 prepare/ingest/run/score/compare 写参数与错误路径测试
- [x] T024 实现五个子命令，凭据只读现有环境，日志和报告不得保存 token/API key/向量
- [x] T025 在 Makefile 添加 `eval-retrieval-benchmark-*` 薄入口并更新 `README.md` 验证命令说明
- [x] T026 对同一 raw run 连续 score 两次，除生成时间外逐字段一致；自比输出 IDENTICAL
- [x] T027 制造一个隔离的排名副本，确认 compare 精确列出 Recall/MRR/NDCG 的 delta 和退化 query；不兼容指纹非零退出

## Phase 6：完整验收与报告

- [x] T028 运行真实 MIRACL Mini benchmark，保存 `bge-m3` 基线；报告 50 query、实际 document/chunk 数、模型 digest、配置、指标、失败数和耗时
- [x] T029 与 T001 的确定性门禁报告比较，除 `ran_at` 外 MUST IDENTICAL；14 条 case 无 skip且四项仍为 1.0
- [x] T030 依次运行 `go test -race -count=1 ./...`、`go vet ./...`、`make check-deps`、`git diff --check`；任何数据库 skip 单独列为未验证
- [x] T031 产出 `docs/eval-phase18-miracl-zh-mini-report.md`，分开写机制证明、真实 Mini 效果、完整 MIRACL/生产边界和下一步可验证优化假设
- [x] T032 对照 FR-001～020、SC-001～008 和 quickstart 逐条验收，更新本 tasks 勾选；不得自动 commit/push

## Dependencies

`Phase 1 → Phase 2 → Phase 3 → Phase 4 → Phase 5 → Phase 6`。T002～T005 可并行；公开数据准备与真实入库不可跳过纯逻辑门禁。T028 前必须确认用户未同时运行占用大量内存的本地生成模型。
