# Hify RAG 第二阶段：Chunk 长度不变量修复报告

范围：只修复审核指出的 `internal/knowledge/chunk.go` 长度计算 bug 及配套测试。没有触碰 Hybrid
Search、没有回退阶段一的 eval 改动、没有碰 `_to_delete/`、没有执行任何 git 操作。

## 1. 根因

`chunkMarkdown`/`chunkPlainText`/`chunkBySentence` 三个函数共享同一套"累加器 + `flush`"结构：正常情况
下，往累加器 `acc` 里加下一段/句/block 之前，都会检查 `accRuneLen()+分隔符+新内容长度 > 预算` 来决定
要不要先 `flush`。但 `flush(true)` 把上一块的尾部存进 `pendingOverlap` 之后，下一次真正把
`pendingOverlap` 拼到新内容前面（`acc` 刚清空、`len(acc)==0` 那一次）是三处几乎相同的代码：

```go
next := block.Text // 或 para / sentence
if len(acc) == 0 && pendingOverlap != "" {
    next = pendingOverlap + "\n" + block.Text // 或 " " + sentence
    pendingOverlap = ""
}
acc = append(acc, next)
```

这一步**没有做任何预算检查**——`pendingOverlap + 分隔符 + 新内容` 直接拼好塞进 `acc`，之前算好的
"新内容自己 <= 预算"这个事实，并不能保证"overlap + 分隔符 + 新内容"也 <= 预算。审核给的例子
（`chunk_size=10, overlap=4`，两段各 8 字）里，第二段自己 8 <= 10 通过检查，但拼上 4 字 overlap + 1
字换行后变成 13，超限却没有任何代码路径会发现——这个越界的 `next` 会一路带着，直到这个 chunk 最终
`flush` 时才变成一个真实产出的、长度超限的 chunk。

Markdown 另外两个问题都是同一类"预算计算和最终拼接不同步"：

- `bodyBudget()` 为了不让深层标题把正文挤没，有个 `size/2` 地板（`if b < size/2 { b = size/2 }`）。
  这个地板只保证了"正文不会被挤没"，没有反过来保证"breadcrumb 加上这个地板保护出来的正文，两者加起来
  还在 `size` 以内"——`emit()` 之前是无条件 `bc + "\n\n" + body`，breadcrumb 越长这个缺口越明显。
- oversized code/table 的 fallback 分支 `chunkText(block.Text, size, overlap)` 传的是**完整的**
  `size`，切出来的每一段本身就可能顶到 `size`，但紧接着 `emit(sub)` 还会再在前面加一层 breadcrumb——
  两者根本没有互相扣除过。

## 2. 具体修复策略

### 2.1 统一的 overlap 预算检查：`prependOverlap`

新增一个共享辅助函数，三处拼接全部改用它：

```go
func prependOverlap(overlapTail, sep, content string, budget int) string {
    if overlapTail == "" {
        return content
    }
    available := budget - len([]rune(content)) - len([]rune(sep))
    if available <= 0 {
        return content
    }
    return tailRunes(overlapTail, available) + sep + content
}
```

优先级完全按审核要求来：content 永远不动（调用点在拿到这里之前都已经确认了 content 自己 <= 预算，
`prependOverlap` 内部不会、也不需要再截它）；`overlapTail` 能放多少放多少（`available > 0` 时按
`tailRunes` 截到刚好塞得下——保留的是 overlapTail 自己的尾部，因为 `pendingOverlap` 本来就是"上一块的
尾部"，越靠近拼接点的字越有上下文价值）；`available <= 0` 时直接整段丢弃 overlap，只留 content。三处
调用分别传各自的预算（Markdown 传 `budget`——即 `bodyBudget()` 算出来的、breadcrumb 感知的正文预算；
TXT 段落/句子传 `size`，因为它们没有 breadcrumb）。

### 2.2 Markdown breadcrumb 预算：`buildBreadcrumbContent`

新增一个函数专门负责"body 已经定了、breadcrumb 该怎么安全地拼上去"，替换掉 `emit()` 原来的无条件拼接：

```go
func buildBreadcrumbContent(headingStack []string, body string, size int) string {
    // available = size - len(body) - 2（"\n\n" 分隔符）
    // 依次尝试：完整 breadcrumb → 从最外层开始丢标题层级，保留最内层 → 单独最内层标题本身
    //           rune 截断到刚好塞得下 → 一点标题都放不下时不带 breadcrumb
}
```

这是最终兜底，不依赖 `bodyBudget()` 的地板算得准不准——不管正文是被地板保护出来的、还是正常预算算出来
的，这里都会重新按"body 已经定长，breadcrumb 还剩多少空间"来算，所以哪怕 `bodyBudget()` 的 `size/2`
地板让正文占的空间比"扣完 breadcrumb 该留的空间"还多，最终拼接也不会超限——只是 breadcrumb 会被相应
缩短甚至丢弃，符合"可以安全缩短或降级，但不能让最终 Content 超限"的要求。

### 2.3 oversized code/table fallback：先扣 breadcrumb 再切正文

```go
// 改之前：chunkText(block.Text, size, overlap)，切完再在 emit 里加 breadcrumb，两者不互相知道
// 改之后：
for _, sub := range chunkText(block.Text, budget, overlap) {
    emit(sub)
}
```

