# Phase 10：检索元数据过滤（Metadata Filtering）实施报告

**功能分支**：`002-metadata-filter` ｜ **规格**：`specs/002-metadata-filter/`
**实施日期**：2026-09-02
**开发方式**：spec-kit 流程（specify → clarify → plan → tasks → implement），AI 辅助开发

## 0. 一句话总结

给 `knowledge.Service.Retrieve` 增加一个**可选、可关闭、默认关闭**的范围限定入参，
支持按 `document_id` 集合与 PDF 页码范围缩小候选来源，且该限定**下推到向量路与关键词路的
召回 SQL**，而不是召回之后在 Go 里筛。

**这份报告最重要的两句话**：

1. **第 6 节**：本阶段的效果结论全部是**机制证明**，没有任何真实效果幅度数字。
2. **第 8 节**：本期**不提供任何设置过滤器的入口**，因此这个能力上线后**无人可用**。
   它的价值兑现依赖下一期。

## 1. 规格起草时的一个事实错误（先说这个）

spec 起草时写的是：「`parse.go` 解析 PDF 时已经持有 1-indexed 页码，但这个信号在切块时被丢弃，
chunk 落库后无法回答『第 12 页附近说了什么』」。

**进入 plan 前核查代码，发现这句话是错的。** 页码链路早已完整存在：

```
parse.go: pdfPage.Number (1-indexed)
  → chunk.go: chunkPDFPages  →  chunkPiece{PageNumber: &num}
  → model.go: Chunk.PageNumber
  → service.go: ProcessDocument  →  repository.go: createChunks
  → chunks.page_number 列（pgmigrations 000003）
  → 四条检索查询的 SELECT 列表  →  RetrievedChunk.PageNumber
  → Citation V1 已在使用
```

误判的来源是 `pgqueries/chunks.sql` 里 `CreateChunk` 的一句注释——
"page_number/section_title 当前解析器不产出可靠值，调用方一律传 NULL"。
那是 `000003` 时期的描述，Phase 4 结构感知切块落地后就已不符实，但注释没跟着改。
本期顺带更正了这条注释（只改注释文字，SQL 语义未动）。

**后果**：US2 的工作量从"把页码接上 + 让它可过滤"缩减为"让已有的页码可过滤"，
并且连带推翻了起草时"新增 `metadata jsonb` + GIN 索引"的预设（见下一节）。

> 这条记在报告最前面，是因为它是本期最大的一次范围修正，而且修正方向是**变小**。
> 一份不写明"我们原以为要做 A，核查后发现 A 已经存在"的报告，会让读者高估这次改动的分量。

## 2. 四条澄清决策（都让范围变小）

| 问题 | 起草时预设 | 核查后决定 | 理由 |
|---|---|---|---|
| 元数据存哪 | 新增 `metadata jsonb` + GIN | **只用现有 `page_number` 列** | 本期唯一的键就是页码，而它已有专用列。加 jsonb 只会制造第二个真相来源、一次存量回填、一个单键 GIN 索引 |
| 跨页 chunk 的起止页 | 必须记录 | **承认当前不可达** | `chunkPDFPages` 严格按页切块，跨页 chunk 在当前设计下不存在；FR-005 又禁止改切块边界。这是个没有输入能触发的字段 |
| 过滤维度 | 含文件名/类型/上传者（需跨库） | **只做 document_id + 页码** | 两者都已在 `chunks` 表上，**本期零跨库查询**。US1/US2 都不需要文档属性过滤 |
| `Retrieve` 接口 | — | **加 options 结构参数** | 只保留一条检索入口，不出现"有人走了没过滤的老方法"的分叉 |

**结果：本期没有任何 migration。** 过滤需要的两个列（`document_id`、`page_number`）都已存在。

## 3. 过滤插在链路的哪一步

