# Phase 1 数据模型：PDF 版面感知解析与跨页分块

**Feature**: `006-pdf-layout-chunking` | **Date**: 2026-09-04

本文定义本功能引入与修改的全部实体、字段、不变量与校验点。
技术路线不在此重新论证，见 [research.md](./research.md)（R1~R7）。

---

## 1. 实体总览

| 实体 | 位置 | 状态 | 一句话职责 |
|---|---|---|---|
| `pdfLine` | `internal/knowledge/parse.go` | **新增** | 抽取阶段的最小单位：一行文本 + 判定所需的排版特征 |
| `paragraphUnit` | `internal/knowledge/layout.go` | **新增** | 跨页重建后的段落流单元：内容 + 它覆盖的页码区间 |
| `chunkPiece` | `internal/knowledge/chunk.go` | **扩展** | 分块产物，新增页码区间的结束端 |
| `Chunk` | `internal/knowledge/model.go` | **扩展** | 领域实体，新增 `PageEnd *int` |
| `chunks` 表 | `internal/db/pgmigrations/000005` | **扩展** | 新增列 `page_end integer NULL` |
| `RetrieveFilter` | `internal/knowledge/model.go` | **语义变更** | 字段不变，匹配规则从"点落区间"改为"区间相交" |
| 版面噪音审计记录 | `internal/knowledge/service.go` 的 slog | **新增** | 结构化日志字段，**不落库** |

⭐ **数据流方向**（每一步都是纯函数，除首尾两端）：

```
PDF 文件
  → extractPDFPages   → []pdfLine          （唯一接触 rsc.io/pdf 的地方）
  → stripLayoutNoise  → []pdfLine          （纯函数，剥离页眉页脚/纯页码行）
  → buildParagraphStream → []paragraphUnit （纯函数，跨页合并）
  → chunkPDFStream    → []chunkPiece       （纯函数，分块 + 区间归属）
  → Chunk             → chunks 表          （repository，唯一接触 DB 的地方）
```

中间三步全部是宪法第 V 条要求的"不依赖数据库、不依赖 PDF 库的纯函数"，
输入输出都是值类型切片，可以直接用手写字面量做单测。

---

## 2. `pdfLine`（新增，抽取阶段）

`extractPDFPages` 现在把每页拍平成一个 string 就返回了——`rsc.io/pdf` 的 `Text` 片段带着
`FontSize` 和 X/Y/W，**其中 `FontSize` 完全没被使用**。本功能把"拍平"从抽取的**输出**
降级成下游的**一步**，抽取改为产出行级结构。

| 字段 | 类型 | 含义 | 来源 |
|---|---|---|---|
| `Text` | `string` | 该行重建后的文本 | 沿用既有的 X 间隙补空格逻辑（`rsc.io/pdf` 逐字形吐片段且**丢弃空格**，词边界只能靠 `X - lastRight` 重建） |
| `Page` | `int` | 1-indexed 页码 | `doc.Page(pageNum)` 的位置，与现状 `pdfPage.Number` 完全同源 |
| `FontSize` | `float64` | 该行的字号 | 该行各字形 `Text.FontSize` 的**众数**；取不到时为 `0` |
| `Y` | `float64` | 该行的 Y 坐标 | 该行首字形的 `Text.Y`（PDF 坐标系自下而上，**Y 大 = 靠近页顶**） |
| `Width` | `float64` | 渲染宽度 | `max(X+W) - min(X)`，即最右字形右边缘减最左字形左边缘 |

**不变量**：

| # | 不变量 | 谁保证 |
|---|---|---|
| L1 | `strings.TrimSpace(Text) != ""` —— 空行不产出 `pdfLine` | `extractPDFPages` |
| L2 | `1 <= Page <= doc.NumPage()` | `extractPDFPages`（页码是唯一"不用猜"的信号，与现状一致） |
| L3 | `Width >= 0` | 构造时由 max/min 保证 |
| L4 | `FontSize >= 0`；**`0` 表示"未知"，不是"字号很小"** | 见下方判定规则 |
| L5 | 同页内 `pdfLine` 按 **Y 降序 → X 升序**稳定排列 | `sort.SliceStable`（沿用现状排序，只是从字形级上移到行级） |