`budget` 就是同一个 `bodyBudget()` 算出来的、已经扣过 breadcrumb 的预算，所以 `chunkText` 切出来的每一
段本身就已经给 breadcrumb 留了余量；`emit()` 再套上 2.2 的 `buildBreadcrumbContent` 双重兜底，即使
`budget` 因为 `size/2` 地板而略微高估了实际可用空间，最终结果依然不会超过 `size`。

### 2.4 heading-only Markdown 不再被当成空文档

`chunkMarkdown` 的主循环只有两个地方会真正产出 `chunkPiece`：`flush` 里 `acc` 非空时的 `emit`，和
oversized-block fallback 里的 `emit`。一份纯标题、没有任何正文 block 的文档（比如"# 安装说明\n##
Linux"）两条路径都走不到——`acc` 从头到尾都是空的，最后 `pieces` 是 `nil`，`chunkDocument` 返回空切
片，`service.go` 里 `len(pieces)==0` 直接判 `ErrEmptyContent`，把一份"有内容（至少有标题）"的文档误判
成"空文档"。

新增一个独立于 `headingStack`（会随同级/更高级标题出现而弹栈）的 `headingTexts`，按文档顺序记录*每一
个*遇到过的标题；主循环跑完后，如果 `pieces` 还是空但 `headingTexts` 不空，就把所有标题按 `" > "` 拼
起来，过一遍 `chunkText` 切安全长度，产出至少一个可检索的兜底 chunk，`SectionTitle` 设成最后一个标题
（真实存在、不是编的）。

## 3. 新增测试

全部加在 `internal/knowledge/structure_test.go`，按审核清单逐条对应：

| 测试函数 | 对应场景 |
|---|---|
| `assertChunksWithinSize` / `assertStringsWithinSize` | 审核要求的统一断言辅助函数（分别对应 `[]chunkPiece` 和 `[]string` 两类返回值） |
| `TestChunkPlainTextOverlapNeverExceedsSizeAndKeepsBothParagraphs` | TXT 段落 overlap（size=10, overlap=4, 两段各 8 字）——所有 chunk <= size，两段正文都在 |
| `TestChunkPlainTextSentenceSplitOverlapStaysWithinSizeAndKeepsAllSentences` | TXT 句子切分——超过 size 的单段落、4 句中文句子，chunk <= size，关键句子都在，overlap 不越界 |
| `TestChunkMarkdownBlockOverlapNeverExceedsSizeAndKeepsLaterBlockBody` | Markdown 普通 block——同一 section 两个 8 字 block、size=14/overlap=4，逼到"overlap 完全放不下、必须取消"的边界情形，最终 Content（含 breadcrumb）不超 size，第二个 block 正文没丢 |
| `TestChunkMarkdownDeepOrOverlongBreadcrumbNeverPanicsOrOverflows` | 三级深标题、单级标题本身就比 size 长——不 panic、不产生空 chunk、Content 不超 size、`SectionTitle` 仍是真实的最内层标题 |
| `TestChunkMarkdownOversizedCodeBlockFallsBackToFixedLength`（收紧了原有断言） | 原来的断言允许 `size + len(breadcrumb) + 4` 的固定余量（等于承认了这个 bug），现在改成严格 `assertChunksWithinSize`，并新增代码正文没有被 fallback 静默丢失的检查 |
| `TestChunkMarkdownOversizedTableFallbackKeepsAllRowsWithinSize`（新增，替代原来只判 chunk 数的版本） | 同上，table 版本，额外检查首行/末行都还在 |
| `TestChunkMarkdownHeadingOnlyDocumentStillProducesRetrievableChunk` | "# 安装说明\n## Linux" 这种纯标题文档——至少一个 chunk，不超 size，`SectionTitle` 合理，Content 包含两级标题 |
| `TestChunkMarkdownHeadingOnlyDocumentOversizedFallsBackWithinSize` | 纯标题但标题本身拼起来超过 size 的边界情形——fallback 切分依然遵守 size |

没有新增测试改动任何一条**已有**测试的断言方向（除了上面明确写出来的两条被收紧的），也没有删除或跳过
任何已有测试。

## 4. 验证结果

最终验收环境使用可写的 Go 构建缓存执行了以下命令：

```text
env GOCACHE=/tmp/hify-go-build-cache go test ./...       # 通过
env GOCACHE=/tmp/hify-go-build-cache go test -race ./... # 通过
env GOCACHE=/tmp/hify-go-build-cache go vet ./...        # 通过
env GOCACHE=/tmp/hify-go-build-cache make check-deps     # 通过
```

## 5. 仍存在的限制

1. **`make check-deps` 的假阳性问题本身没有修**——脚本在 `go list` 失败时仍可能静默判过；本次验收通过
   指定可写的 `GOCACHE` 确认 `go list` 正常执行后再采信结果。这个脚本健壮性问题不属于本阶段范围。
2. **人工验证仍作为补充证据**：新增测试覆盖 overlap、breadcrumb、超长结构和 `chunk_size=1` 等边界；
   同时按实际分支逻辑复核了正文预算、重叠截断和 breadcrumb 降级顺序。
3. **没有触碰的部分**：Hybrid Search 完全没有开始；阶段一的 eval 相关改动（`internal/eval`、
   `eval/testset.yaml`、`eval/baseline.json`）保持在本次工作区中；`_to_delete/` 和阶段压缩包不纳入提交。