```
Retrieve(kbIDs, query, topK, opts)
  │
  ├─ 0. 校验 opts.Filter（上限/页码范围/开关）        ← 新增，失败即返回，不做任何检索
  │
  ├─ 1. 向量路   SearchVectorChunks(... + filter)     ← 过滤在这里下推
  ├─ 1. 关键词路 SearchKeywordChunks(... + filter)    ← 过滤在这里下推
  │
  ├─ 2. rrfFuse：RRF 融合 → Phase 8 准入 → Phase 5 内容去重   ← 完全不变
  ├─ 3. Phase 9 重排                                          ← 完全不变
  ├─ 4. topK 截断                                             ← 完全不变
  └─ 5. 邻接窗口批量查询                                       ← 完全不变
```

**第 2 步及之后一行代码都没改。** 这是"过滤只缩小候选来源、不参与打分"（FR-012）在实现层面的
直接体现，也让它成为一句可验证的话而不是一句承诺。

## 4. 三个值得单独说明的实现判断

### 4.1 可选过滤如何进入 sqlc 的静态 SQL

sqlc 的 SQL 文本是编译期静态的，而 FR-016 禁止字符串拼接。采用**可空参数 + 恒真短路谓词**：

```sql
AND (sqlc.narg(filter_document_ids)::text[] IS NULL
     OR document_id = ANY(sqlc.narg(filter_document_ids)::text[]))
AND (sqlc.narg(filter_page_min)::int IS NULL OR page_number >= sqlc.narg(filter_page_min)::int)
AND (sqlc.narg(filter_page_max)::int IS NULL OR page_number <= sqlc.narg(filter_page_max)::int)
```

未指定该维度时传 NULL，`NULL IS NULL` 为 TRUE，整个 OR 短路成恒真，等价于这一行谓词不存在。
一条静态 SQL 覆盖全部组合，全部参数化绑定。

被否决的方案：按过滤组合写 4 条查询（两个维度 4 条、三个维度 8 条，且 SELECT 列表与 ORDER BY
要维护成多份逐字同步的副本）；在 Go 里拼 WHERE（直接违反 FR-016）。

### 4.2 无页码的 chunk 靠 SQL 三值逻辑天然被排除

非 PDF 文档、以及 `000003` 迁移之前的存量行，`page_number` 为 NULL。
`NULL >= 10` 求值为 NULL（不是 FALSE），`FALSE OR NULL` = NULL，而 `WHERE` 只接受 TRUE，
该行被排除——正是"无页码 MUST 视为不匹配，MUST NOT 当作无元数据即通过"的要求。

**没有写显式的 `IS NOT NULL`**（那是冗余谓词），但这个隐式依赖容易被后来者"修好"
（典型错误修法是 `COALESCE(page_number, 0)`，那等于给没有页码的 chunk 编造出第 0 页），
所以它由 `TestFilterPageRangeExcludesNullPageChunks` 锁定，SQL 注释里也明确标了 ⚠️ 禁止改写。

### 4.3 邻接块的过滤语义：需要的代码改动量是零

FR-011 要求邻接块满足文档级过滤、豁免 chunk 级过滤。核查后结论是**两条都不需要改邻接查询**：

- **文档级是结构性满足的**：邻接坐标全部由 `buildNeighborRequests` 从 anchors 自身的
  `(document_id, document_version, chunk_index±1)` 生成，而 anchors 已经在第一步的召回 SQL 里
  通过了过滤；`FindPublishedNeighborChunksBatch` 又按 `document_id` 等值 JOIN。
  邻接块**不可能**来自过滤范围外的文档。
  （这与 `chunks.sql` 里既有的版本隔离论证同构——那条注释同样说这是"WHERE 条件本身的结构性
  保证，不需要额外代码"。）
- **chunk 级豁免的实现方式就是"不加"**：需要主动做的事情是不做。

因此本条真正的风险不是"忘了实现"，而是将来有人"顺手统一一下"给邻接查询也加上过滤条件。
`TestIntegrationRetrieveFilterExemptsNeighborsFromPageFilter` 就是为拦住那次改动而存在的。

## 5. 降级与失败矩阵（全部有测试覆盖）