⚠️ **`FontSize == 0` 的处理必须是"信号不成立"，不是"按 0 参与比较"**：
R5 的标题判定要求"字号显著大于正文众数"，如果把未知当成 0，任何未知字号的行都会被判成**非**标题
（安全方向，符合 FR-016「识别不出就留空」）；但如果反过来把它当成"小于任何阈值"从而参与
R3 的行宽/字号判据取反，就可能误伤。**统一口径：`FontSize == 0` 时，一切依赖字号的判据一律返回
"不成立"，绝不猜测。**

⚠️ **换行阈值仍是启发式**：现状用 `lastY - t.Y > 2` 判定换行，这个魔数在字号远大于 12 或行距很密的
文档上都会出错。本功能**不修复它**（超出范围，属于 R1 已知能力上限的一部分），
但由于现在要靠"行"做位置与重复率判定，**这个阈值的误判会直接传导到噪音判定上**——
这一点必须写进阶段报告的诚实边界，不能装作行切分是可靠的。

---

## 3. `paragraphUnit`（新增，跨页重建后）

段落流是分块的**真正输入**。它取代了现状里"每页一个 string"的中间形态。

| 字段 | 类型 | 含义 |
|---|---|---|
| `Content` | `string` | 该段落的完整文本（可能来自多页拼接） |
| `PageStart` | `int` | 该段落**首次出现**的页码 |
| `PageEnd` | `int` | 该段落**最后延续到**的页码 |

**不变量**：

| # | 不变量 | 说明 |
|---|---|---|
| P1 | `strings.TrimSpace(Content) != ""` | 空单元不进流 |
| P2 | `1 <= PageStart <= PageEnd <= 文档总页数` | 跨页合并只会把区间向后延伸，不可能倒退 |
| P3 | 未跨页时 `PageStart == PageEnd` | 这是绝大多数单元的形态 |
| P4 | 序列按 `PageStart` **非递减**，同页内保持原始阅读顺序 | 确定性（FR-021）；由输入 `[]pdfLine` 已排序保证 |
| P5 | 合并只发生在**相邻页的边界**（上页最后一个单元 + 下页第一个单元） | R3 的三条判据只在页边界处求值，页内段落切分沿用既有 `splitParagraphs` |

**合并判据**（R3，三条**同时**成立才合并，纯函数 `shouldMergeAcrossPage`）：

1. 上页末行**不以句末标点结尾**（`。．！？；：.!?;:` 及全半角变体）
2. 上页末行**不是列表项或标题**（不匹配 `^\s*\d+[.、)]`、`^\s*[-*·]`、`^第.{1,3}[章节条]`，
   且 `FontSize` 未显著大于正文众数）
3. 上页末行**接近满行宽**：`Width >= 该页正文行宽众数 × 阈值比例`

⭐ **行宽众数的确定性**：众数需要对浮点 `Width` 分桶统计，而 Go 的 map 迭代顺序是随机的。
**必须**：(a) 分桶粒度固定为包内常量（例如按 1pt 取整为桶键）；
(b) 统计后把桶键**排序后**再遍历取最大计数；(c) 并列时取**桶键较大者**（更宽的那一档更可能是正文行宽）。
⚠️ 直接 `range` 一个 map 取最大值会让同一份 PDF 两次处理产出不同结果，**直接违反 SC-004**，
而且这种 bug 在单测里可能几十次才复现一次。这是本功能最危险的确定性坑。

**取舍方向**：**倾向合并**。代价不对称——错误合并最多让一个片段包含两个主题（下游还有准入与重排兜底），
错误切断则直接导致内容检索不到，**且无法在下游任何环节补救**（沿用 spec 的 Assumptions）。

---

## 4. `chunkPiece`（扩展，`internal/knowledge/chunk.go`）

现状：

```go
type chunkPiece struct {
    Content      string
    PageNumber   *int
    SectionTitle *string
}
```

**新增一个字段 `PageEnd *int`**，`PageNumber` 语义收紧为**起始页**。

| 字段 | 变化 | 语义 |
|---|---|---|
| `Content` | 不变 | 片段正文 |
| `PageNumber` | **语义收紧** | 片段覆盖的**起始页**（原语义是"所在页"，因为过去不可能跨页） |
| `PageEnd` | **新增** | 片段覆盖的**结束页** |
| `SectionTitle` | 不变（US4 可能开始写入 PDF 分支） | 最近的上级标题；识别不出保持 `nil`（FR-016） |

