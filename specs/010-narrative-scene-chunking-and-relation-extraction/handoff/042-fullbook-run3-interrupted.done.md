# T042 fullbook run3 cand8 回执

## 运行

- 命令：`HIFY_FULLBOOK_MODEL=qwen2.5:14b HIFY_FULLBOOK_ALIAS_CANDIDATES=8 go test ./internal/knowledge/ -run TestFullBook14B -count=1 -v -timeout 90m`
- 结果：约 1976.462 秒（32 分 56 秒）收到 `signal: interrupt`，未到 90 分钟 Go timeout；job 状态为 `running`。
- 总块数 62；已推进到 `chunk_index=26`（第 27 块，正处于 running 时中断）；中断时状态：成功 16、失败 10、pending 35、running 1；`degraded_items=6`。
- 本轮实际数据库 attempts 65；流式 raw 文件 64。差额 1 是中断时仍为 `extract/reserved`、尚未发出的调用。
- 运行尾部：

  ```text
  2026/09/08 15:44:11 WARN knowledge: alias phase failed ... model gave up after the attempt limit: alias phase, outcome=unknown ... code=http_error
  signal: interrupt
  FAIL    hify/internal/knowledge    1976.462s
  ```

## run2 vs run3

| 口径 | run2 | run3 cand8 |
|---|---:|---:|
| 处理块数 / 成功 / 失败 / pending | 44 / 26 / 17 / 18 | 62 / 16 / 10 / 35（另 running 1） |
| degraded_items | 12 | 6 |
| characters / relations / evidence | 69 / 72 / 89 | 39 / 38 / 45 |
| 归一 completed / failed / unknown；平均耗时 | 12 / 26 / 16；43.0s / 33.7s / 60.0s | 8 / 9 / 9；41.786s / 33.426s / 60.001s |
| 抽取 completed / failed；平均耗时 | 35 / 27；19.8s / 20.4s | 20 / 18；19.039s / 21.928s（另 reserved 1） |
| 墙钟 / 活跃秒数 | 3600s / 3595s | 1976.462s / 1950.634s |

## 原因完整分布

- item `last_error_code`：`{extract_invalid: 6, quote_unverifiable: 4}`。
- attempts：extract failed `{invalid_output: 18}`；alias failed `{invalid_output: 9}`；alias unknown `{http_error: 9}`；completed `{<none>: 28}`；reserved `{<none>: 1}`。

## 产物与开放问题

- 产物目录：`evidence/fullbook-14b-run3-cand8/`；原有 `evidence/fullbook-14b/` 已恢复，未覆盖 run2。
- `raw/`：64 个流式归档文件；数据库 attempts：65 条，已在 `ledger.json` 核对。
- 开放问题：本轮在 90 分钟上限前被外部 SIGINT 中断，未完成 62 块，因此该对照只能按中断现场数字阅读；run3 的 characters/relations/evidence 是中断时已发布部分，不是全书完成量。数字本身不作“更好/更差”定性。
- 未修改 prompt、校验、schema、契约、生产常量、`eval/annotations/` 或 `tasks.md`；未 commit/push。