| 情形 | 行为 | 覆盖用例 |
|---|---|---|
| 空过滤器 + 开关关闭 | 与上线前**逐字一致** | 门禁 12 个既有 case 逐字段比对 |
| 空过滤器 + 开关开启 | 与上线前**逐字一致**（三条谓词恒真） | 同上 |
| 非空过滤器 + 开关关闭 | `ErrMetadataFilterDisabled`，**不检索** | `TestRetrieveFilterDisabledRejectsNonEmptyFilterWithoutTouchingDB` |
| 过滤器超限 / 页码非法 | 中文错误，**不截断**、**不检索** | `TestRetrieveFilterValidate*` |
| 过滤后候选不足 | 只返回范围内的，**不放宽** | `TestIntegrationRetrieveFilterNoAutoRelax` |
| document_id 不存在 / 属于别的知识库 | 空结果，**不报错** | `TestIntegrationRetrieveFilterUnknownDocument` |
| 页码过滤遇到 NULL 页码 | 不匹配；但无过滤时照常命中 | `TestFilterPageRangeExcludesNullPageChunks` |

**关于开关语义的一个刻意设计**：`HIFY_RAG_METADATA_FILTER_ENABLED=false` 时，
收到非空过滤器是**明确报错**，不是"忽略过滤器照常做无过滤检索"。
后者会造成"我限定了范围，但系统用了范围外的资料回答"——比"没找到"严重得多，因为它不可见。
所以开关关闭的是**接受过滤请求这个能力**，而不是**过滤是否生效**。
这条是 FR-013（可关闭）与 FR-009（禁止悄悄忽略）之间冲突的解法，起草时没有被显式回答。

## 6. 效果证据，以及它的边界（本报告最重要的一节）

### 6.1 本期给不出真实效果幅度

理由有三条，每一条单独成立：

1. **过滤是布尔的范围缩小，不改变打分**（FR-012）。它的"效果"完全取决于调用方是否指定了
   *正确的*范围——这是输入质量问题，不是本功能的质量问题。
2. **缺少带范围标注的语料**。要度量"元数据过滤提升了检索质量"，需要一份标注了
   "每个问题的正确文档/页码是哪个"的真实语料。仓库没有，`eval/testset.yaml` 也不含范围标注。
3. **`make eval` 不能用**。它带 LLM 裁判，同一份代码跑两次都不一致（宪法已明确禁止用它证明
   行为未变），更不能用来给一个布尔过滤功能编造提升百分比。

### 6.2 改用确定性门禁做机制证明

`make eval-retrieval-gate` 新增两条受控用例（无 LLM、无裁判、真实 PostgreSQL、受控向量）：

- **`filter_scopes_to_document`**（SC-001）：诱饵文档的余弦分数**更高**（0.99 > 0.90），
  不带过滤时它排第一；限定到目标文档后，结果里只剩目标文档那一条。
  分数刻意反向设置，是为了让"过滤后目标排第一"不可能是碰巧。
- **`filter_scopes_to_page_range`**（SC-002）：目标片段在第 12 页。
  页码范围 `[1,5]` 时不召回，`[10,15]` 时命中且排第一，第 2 页那条被挡住。

### 6.3 下推证明（SC-004，本期最关键的一条断言）

这是"过滤下推到召回 SQL"与"先召回 topK 再在应用层筛"**唯一能被外部观察到的区别**：

`TestIntegrationRetrieveFilterPushedDownToRecall` 构造一个知识库，让目标文档的 3 条片段
全部排在全库相似度榜的 `candidateK`（=12）名之外——15 条诱饵的余弦都接近 1.0，
目标文档的只有 ~0.743。

- 若过滤是应用层筛选：候选窗口里全是诱饵，筛完结果为**空**；
- 若过滤真的进了 SQL：SQL 一开始就只看目标文档，照常返回它内部的 topK。

用例先断言"不带过滤时目标文档确实一条都进不了结果"这个前提，再断言过滤后拿到完整的 3 条。