**不变量**（与下面每一层完全一致，这是本功能的核心不变量，出现四次不是重复而是四道闸）：

| # | 不变量 |
|---|---|
| C1 | `PageEnd == nil` ⟺ `PageNumber == nil` |
| C2 | 两者都非 nil 时 `*PageNumber <= *PageEnd` |
| C3 | 两者都非 nil 时 `1 <= *PageNumber` 且 `*PageEnd <= 文档总页数` |
| C4 | txt / md 分支产出的 `chunkPiece`，两者**恒为 nil**（FR-014 / FR-020） |

C3 的上界（`<= 文档总页数`）**数据库检查不到**（DB 不知道文档有几页），必须由 `layout.go` 的
纯函数在产出时断言，并被单测锁定。C1/C2 可以也应该由数据库强制，见 §6。

### ⭐ 4.1 overlap 文本的页码归属（research.md 未覆盖，本文件新增决策）

改动前 overlap 不跨页（`chunkPlainText` 的 `pendingOverlap` 每页重置），所以这个问题不存在。
改动后必须定：**一个片段开头携带的 overlap 种子来自上一个片段的尾部，如果那段文字来自第 3 页，
而本片段的正文从第 4 页开始，本片段的 `PageNumber` 是 3 还是 4？**

**决策：计入。`PageNumber` 取"包括 overlap 种子在内、片段中实际出现的最小页码"。**

**理由**：FR-010 的字面要求是"每个片段 MUST 记录其**实际覆盖**的页码范围"。overlap 种子确实在片段
文本里，它确实覆盖了那一页。引用诚实的定义是"不编造"，把区间做**宽**不是编造；把区间做**窄**反而
会让用户拿着"第 4 页"去找一句实际印在第 3 页的话——那才是不诚实。

**被否决的反方案**：只按正文算，把 overlap 视为"上文种子而非本片段的主张内容"。
否决理由：它需要读者理解"片段里有一段文字不属于这个片段"这个额外概念，而收益只是区间稍窄一点。
⚠️ 代价要如实记：**采用本决策后，跨 chunk 的 overlap 会让页码区间比"正文实际所在页"略宽**，
在 overlap 恰好跨页时表现为一个本可以标"第 4 页"的片段标成了"第 3-4 页"。这是有意的偏宽，
不是 bug，须写进阶段报告。

### 4.2 `prependOverlap` 的不变量不受影响

`chunk.go:95` 的 `prependOverlap` 保证「任何非空输出 ≤ chunk_size」，规则是**正文优先、overlap 自己缩**。
本功能**不改动这个函数**，只是它现在可能在跨页边界上被调用。FR-004（合并后仍服从长度上限）
因此是免费得到的——单个合并段落超限时照旧回退 `chunkText` 定长切分（按 rune 不按 byte）。

---

## 5. `Chunk`（扩展，`internal/knowledge/model.go`）

现状 `model.go:287` 已有 `PageNumber *int`。**新增 `PageEnd *int`**，紧邻它。

不变量与 §4 的 C1/C2/C3 逐条相同。桥接由既有 helper 完成，**不需要新写转换函数**：

| 方向 | helper | 位置 |
|---|---|---|
| `*int` → `sql.NullInt32` | `intPtrToNullInt32` | `repository.go:389` 一带（`createChunks`） |
| `sql.NullInt32` → `*int` | `nullInt32ToIntPtr` | `repository.go` 的四处行映射（501 / 552 / 604 / 686） |

⚠️ **四处行映射一个都不能漏**：向量召回、关键词召回、邻接查询、按文档列分片各一处。
漏掉任何一处的后果是那条路径返回的 chunk 的 `PageEnd` 恒为 nil——**违反 C1**，
且如果 §6 的 CHECK 约束没加，这个错误会一路静默传到前端显示成「—」。

### 5.1 邻接块（`neighbor.go`）

`neighbor.go` 的文档注释第 4 条已经写明：邻接块的
`Content/DocumentName/PageNumber/SectionTitle/etc.` **全部是它自己的真实字段**，
只有 `Score` 被覆写（继承 anchor）、`NeighborOf` 被设置。

