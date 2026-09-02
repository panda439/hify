# Phase 11：知识库试检索面板（Retrieval Playground）实施报告

**功能分支**：`003-retrieval-playground` ｜ **规格**：`specs/003-retrieval-playground/`
**实施日期**：2026-09-02
**开发方式**：spec-kit 流程（specify → plan → tasks → implement），AI 辅助开发

## 0. 一句话总结

给 Phase 10（002-metadata-filter）的过滤能力接上第一个真实调用方：
新增 `POST /api/v1/knowledge-bases/:id/retrieve` 端点 + 知识库页的「试检索」对话框。
**Phase 10 报告第 8 节记录的「功能上线后无人可用」这个缺口，本期关闭。**

定位是**检索调试工具，不是对话功能**：不调用对话模型、不创建会话或消息、不写 `trace_spans`。

## 1. 为什么是这个形态

Phase 10 交付了过滤能力却没有入口。补入口有三个方向，本期选了工作量最小、风险最低的那个：

| 方向 | 结论 |
|---|---|
| **知识库页「试检索」面板** | ✅ 选定。零 migration、不动 conversation/SSE、不改 Agent 语义，且能完整验证 Phase 10 的两个过滤维度 |
| Agent 配置里绑定文档范围 | ❌ 最贴合「三个版本手册只想用 2026 版」那个故事，但要改表 + migration + Agent 表单改造，还引出「绑定的文档被删了怎么办」这类新的一致性问题。单独立项 |
| 对话时临时指定范围 | ❌ 最灵活也最危险：要改 conversation 链路、SSE 协议和消息持久化语义（这一轮用了什么范围要不要存？重新生成时还算数吗？） |

**代价要说清楚**：选定方向本质上是个调试/精确查询工具，
它让过滤能力**可被触达、可被验证**，但**不直接改善普通对话的回答质量**——
那需要上面被否决的两个方向之一。

## 2. 关键设计判断

### 2.1 为什么新端点用 POST

语义上它是查询而非创建资源。用 POST 的理由有两条，第二条是决定性的：

1. 请求体里有文档 ID 数组（最多 50 个）和问题原文，塞进 query string 既难看又会撞长度限制；
2. **问题原文不应该进 URL**——URL 会被网关/代理的访问日志完整记下来。
   这与 Phase 10 的 FR-018 不把过滤取值写进应用日志是同一个隐私口径。

这属于 REST 的常见妥协，已在 contracts 和 `wire.go` 的注释里写明，避免后来者误以为它会创建资源。

### 2.2 未知知识库返回 404 而不是空结果

`Service.Retrieve` 自己把未知 KB 当成「不贡献候选」——那对 conversation 是**对的**
（一个被删的知识库不该让整轮对话失败）。但对试检索面板是**错的**：空结果会被用户读成
「没匹配到」，把真正的问题（ID 写错了）掩盖掉。所以 handler 在检索前先 `GetKnowledgeBase`
确认存在。这是同一个 Service 方法在两种调用场景下需要不同错误语义的一个具体例子。

### 2.3 无页码的片段序列化成 `null` 而不是 `0`

`chunkResult.PageNumber` 是 `*int`。txt/md 片段和 `000003` 迁移前的存量行本来就没有页码，
`0` 会是一个被编造出来的值（页码 1-indexed）。前端把 `null` 渲染成「—」。
有一条专门的断言锁定它（`TestRetrieveHandlerSerializesMissingPageAsNull`）。

### 2.4 邻接块必须在界面上被解释

Phase 10 的 FR-011 规定邻接块豁免页码过滤。后果是：用户限定第 10-15 页，
结果里**合法地**出现第 9 页的片段。不解释的话，这看起来就是个 bug。
所以响应带 `is_neighbor` / `neighbor_of`，界面用虚线边框 + 「邻接块」徽章区分，
并在有邻接块时显示一段说明：它是上下文补全、不受页码约束、但始终来自限定的文档之内。

## 3. 真实测试结果

### 3.1 自动化

