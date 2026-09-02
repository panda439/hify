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

### 4.1 SC-002（页码过滤）没能在真实服务上验证

**只在集成测试层面（真实 PostgreSQL）验证过，没有通过真实运行的服务验证。**

原因：开发环境的 embedding 供应商连不上（日志里是
`query embedding failed ... 无法连接到供应商`，随后熔断）。文档处理需要 embedding，
所以库里唯一那份 PDF 处于 `failed` 状态、零分片，没有任何带页码的数据可供检索。
两份可用文档都是 txt，本来就没有页码。

因此本报告中「限定页码」的证据是：
- ✅ **集成测试**：`TestRetrieveHandlerFilterByPageRange`（真实 PG，第 12 页命中、txt 被挡住）
- ✅ **真实服务**：页码过滤作用在无页码的 txt 上正确返回空结果
- ❌ **真实服务 + 真实 PDF 页码命中**：未验证

要补齐这一条，需要一个可用的 embedding 供应商配置，重新处理一份 PDF。

### 4.2 向量路在本次真实验证中是降级状态

同上原因，真实服务上的验证全部只走了 pg_trgm 关键词路。
过滤在两路召回 SQL 里的下推**在集成测试中双路都验证过**（Phase 10 的用例），
但本次真实服务验证只覆盖了关键词一路。

### 4.3 本期没有效果度量

与 Phase 10 同理：过滤是布尔的范围缩小，不改变打分。本期交付的是**可达性**——
让一个此前只能被测试调用的能力变得可以被人使用。
「用户用了这个面板之后回答质量提升了多少」不是本期能回答的问题，也没有语料能回答它。
**上述全部结论都是机制证明，不是效果幅度。**

## 5. 未验证 / 剩余风险

1. **前端只做了构建验证，没有浏览器点击验证**。`tsc -b && vite build` 通过、无类型错误，
   但界面在浏览器里的实际交互（勾选、空态、错误提示的呈现）未经人工点击确认。
   后端的等价路径全部有 HTTP 级验证。
2. **`document_name` 对存量 chunk 是空串**（`000003` 迁移前入库的行）。
   前端已做兜底（回退显示 document_id），但显示效果不理想。
   正当修法是重新处理文档，不在本期范围。
3. **面板默认状态下勾选文档会报错**。`HIFY_RAG_METADATA_FILTER_ENABLED` 默认仍是 `false`，
   这是**有意保持**的（spec 明确不改默认值）。界面把需要设置的环境变量名直接写在错误提示里。
4. **没有分页**。topK 上限 50（`clampTopK`），一屏够放。
5. **权限沿用知识库既有模型**（登录用户皆可检索），本期未新增权限维度。

## 6. 改动清单

**新增**：`internal/knowledge/retrieve_handler_test.go`、
`web/src/routes/knowledge-retrieval-dialog.tsx`、`specs/003-retrieval-playground/*`、本报告。

**修改**：`internal/knowledge/{dto,handler,wire}.go`、
`web/src/lib/knowledge.ts`、`web/src/routes/knowledge.tsx`。

**未修改**：`internal/knowledge/{model,errors,repository,service}.go` —— 检索逻辑一行没动，
这是 SC-004（门禁既有用例逐字段不变）能够成立的前提。
也未触及 conversation / workflow / agent / provider / config。

**没有新增 migration**（试检索不持久化任何东西）。