因此 `PageEnd` 会**自动**沿着同一条路径继承下来，预期**零代码改动**。
但"预期零改动"不是证据——必须有一条断言锁定：邻接块的 `PageEnd` 是它**自己**的值，
而不是 anchor 的、也不是 nil。

---

## 6. `chunks` 表（PostgreSQL，pgmigration `000005`）

### 6.1 up

```sql
ALTER TABLE chunks ADD COLUMN page_end integer NULL;
UPDATE chunks SET page_end = page_number;
ALTER TABLE chunks ADD CONSTRAINT chunks_page_range_valid
  CHECK ((page_number IS NULL) = (page_end IS NULL)
         AND (page_end IS NULL OR (page_number >= 1 AND page_number <= page_end)));
```

**三步的顺序不可调换**：必须先加列、再回填、最后加约束——约束在回填之前加会立刻拒绝所有
`page_number` 有值的存量行。

**`UPDATE` 覆盖 NULL 行**：`page_number IS NULL` 的行（全部 txt/md chunk，以及 `000003` 迁移之前
写入的存量行）会被写成 `page_end = NULL`，正好满足不变量 C1，**不需要 `WHERE page_number IS NOT NULL`**。

**CHECK 的真值验证**（PostgreSQL 的 CHECK 只在结果为 `FALSE` 时拒绝，`NULL` 视为通过，
所以必须逐种情况验一遍）：

| `page_number` | `page_end` | `(num IS NULL)=(end IS NULL)` | `end IS NULL OR (num>=1 AND num<=end)` | AND | 结果 |
|---|---|---|---|---|---|
| NULL | NULL | `TRUE=TRUE` → TRUE | `TRUE` | TRUE | ✅ 通过 |
| 3 | 4 | `FALSE=FALSE` → TRUE | `FALSE OR TRUE` → TRUE | TRUE | ✅ 通过 |
| 3 | 3 | TRUE | TRUE | TRUE | ✅ 通过 |
| 4 | 3 | TRUE | `FALSE OR FALSE` → FALSE | FALSE | ❌ 拒绝 |
| 0 | 0 | TRUE | `FALSE OR FALSE` → FALSE | FALSE | ❌ 拒绝 |
| 3 | NULL | `FALSE=TRUE` → FALSE | `TRUE` | FALSE | ❌ 拒绝 |
| NULL | 4 | `TRUE=FALSE` → FALSE | `FALSE OR NULL` → **NULL** | `FALSE AND NULL` → **FALSE** | ❌ 拒绝 |

最后一行是唯一需要小心的：`FALSE AND NULL` 在 SQL 三值逻辑里等于 `FALSE`（不是 `NULL`），
所以这一格确实会被拒绝。⭐ **两个条件必须写在同一个 CHECK 里用 `AND` 连接**——
拆成两个独立约束时 `NULL / 4` 这一格会从第二个约束里逃逸（`NULL` 视为通过）。

**`page_number >= 1` 是否安全**：已核查——`parse.go` 只产出 `1..NumPage`，
现有测试的页码字面量全部 `>= 1`（`admission_test.go:66`=3、`hybrid_test.go:403`=7、
`integration_test.go:2271`=12 等），`filter_test.go` 里的 0 与 -1 是**过滤入参**校验用例，
不落库。因此加这个下界不会打到任何存量或测试数据。

**不建索引**。沿用 `000003` 给 `page_number` 的处理：页码谓词是 GIN / 向量索引已经筛出候选之后的
**残余谓词**，只在至多 `candidate_k` 数量级的行上求值（002 的 data-model 已论证）。
`page_end` 的用法完全对称，同理不需要索引。

### 6.2 down

```sql
ALTER TABLE chunks DROP CONSTRAINT IF EXISTS chunks_page_range_valid;
ALTER TABLE chunks DROP COLUMN page_end;
```

⚠️ **down 是不可逆的信息损失**：回滚后跨页片段只剩起始页，"这个片段还覆盖第 4 页"这件事没了。
`page_number` 本身不受影响（仍是有效的起始页），所以回滚不会让检索坏掉，只会让跨页信息退回改动前的
精度。这一点必须写在 down 迁移的注释里。

