# Implementation Plan: Voyage Rerank 对照评测

**Branch**: `014-voyage-rerank-benchmark` | **Date**: 2026-09-11 | **Spec**: [spec.md](./spec.md)

## 方案概述

014 在独立 worktree 中机械复用 012 的 MIRACL benchmark 能力与 013 的 Voyage 协议适配，
再把 012 中仅适用于本地 sidecar 的身份、统计和决策逻辑抽成“本地 BGE / 托管 API”两种证据源。
查询始终经过 `knowledge.Service.Retrieve → provider.Client.Rerank`，不直接请求 Voyage。

## 关键设计

### 1. 固定输入与制品隔离

只读复用 011 baseline、012 BGE 报告以及同一 MIRACL Mini 数据。Voyage 的 1.5 秒和 30 秒
制品使用独立文件名，compare 继续拒绝任何非 Rerank 指纹差异。

### 2. 托管 Rerank 证据

Voyage 没有本地 `/health`、`/stats` sidecar，因此身份来自 Hify 数据库中的 Provider/Model 配置，
真实性来自 Hify observer 的 applied/degraded/input/duration，加上 Voyage 成功响应的
`usage.total_tokens`。observer 保存每次 Rerank 延迟并重算 p50/p95，不保存 query、正文或逐条分数。

### 3. 用量与费用

`provider.RerankResult` 增加可选 token usage，Voyage adapter 读取响应中的 `usage.total_tokens`；
knowledge 只向 benchmark observer 上报聚合值。报告按运行时固定的官方价格快照计算列表价估算，
并单独说明账户免费额度，不能把估算价写成实际扣款。

### 4. 两类门禁

- `deployment_gate`：固定 1.5 秒，50 次均 applied、零 degraded 后才评指标。
- `quality_diagnostic`：固定 30 秒，只判断质量，不覆盖部署结论。

决策改为模型无关：Recall@10 不低于 baseline；MRR/MAP/NDCG@10 均不下降且至少一项提升。
证据缺失为 INCONCLUSIVE，真实下降为 FAIL/DO_NOT_ADOPT。

### 5. 最终对照报告

读取 baseline、BGE deployment/quality、Voyage deployment/quality，输出同表指标、延迟、完整性、
token、列表价估算和结论。BGE 的既有结论原样引用，不回算成对 Voyage 有利的新标准。

### 6. 账户限流下的限速质量诊断（FR-015/FR-016，2026-09-11 追加）

首轮真实运行确认 Voyage 账户未添加付款方式（3 RPM / 10K TPM），1.5s 与 30s 两轮都只有 1/50 次
实际应用 Rerank，制品保留为不完整证据。用户决定不添加付款方式，只对质量诊断限速：

- CLI 新增 `--pacing-interval`，只允许 `hosted_api` + `quality_diagnostic`；相邻 query 起点至少间隔
  65 秒（单次调用约 6K tokens），检索、候选、Rerank 输入、timeout 与指纹均不变。
- raw run/report 记录 `rerank_pacing`（间隔、总等待、原因）；带限速记录的运行作为部署证据一律
  `INCONCLUSIVE`。

### 7. 审核修复：最小化证据与可解释统计（FR-017~FR-019）

- Hosted raw run 在保存前复制 query 列表并清空 `Text`；评分继续按 query ID 关联，禁止改变通用
  MIRACL prepared dataset。已有 Voyage raw run 先生成临时脱敏副本，确认重算制品逐字节一致后再原子替换
  原文件，并在报告记录替换前后 SHA256；绝不触发 `run`。
- observer 继续记录每次步骤总耗时，但 p50/p95 样本只收集 `Applied=true` 的调用；历史不完整运行中
  无法还原的成功延迟删除并在报告标为 unavailable，完整 50/50 paced run 的数值保持不变。
- provider 包负责把错误映射为固定、安全的 failure kind；knowledge observer 只透传枚举，benchmark
  汇总为 `map[kind]count`。不得持久化原始 error string 或响应 body。
- 既有运行没有结构化 failure kind，不反向猜测。报告删除“精确 429/timeout/circuit 次数已被制品证明”的
  表述，区分“运行时观察”与“当前可复核制品”。

修复采用 TDD：先覆盖正文脱敏、评分等价、成功延迟采样、固定失败分类和敏感信息扫描，再做最小实现。
验收只读取既有制品并离线重算，不发起 Voyage 请求。
- 制品使用独立 `-quality-paced` 前缀；1.5s 部署门禁在该账户下不重跑。

## TDD 与验收

先写以下失败测试：Voyage usage 解码；observer token/延迟聚合；托管模型证据校验；决策包含 MAP；
价格计算；两种 run 输出隔离。随后写最小实现使其通过，再运行真实 50-query 两种 timeout。

最终运行全量 race、vet、依赖检查、14 条检索门禁、011 baseline 重算和敏感信息扫描。

## 范围控制

不引入新依赖，不修改数据库 schema，不改前端，不运行其他付费模型，不自动启用默认 Rerank，
不提交或推送。
