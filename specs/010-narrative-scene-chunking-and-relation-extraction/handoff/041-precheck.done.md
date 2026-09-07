# T041 模型预检交接

日期：2026-09-07。未修改 `tasks.md`，未触碰 `eval/annotations/`，未 commit/push。

## 改动文件

- 新增 `internal/knowledge/narrative_precheck_test.go`：复用生产 `chunkNarrative`、extract/alias prompt、fit、parse、resolve；门禁环境变量未设置时 skip。HTTP 有意直连 Ollama，绕过 `provider.ChatOnce` 的限流、熔断和账目。
- 新增 `evidence/precheck/raw/<model>/`、`summary.json`、`README.md`。
- 更新本交接文档。

## 四条验收命令实际输出

1. `GOCACHE="$PWD/.tmp-gocache" go vet ./internal/knowledge/`

```text
(无输出，退出码 0)
```

2. `GOCACHE="$PWD/.tmp-gocache" go test ./internal/knowledge/ -run TestNarrativePrecheck -count=1 -v`

```text
=== RUN   TestNarrativePrecheck
    narrative_precheck_test.go:78: HIFY_PRECHECK_MODELS 未设置，跳过本地模型预检
--- SKIP: TestNarrativePrecheck (0.00s)
PASS
ok   hify/internal/knowledge  1.002s
```

3. `GOCACHE="$PWD/.tmp-gocache" HIFY_PRECHECK_MODELS=qwen2.5:14b,qwen2.5:7b go test ./internal/knowledge/ -run TestNarrativePrecheck -count=1 -v -timeout 40m`

```text
--- PASS: TestNarrativePrecheck (691.01s)
    --- PASS: TestNarrativePrecheck/qwen2.5:14b (431.38s)
    --- PASS: TestNarrativePrecheck/qwen2.5:7b (259.62s)
PASS
ok   hify/internal/knowledge  691.939s
```

4. `GOCACHE="$PWD/.tmp-gocache" go test ./... -race -count=1`

```text
?    hify/cmd/evalrunner [no test files]
?    hify/cmd/hify [no test files]
ok   hify/internal/agent 1.629s
ok   hify/internal/conversation 5.030s
ok   hify/internal/eval 2.860s
ok   hify/internal/eval/retrieval 2.172s
ok   hify/internal/knowledge 17.802s
ok   hify/internal/mcp 2.266s
ok   hify/internal/provider 6.074s
ok   hify/internal/server/middleware 2.733s
ok   hify/internal/workflow 5.354s
```

退出码均为 0。第一次不带 `GOCACHE` 的带模型命令因沙箱拒绝系统 cache 退出码 1，未进入测试；改用工作区临时 cache 后完成验收。

## 产物计数

`raw/` 共 26 个 JSON；`summary.json` calls 合计 `13+0+13+0=26`，一致。

模型摘要事实：14B digest `7cdf5a0187d5c58cc5d369b255592f7841d1c4696d45a8c8a9489440385b22f6`，13 次 extract，JSON valid 9、invalid 4、resolve ok 3、resolve failed 6；7B digest `845dbda0ea48ed749caafd9e6037047aa19acfcfd82e704d7ca97d631a0b697e`，13 次 extract，JSON valid 0、invalid 13。两者本次均未触发 alias 调用。

## 开放问题

- 7B 的 13 次 extract 中 11 次是截断、2 次归入缺字段/其他；14B 仍有 4 次 JSON 无效、6 次引用定位失败。按红线未改 prompt、schema、校验或放宽引用规则。
- 本次只覆盖两章、13 个块/模型；没有人工真值，不能推出 precision、recall、准确率或生产质量结论。
- 预检直连 Ollama，绕过 `provider.ChatOnce`，因此不包含生产限流、熔断、账目和真实 provider 路径证据。

## 第二轮：按 review 修正

按 `041-precheck.review.md` 修正统计归类，未重跑模型；没有修改 prompt、schema、校验逻辑、`eval/annotations/` 或 `tasks.md`，未 commit/push。