### 6.4 变异测试：确认这些断言真的有牙齿

一组"测试全绿"本身不证明任何事——测试可能根本抓不到 bug。因此做了两轮变异验证：

| 变异 | 预期 | 实际 |
|---|---|---|
| 让 `filterToPGParams` 恒返回零值（过滤根本不下推） | 7 条过滤用例全部失败 | ✅ 7 条全部 FAIL |
| 给**两侧**页码谓词都加上 `OR page_number IS NULL`（"无元数据即通过"） | NULL 页码用例失败 | ✅ FAIL |
| 只给 `PageMin` 一侧加 `OR page_number IS NULL`（单侧回归） | 应当失败 | ❌ **通过了** |

**第三行是这轮变异测试的真实收获**：原来的用例只测了闭区间 `[1,100]`，
于是即使有人把 `PageMin` 一侧改错，未被改动的 `PageMax` 一侧仍会把 NULL 行挡下来，
用例照样通过——这条断言对**单侧回归是瞎的**。
已修正：`TestFilterPageRangeExcludesNullPageChunks` 现在拆成"闭区间 / 只给下界 / 只给上界"
三个子用例，把每一侧独立暴露出来。重跑同一个单侧变异后，`只给下界 >=1` 子用例正确失败。

### 6.5 必须如实承认的边界

> 上述证据证明的是**机制成立**：过滤条件确实进了两路召回 SQL、确实缩小了候选来源、
> 确实在关闭与空过滤器时对既有行为零影响、邻接豁免确实按设计工作。
>
> 它**不是**真实世界的效果幅度。本阶段**没有**、也无法给出"限定文档后回答准确率提升了 N 个
> 百分点"这类结论——那需要一份带范围标注的真实语料和真实嵌入模型，本仓库当前两者都不具备。
>
> 任何对外材料（简历、面试话术、演示）都不得把门禁通过表述成真实效果提升。

## 7. 真实测试结果

改动前基线（**在任何代码改动之前**跑并归档，这是 001 期 T001 踩过的坑）：
`go vet` 无输出、`make check-deps` OK、`go test ./... -race` 全绿、门禁 12/12 通过。

改动后：

```
$ go vet ./...
（无输出）

$ make check-deps
check-deps: OK - no cross-layer or same-layer violations

$ go test ./... -race -count=1
（全部 ok，无 FAIL）

$ make eval-retrieval-gate
--- PASS: TestRetrievalGatePhase6 (0.95s)
    ... 14 个 case 全部 PASS（原 12 + 本期新增 2）

$ python3 scripts/compare-retrieval-gate.py /tmp/gate-baseline.json eval/runs/phase6-retrieval-gate-latest.json
IDENTICAL（12 个既有用例逐字段一致，metrics/pass 未变）
新增用例（允许）: filter_scopes_to_document, filter_scopes_to_page_range
```

冒烟测试（真实 HTTP，本期不改任何端点，验的是签名变更没破坏 `buildApp` 装配）：
health 200 / ready 200 / 无 token 401 / 登录拿到 JWT / users/me 200 /
providers、knowledge-bases、agents、conversations、workflows 列表全部 200。

### 7.1 关于 SC-003 比对方式的一处修正

规格原文要求"检索输出与上线前**逐字一致**"。最初按"整份门禁报告逐字节相同"执行，
但这与 SC-001/SC-002 要求的"门禁中存在过滤用例"直接冲突——新增用例必然让 `cases` 数组变长。

修正为 `scripts/compare-retrieval-gate.py` 的三条规则：基线里每个 case 必须存在且逐字段相同、
`metrics` 与 `pass` 必须相同、**允许新增 case**。
SC-003 真正要断言的是"既有行为一个字都没变"，不是"报告文件永不增长"——
按后者执行会让任何新增门禁用例都被判成回归，最后逼着人干脆放弃这条断言。
新增的两条用例都命中 rank 1，四项指标仍是 1.0，因此 `metrics` 本身也未变。

## 8. 已知缺口：这个功能上线后无人可用

