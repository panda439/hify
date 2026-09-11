# Implementation Plan: Voyage Rerank 适配

**Branch**: `013-voyage-rerank-adapter`  
**Spec**: [spec.md](./spec.md)

## 技术方案

复用现有 `openAICompatClient`，不新增 Provider 类型。`ExtraConfig` 增加可选
`rerank_format`；registry 构造客户端时将该值传入 Rerank 协议层。

- 空值：保持现有 `{top_n}` 请求和 `{results}` 响应。
- `voyage`：发送编解码改为 `{top_k}` 请求和 `{data}` 响应。
- 两条路径最终归一成同一个内部结果结构，继续复用完整性校验、错误分类、重试、熔断和降级。

## 变更文件

- `internal/provider/model.go`：增加格式常量和 `ExtraConfig.RerankFormat`。
- `internal/provider/service.go`：创建、更新 Provider 时校验格式白名单。
- `internal/provider/registry.go`：把格式传给 HTTP 客户端。
- `internal/provider/openai_compat.go`：按格式编码请求、解析响应。
- `internal/provider/rerank_test.go`：Voyage 与通用格式回归测试。
- `internal/provider/service_test.go`：非法格式校验测试。
- `specs/013-voyage-rerank-adapter/`：规格、计划、任务和验收记录。

## 数据与兼容性

`extra_config` 已是 JSON 字段，因此不需要 migration。已有 Provider 缺少该字段时反序列化为
空值，行为保持不变。API Key 仍走原有 AES-256-GCM 加密存储。

## 验证

1. TDD：Voyage 请求、响应与非法格式先 RED 后 GREEN。
2. Provider 包及全量 Go race、vet、依赖和 diff 门禁。
3. 使用数据库中已加密的 Voyage Key，通过 Hify Provider 客户端真实调用
   `rerank-3`，确认相关文档排名第一；证据只记录状态、模型和分数，不记录 Key 或正文。
4. 012 的代码和评测制品不进入本分支。

## 风险控制

- 不按 URL 猜供应商，避免代理域名误判。
- 不自动启用全局 Rerank，避免无意产生费用。
- Voyage 返回不完整或异常结构时整体失败，由知识库层保持原排序。