### 修正内容与重算结果

- 归类只看响应事实：content 去空白后是否以 ``` 开头、`finish_reason == "length"`，以及 HTTP 是否拿到响应；不再用错误文本判断围栏/截断。
- HTTP 失败独立计为 `call_failed`，不进入 JSON 分母；`calls` 是发起调用数，`json_valid/json_invalid` 分母是拿到响应的次数。
- 兜底桶改为「其他」；HTTP 原始错误继续保存在对应 raw 的 `error` 字段。
- `HIFY_PRECHECK_RECOMPUTE=1` 只读取现有 raw 重算 `summary.json`，本次实跑没有请求 Ollama。

| 模型 | 口径 | 修正前 | 修正后 |
|---|---|---:|---:|
| 14B | json_valid | 9 | 9 |
| 14B | json_invalid | 4 | 1 |
| 14B | call_failed | 未单列 | 3 |
| 14B | 围栏 / 截断 | 0 / 0 | 0 / 0 |
| 7B | json_valid | 0 | 0 |
| 7B | json_invalid | 13 | 11 |
| 7B | call_failed | 未单列 | 2 |
| 7B | 围栏 / 截断 | 0 / 11 | 11 / 0 |

复核四条：7B 围栏 `11/13` 与 `reject_reasons.围栏=11` 一致；`finish_reason=length` 为 0 且截断为 0；14B 的 `call_failed=3` 与 `over_60s=3`，7B 为 `2` 与 `2`；14B `9+1+3=13`，7B `0+11+2=13`。本批 raw 没有 digest 字段，所以重算后的两个 digest 为空，已在 README 记录，未猜填。

### 第二轮四条命令实际输出

1. `GOCACHE="$PWD/.tmp-gocache" go vet ./internal/knowledge/`

```text
(无输出，退出码 0)
```

2. `GOCACHE="$PWD/.tmp-gocache" go test ./internal/knowledge/ -run TestNarrativePrecheck -count=1 -v`

```text
=== RUN   TestNarrativePrecheck
    narrative_precheck_test.go:84: HIFY_PRECHECK_MODELS 未设置，跳过本地模型预检
--- SKIP: TestNarrativePrecheck (0.00s)
PASS
ok   hify/internal/knowledge  0.592s
```

3. `GOCACHE="$PWD/.tmp-gocache" HIFY_PRECHECK_RECOMPUTE=1 go test ./internal/knowledge/ -run TestNarrativePrecheck -count=1 -v`

```text
=== RUN   TestNarrativePrecheck
--- PASS: TestNarrativePrecheck (0.01s)
PASS
ok   hify/internal/knowledge  0.600s
```

4. `GOCACHE="$PWD/.tmp-gocache" go test ./... -race -count=1`

```text
?    hify/cmd/evalrunner [no test files]
?    hify/cmd/hify [no test files]
ok   hify/internal/agent 1.546s
ok   hify/internal/conversation 5.495s
ok   hify/internal/eval 1.977s
ok   hify/internal/eval/retrieval 1.438s
ok   hify/internal/knowledge 14.891s
ok   hify/internal/mcp 3.114s
ok   hify/internal/provider 3.719s
ok   hify/internal/server/middleware 3.510s
ok   hify/internal/workflow 4.873s
```

退出码均为 0；其余包为 `[no test files]` 或同样通过。

### 开放问题（更新）

- 7B 的 11/13 条响应以 Markdown 围栏开头；14B 的 1 条拿到响应但 JSON 无效，3 条 HTTP 调用失败。7B 围栏问题与 14B 引用编造/定位问题仍未修复，留给 T042 之后。
- 本批 raw 缺少模型 digest 字段，离线重算无法恢复 digest；下次预检需在 raw 记录中补存该字段。
- 本次只覆盖两章、13 个块/模型；没有人工真值，不能推出 precision、recall、准确率或生产质量结论。
- 预检直连 Ollama，绕过 `provider.ChatOnce`，因此不包含生产限流、熔断、账目和真实 provider 路径证据。
