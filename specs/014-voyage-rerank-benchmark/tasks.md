# Tasks: Voyage Rerank 对照评测

## Phase 1：隔离与固定证据

- [x] T001 记录三个 worktree 状态与基线 commit，确认 014 不修改 012/013。
- [x] T002 机械导入 012 benchmark 与 013 Voyage adapter，核对 diff 来源，不导入已有 run 作为新结果。（Claude Code 接手后改为 git 合并 `2984a43` + `f567ad3`，与 Codex 手工拷贝逐字节一致）
- [x] T003 固定 011 baseline、012 BGE deployment/quality 和 MIRACL Mini 文件哈希。
- [x] T004 固定 Voyage 官方价格快照、免费额度说明、模型名和 API 地址。

## Phase 2：TDD 托管 API 证据

- [x] T005 先写 Voyage `usage.total_tokens` 解码失败测试，再实现 RerankResult usage。
- [x] T006 先写 Hify observer 的逐次延迟、token 聚合与 p50/p95 失败测试，再实现最小字段传递。
- [x] T007 先写托管 Provider/Model 身份和配置不符的失败测试，再实现 precheck。
- [x] T008 先写列表价计算、免费额度分离和非有限输入失败测试，再实现费用报告。
- [x] T009 先写模型无关决策测试，覆盖 MAP、Recall 不下降、至少一项提升和证据不完整。
- [x] T010 先写 deployment/quality 文件隔离和指纹唯一变量测试，再扩展 CLI/Makefile。

## Phase 3：真实 Voyage 实验

- [x] T011 预检加密 Key、`voyage-ai/rerank-3`、800/50 固定输入和单次中文 smoke test。（smoke：相关 0.894531 / 无关 0.250000、0.249023，total_tokens=27）
- [x] T012 运行 1.5 秒 deployment gate，保存 raw/report/comparison/decision。（applied 1 / degraded 49：2 次 context deadline exceeded 后熔断打开；决策 `INCONCLUSIVE`）
- [x] T013 运行 30 秒 quality diagnostic，保存独立 raw/report/comparison/decision。（applied 1 / degraded 49：Voyage 429，账户未添加付款方式被限为 3 RPM / 10K TPM；决策 `QUALITY_INCONCLUSIVE`）
- [x] T014 核对 50 次 applied/degraded、候选数、p50/p95、token 和列表价估算。（两轮均 input 1759、tokens 5773、列表价 $0.00028865；证据不完整如实保留，未自动重跑）
- [x] T015 生成 baseline/BGE/Voyage 同表报告，不覆盖 012 原结论。（`docs/eval-phase20-voyage-rerank-report.md`）

## Phase 3b：限速质量诊断（2026-09-11 用户决策：不添加 Voyage 付款方式）

- [x] T020 先写失败测试：限速间隔计算、仅允许托管质量诊断、限速记录进入 raw run/report、限速运行不能作为部署证据。
- [x] T021 实现 `--pacing-interval`、限速记录与决策校验，Makefile 增加独立 `-paced` 制品目标；首轮未限速制品不覆盖。（变异：去掉部署拒绝后决策变 `ADOPT`，测试失败；恢复后通过）
- [x] T022 运行 50-query 限速质量诊断（固定 65 秒间隔），保存 raw/report/comparison/decision。（applied 50 / degraded 0，`QUALITY_PASS`）
- [x] T023 核对 applied/degraded、429、token、列表价与限速总等待时间。（0 次 429；tokens 238498；列表价 $0.0119249；总等待 3148784ms）

## Phase 4：完整验收

- [x] T016 全量 `go test -race -count=1 ./...`、`go vet ./...`、`make check-deps`、`git diff --check`。（12 个包 ok；唯一 skip 为 opt-in `TestVoyageRerankLive`，已在 T011 单独通过）
- [x] T017 运行 14 条检索门禁并重算 011 baseline，确认非 Rerank 行为未变。（14 PASS / 0 skip；011 重算 SHA256 一致）
- [x] T018 扫描 Key/Token/正文泄漏，对照 FR/SC 和真实 diff 独立验收。（diff、新增代码、Voyage 制品无 Key/Bearer；report/comparison/decision 无正文；raw run 沿用 011 格式保存 MIRACL 公开 query 文本，报告中列为 FR-011 字面偏差）
- [x] T019 更新任务与验收报告；不提交、不推送、不启用默认 Rerank。（`rerank-3` `is_default=0`，`.env` 未设置 Rerank）

## Phase 5：Codex 审核修复（不调用 Voyage）

- [x] T024 先写失败测试：Hosted raw run 清空 query text，且脱敏前后 score 结果完全一致。（先观察 `undefined: SanitizeHostedRun` RED，再实现；脱敏前后离线 score report 字节一致）
- [x] T025 先写失败测试：只有 applied 调用进入成功延迟；零成功时不输出 p50/p95。（先观察 `undefined: successfulRerankDurations` RED，再实现；混合 applied/degraded 与零样本测试通过）
- [x] T026 先写失败测试：timeout、HTTP 429、circuit open、response invalid、other 映射为固定失败分类，制品只保存分类计数。（provider 固定枚举测试、未知分类归 other、Hosted 证据未知/负计数拒绝均先 RED 后 GREEN）
- [x] T027 实现 hosted query 脱敏、成功延迟采样和失败分类透传/汇总；不改变普通 MIRACL/BGE 路径。（provider → knowledge observer → benchmark 聚合；raw 不保存原始 error）
- [x] T028 用临时副本离线脱敏现有三份 Voyage raw run；确认重算 report/comparison/decision 逐字节一致后原子替换 raw 文件，并记录新旧 SHA256；不执行 `run` 或 live test。（三份 `queries[].text=0`；comparison/decision 字节一致；未调用 Voyage）
- [x] T029 修正文档：历史失败原因只标为运行时观察，移除不可由制品复核的精确断言；更新脱敏后哈希与成功延迟口径。（不完整两轮 p50/p95 标记 unavailable；paced 保留 509/836ms）
- [x] T030 全量 race、vet、依赖检查、检索门禁、diff 检查和敏感正文扫描；记录真实证据，保持未提交、未推送、未启用默认 Rerank。（Codex 独立验收：全量 race 通过；vet/check-deps/diff 通过；14 条检索门禁 PASS、0 skip；三份 Voyage raw query text=0；凭据模式扫描 0；三组离线 report/comparison/decision 逐字节一致）
