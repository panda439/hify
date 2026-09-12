# Feature Specification: Voyage Rerank 对照评测

**Feature Branch**: `014-voyage-rerank-benchmark`
**Created**: 2026-09-11
**Status**: Draft

## 目标

在不改变 MIRACL 中文 Mini 基线口径的前提下，通过 Hify 真实调用 Voyage
`rerank-3`，分别判断它的排序质量和 1.5 秒部署可用性，并与无 Rerank 基线、
本地 `BAAI/bge-reranker-v2-m3` 做公平对比。

## 固定实验口径

- 语料：012 已使用的 800 篇 MIRACL 中文文档。
- 查询：同一组 50 queries 与人工 qrels。
- 保持不变：Embedding、分块、Hybrid Search、metadata filter、候选上限和指标实现。
- 唯一实验变量：Rerank 模型与对应协议；不得修改 qrels、阈值或失败判定。
- 参考结果：011 无 Rerank 基线与 012 BGE 结果按哈希只读复用，不重新解释或覆盖。

## 用户场景

运行者可以得到两份相互独立的 Voyage 结果：1.5 秒部署门禁回答“当前 Hify 配置能否稳定使用”，
30 秒质量评测回答“排除本地延迟限制后，模型是否改善排序”。最终报告并列展示 baseline、BGE、
Voyage，不能用质量通过代替部署通过。

## 功能要求

- **FR-001**: MUST 在独立 014 worktree 中组合 012 评测框架与 013 Voyage 协议适配，不修改原 worktree。
- **FR-002**: MUST 验证 baseline、BGE、Voyage 的非 Rerank 指纹完全一致；不一致时停止比较。
- **FR-003**: MUST 使用数据库中已配置且加密保存的 `voyage-ai/rerank-3`，不得绕过 Hify Provider 和 Knowledge 链路。
- **FR-004**: MUST 先运行固定 1.5 秒 timeout 的部署门禁；任一查询降级、超时或未实际应用 Rerank，正式结论不得为 `ADOPT`。
- **FR-005**: MUST 独立运行固定 30 秒 timeout 的质量评测；其文件名和结论不得覆盖部署门禁制品。
- **FR-006**: MUST 输出 Recall@10、MRR@10、MAP@10、NDCG@10，以及逐 query 改善、退化、不变数量。
- **FR-007**: MUST 输出 Hify applied/degraded/input count 和 p50/p95 延迟；不得用客户端总耗时替代模型调用耗时。
- **FR-008**: MUST 从 Voyage 响应 `usage.total_tokens` 汇总实际处理 token，并按评测当日官方单价计算列表价估算；免费额度抵扣与列表价分开表述。
- **FR-009**: MUST 固定决策阈值后再运行真实评测；看到结果后不得修改阈值。
- **FR-010**: MUST 分别输出部署结论 `ADOPT/DO_NOT_ADOPT/INCONCLUSIVE` 与质量结论 `QUALITY_PASS/QUALITY_FAIL/QUALITY_INCONCLUSIVE`。
- **FR-011**: MUST 保存模型名、API base URL、评测时间和官方价格来源；不得保存 API Key、查询正文、文档正文或逐条外部请求。
- **FR-012**: MUST 保留 012 BGE 的原始结论，包括其 1.5 秒部署不兼容与 30 秒质量结果，不能只挑有利指标。
- **FR-013**: MUST 设定单次实验上限为 50 queries、每次最多 50 candidates；超出时停止，不自动扩大样本或重复付费运行。
- **FR-014**: MUST NOT 自动把 Voyage 设置为 Hify 默认 Rerank；采用动作不属于本任务。
- **FR-015**（2026-09-11 追加，用户决策）: 首轮真实运行时观察到 Voyage 账户未添加付款方式，被限为
  3 RPM / 10K TPM；该 429 原文未进入当前制品。1.5 秒与 30 秒两轮均只有 1/50 次实际应用 Rerank，制品保留为
  `INCONCLUSIVE` / `QUALITY_INCONCLUSIVE` 证据。用户决定不添加付款方式：只对 30 秒质量诊断增加
  benchmark-only 固定限速——相邻 query 起点至少间隔 65 秒（单次调用约 6K tokens，≤ 1 RPM、
  不超过 10K TPM）。限速在运行前固定并写入 raw run/report，不改变检索、候选、Rerank 输入、
  timeout、指纹或决策阈值；限速运行使用独立 `-paced` 文件名，不覆盖首轮制品。