**本期不提供任何设置过滤器的入口**——没有 HTTP 参数、没有前端界面、没有 Agent 配置项。
`dto.go` / `handler.go` / `wire.go` 因此都没有改动，两个现有调用方
（`conversation/context.go`、`workflow/executor.go`）传的都是零值。

这是 spec 明确的 Out of Scope（"本期只提供后端能力与契约"），不是遗漏。
但后果必须如实写明：**这个能力目前只能被单元测试和集成测试调用到，生产路径上无人可用。**
它的价值兑现依赖下一期（过滤条件从哪来）。

同样被明确排除的还有：由 LLM 从用户问题里自动抽取过滤条件、Markdown 标题层级等其他元数据类型、
基于元数据的排序加权。

## 9. 未验证 / 剩余风险

1. **真实规模下的查询计划未验证**。研究阶段的判断是不需要新建索引——向量路本来就是知识库内的
   顺序扫描（`embedding` 列不声明维度因而建不了 HNSW，见 `000001` 注释），加过滤只会让参与打分
   的行更少；关键词路的过滤是 GIN 索引筛出候选集之后的残余谓词。
   这是**基于查询形态的推理**，不是在百万行数据上跑过 `EXPLAIN ANALYZE` 的实测。
   每知识库 5000 chunks 软上限下不构成风险，突破该量级时需重新评估。
2. **存量 PDF 的页码无法回填**。`000003` 迁移之前入库的 chunk，`page_number` 为 NULL，
   页码过滤对它们一律不匹配。唯一正当的补救路径是重新处理该文档（既有的 `RetryDocument`）——
   本期**没有**做回填命令，也不应该做：页码只能从原文件重新解析得到，凭空回填就是伪造。
3. **`document_id` 上限 50 是拍的**。参照 `maxTopK=50`，没有真实使用数据支撑。
   超限是明确报错而不是截断，所以调错了也只是不好用，不会静默产生错误结果。
4. **过滤前候选数量未记录**。FR-017 字面要求"过滤前后候选数量"，本期只记过滤后。
   要拿到"过滤前"必须把两路召回各跑两遍，那正是 FR-007 禁止的形态的成本。
   已登记在 `plan.md` 的复杂度追踪一节。

## 10. 规格制品

```
specs/002-metadata-filter/
├── spec.md              # 两轮澄清后的规格（Status: Clarified）
├── plan.md              # 技术方案、宪法检查、范围边界、复杂度追踪
├── research.md          # R1-R8：技术判断与被否决的方案
├── data-model.md        # 类型/SQL/配置/可观测字段
├── contracts/
│   └── internal-contracts.md
├── quickstart.md        # 验收步骤与清单
└── tasks.md             # 37 条任务
```

## 11. 改动清单

**新增**：`internal/knowledge/filter_test.go`、`scripts/compare-retrieval-gate.py`、
`specs/002-metadata-filter/*`、本报告。

**修改**：`internal/db/pgqueries/chunks.sql`（两条召回查询各加三行谓词 + 更正 `CreateChunk`
过期注释）、`internal/db/pggen/*`（sqlc 重新生成，未手改）、
`internal/knowledge/{model,errors,repository,service,wire}.go`、`internal/config/config.go`、
`internal/conversation/context.go`、`internal/workflow/executor.go`、
`cmd/{hify,evalrunner}/main.go`（各一行构造参数）、
测试文件（`integration_test.go`、`eval_gate_test.go`、`structure_test.go`、
`conversation/integration_test.go`）。

**未修改**：全部前端、`agent`、`mcp`、`auth`、`user`、`provider`，
以及 `knowledge/{dto,handler,parse,chunk,hybrid,admission,dedup,neighbor,rerank}.go`。
`parse.go`/`chunk.go` 未改是本期的一条关键结论——页码产出链路已经正确，
本期只为它补了一条回归断言（`TestChunkPDFPagesTagsEveryPieceWithItsSourcePage`）。

**没有新增 migration。**
