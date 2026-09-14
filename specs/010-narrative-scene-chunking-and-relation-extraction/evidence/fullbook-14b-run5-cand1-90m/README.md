# T042 run5：14B / alias candidates 1 / 90m wall clock

本轮真实调用在约 16.5 分钟结束，原因不是 90 分钟墙钟门禁，而是连续 5 个 item
最终失败后，关系抽取的止损保护将 job 暂停：`stop_reason=consecutive_failures`。

- 模型：`qwen2.5:14b`，digest `7cdf5a0187d5c58cc5d369b255592f7841d1c4696d45a8c8a9489440385b22f6`。
- 协议：`extract/v5`，schema 2；归一候选窗口 1（实验参数，默认 32）。
- 现场：62 个 item 中仅 9 succeeded、7 failed，job paused；40 次 attempt 中 37 次
  已确认发出、3 次 unknown。API 调用虽完成 37 次，但服务端结构/引用/归一校验后仅
  有一半请求合法（`quality.json.legal_rate=0.5`）。
- 结果现场：12 人物、11 关系、16 条证据；这些都是被暂停时的局部数据。
- 耗时：墙钟 992,862 ms，活跃调用 989,148 ms。本地 Ollama 无货币成本数据，不填 0。

`raw/`、`ledger.json`、`results.json` 和 `quality.json` 已归档。这里没有人工冻结真值，
也不是完整全书运行；任何字段都不得作为 precision、recall、准确率或 T042 完成证据。
