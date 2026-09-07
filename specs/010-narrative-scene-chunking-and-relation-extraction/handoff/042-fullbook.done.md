# T042 fullbook-14b 回执

本轮已加入生产路径门禁测试 `internal/knowledge/fullbook_14b_test.go`，并在 runner 中累加 `AmbiguousPositions` 与 `AmbiguousEvidence`。没有设置 `HIFY_FULLBOOK_MODEL`，所以真实 14B 未执行，未生成或伪造 fullbook evidence 数字。

## 验收命令

- `go vet ./internal/knowledge/`：通过（使用临时 `GOCACHE`；默认缓存目录被环境权限拒绝）。
- `go test ./internal/knowledge/ -run TestFullBook -count=1 -v`：通过，`TestFullBook14B` SKIP，原因是 `HIFY_FULLBOOK_MODEL 未设置`。
- `go test ./... -race -count=1`：通过；各包均为 `ok` 或 `[no test files]`，`internal/knowledge` 为 `ok ... 15.093s`。

## 本轮状态

- 真实模型运行：未开始；停在门禁变量检查，不是租约、预算、超时或事务发布失败。
- 运行时长：真实运行 0s；门禁测试约 0.93s；全量 race 约 15.1s。
- 产物：`evidence/fullbook-14b/raw/`、`ledger.json`、`quality.json`、`results.json` 尚未生成，因为没有真实运行账目。
- 直连实现：测试客户端已实现 Ollama OpenAI-compatible `/v1/chat/completions`，但本轮未调用；若下轮设置变量，将显式记录绕过 `provider.ChatOnce`。

## 开放问题

1. 需要在可访问 Ollama 的环境设置 `HIFY_FULLBOOK_MODEL=qwen2.5:14b` 后运行门禁；不得改用 7B。
2. 运行前需确认 `HIFY_OLLAMA_BASE_URL`（默认 `http://127.0.0.1:11434`）及真实模型 digest，并把 digest 写入 README。
3. 尚无人工真值，因此后续报告仍只能写覆盖率、账目、合法率、可核验引用率、多义证据等，不得写准确率/召回率/精确率。

## 第二轮：真实运行

### 实际运行

- 命令：`HIFY_FULLBOOK_MODEL=qwen2.5:14b go test ./internal/knowledge/ -run TestFullBook14B -count=1 -v -timeout 90m`
- 首次实际输出尾部：`open /Users/lishurong/Library/Caches/go-build/...: operation not permitted`，退出码 1；这是环境缓存权限问题，未进入测试。
- 使用临时 `GOCACHE` 保持其余命令参数不变后，实际输出尾部：`=== RUN   TestFullBook14B`、`fullbook_14b_test.go:115: 文档分块数量超出单文档上限，请拆分文件后重新上传`、`--- FAIL: TestFullBook14B`、`FAIL`，退出码 1。
- 运行时长：临时缓存重跑测试进程约 1.644s（墙钟）；模型活跃时长 0s。
- 停止位置：document upload；job 未创建，state=`not_created`，stop_reason=`upload_rejected_too_many_chunks`。
- 账目：0 次预留、0 次确认派发、0 次未知调用、0 条 `relation_extraction_attempts`。

### 四个产物关键数字

- `ledger.json`：`reserved_calls=0`、`confirmed_dispatches=0`、`unknown_attempts=0`、`attempts=0`、`active_ms_used=0`。
- `quality.json`：合法率、可核验引用率、`AmbiguousPositions`、`AmbiguousEvidence` 均为不适用；其他桶为 1 条，含真实错误串“文档分块数量超出单文档上限，请拆分文件后重新上传”。
- `results.json`：characters/relations/evidence 均未产生（null）。
- `raw/`：无记录；保留目录，因未创建 attempt，失败与未知结局记录数为 0。

### 开放问题

合并 ch03～ch09 的单文档上传被既有单文档分块上限拒绝，真实模型尚未获得调用机会。按红线未修改 prompt、校验、schema、契约，未拆分重跑，未跑 7B，未报告精确率/召回率/准确率，未碰 eval/annotations、tasks 勾选、commit 或 push。

## 第三轮：修好夹具后的真实运行

### 实际运行

- 原命令：`HIFY_FULLBOOK_MODEL=qwen2.5:14b go test ./internal/knowledge/ -run TestFullBook14B -count=1 -v -timeout 90m`
- 原命令实际输出尾部：`open /Users/lishurong/Library/Caches/go-build/...: operation not permitted`，退出码 1；测试尚未启动。
- 仅切换临时 `GOCACHE` 后，保持其余命令参数不变真实运行；输出尾部：`--- PASS: TestFullBook14B (1005.11s)`、`PASS`、`ok hify/internal/knowledge 1006.014s`。
- 墙钟：`1005.092s`；活跃时长：`1001.982s`。
- 全书上传成功，分块/item 总数为 62；实际处理到第 18 个 item outcome 后停止，覆盖 17 个 chunk。
- 作业 state=`paused`，stop_reason=`consecutive_failures`；不是上传守卫、预算耗尽或租约丢失。
- 账目：45 次预留，44 次确认派发，1 次未知调用；45 次 attempts，其中 18 completed、26 failed、1 unknown；已知 usage 44 次。

### 四个产物关键数字

