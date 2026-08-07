# Hify 核心链路清单（改造前必测）

> 筛选标准：不是所有链路，只收"改造时容易出问题"的——跨模块协作多、有异步/流式/跨库等隐性契约、或失败后果严重（数据不一致、密钥泄露、静默丢数据）。共 9 条。
>
> 生成时间：2026-07-21（链路 3 于 2026-08-07 随 Hybrid Search Phase 3 更新，见 [docs/eval-phase3-hybrid-search-report.md](docs/eval-phase3-hybrid-search-report.md)；同一天随 Neighbor Window Retrieval Phase 4 再次更新，并在同一天的审核修复轮次里修正了邻接块预算优先级描述，见 [docs/eval-phase4-neighbor-window-report.md](docs/eval-phase4-neighbor-window-report.md) 第 0 节）

| # | 链路名 | 起点（接口） | 关键节点（service / DB 操作） | 终点（什么状态算成功） |
|---|--------|--------------|-------------------------------|------------------------|
| 1 | 对话流式问答主链路 | `POST /api/v1/conversations/:id/messages` | `StreamMessage` → `assembleContext`（历史消息按预算截断 `truncateByBudget`、RAG `knowledge.Retrieve`、`loadTools` 拉 MCP 工具）→ `runStream`（`ChatStream` 流式、`mergeToolCallDeltas` 按 Index 合并分片、`runToolCall` 工具循环 + 最大迭代保护）→ `persistAssistantTurn` | SSE 事件按序推完（含检索调试信息、tool 事件）；user + assistant 消息落库，tool_calls JSON 完整；客户端断流时已生成的部分内容仍落库 |
| 2 | 文档入库异步流水线 | `POST /api/v1/knowledge-bases/:id/documents` | `UploadDocument`（校验 embedding 模型、`saveFile` 落盘、MySQL 建 document 记录）→ `enqueueProcessDocument` 入 asynq 队列 → `ProcessDocument`（解析 + `chunkDocument` 结构感知分块——md 用 `chunkMarkdown` 保留标题/段落/列表/代码块/表格并带 section_title，txt 用 `chunkPlainText` 按段落/句子边界切，pdf 用 `chunkPDFPages` 逐页切并带 page_number，单个结构超限统一回退 `chunkText` 定长切分、`Embed` 批量生成、向量数与分块数一致性校验、写 PG chunks 表）| document 状态变为 completed；PG 中 chunk 行数 = 分块数、embedding_dimension 正确；任一环节失败时 `failDocument` 把状态置 failed 而不是卡在 processing |
| 3 | Hybrid Search + 邻接分块扩展检索链路（Phase 3 + Phase 4） | 由链路 1 / 链路 5 内部触发（`knowledge.Retrieve`） | 向量一路：按 embedding 模型分组 `kbsByModel` → `embedQuery` → `searchVectorChunks`（pgvector `<=>` 余弦距离，打分/排序/candidateK 全下推，返回 `document_version`）；关键词一路：`searchKeywordChunks`（pg_trgm `<%` word-similarity，GIN 索引，不依赖 embedding，同样返回 `document_version`）；两路候选各取 `candidateK = min(max(topK*4, topK), 100)` → `rrfFuse` 按 chunk ID 去重、Reciprocal Rank Fusion 排序、截断全局 topK 核心命中（anchors）→ Phase 4：`expandWithNeighborWindow` 按 `(document_id, document_version)` 给 anchors 分组，每组一条 `findPublishedNeighborChunks` 查同文档同版本已发布的前一个/后一个 chunk_index → `expandWithNeighbors` 拼装成两个完整分层输出：全部 topK 核心块（保持 RRF 排名）在前，全部邻接块（按所属核心块排名分组，组内 previous/next）整体在后——不是每个核心块紧跟自己的邻接块再轮到下一个核心块，这个两层布局是让 `conversation/budget.go` 的 `selectEvidence`（严格按输入顺序贪心消耗预算）天然保证"预算不足时核心块优先于所有邻接块、邻接块绝不挤出排名更低的核心块"的唯一原因 → 按 chunk ID 全局去重 | 返回核心命中 chunk 且融合排序正确（fusionScore 降序/Score 降序/ID 升序三级确定性排序）；`RetrievedChunk.Score` 始终是 0..1 相关度（向量命中=余弦相似度，关键词命中=word-similarity，两路命中取较大值；邻接块继承所属核心块的 Score，不冒充独立相关度），不泄漏 fusionScore；混合维度知识库互不串扰；embedding 服务失败时关键词一路仍可返回结果；两路都失败/无命中时返回空而非报错；邻接查询失败或核心块所属文档版本已被重新处理删除时静默降级为只返回核心块，绝不跨版本混入邻接内容；context 取消/超时在两个阶段都正常传播，不被吞掉；最终返回的 chunk 数量上界是 topK*3，不再是 topK；邻接块在预算不足时绝不能把排名更低的核心块挤出结果 |
| 4 | 文档删除跨库一致性 | `DELETE /api/v1/knowledge-bases/:id/documents/:docId` | `DeleteDocument`：权限校验 → 先删 PG chunks → 再删 MySQL document 记录（无分布式事务，靠删除顺序保证）| 两库均无残留；删除后立即检索不再返回该文档的 chunk；中途失败不会出现"MySQL 已删、PG 还在"的孤儿向量 |
| 5 | 工作流执行链路 | `POST /api/v1/workflows/:id/executions` | `Execute` → `Definition.Validate`（无环、可达性）→ `runWorkflow` 按 DAG 顺序执行节点：`runLLMCall` / `runKnowledgeRetrieval` / `runConditional`（expr-lang 求值）/ `runToolCall`，节点间 `renderTemplate` 模板变量渲染 → run + steps 执行轨迹落库 | run 状态 completed，每个已执行节点有 step 轨迹记录且输入输出可回放；条件分支走向正确；SSE 推送的节点事件与 DB 轨迹一致；非法 DAG 在执行前被拒绝 |
| 6 | Provider 凭证与客户端解析 | `POST /api/v1/providers`、`PUT /api/v1/providers/:id`、`POST /api/v1/providers/:id/test` | `CreateProvider` / `UpdateProvider`（API Key AES-256-GCM 加密后落库）→ `ResolveClient`（读库解密、构造 OpenAI 兼容 client、`WithResilience` 装饰）→ `TestConnection` | DB 中只存密文、任何接口响应不回显明文 Key；ResolveClient 出的 client 能实际调通；更新 Key 后后续调用用的是新 Key（无脏缓存） |
| 7 | 弹性装饰器行为 | 无独立接口，包裹所有 LLM 调用（链路 1/2/3/5 共用） | `acquire`（semaphore 并发限流）→ `checkRateLimit`（Redis 令牌桶）→ gobreaker 熔断 → `callWithRetry`（`isRetryable` 错误分类 + `retryAfterAwareDelay` 退避）| 可重试错误（429/5xx）按退避重试，不可重试错误立刻透传；连续失败触发熔断、恢复后半开放行；**流式场景重试不会向客户端重复推送已发内容**；限流满时排队或快速失败而非 goroutine 泄漏 |
| 8 | 认证与权限门禁 | `POST /api/v1/auth/login` → `POST /api/v1/auth/refresh`；门禁覆盖所有业务路由 | 登录校验 → 签发 JWT → `RequireAuth` 中间件解析 token → role 中间件 RBAC 校验 → 各 service 内的资源归属校验（如 userID 匹配） | 无 token / 过期 token 返回 401，角色不足返回 403；refresh 能换取新 token 且旧流程不断；用户 A 拿不到用户 B 的会话/知识库（越权返回 403/404 而非数据） |
| 9 | MCP 工具同步链路 | `POST /api/v1/mcp-servers/:id/sync` | `SyncTools`：`listRemoteTools`（按 transport 建连，stdio 起子进程 / SSE 建网络连接，MCP 握手 + 工具发现）→ 逐个 `upsertTool`（按 server_id + tool_name 幂等）→ 远端已消失的工具 `deactivateTool` 软停用（不硬删，agent_mcp_tools 仍在引用）→ `updateServerSyncResult` 记录状态 → 对话时 `loadTools` 消费 | 同步后 DB 工具集与远端一致：新增被插入、重复同步不产生重复行、消失的被置 inactive 而非删除；连接失败时 server 状态置 failed 且不污染已有工具；同步后的工具能被链路 1 的工具调用实际调通 |