- **FR-016**: 部署门禁 MUST NOT 使用限速；任何带限速记录的运行作为部署证据时一律 `INCONCLUSIVE`。
  1.5 秒部署门禁在当前账户限流下无法得到有效证据，本任务不重跑。
- **FR-017**（审核修复）: 托管 Rerank raw run 中的 `queries` MUST 只保存 query ID，`text` 必须为空；
  score/recompute 只能依赖 ID、qrels 与 ranking。已有 Voyage raw run 通过确定性离线脱敏移除正文，脱敏前后
  metrics/comparison/decision 必须逐字节一致；不得重新调用 Voyage。
- **FR-018**（审核修复）: 托管 Rerank p50/p95 MUST 只统计 `applied=true` 的成功调用；超时、429、
  熔断快速失败和响应校验降级不得混入成功延迟。若没有成功调用，成功延迟字段保持缺失；历史不完整运行
  无法从聚合数据还原时必须标为不可用，不得猜测。
- **FR-019**（审核修复）: 后续托管运行 MUST 按 query 保存脱敏失败分类汇总，固定分类至少包含
  `timeout`、`http_429`、`circuit_open`、`response_invalid`、`other`；只保存分类与计数，不保存错误正文、
  API 响应、query、documents 或逐条请求。既有运行缺少该结构化证据时，报告不得把终端观察写成可复核结论。

## 预先固定决策标准

### 1.5 秒部署门禁

- 全部 50 queries 实际应用 Rerank，`degraded=0`。
- Recall@10 不低于 baseline。
- MRR@10、MAP@10、NDCG@10 均不低于 baseline，且至少一项有正向提升。
- 任一条件缺少可信证据时为 `INCONCLUSIVE`；指标真实下降时为 `DO_NOT_ADOPT`。

### 30 秒质量评测

- 全部 50 queries 实际应用 Rerank，`degraded=0`。
- Recall@10 不低于 baseline。
- MRR@10、MAP@10、NDCG@10 均不低于 baseline，且至少一项有正向提升。
- 证据完整且满足时为 `QUALITY_PASS`；下降时为 `QUALITY_FAIL`；证据不完整时为 `QUALITY_INCONCLUSIVE`。

## 验收标准

- **SC-001**: 两种 timeout 均产生独立 raw run、report、comparison 和 decision 文件。
- **SC-002**: 每份结果都能证明 50 次 Hify Rerank 的 applied/degraded 状态和输入数量。
- **SC-003**: Voyage token 用量、列表价估算与免费额度说明可核对，Key 未进入 Git diff 或日志。
- **SC-004**: 最终报告用同一张表并列 baseline、BGE、Voyage 指标、延迟和结论。
- **SC-005**: 全量 race、vet、依赖检查、检索门禁和 `git diff --check` 通过；数据库测试不得静默 skip。
- **SC-006**: Voyage raw run 扫描不到非空 query text；离线重算结果与脱敏前一致，且没有外部 API 请求。
- **SC-007**: 单测证明 degraded/熔断调用不会进入成功延迟，并证明失败分类只输出固定枚举与计数。

## 范围外

- 不评测 Cohere、Jina、Qwen 或其他新模型。
- 不生成答案、不使用 LLM Judge、不微调模型、不自动调参。
- 不修改人工 qrels、不扩大到完整 MIRACL 数据集。
- 不提交、不推送、不启用生产默认配置。
