# Feature Specification: Voyage Rerank 适配

**Feature Branch**: `013-voyage-rerank-adapter`  
**Created**: 2026-09-11  
**Status**: Accepted

## 目标

让 Hify 现有 `provider.Client.Rerank` 能通过已配置的 Voyage Provider 调用
`https://api.voyageai.com/v1/rerank`，支持 `rerank-3`，同时保持现有本地 BGE、Jina、
Cohere 等通用 Rerank 契约不变。

## 用户场景

管理员为 Provider 配置 `extra_config.rerank_format=voyage` 并注册 `rerank-3` 模型后，
Hify 可以使用该模型重排检索候选。未配置该标记的 Provider 继续走现有通用格式。

## 功能要求

- **FR-001**: Provider 的 `extra_config` MUST 支持可选字段 `rerank_format`，当前仅允许空值或 `voyage`。
- **FR-002**: `rerank_format=voyage` 时，请求 MUST 使用 `top_k=len(documents)`；不得同时发送 `top_n`。
- **FR-003**: Voyage 请求 MUST 固定 `return_documents=false`，并保留 `model`、`query`、`documents`。
- **FR-004**: Voyage 响应 MUST 从 `data` 读取 `index` 与 `relevance_score`。
- **FR-005**: 非 Voyage Provider MUST 保持现有 `top_n` 请求与 `results` 响应契约，不产生行为变化。
- **FR-006**: 两种格式 MUST 共用既有完整性校验：结果数量完整、index 不越界/不重复/不缺失、分数可解析且为有限数。
- **FR-007**: Voyage 的 4xx、429、5xx、超时及非法响应 MUST 继续进入既有错误分类、熔断和静默降级路径，不得导致对话失败。
- **FR-008**: API Key MUST 继续由 Provider 模块加密保存，不写入代码、规格、测试、日志或 Git 跟踪文件。
- **FR-009**: 不得按域名自动识别 Voyage；格式必须由 Provider 配置显式声明。

## 验收标准

- **SC-001**: 单元测试证明 Voyage 请求只含 `top_k`，响应 `data` 能映射为完整分数。
- **SC-002**: 既有通用 Rerank 请求/响应测试不修改预期且全部通过。
- **SC-003**: 非法 Voyage 响应触发整体失败，不返回部分结果。
- **SC-004**: 使用真实 Voyage `rerank-3`，通过 Hify Provider 客户端完成一次中文相关性调用，相关文档排名第一。
- **SC-005**: `go test -race -count=1 ./...`、`go vet ./...`、`make check-deps`、`git diff --check` 全部通过；数据库测试不得静默跳过。

## 范围外

- 不比较 Voyage 与 BGE 的质量指标；模型 A/B 另立评测任务。
- 不接入 Cohere、Jina 或其他新的供应商格式。
- 不修改 012 的数据集、qrels、阈值、报告和结论。
- 不自动开启 `HIFY_RAG_RERANK_ENABLED`，不把 Voyage 设为默认模型。
- 不新增前端表单；本期通过现有 Provider API 配置 `extra_config`。

## 约束与假设

- 已存在名为 `voyage-ai` 的 Hify Provider，其 API Key 已加密保存，且注册了 `rerank-3`。
- Voyage 当前 REST 路径为 `/v1/rerank`，请求字段为 `top_k`，结果数组字段为 `data`。
- 本功能只解决协议适配，不据此宣称付费模型质量优于本地模型。