```
$ go vet ./...
（无输出）

$ make check-deps
check-deps: OK - no cross-layer or same-layer violations

$ go test ./... -race -count=1
（全部 ok，无 FAIL）

$ go test ./internal/knowledge/ -run TestRetrieveHandler -race -v
--- PASS: TestRetrieveHandlerWithoutFilterReturnsBothDocuments
--- PASS: TestRetrieveHandlerFilterByDocument
--- PASS: TestRetrieveHandlerFilterByPageRange
--- PASS: TestRetrieveHandlerSerializesMissingPageAsNull
--- PASS: TestRetrieveHandlerPropagatesFilterErrors   （4 个子用例）
--- PASS: TestRetrieveHandlerEmptyFilterUnaffectedByToggle
--- PASS: TestRetrieveHandlerRejectsBadRequests       （4 个子用例）
--- PASS: TestRetrieveHandlerUnknownKnowledgeBaseIs404
--- PASS: TestRetrieveHandlerMarksNeighborChunks

$ make eval-retrieval-gate && python3 scripts/compare-retrieval-gate.py ...
IDENTICAL（14 个既有用例逐字段一致，metrics/pass 未变）

$ make web-build
tsc -b && vite build  →  ✓ built in 325ms（无 TS 错误）
```

这些用例走的是**真实 gin handler + 真实 Service + 真实 PostgreSQL**，不是直接调 Service——
本期新增的代码全在 handler/dto 这一层，绕过它等于什么都没测。

### 3.2 真实运行的服务（SC-001）

在真实启动的服务上用 HTTP 走了一遍（`HIFY_RAG_METADATA_FILTER_ENABLED=true`）：

| 场景 | 结果 |
|---|---|
| 不限定范围 | `hit_count=2`，命中来自两份不同文档 |
| 限定到 FAQ 文档 | `hit_count=1`，只剩该文档的片段（+1 个同文档邻接块） |
| 限定到另一份文档 | `hit_count=1`，只剩那一份的片段 |

错误路径逐条验证（全部真实 HTTP 响应）：

| 输入 | 响应 |
|---|---|
| 页码起止颠倒 / 页码为 0 | 400 `knowledge.invalid_page_range` |
| 文档数 51 个 | 400 `knowledge.too_many_filter_documents`（**不截断**） |
| 问题只有空白 | 400 `knowledge.invalid_request` |
| 知识库不存在 | 404 `knowledge.not_found` |
| 页码过滤作用在 txt 上 | 200 + 空结果（无页码视为不匹配，符合 Phase 10 语义） |
| **开关关闭 + 非空过滤器** | 400 `knowledge.metadata_filter_disabled`（**不静默降级**） |
| **开关关闭 + 空过滤器** | 200 `hit_count=2`（照常工作） |

最后两行是 Phase 10 那条设计判断的端到端确认：开关关闭的是「接受过滤请求」这个能力，
而不是「过滤是否生效」。

## 4. 必须如实承认的边界

### 4.1 SC-002（页码过滤）已在真实服务上验证 —— 补记

> **本节是对初版报告的更正。** 初版写的是「SC-002 只在集成测试层面验证过，没能在真实运行的
> 服务上验证」，原因是开发环境的 embedding 供应商连不上、唯一的 PDF 处于 `failed` 状态零分片。
> 随后把 embedding 换成本地 Ollama + `bge-m3:567m`（1024 维，做法见 §4.4），该限制不再成立，
> 故据实更正。原文保留在 git 历史里。

**验证方式**：生成一份 15 页 PDF（每页一个唯一标记 `PAGEMARKER01~15` + 各不相同的正文，
"部署流程"相关内容集中在第 9-12 页），用真实 bge-m3 向量入库（15 页 → 15 个分片，
页码 1-15 全部正确落库），再通过真实服务的 HTTP 端点检索。

问题固定为 `deployment procedure`，topK=3：

| 场景 | 命中的页 | 观察 |
|---|---|---|
| A. 不限定页码 | 9, 11, **5** | 第 5 页（数据库配置）与问题无关，却占掉一个 topK 名额 |
| B. 限定 `[9,12]` | 9, 11, **10** | 第 5 页被排除，**第 10 页顶替进来** |
| C. 限定 `[1,5]` | 5, 1, 4 | 范围外的部署页一条都没有 |

**B 这一行同时是 SC-004（下推证明）在真实数据上的自然复现，比 Phase 10 那条构造出来的用例更有力**：