### 6.3 加不加 CHECK：对比

| | 加 CHECK | 不加，只在 Go 侧断言 |
|---|---|---|
| C1/C2 被破坏时 | 数据库**响亮拒绝** INSERT | 静默写入，新过滤 SQL **静默漏召回**（`page_number` 有值而 `page_end` 为 NULL 的行，任何 `PageMin` 过滤都不匹配它） |
| 连带改动 | 约 10 处测试数据构造点要补 `PageEnd` | 无 |
| 防的是什么 | 将来某次改动在某条路径上忘了写 `page_end` | —— |

⭐ **推荐加**。R2 的整个等价性论证（§7）建立在 C1 之上；一个只靠约定维持的不变量，
在四处行映射 + 一处写入 + 未来任意改动面前是撑不住的。
连带改动虽然有约 10 处，但都是机械的补字段，且**只补字段、不改这些测试的其他部分**（宪法第 IX 条）。
最终取舍由所有者在 `tasks.md` 前拍板（见 plan.md「进入 tasks 前待拍板」第 2 条）。

---

## 7. `RetrieveFilter` 的语义变更（⭐ 本功能最关键的设计点）

**字段一个都不改。** `model.go:150` 的 `PageMin *int` / `PageMax *int` 保持闭区间、保持 1-indexed、
保持 `nil` = 不设限。**变的是"什么叫匹配"。**

| | 现状（点落区间） | 改后（区间相交） |
|---|---|---|
| 含义 | chunk 的**那一页**落在 `[min, max]` 内 | chunk 的**页码区间** `[page_number, page_end]` 与 `[min, max]` **有交集** |
| SQL 下界 | `page_number >= min` | **`page_end >= min`** |
| SQL 上界 | `page_number <= max` | `page_number <= max` |

改后的完整谓词（`internal/db/pgqueries/chunks.sql`，`SearchVectorChunks` 94-95 行与
`SearchKeywordChunks` 143-144 行**两处都要改，一处不改就等于关键词路把范围外的片段重新带回候选池**）：

```sql
  AND (sqlc.narg(filter_page_min)::int IS NULL OR page_end     >= sqlc.narg(filter_page_min)::int)
  AND (sqlc.narg(filter_page_max)::int IS NULL OR page_number  <= sqlc.narg(filter_page_max)::int)
```

区间相交的标准形式是 `A.start <= B.end AND A.end >= B.start`，这里 A = chunk 的区间、
B = 过滤区间，展开就是上面两行——**下界谓词作用在 `page_end` 上、上界谓词作用在 `page_number` 上**，
两行是交叉的，写反了会得到一个恒不成立的条件。这是最容易写错的一处。

### ⭐ 7.1 存量数据零影响的论证（必须逐条走完）

**命题**：回填之后（`page_end = page_number`），对所有存量行与所有单页片段，
新旧两套谓词**逐字节等价**。

**情况 A：`page_end = page_number = p`**（回填后的全部存量行 + 改动后产出的全部单页片段）

| | 下界 | 上界 |
|---|---|---|
| 旧 | `p >= min` | `p <= max` |
| 新 | `page_end >= min`，而 `page_end = p` → `p >= min` | `page_number <= max`，而 `page_number = p` → `p <= max` |

两个谓词逐项相同 → **命中集合完全相同，顺序也完全相同**（`ORDER BY ... id ASC` 未改）。

**情况 B：`page_number IS NULL`**（全部 txt/md chunk + `000003` 之前写入的存量行）

由不变量 C1，`page_end` 同时为 NULL。设 `min` 有值：

| | 表达式 | 三值逻辑 | WHERE 结果 |
|---|---|---|---|
| 旧 | `FALSE OR (NULL >= min)` | `FALSE OR NULL` = **NULL** | 排除 |
| 新 | `FALSE OR (NULL >= min)` | `FALSE OR NULL` = **NULL** | 排除 |

上界侧同理。→ **无页码的 chunk 在新旧规则下同样不匹配**，`page_number` / `page_end` 换哪一列都一样，
因为它们**同为 NULL**。⭐ 这一步正是不变量 C1 在承重：如果哪天出现一行 `page_number` 有值而
`page_end` 为 NULL，下界谓词会把它排除掉而上界谓词会放它过，结果是**任何设了 `PageMin` 的检索都
静默丢掉这一行**——静默漏召回，没有报错、没有日志。这就是 §6.3 推荐加 CHECK 的全部理由。

