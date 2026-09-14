# T042 run3：14B / alias candidates 8

- 命令显式设置了 `HIFY_FULLBOOK_OUTPUT_DIR`，本轮流式响应只写入本目录。
- 运行在 3922.199 秒时按 60 分钟实验墙钟门禁中止；Go 的 90 分钟保护性 timeout 尚未触发。
- 62 个 item 的停止现场：28 succeeded、19 failed、14 pending、1 running；这不是一次完整全书运行。
- `raw/` 有 126 份响应，数据库有 127 条 attempt；差额是中止时的 1 条 alias reserved。
- 人物/关系/证据为停止现场的 81/78/92，不能与完整运行结果等同比较。
- 人工真值尚未冻结，因此本目录不生成 `quality.json`，也不报告精确率、召回率或准确率。
- 原 `evidence/fullbook-14b/` 在本轮运行后保持 Git clean；run2 未被覆盖。

完整账目见 `ledger.json`。第一次误写后抢救的 32 分钟现场另存于相邻的
`fullbook-14b-run3-cand8-interrupted/`，不可与本轮合并计数。