不限定时 topK 是 9、11、5。如果过滤是"先召回 topK 再在应用层筛"，B 只该剩 9 和 11 **两条**。
实际是**三条**，多出来的第 10 页原本排在全局 topK 之外——过滤没有**吃掉**名额，
而是把名额**重新定向**了。这正是 FR-007 要求下推的全部理由，此前只有构造用例能证明。

顺带也是一次真实的质量改善：对"deployment procedure"这个问题，
第 10 页（部署第二步）显然比第 5 页（数据库配置）更该出现在结果里。

**FR-011 邻接豁免也在真实数据上可见**：B 场景返回的邻接块里包含**第 8 页**，
它落在 `[9,12]` 之外——邻接块是上下文补全、不受 chunk 级过滤约束，此前同样只有测试断言。

### 4.2 向量路已复活（补记）

初版报告写的「真实服务上的验证全部只走了 pg_trgm 关键词路」同样不再成立。
§4.1 的三组结果是在 1024 维真实向量下跑出来的，向量路与关键词路都参与了。

### 4.3 本期没有效果度量

与 Phase 10 同理：过滤是布尔的范围缩小，不改变打分。本期交付的是**可达性**——
让一个此前只能被测试调用的能力变得可以被人使用。
「用户用了这个面板之后回答质量提升了多少」不是本期能回答的问题，也没有语料能回答它。
**上述全部结论都是机制证明，不是效果幅度。**

### 4.4 embedding 从 mock 换成了本地 Ollama（本次连带改动）

这不是 003 规格里的内容，但它是 §4.1 能够成立的前提，必须记录。

**改动前**：知识库唯一可用的 embedding 模型是 `mock-embedding-model`，
指向 `http://127.0.0.1:8090/v1` 的本地 mock server（当时并未运行），
库里存量向量是 **32 维**——不是任何真实 embedding 模型的输出维度。
这意味着**在此之前，本仓库 RAG 链路的向量一路从未跑过真实模型**。

**改动后**：复用 `~/AI-self-study/research-agent` 项目 005 期已验证的方案——
本地 Ollama + `bge-m3:567m`（1024 维）。Ollama 暴露 OpenAI 兼容的 `/v1/embeddings`，
而 hify 的 provider 层本就是 OpenAI 兼容的，因此**不需要任何代码改动**，
只是在运行时注册了一个 provider 与一个模型：

| 对象 | 值 |
|---|---|
| provider | `ollama-local`，`base_url=http://127.0.0.1:11434/v1`，`auth_type=none`（Ollama 不需要认证） |
| model | `bge-m3:567m`，`capability=embedding`，`embedding_dimension=1024` |

**这是运行时数据，不是代码改动**——没有任何文件因此被修改。
换机器或重建数据库后需要重新注册（Ollama 侧需先 `ollama pull bge-m3:567m`）。

> **对既有报告的影响**：Phase 1-11 全部报告里"机制证明、不是效果数字"那句话，
> 根源就是这里的 mock 向量。本次改动**只是让真实度量成为可能**，
> 并不追溯性地让此前任何一期的结论变成真实效果数字。

## 5. 未验证 / 剩余风险

1. ~~**浏览器验证中发现的 UI 缺陷未修复**：前端校验失败时旧结果不清空~~ —— 已于 2026-09-02 修复，见 §6.1。
2. **`document_name` 对存量 chunk 是空串**（`000003` 迁移前入库的行）。
   前端已做兜底（回退显示 document_id），但显示效果不理想。
   正当修法是重新处理文档，不在本期范围。
3. **面板默认状态下勾选文档会报错**。`HIFY_RAG_METADATA_FILTER_ENABLED` 默认仍是 `false`，
   这是**有意保持**的（spec 明确不改默认值）。界面把需要设置的环境变量名直接写在错误提示里。
4. **没有分页**。topK 上限 50（`clampTopK`），一屏够放。
5. **权限沿用知识库既有模型**（登录用户皆可检索），本期未新增权限维度。

## 6. 浏览器端到端验证（补做）

初版报告写的「前端只做了构建验证，没有浏览器点击验证」已补做。
在真实运行的服务 + 真实 bge-m3 向量 + 15 页测试 PDF 上，逐项点过：