**情况 C：真正跨页的新片段**（`page_number=3, page_end=4`）——**唯一会表现出新行为的情况**

| 过滤条件 | 旧规则（按起始页） | 新规则（区间相交） | 变化 |
|---|---|---|---|
| `min=3, max=3` | 命中 | 命中 | 无 |
| `min=4, max=4` | **不命中** | **命中** | ⭐ 变了，且这正是想要的——片段确实包含第 4 页的内容 |
| `min=4, max=9` | **不命中** | **命中** | ⭐ 变了 |
| `min=1, max=2` | 不命中 | 不命中 | 无 |
| `min=5, max=9` | 不命中 | 不命中 | 无 |
| `min=1, max=10` | 命中 | 命中 | 无 |
| 只给 `min=4` | **不命中** | **命中** | ⭐ 变了 |
| 只给 `max=3` | 命中 | 命中 | 无 |

**结论**：新行为**只在跨页片段上出现，且只表现为"该命中的现在命中了"**——
不存在"原本命中的现在不命中"的格子。这是 FR-022（存量文档保持可检索、页码信息保持有效）
与 SC-006 的直接根据。

### 7.2 禁令（沿用 `000003_chunk_source_metadata.up.sql` 的既有禁令）

⚠️ **禁止 `COALESCE(page_number, 0)` 或 `COALESCE(page_end, 0)` 一类给 NULL 页码兜底的写法。**
那等于给一个本来没有页码的 chunk 编造出"第 0 页"，与"绝不伪造"的既有约定正面冲突。
`page_end` 完全适用这条禁令，一个字都不放宽。
现有的 `TestFilterPageRangeExcludesNullPageChunks` 锁定了 `page_number` 侧，
本功能必须给 `page_end` 侧补上对称的用例——**并且要单侧各试一次**（只给下界 / 只给上界 / 闭区间），
002 的经验是只测闭区间时单侧的错误会被另一侧未改动的谓词掩盖掉。

### 7.3 邻接查询不受影响

`FindPublishedNeighborChunksBatch` **故意没有**页码谓词（002 的 FR-011：邻接块是上下文补全而非检索命中，
豁免 chunk 级页码过滤）。本功能**不动它**，但它 `SELECT` 的列要加上 `page_end`——
否则邻接块的 `PageEnd` 恒为 nil（见 §5.1）。

---

## 8. 版面噪音审计记录（结构化日志，**不落库**）

FR-008 要求"记录被剥离的内容，供事后核查"。落点是 `service.go` 的 slog，
**不新建表、不新增列**——这是审计线索而不是业务数据，Redis / MySQL / PG 都不该存它。

| 字段 | 类型 | 含义 |
|---|---|---|
| `document_id` | string | 哪份文档 |
| `page` | int | 被剥离行所在页（1-indexed） |
| `reason` | string | 枚举：`repeated_header` / `repeated_footer` / `page_number_line` |
| `line_length` | int | 该行长度（rune 数） |
| `repeat_page_ratio` | float64 | 该行归一化文本出现的页数占比（`reason` 为 `repeated_*` 时才有意义） |
| `text` | string | **归一化后**的被剥离文本，截断到上限 |
| `stripped_total` | int | 该文档本次共剥离多少行（汇总行，每份文档一条） |

**日志语言**：英文（宪法第 VII 条——日志读者是开发者；且落点 `service.go` 与既有的
`knowledge: retrieval candidate admission and dedup` 一类日志同源）。

### ⚠️ 8.1 记录 `text` 与 002 脱敏口径的张力（有意的偏离，必须明示）

002-metadata-filter 确立的口径是**日志只记种类与数量，绝不记 document_id 取值、页码数值、
query 原文、片段正文**。本节的 `text` 与 `page` 字段与它方向相反。

**为什么仍然记**：

1. **FR-008 的目的是核查误删**，而 SC-005 要求正文误删率为 **0**。要验证"没删错东西"，
   就必须能看到删掉的**是什么**。只记一个计数无法履行这条要求——它会退化成一个永远无人能核对的数字。
