# T042 prompt 定向迭代

开发集固定为 `ch01/ch02`，分块参数固定为 `chunkSize=500、overlap=0`。本轮没有读取、请求或依据 `ch03～ch09` 的任何输出；它们保留为后续验收集。

round-0 直接复制 T041 的 `evidence/precheck/summary.json`，没有重跑。round-1 和 round-2 使用现有 `TestNarrativePrecheck`，每次调用的原始响应写入对应 `raw/`；`prompt.md` 是该轮抽取指令快照。测试写入临时 `evidence/precheck/` 后，再复制到本目录，避免混淆轮次。

## 这轮改了什么/为什么

- round-1：`extract/v1` → `extract/v2`。把纯 JSON 要求改成可执行的输出边界和回复前检查清单，并要求先从原文逐字复制 quote；目的是降低 7B Markdown 围栏和模型编造引用。
- round-2：`extract/v2` → `extract/v3`。进一步强调 quote 必须在原文中实际核对、不能把符合情节但原文没有的句子当引用，并要求从原文实际计数 occurrence；目的是定向压低引用定位失败。
- alias 指令未改，`aliasPromptVersion` 未 bump。

## 数字对照

| 轮次/模型 | extract / alias | call_failed | json_valid / json_invalid | 围栏 | resolve_ok / failed | 引用找不到* | p50 / p95 | over_60s |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| baseline 14B | 13 / 0 | 3 | 9 / 1 | 0 | 3 / 6 | 6 | 28.6s / 60s | 3 |
| baseline 7B | 13 / 0 | 2 | 0 / 11 | 11 | 0 / 0 | — | 12.1s / 60s | 2 |
| round-1 14B | 13 / 0 | 2 | 10 / 1 | 0 | 3 / 7 | 7 | 27.2s / 60s | 2 |
| round-1 7B | 13 / 0 | 1 | 8 / 4 | 0 | 3 / 5 | 5 | 5.1s / 14.1s | 1 |
| round-2 14B | 13 / 0 | 4 | 7 / 2 | 0 | 4 / 3 | 3 | 31.1s / 60s | 4 |
| round-2 7B | 13 / 1 | 0 | 7 / 6 | 1 | 3 / 4 | 4 | 5.4s / 10.2s | 0 |

`json_valid/json_invalid` 的分母是拿到响应的次数，`call_failed` 独立计数；`resolve_ok/failed` 只在 JSON 合法响应上统计。`引用找不到*` 沿用 T041 报告口径：resolve 失败中的引用定位失败；当前实现的细分桶把包含 occurrence 的错误归到“occurrence越界”，所以表中同时保留 `resolve_failed`，不把它伪装成更精确的原因。

round-2 7B 的 13 次 extract 之外还有 1 次 alias 调用，因此该模型 raw 为 14 条；两模型合计 raw 27 条，与 summary 的 calls 总和一致。

## 结论

指令层面明显改善了 7B 的围栏问题，但不稳定：round-1 为 0，round-2 又出现 1 条；不能宣称 7B 已可靠可用。14B 的可核验引用从 3 块波动到 3、4 块，仍有多次引用定位失败，不能宣称解决编造引用。三轮上限内未继续第三轮：当前证据支持“指令增强不足以把 14B 引用问题压下去；7B 围栏也未稳定消除”，后续应考虑换模型或换方案。

## 离线重算

现有 `HIFY_PRECHECK_RECOMPUTE=1` 分支只读 `evidence/precheck/raw/` 并重算 summary，不发起 HTTP 请求。它用于 T041 基线重算；本目录的每轮 summary 是该轮实跑产物副本。