## 为什么是这 9 条

- **1、2、5 是三条"长链路"**：跨 3 个以上模块、含异步或流式环节，改任何一个中间模块（provider 接口签名、knowledge 检索返回结构、消息落库 schema）都可能在链路末端才暴露问题。
- **3、4 是 pgvector 迁移引入的跨库契约**：MySQL 和 PG 之间没有事务保证，一致性靠代码约定（删除顺序、backfill 幂等），重构时最容易被无意破坏。
- **6 是安全敏感点**：加解密逻辑改错的后果是密钥泄露或全部 provider 不可用。
- **7 是隐形行为契约**：熔断/重试/限流的正确性不影响编译、普通手测也测不出来，只有故障注入才能验证——恰恰是 AI 改代码时最容易悄悄改坏的地方。
- **8 是所有链路的门禁**：中间件顺序或 RBAC 判断被改动时，失败模式是"静默放行"，必须有越权用例兜底。
- **9 是链路 1 的上游依赖**：涉及外部进程/网络（stdio 子进程、SSE 连接），同步逻辑（幂等 upsert、软停用而非硬删）改坏后的表现是对话时工具静默缺失或引用悬空，不会在同步接口本身报错。

刻意未收：Agent 配置引用一致性（Agent 引用的 model/知识库/MCP 工具被停用后的行为）——偏数据契约而非链路，其失败表现会在链路 1 的用例中暴露，作为链路 1 的测试用例覆盖即可。