| 检查项 | 结果 |
|---|---|
| 知识库列表页出现「试检索」按钮（FR-008） | ✅ |
| 面板顶部说明「不会产生对话」（FR-012） | ✅ |
| 已就绪文档按**文件名**列出可勾选（FR-009） | ✅ 显示 `ops-handbook.pdf` |
| 不限定页码检索 | ✅ 命中第 9/11/**5**/10/12 页 |
| 限定 `[9,12]` | ✅ 命中第 9/11/10/12 页，**第 5 页消失** |
| 结果显示来源文档名 + 页码 + 分数（FR-011） | ✅ |
| 命中与邻接块视觉区分（US4） | ✅ 实线/虚线边框 + 「命中」/「邻接块」徽章 |
| 邻接块说明文字 | ✅ 有邻接块时才出现 |
| **邻接豁免在界面上可见** | ✅ `[9,12]` 的邻接块是第 **8** 页和第 **13** 页，都在范围外 |
| 「已按指定范围过滤」提示 | ✅ |
| 前端校验：起始页 > 结束页 | ✅ 红字「起始页不能大于结束页」，不发请求 |

### 6.1 发现的一个 UI 缺陷（已修复）

**前端校验失败时，上一次的检索结果仍然留在下方。** 用户可能把旧结果误读成新输入的结果。

正确行为应该是校验失败时清空结果区，或者给结果区加一个"结果对应的是上一次查询"的标记。
这是构建验证（`tsc -b && vite build`）抓不到、只有真的点一遍才会暴露的问题——
也说明"编译通过"不等于"交互正确"。

**修复（2026-09-02）**：`handleSearch` 里 `validate()` 返回非 null 时，
除了 `setLocalError(problem)` 之外一并调用 `probe.reset()`，
把上一次的结果和错误状态一起清掉，结果区随之消失。
选的是"清空"而不是"给旧结果加标记"——更简单，也不给用户留误读的余地。
文件：`web/src/routes/knowledge-retrieval-dialog.tsx`。

## 7. embedding 换成真实模型之后的首次 RAG 评测

§4.4 的 embedding 迁移让 `make eval`（带 LLM 裁判的完整评测）第一次能跑出**非 mock** 的数字。

**跑法**：把原 `eval/testset.yaml` 里的两份文档重新上传进一个用 bge-m3 建的新知识库，
新建一个绑定该库的 Eval Agent，用**临时副本 testset** 跑——
**`eval/testset.yaml` 与 `eval/baseline.json` 均未改动**，这次结果不作为新基线。

裁判模型 `deepseek-v4-flash`，14 个用例：

| 指标 | 值 |
|---|---|
| 检索命中率 / Recall@1 / Recall@3 / MRR | 0.917（12 条可评估用例中 11 条命中） |
| 期望文档被引用 | 0.917 |
| 引用要求满足 | 0.923 |
| 裁判平均分 | 4.57 / 5（11 个满分，1×4，1×3，1×2） |

> **这些是真实测量值，但不是"提升幅度"。** 没有可比的"改动前"数字——
> 改动前的向量路根本跑不起来（mock server 未运行 + 32 维 mock 向量），
> 得不到一个有意义的对照组。所以这组数字说明的是"当前系统在这个 14 条用例的小测试集上表现如何"，
> **不能**被表述成"元数据过滤/试检索面板带来了多少提升"——这两期功能压根没参与这些用例
> （测试集里没有任何用例使用过滤器）。
>
> 另外测试集只有 14 条、语料只有两份 txt，样本量不足以支撑任何统计性结论。

## 8. 改动清单

**新增**：`internal/knowledge/retrieve_handler_test.go`、
`web/src/routes/knowledge-retrieval-dialog.tsx`、`specs/003-retrieval-playground/*`、本报告。

**修改**：`internal/knowledge/{dto,handler,wire}.go`、
`web/src/lib/knowledge.ts`、`web/src/routes/knowledge.tsx`。

**未修改**：`internal/knowledge/{model,errors,repository,service}.go` —— 检索逻辑一行没动，
这是 SC-004（门禁既有用例逐字段不变）能够成立的前提。
也未触及 conversation / workflow / agent / provider / config。

**没有新增 migration**（试检索不持久化任何东西）。