2. **被记录的内容按定义是版面噪音**：页眉页脚是每页重复的章节名、公司名、页码，
   不是用户正文。判据的第 2 条（跨页重复率 ≥ 阈值）本身就限定了这一点。
3. **宪法第 VII 条的日志禁令针对的是凭据类敏感信息**（密码 / token / API Key），
   本场景不涉及任何此类数据。

**缓解措施（三条都必须落地）**：

- 只记**归一化后**的文本（数字已被抹掉，正是重复率判定用的那个形态）
- 截断到包内常量上限（页眉页脚本来就短，判据第 3 条要求"显著短于正文行宽"）
- 每份文档的逐行记录条数设上限，超出只累加 `stripped_total`（防止一份千页文档刷爆日志）

此偏离已在 plan.md 的「已知范围边界」第 2 条登记。**它不是宪法违规**（禁的是凭据类信息），
但它是一条与既有口径相反的选择，必须被看见，不能悄悄做。

---

## 9. 阈值常量（全部包内，**不做运行时可配置**）

沿用 R7 的统一约定，以及 `admission.go` / `toolloop.go` 的既有风格：

| 常量（示意名） | 用途 | 判据 |
|---|---|---|
| 顶部/底部 Y 带比例 | 噪音的位置判据 | R4 第 1 条 |
| 跨页重复率阈值 | 噪音的重复判据 | R4 第 2 条 |
| 噪音行长度比例 | 噪音的长度判据 | R4 第 3 条 |
| 最小页数门槛 | 低于此页数跳过重复率判定 | R4 / FR-009 |
| 满行宽比例阈值 | 跨页合并的第 3 条判据 | R3 第 3 条 |
| 标题字号倍数 | 标题的字号信号 | R5 第 1 条 |
| 行宽众数分桶粒度 | 众数统计的确定性 | §3 的 ⭐ |
| 噪音日志文本截断长度 / 每文档条数上限 | §8 的缓解措施 | FR-008 |

**每个常量的注释必须写明取值理由和它在防什么**（R7），不能只写一个数字。
**不暴露成配置**——它们是启发式的实现细节，做成开关只会把调参责任推给不可能知道怎么调的用户。

---

## 10. 不变量总表（回归断言的目标）

| # | 不变量 | 层 | 谁保证 / 断言在哪 |
|---|---|---|---|
| L1-L5 | `pdfLine` 的非空、页码范围、排序稳定 | 抽取 | `structure_test.go`（真实 PDF 字节） |
| P1-P5 | `paragraphUnit` 的区间单调、合并只在页边界 | 段落流 | `layout_test.go`（纯函数，手写字面量） |
| C1 | `PageEnd == nil` ⟺ `PageNumber == nil` | piece / Chunk / 表 | `layout_test.go` + `chunk_test.go` + `chunks_page_range_valid` |
| C2 | `*PageNumber <= *PageEnd` | 同上 | 同上 |
| C3 | 两端都在 `[1, 文档总页数]` 内 | piece / Chunk | 纯函数断言 + 集成测试（**DB 检查不到上界**） |
| C4 | txt / md 的两个字段恒为 nil | piece | `chunk_test.go`（FR-014 / FR-020） |
| O1 | `prependOverlap` 的「非空输出 ≤ chunk_size」 | 分块 | 既有 `chunk_test.go` 全绿即可，本功能不改这个函数 |
| F1 | 单页片段与存量行在新旧过滤下命中集合相同 | 检索 | `make eval-retrieval-gate` 逐字节一致 + `filter_test.go` |
| F2 | NULL 页码的 chunk 在设了任一端时不匹配 | 检索 | `TestFilterPageRangeExcludesNullPageChunks` 扩展（闭区间 / 只给下界 / 只给上界三个子用例） |
| F3 | 跨页片段被"命中它区间内任一页"的过滤召回 | 检索 | 新增集成用例（§7.1 情况 C 的表逐行） |
| N1 | 邻接块的 `PageEnd` 是它自己的值，不是 anchor 的、也不是 nil | 邻接 | 新增断言（§5.1） |
| D1 | 同一份 PDF 连续处理两次，片段序列与页码归属逐字节一致 | 全链路 | 新增集成用例（SC-004；⭐ 专门盯 §3 的 map 迭代顺序坑） |