## 测试覆盖状态（2026-07-21）

`make test`（容器没起时集成测试自动 skip）/ `make test-race`；CI 见 `.github/workflows/ci.yml`。

| 链路 | 覆盖 | 位置 |
|------|------|------|
| 1 对话流式 | 单测（分片合并/截断）+ 集成（完整工具循环、断流落库、越权、幻觉工具容错） | `conversation/{merge,context,integration}_test.go` |
| 2 入库流水线 | 单测（结构感知分块：md 标题/段落/列表/代码块/表格、txt 段落/句子边界、pdf 页码隔离、超限回退、非法 overlap 配置、空文档）+ 集成（happy path、向量数不一致、文件丢失、pdf 页码端到端、md section_title 端到端） | `knowledge/{chunk,structure,integration}_test.go` |
| 3 Hybrid Search + 邻接分块扩展 | 单测（`rrfFuse`/`sortVectorCandidatesByScoreThenID` 纯逻辑：双路命中提升排名、去重、单路场景、确定性排序、topK 截断、Score 语义不被 fusionScore 污染、顺序无关、向量候选并列 Score 按 ID 稳定排序；`expandWithNeighbors`/`buildNeighborGroups`/`neighborIndexesFor` 纯逻辑：前后邻接块补齐、chunk_index=0/文档末块边界、缺号跳过、核心块排名不变、**全部核心块在全部邻接块之前的两层输出布局**、邻接块本身是核心块时不重复不提前、共享邻接块归属排名更高的核心块、Score 继承、`NeighborOf` 归属、Citation 元数据不被核心块覆盖、确定性、`anchorCount*3` 上界、空核心/空邻接边界；`conversation/budget.go` 的 `selectEvidence` 预算优先级回归：预算只够两个 source 时邻接块绝不挤掉排名更低的核心块、预算充足时按两层顺序全部保留、预算只够核心块时邻接块全部过滤、预算连核心块都放不下时仍按核心排名依次取舍、邻接块内部顺序变化不影响核心块保留结果）+ 集成（pgvector `<=>` 排序分数/维度过滤/topK/跨模型合并/inactive 跳过/并列 Score 按 ID 稳定；pg_trgm 中英文关键词命中、未发布/跨知识库过滤、维度无关；融合把强关键词命中提进 topK、去重、Citation 元数据完整、空 query/空知识库返回空；`findPublishedNeighborChunks` 前后块查询、跨文档/跨版本隔离、未发布排除、chunk_index=0 无负数查询、`chunk_index ASC, id ASC` 排序、Citation 元数据、`document_version` 在向量/关键词两路都正确返回、文档重新处理后旧版本邻接查询返回空而非串版本、邻接查询普通失败时降级为只返回核心块、context 取消传播） | `knowledge/hybrid_test.go`、`knowledge/neighbor_test.go`、`knowledge/integration_test.go`、`conversation/budget_test.go` |
| 4 跨库删除 | 集成（三处清理、越权拒绝、admin 越过归属、删后检索为空） | `knowledge/integration_test.go` |
| 5 工作流执行 | 单测（DAG 校验、模板、条件）+ 集成（双分支轨迹、步失败收敛、环拒绝、inactive 拒绝） | `workflow/{dag,executor,integration}_test.go` |
| 6 凭证加密 | 单测（往返、随机 nonce、篡改/错 key 拒绝） | `provider/crypto_test.go` |
| 7 弹性装饰器 | 单测+故障注入（错误分类、熔断、断流不重试、空闲超时） | `provider/resilience_test.go` |
| 8 认证门禁 | HTTP 层测试（401/403 矩阵、claims 传递、中间件顺序） | `server/middleware/auth_test.go` |
| 9 MCP 同步 | 入口校验单测；SyncTools 的幂等/软停用尚未覆盖（repo 无接口缝隙，需真 MCP server 或抽接口） | `mcp/service_test.go` |

集成测试基建：`internal/testutil`（每包独立 `hify_test_<pkg>` 库，重建+迁移，支持 `go test ./...` 并行）。

## 使用建议

改造任何模块前，先确认其所在链路有对应的 characterization test 或最小冒烟脚本；改造后按链路终点的成功标准逐条验收，而不是只看单元测试绿灯。
