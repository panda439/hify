# 041-precheck Review（第一轮）：打回，只改统计口径，**不用重跑模型**

审阅人：Claude（Opus 5），2026-09-07

## 通过的部分

- **复用生产函数这条硬要求满足了**：`buildExtractInstruction` / `fitExtractionInput` /
  `parseExtractionResponse` / `resolveExtraction` / `buildAliasInstruction` /
  `parseAliasResponse` / `resolveAliasDecisions` / `needsAliasPhase` 全部调用的是包内实现，
  grep 不到任何自撰的指令字符串。预检测的是生产路径，不是副本。
- 门禁正确：未设 `HIFY_PRECHECK_MODELS` 时 SKIP，一个模型都不调（我重跑确认）。
- `go vet ./internal/knowledge/`、`go test ./... -race -count=1`、`make check-deps` 我都重跑过，全过。
- **26 条原始响应一条不少**，与 summary 的 calls 之和一致。正因为原始数据全，这次返工不需要重跑模型。

## 必改（三处，全在统计归类上）

### R1 围栏与截断的桶装反了，而且「围栏」分支永远走不到

事实（我从 raw 逐条数的）：

| | summary 报的 | 实际 |
|---|---|---|
| 7B 以 ```` ``` ```` 开头 | 0 | **11 / 13** |
| 7B `finish_reason == "length"` | 11 | **0**（全是 `stop`） |

根因在 `precheckReject`：带围栏的响应报的错误是
``invalid character '`' looking for beginning of value``，错误串里只有**一个**反引号，
而 `围栏` 分支找的是三个；同时 `截断` 分支排在前面并匹配 `invalid character`，
于是把围栏全吃掉了。

**这个错分会把结论引向完全相反的下一步**：
「11 次截断」→ 去调 token 上限或缩输入；
「11 次围栏」→ 是指令遵循问题（7B 不理会「不要 Markdown 围栏」这句）。

修法：**不要 match 错误文本**。按响应事实归类——
content 是否以 ```` ``` ```` 开头、`finish_reason` 是不是 `length`、是不是 HTTP 层就失败了。
错误串是给人看的，随时会变；拿它当分类依据，这类错分还会再发生。

### R2 调用失败被算成了「JSON 无效」

14B 的 `json_invalid: 4` 里有 **3 条是 HTTP 调用本身失败**（raw 里 error 以 `Post ` 开头、
content 为空），正是 `over_60s: 3` 那三次。真正 JSON 不合法的只有 1 条。
这 3 条同时被塞进了 `reject_reasons.缺字段`。

正确口径：13 块 = 3 块**没拿到响应** + 10 块有响应（9 合法 / 1 不合法）。

修法：加一层 `call_failed`（超时、连接错误）独立计数，**不进 json_valid/json_invalid 的分母**；
比率要写清楚分母是「拿到响应的次数」还是「发起的次数」。

### R3 兜底桶伪装成了具体原因

`precheckReject` 最后 `return "缺字段"`，把所有认不出来的错误都记成缺字段。
兜底桶必须显式叫 `其他/未分类`，并把**原始错误串写进 raw 记录**，否则下次再遇到新错误
仍然会伪装成一个看起来很具体的原因。

## 不用改的

- 不用重跑模型。26 条 raw 是全的，改完归类逻辑**从 raw 重算 summary** 即可。
  请提供一条可重跑的重算路径（比如同一个测试加一个 `HIFY_PRECHECK_RECOMPUTE=1` 的分支，
  只读 raw、不发请求），并在 README 里写明。
- prompt / schema / 校验逻辑仍然一个字都不要动。7B 的围栏问题、14B 的引用编造问题
  都留在「开放问题」里，那是 T042 之后的事。

## 复核时我会重新数的

1. `grep -c '```' 每条 7B raw 的 content 开头` 与 `reject_reasons.围栏` 是否一致
2. `finish_reason == "length"` 的条数与 `截断` 是否一致
3. `call_failed` 条数与 `over_60s` 的关系是否说得清
4. `json_valid + json_invalid + call_failed == calls`

---

# 041-precheck Review（第二轮）：通过

审阅人：Claude（Opus 5），2026-09-07

## 我自己重数的四条，全对

| 检查 | summary | 我从 raw 数 | |
|---|---|---|---|
| 7B 以围栏开头 | 11 | 11 | ✅ |
| 7B / 14B `finish_reason=length` | 0 / 0 | 0 / 0 | ✅ |
| call_failed（= over_60s） | 2 / 3 | 2 / 3 | ✅ |
| json_valid + json_invalid + call_failed == calls | 13 / 13 | 13 / 13 | ✅ |

另外重跑确认：`go vet` 通过；不设 env 时 SKIP；`HIFY_PRECHECK_RECOMPUTE=1` 只读 raw
（代码里提前 return，路径上没有任何 HTTP 调用）；`go test ./... -race -count=1` 全绿；
`make check-deps` 通过。5 条 call_failed 的原始错误串（`context deadline exceeded`）都在 raw 里。

## 修正后的真实口径

| | qwen2.5:14b | qwen2.5:7b |
|---|---|---|
| 块数 | 13 | 13 |
| 调用失败（60s 超时） | 3 | 2 |
| 拿到响应 | 10 | 11 |
| JSON 合法 | 9 | **0** |
| 其中引用能定位（resolve_ok） | **3** | 0 |
| 引用在原文里找不到 | 6 | — |
| 单块延迟 p50 / p95 | 28.6s / 60s | 12.1s / 60s |
| 平均每块抽到的 mention | 0.38 | 0 |

**7B 的 11 条响应全部带 Markdown 围栏**——不是截断，是不遵守「不要围栏」这条指令。
**14B 唯一那条不合法的响应**（被归进「其他」桶）我用真解析器问过了，是
`duplicate mention ref "m1"`：同一个 ref 给了两个不同人物。

## 遗留的小项（不阻塞，下一轮顺带做）

「其他」桶目前只记数量，不记真实拒绝原因。这一批里它只有 1 条，我已经手工问出来了；
但下次跑之前应当把 `parseExtractionResponse` 返回的错误串一并写进 raw 记录——
预检的用途正是"告诉我们模型具体怎么不守协议"，而语义层的拒绝（重复 ref、
关系类型越界、证据条数不合法……）现在全落在这个桶里，看不出是哪一种。

## 结论

T041 的产物可信，可以作为 T042 的输入。**但它给出的结论是负面的**：
当前 prompt 下，14B 13 块里最终只有 3 块的引用可核验，7B 一条合法输出都没有。
下一步不该是"跑全书"，而是先解决围栏、引用编造、60s 超时这三件事——
详见 tasks.md 的补录。
