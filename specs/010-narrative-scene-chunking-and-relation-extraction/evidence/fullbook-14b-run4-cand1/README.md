# T042 run4：14B / alias candidates 1

这是一次真实 Ollama 调用的全书工程实验，但在 60 分钟墙钟门禁处被取消，**不是完整
9 章运行，也不是效果验收**。

- 模型：`qwen2.5:14b`，digest `7cdf5a0187d5c58cc5d369b255592f7841d1c4696d45a8c8a9489440385b22f6`。
- 协议：`extract/v5`，schema 2；归一候选窗口仅为 1（实验参数，默认契约为 32）。
- 现场：62 个 item 中 23 succeeded、22 failed，余下未完成；125 次 attempt 中
  111 次已确认发出、13 次 unknown、1 次仍 reserved。
- 耗时：墙钟 3,600,018 ms，活跃调用 3,575,257 ms；本地 Ollama 无货币成本数据，
  不填写为 0。
- 已保存 `raw/`、`ledger.json`、`results.json`。`quality.json` 只有合法响应率
  和可核验引用率等工程口径，不含 precision、recall 或准确率。

`run_result.stop_reason=canceled`、job 状态仍为 `running` 是测试墙钟取消时的数据库现场，
不是业务任务成功结束。没有人工冻结真值，且本轮并未跑完整全书，因此不得把此目录的
数字作为模型效果或 T042 完成证据。
