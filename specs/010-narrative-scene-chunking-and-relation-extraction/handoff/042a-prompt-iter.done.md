# T042 prompt 迭代回执

完成 round-1、round-2，未进行 round-3。只改了 `internal/knowledge/extract_prompt.go` 的抽取指令文本，并将 `extractPromptVersion` bump 到 `extract/v3`；`aliasPromptVersion` 未改。解析围栏、引用定位、封闭 8 类关系均未放宽。

产物见 [`evidence/prompt-iter/README.md`](../evidence/prompt-iter/README.md)，每轮包含 prompt 快照、summary 和全部 raw 响应。round-0 summary 是 T041 基线的直接副本。

## 验收命令实际输出

- `go vet ./internal/knowledge/`：`通过`。
- `go test ./internal/knowledge/ -run TestNarrativePrecheck -count=1 -v`（未设置 `HIFY_PRECHECK_MODELS`）：`SKIP`，输出为 `HIFY_PRECHECK_MODELS 未设置，跳过本地模型预检`。
- `go test ./... -race -count=1`：`通过`，所有包测试通过。

本轮模型实跑使用临时 `GOCACHE=/tmp/hify-go-cache`，因为默认 Go build cache 在受限环境中不可读；这不改变测试代码或运行口径。

## 隔离声明

开发集与验收集严格隔离：本轮只读取并请求 `eval/corpus/aq-010/ch01.txt`、`ch02.txt`，固定 `chunkSize=500、overlap=0`；没有读取、请求、查看或依据 `ch03～ch09` 的输出。各轮 raw 的 chunk id 仅为 ch01/ch02。

## 开放问题

- 7B round-1 围栏为 0，但 round-2 为 1，说明指令改善不稳定，不能判定 7B 可用。
- 14B round-1/2 的 `resolve_ok` 为 3/4，仍不能解决引用定位失败；没有人工真值，不下 precision/recall 结论。
- 60 秒调用上限仍频繁触发，尤其 round-2 14B 为 4 次；本轮按红线没有改 timeout、分块或模型方案。
- round-2 7B 触发 1 次 alias 调用；alias 指令未改，未将其与抽取 prompt 效果混为一谈。
- 没有修改 `eval/annotations/`、`tasks.md`，没有 git commit/push。

---

# Review（Claude / Opus 5，2026-09-07）：通过，但**基线的解读要更正**

## 四条硬检查，全过

| 检查 | 结果 |
|---|---|
| 验收集隔离（ch03～ch09 一个都不许碰） | ✅ raw 里只有 ch01(25) / ch02(28) |
| 有没有放宽校验 | ✅ 生产代码只动了 `extract_prompt.go` 的**指令文本**；`parseExtractionResponse` / `resolveExtraction` / 封闭 8 类一行没改 |
| `extractPromptVersion` 有没有 bump | ✅ `extract/v1` → `extract/v3` |
| 每轮 raw 条数 == summary calls | ✅ round-1 26/26；round-2 27/27（第 27 条是 7B 触发的归一阶段调用，合法） |

`go vet` / 门禁 SKIP / `go test ./... -race` / `make check-deps` 我都重跑过，全绿。

## 结果对照

| | 14b 基线 → r1 → r2 | 7b 基线 → r1 → r2 |
|---|---|---|
| JSON 合法 | 9 → 10 → 7 | **0 → 8 → 7** |
| Markdown 围栏 | 0 → 0 → 0 | **11 → 0 → 1** |
| 引用可定位(resolve_ok) | 3 → 3 → **4** | 0 → 3 → 3 |
| 调用失败(60s) | 3 → 2 → 4 | 2 → 1 → **0** |
| p50 延迟 | 28.6s → 27.2s → 31.1s | 12.1s → **5.1s** → 5.4s |

**围栏问题基本解决了**（7B 11 → 0/1），指令里加"回复的第一个字符必须是 {"这类硬约束是有效的。
7B 从"完全不可用"变成"多数响应合法"。

## ⚠️ 基线解读的更正（这条比上面的表更重要）

T041 第一版 summary 把 14B 的 6 次定位失败归成「引用找不到」，我在复核结论里照此写了
"14B 编造引用"。**这个说法是错的**，修正轮把它们改判为「occurrence 越界」才是对的。
我用真的 `resolveExtraction` 逐条问过：

```
"趙太爺" occurrence 5 (found 5)   ← 差一：存在 5 次，0 起的合法最大值是 4
"阿Q"    occurrence 2 (found 2)   ← 差一
"我們"   occurrence 2 (found 1)   ← 存在 1 次，报了 2
"狀元"   occurrence 3 (found 1)
```

引文**全部真实存在于原文**。模型不会编造引用，它是**数不清"第几次出现"**，
而且有明显的 off-by-one 模式。

## 由此引出的设计问题（留给后续判断，本轮按红线没动）

我们的 occurrence 口径（0 起、重叠也算一次）对模型来说本身就很难正确执行，
而它承担的作用其实很有限：**对"证据"这个用途，同一句话出现在哪一次并不重要**——
任一处出现都同样支持这条关系。

可选方向（都要改契约，不是 prompt 能解决的，需要显式决策）：
- 让模型只给 quote，服务端自己定位；quote 在块内多次出现时取第一处并如实标注；
- 或保留 occurrence 但当它越界时降级到"取第一处"，同时在证据上打标记。

⚠️ 两者都会让"引用指向原文的确切位置"这个保证变弱一档，必须显式取舍，不能顺手改。
在做出决定之前，**T042 跑全书会持续损失约 1/3 的合法响应**（r2：14B 7 条合法里 3 条卡在越界）。
