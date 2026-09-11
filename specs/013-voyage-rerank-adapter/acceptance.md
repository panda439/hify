# Voyage Rerank 适配验收

## 结论

实现与工程门禁通过。Hify 已能根据 Provider 的 `rerank_format=voyage` 使用 `top_k/data`
协议调用 `rerank-3`；未配置该字段的 Provider 保持原 `top_n/results` 行为。

## 真实调用证据

- Provider：`voyage-ai`，Key 保持加密存储，配置为 `rerank_format=voyage`。
- 模型：`rerank-3`。
- Hify 真实链路：MySQL Provider/Model → Key 解密 → registry → HTTP adapter → Voyage。
- 固定中文样本得分：相关文档 `0.894531`，两个无关文档 `0.250000`、`0.249023`。
- `TestVoyageRerankLive`：PASS，耗时约 `1.26s`。

## 工程验证

- TDD RED：Voyage 构造器、格式常量和配置字段缺失，目标测试按预期编译失败。
- Provider 全包测试：PASS。
- `go test -race -count=1 ./...`：PASS，真实 Voyage 测试包含在本次运行中。
- `go vet ./...`：PASS。
- `make check-deps`：PASS。
- `make eval-retrieval-gate`：14/14 PASS。
- `git diff --check`：PASS。

## 边界

- 本次只证明协议适配和真实调用，不比较 Voyage 与 BGE 的 Recall、MRR、MAP、NDCG。
- Voyage 尚未设为 Hify 默认 Rerank，不会自动产生调用费用。
- 未提交、未推送。
