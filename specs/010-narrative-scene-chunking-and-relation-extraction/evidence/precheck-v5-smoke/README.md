# extract/v5 小范围真实模型验证

- 模型：`qwen2.5:14b`，digest `7cdf5a0187d5c58cc5d369b255592f7841d1c4696d45a8c8a9489440385b22f6`
- 范围：开发集《阿Q正传》`ch01/ch02`，固定 `chunkSize=500`、`overlap=0`；13 个片段。
- 命令：`HIFY_PRECHECK_MODELS=qwen2.5:14b HIFY_PRECHECK_OUTPUT_DIR=specs/010-narrative-scene-chunking-and-relation-extraction/evidence/precheck-v5-smoke go test ./internal/knowledge -run '^TestNarrativePrecheck$' -count=1 -v -timeout 25m`
- 产物：14 条原始 HTTP 响应和 `summary.json`。预检有意绕过生产的账本、熔断、重试，不能当作全书效果或成本数字。

## 结果

- 抽取：13 次；12 条 JSON 通过、11 个片段完成服务端解析，1 条引用无法核验。
- 归一：仅 1 个片段需要调用；该调用在 60 秒客户端超时，没有响应。
- 延迟：p50 `13.947s`、p95 `26.961s`，另有 1 次超过 60 秒。

`ch01-005` 的 alias proposal 现在输出 `left:"m1"`、`right:"m2"`；旧 `extract/v4` 在同一片段输出的是人物名字，和服务端要求的 mention ref 相冲突。这个样本证明协议修复生效，但样本量不足以给出成功率或决定全书门禁。