- `ledger.json`：`total=62`、`succeeded=6`、`failed_items=12`、`active_ms_used=1001982`、`wall_ms=1005092`、`AmbiguousPositions=8`、`AmbiguousEvidence=0`。
- `quality.json`：`legal_rate=0.4`；`verifiable_quote_rate=null`；`ambiguous_positions=8` 与 `ambiguous_evidence=0` 分开；“其他”桶含真实错误串，包括 `alias phase: knowledge: alias response invalid: 1 decisions for 0 mentions`，并记录 `http_error`。
- `results.json`：characters=5、relations=6、evidence=9。
- `raw/`：45 条 attempt 原始记录。
- `README.md`：记录 14B、墙钟/活跃时长、`cost_kind=not_applicable`（未填金额）、契约未改和无准确率/召回率/精确率声明。

### 开放问题

1. 14B 真实运行在连续失败停止守卫处暂停，尚未覆盖剩余 44 个 item；失败主要来自模型输出违反现有 extract/alias/quote 契约，具体错误串已写入 raw 与 quality。
2. 默认 Go build cache 权限仍不可用；本轮使用临时 `GOCACHE`，不影响测试业务条件。
3. 未修改 prompt、校验、schema、契约，未碰 `eval/annotations/`、`tasks.md`，未 commit/push。

---

# Review（Claude / Opus 5，2026-09-07）

## 硬检查

| 检查 | 结果 |
|---|---|
| raw 条数 == 账目 attempts 总数 | ✅ 46 个文件 = 45 次尝试 + 1 个 `.gitkeep` |
| 只跑 14B | ✅ |
| prompt / 校验 / schema 未改 | ✅ git diff 只有 runner 的多义计数与新增测试 |
| 「其他」桶带真实错误串 | ⚠️ 带了，但**只有样本没有分布**（见下） |
| `AmbiguousEvidence` 单独报 | ✅ = 0 |

## 这一跑真正发生了什么

**链路第一次跟真实模型跑，守卫全部按设计生效**：62 块跑到第 18 块时连续 5 次失败，
作业停成 `paused` / `consecutive_failures`，44 块从未被处理。账目一条不少：
45 次尝试、44 次确认发出、1 次未知（60s 超时的归一调用），usage 44/45 已知，
输入 42968 / 输出 10447 token；墙钟 1005s ≈ 活跃 1002s（说明 worker 连续在跑，没有排队）。

产出：5 个人物、6 条关系、9 条证据（关系类型只有 冲突 5、同伙 1）。

## 我自己从数据库补的分布（回执里缺这个）

`quality.json` 只给了 4 条错误样本，没给各类计数。作业还在库里，我直接查了：

| item 失败原因 | 次数 |
|---|---|
| extract_invalid | 5 |
| quote_unverifiable | 4 |
| alias_invalid | 3 |

| 阶段 × 结局 | 次数 | 平均耗时 |
|---|---|---|
| extract completed | 13 | 15.3s |
| extract failed | 15 | 18.3s |
| **alias completed** | **5** | **31.3s** |
| **alias failed** | **11** | **28.3s** |
| alias unknown | 1 | 60.0s（超时） |

**归一阶段才是瓶颈**：失败 11 / 成功 5，且平均耗时是抽取阶段的两倍。
前三轮预检基本没触发归一（候选池是空的），全书跑起来候选积累之后问题才暴露——
这正是"必须在真实规模上跑一次"的价值。

⚠️ 这是第三次因为汇总只给样本/总数而需要我手工挖分布（T041 的「其他」桶、
v4 的 9 条、这次的失败分布）。**下次工单要写死：任何"原因"字段必须给
`{原因: 次数}` 的完整分布，样本消息只是补充。**

## 由此发现并已修掉的一个真实缺陷（我改的，不是 Codex 的问题）

`needsAliasPhase` 只看提案和候选，**不看这一块有没有人物**。于是一块纯写景的段落
（抽不到任何 mention），只要候选池非空（跑到后面必然非空）就会发一次归一调用；
模型无物可判，随便返回一条，校验报 `1 decisions for 0 mentions`，
于是**一个合法的空结果被判成 item 失败**。

三重代价：白花一次约 28s 的调用；覆盖率分子少一块；而且这个**假失败会计进
"连续失败"**，把作业推向提前停止。这一跑的 12 个失败里有 3 个是这么来的。

已修（`needsAliasPhase` 增加 `len(Mentions) > 0`）并加了回归测试，
做过变异检查：去掉这行，用例会红。

## 结论：这一跑不算 T042 完成

- 62 块只处理了 18 块，**全书覆盖没有达成**，成本与覆盖率数字只对这 18 块成立。
- 现在修好了假失败那一条，重跑会走得更远，但**归一阶段 11/16 的真实失败率还在**，
  仍然大概率撞连续失败停止。
- 因此下一步不是"再跑一遍"，而是先决定归一阶段怎么办（见下一节）。

## 留给决策的问题

归一阶段对 14B 太难：它要同时读懂原文、候选列表、四种 reason_code，并为每个称呼
恰好产出一条决策。可选方向（都要改契约，需要显式取舍）：
1. 候选为空时跳过归一，只在**确实有候选**时才调用（现在候选一旦积累就永远非空）；
2. 把归一拆成更小的判断（一次只判一个称呼），用更多次更简单的调用换成功率；
3. 接受归一失败不致命：归一失败时退回"各 mention 独立成人物"，
   item 仍然成功发布关系——⚠️ 代价是人物碎片化，且要显式标注这一块没归一过。

第 3 条最省事也最危险：它会让"没归一"变成静默的默认行为，而碎片化会直接压低
后面的召回率。要做的话必须在数据里留标记，不能只在日志里。
