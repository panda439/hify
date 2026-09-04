# Phase 9：查询优化与结果重排序（Query Rewrite + Rerank）实施报告

**功能分支**：`001-rag-query-rerank` ｜ **规格**：`specs/001-rag-query-rerank/`
**实施日期**：2026-08-24 ～ 2026-08-25
**开发方式**：spec-kit 流程（specify → clarify → plan → tasks → implement），AI 辅助开发

## 0. 一句话总结

在 RAG 流水线两端各加一层——**入口**把依赖上文的省略式追问改写成可独立理解的检索问题，
**出口**在截断到 topK 之前按问题—片段真实相关度重排候选。两者默认关闭、各自可独立开关、
任何失败都只降级为上线前的行为。

**这份报告最重要的一句话在第 6 节**：本阶段的效果结论是**机制证明**，不是真实效果幅度。

## 1. 解决的两个问题

**问题一：多轮追问检索不到。** 用户第一轮问「Hify 的分块策略是什么」，第二轮只说「那它的上限呢」。
这句话里没有任何实词能对上文档，向量表示也没有主语——关键词路几乎打不中，向量路没有语义锚点。
用户只会看到"助手第二轮突然不知道了"。

**问题二：召回到了但没用上。** RRF 只是排名融合，它不理解 query 与 chunk 的真实语义相关度。
真正回答问题的那一段可能排在第 6 位，被 topK 截断或被上下文预算挤掉。

## 2. 新的链路顺序

```
用户消息
  └─ [conversation] 快速路径判定 ──命中──> 原问题
        └─未命中─> 改写 LLM ──失败/超时/歧义──> 原问题（降级）
                      └─成功─> 独立问题
  └─ knowledge.Retrieve(检索问题)
        ├─ 向量路召回 / 关键词路召回
        ├─ RRF 融合排序
        ├─ 来源感知准入（Phase 8，阈值不变）
        ├─ 内容去重（Phase 5）
        ├─ ★ 重排序（前 50 条）──失败/超时/校验不过──> 保持原顺序（降级）
        ├─ topK 截断
        └─ 邻接批量查询 + 二次去重（Phase 4/7）
  └─ [conversation] selectEvidence（0.2 分数线与预算策略不变）
```

**为什么重排必须在这个位置**：与 Phase 8 准入层同构的论证。放在 topK 截断**之后**没有意义——
被截掉的候选已经没机会翻身；放在邻接查询**之后**则会为被重排淘汰的候选白付一次数据库查询。
放在准入与去重之后，保证送进 rerank 的都是合格且无重复的候选，既省 token 又避免重复内容互相挤占。

为此 `rrfFuse` 不再自己截断 topK，截断上移到 `Retrieve` 里重排之后。这是本阶段唯一一处
**行为等价改造**，其等价性由两道证据锁定：`hybrid_test.go` 的同序断言，以及确定性检索门禁的
逐字节比对。

## 3. 关键工程约束（以及为什么）

| 约束 | 为什么 |
|---|---|
| `rerankInputLimit = 50` | 候选池上界是 `2 × candidateK ≤ 200`，全量送重排的延迟与成本不可接受。只重排前 50，其余保持原相对顺序排其后，尾部候选永远不会越过被重排的头部 |
| 响应校验 5 条（长度不符/index 越界/重复/缺失/解析失败）任一命中即**整体丢弃** | 部分采用会产生一个"一半按相关度、一半按融合排名"的混合顺序，比两种纯粹顺序都差，而且无法解释。`provider` 与 `knowledge` 两层各自独立校验一次 |
| 排序键 `rerankScore 降序 → 重排前原始位置升序`，`sort.SliceStable` | 宪法第 V 条：相同输入必须产出逐字相同的顺序 |
| rerank 分数**绝不**写入 `RetrievedChunk.Score` | 后者语义是"向量分与关键词分的较大值"。一旦被 rerank 分数覆盖，就会撞上对话层 `ragMinSimilarityScore = 0.2` 的分数线——量纲不同，可能把全部证据静默过滤掉 |
| 候选数 ≤1 / 开关关闭 / 未配模型 → 不发任何外部请求 | 一条候选时重排不可能改变顺序，不该为它付一次外部调用 |
| 改写快速路径：问题已自足 → 原样通过 | 纯函数判定，零外部调用。初版条件是"无历史且不含指代词"，只覆盖单轮完整提问；后续收紧为"不含指代词，且首轮或（长度与内容信号数达标）"，把多轮里问题本身已完整的那些轮次也纳入快速路径——改写补的是指代，问题自足时那次调用纯属浪费 |
| 单字指代词 `该/其/这/那` **不**进模式集合 | 中文没有词边界，`strings.Contains` 对单字必然假阳性：「我**应该**…」命中「该」、「和**其他**框架…」命中「其」。这些都是首轮就完整、不需要改写的问题，误判一次就白付一次 LLM 调用。同理 `它/他/她` 必须保留（"那**它**的上限呢"全靠它命中），但要先剥掉 `其他/其它/吉他` 这类把它们裹住的词 |
| 改写结果校验：非空 / ≤200 runes / ≤max(3×原长,60) runes / 与原问题至少共享一个内容信号 | 长度上限防"模型开始作答而不是改写"；相关性下限防跑题。相关性判断**刻意 fail open**——原问题剥掉指代词和虚词后没有任何信号时（"它呢"）放行，因为那恰恰是本功能存在的理由 |
| 指代不明时**不打断对话反问用户** | 与参考实现（research-agent 的 `query_rewriter`）的关键差异：那是研究型 Agent，可以停下来澄清；Hify 是对话产品，不能。歧义时静默退回原问题继续答 |

## 4. 降级矩阵（全部有测试覆盖）

| 触发条件 | 行为 |
|---|---|
| 改写/重排开关关闭 | 直接走上线前路径 |
| 快速路径命中（无历史且无指代词） | 用原问题，不调 LLM |
| 改写 LLM 失败/超时 | 用原问题继续检索，本轮正常回答 |
| 改写返回 `ambiguous=true` | 用原问题（不反问用户） |
| 改写结果校验不过 | 用原问题 |
| 候选数 ≤1 / 未配 rerank 模型 | 跳过重排，不发外部请求 |
| rerank 调用失败/超时 | 保持融合排序，本轮正常回答 |
| rerank 响应含未知/重复/缺失 index | **整体丢弃**，保持融合排序 |
| 两者同时失败 | 等价于本功能未启用 |

降级测试不是断言"没报错"：而是真的构造两个 service（一个重排关闭做基线、一个触发降级），
对同一份种子数据各跑一次 `Retrieve`，`reflect.DeepEqual` 比对整个 `[]RetrievedChunk`。
超时也没用 `time.Sleep` 硬等，而是用极小超时配合阻塞在 `<-ctx.Done()` 的假实现——
测的是超时配置在生产路径上真的生效，不是伪造一个错误值。

## 5. 可观测与隐私

- **`query_rewrite` 子 span**（新增 migration `000013` 放行 `trace_spans.kind` 的 CHECK 约束），
  attrs 只有 5 个 key：`rag.rewrite.{enabled,skipped,applied,degraded,duration_ms}`。
- **只在改写开启时才落这条 span**：关闭时 attrs 恒为同一组常量、不携带任何信息，
  而 `trace_spans` 是明确的百万行级增长表，为一个关着的功能每轮多写一行是纯写放大。
  开启后走快速路径仍然记录——"这轮没调 LLM"本身就是快速路径命中率的统计来源。
- **rerank 的 5 个字段并进既有的 admission/dedup 日志行**，触发条件放宽为
  "发生拒绝/去重/邻接去重 **或** 重排被应用/降级"。
- **隐私红线**：日志与 span 只记计数、时长、布尔状态，绝不记问题原文、改写结果、片段正文、
  逐条 rerank 分数。改写解析失败时**刻意不记 `err` 原文**——`encoding/json` 的语法错误会把
  模型输出的字符引出来（`invalid character '这' looking for...`）。
  span 降级时记 `StatusError` 但 `ErrorMessage` 留空，同样是这个理由。
- 隐私测试断言的是**不含具体内容**，而不是"字段数量正确"——后者挡不住有人往 `err` 里带出原文。

## 6. 效果证据，以及它的边界（本报告最重要的一节）

### 6.1 度量方式被迫更换

规格初稿写的是"在固定回归测试集（`make eval`）中相对基线提升 30 / 15 个百分点"。
实施过程中证明**这在本仓库的环境里无法度量**，三层原因，每一层都足以让指标失效：

1. **`make eval` 带 LLM 裁判**：每条用例都调真实对话模型 + 裁判模型，同一份代码跑两次都不会
   一致。用它证明"行为未变"是伪证据。
2. **语料规模**：评测知识库只有 **4 个 chunk、2 篇文档**，而 `retrievalTopK = 5`——
   每次检索都把整个语料全量返回，Hit@1 与 MRR 在结构上恒为 1.00，不存在可提升的余量。
   实测新加的三条"硬"省略式用例，在改写关闭时 `RetrievalHit` 全为 `true`、`MRR` 全为 `1.00`。
3. **嵌入是 mock**：知识库挂的是本地 mock server 的 32 维假嵌入。实测库内向量两两余弦在
   0.78–0.94 之间，且**语义相近的片段（两篇文档中都讲密码重置的段落，0.8515）反而低于毫不相干
   的片段（0.9352）**。向量分不携带任何语义信息。

第 3 层决定了"扩充语料"这条路也是死的——在非语义向量上放大语料只会让检索退化为随机。
接入真实嵌入/重排模型可以解决，但需要付费 API，**仓库所有者明确决定不为学习项目承担这项成本**。

顺带修复的一个环境问题：开发库的 PostgreSQL 迁移停在 version 3，`pg_trgm` 从未安装，
关键词检索每次调用都失败降级为纯向量——**Hybrid Search 在这台开发机上从来没真正双路跑过**。
代码没有错（best-effort 降级按设计工作，集成测试用的是每次全新迁移的测试库），
只有长期存在的开发库落在了 version 3。已修复至 version 4。

### 6.2 改用确定性门禁度量

改用 Phase 6 建立的确定性检索门禁：受控 seed 的向量 + 真实 pg_trgm + 隔离数据集，
零成本、完全可复现、可进 `go test` 当回归门禁。新增 3 个 case（门禁总数 9 → 12）：

**SC-001（成对的两个 case）**：同一个知识库、**同一条目标片段**、同一条流水线，
唯一的变量是送进 `Retrieve` 的 query 字符串——这正是查询改写在生产里做的唯一一件事。
阈值是量过的，不是拍脑袋：

```
word_similarity('它的上限呢',   目标正文) = 0
word_similarity('分块大小上限', 目标正文) = 0.5714286
keywordAdmissionThreshold                = 0.45
```

目标片段的向量与查询向量正交（假嵌入对任何 query 恒返回 x 轴单位向量），
`cos = 0 < vectorAdmissionThreshold = 0.35`——两条召回路都堵死，
目标片段除了靠"改写后的关键词"没有别的解释路径。

结果：`rewrite_before_elliptical_query_misses` 返回 0 条；
`rewrite_after_standalone_query_hits` 命中同一条 `grw-target`，rank 1。

**SC-002**：5 条候选靠正文长度差异拉开关键词相似度，目标稳定排在融合第 4；
case 内**先跑一次不重排的 baseline 断言目标确实不在 topK 里**（前置条件不成立就直接 fail，
避免"本来就在里面"的假通过），再用注入的固定打分把它提到第 1。
结果：`hits = [grr-target, grr-noise-1, grr-noise-2]`。

**变异验证**：把"改写前"那个 case 的 query 换成补全后的字符串，它立即以
`ResultCount = 1, want 0` 失败——断言是真会咬人的，不是永远绿。

### 6.3 必须如实承认的边界

> 上述证据证明的是**机制成立**：改写确实改变了检索结果，重排确实改变了顺序，
> 且两者在关闭时对既有行为零影响。
>
> 它**不是**真实世界的效果幅度。本阶段**没有**、也无法给出"多轮追问召回率提升了 N 个百分点"
> 这类结论——真实幅度需要真实嵌入模型和足够大的语料，本仓库当前两者都不具备。
>
> 任何对外材料（简历、面试话术、演示）都不得把门禁通过表述成真实效果提升。
> 若将来接入真实嵌入与重排模型，应另立任务重新度量。

## 7. 真实测试结果

```
$ go build ./... && go vet ./...
(无输出)

$ go test ./... -race -count=1
ok  hify/internal/conversation      4.028s
ok  hify/internal/eval              1.709s
ok  hify/internal/eval/retrieval    2.200s
ok  hify/internal/knowledge         6.574s
ok  hify/internal/mcp               4.052s
ok  hify/internal/provider          5.305s
ok  hify/internal/server/middleware 5.218s
ok  hify/internal/workflow          6.382s
(零 FAIL，零 SKIP)

$ make check-deps
check-deps: OK - no cross-layer or same-layer violations

$ make eval-retrieval-gate
case 数: 12 | pass: True
metrics: HitAt1=1.0  HitAt3=1.0  MRR=1.0  ContentUniqueRate=1.0

$ 迁移往返（开发库，真实执行）
migrate up   → MySQL 13 / PG 4
migrate down → MySQL 12，chk_provider_models_capability 与 chk_trace_spans_kind 均正确收紧
migrate up   → 恢复；sqlc generate 无 drift
```

**SC-003（关闭时零变化）**：US1、US2、US3 三次实施后各跑一次确定性门禁，输出除 `ran_at`
外与改动前**逐字节相同**。这是 `rrfFuse` 截断改造行为等价性的直接证据。

## 8. 顺带修复的既有缺陷

**`Temperature: 0` 被静默丢弃**（影响面不止本功能）。`ChatRequest.Temperature` 改成 `*float64`
仍然不够——go-openai v1.41.2 的 `ChatCompletionRequest.Temperature` 是 `float32` 且带
`omitempty`，显式 0 在 `encoding/json` 眼里与"未设置"无法区分，照样被整个省略。
调用方的意图在结构体边界就丢了，**Agent 自己配 `Temperature=0` 此前同样无效**。

改为在 transport 层按 ctx 标记把 `"temperature":0` 补回请求体字节。判断"顶层是否已有该键"
只解一层 `map[string]json.RawMessage`，**不能扫整个 body**——工具定义与聊天请求走同一个 JSON，
一个参数名恰好叫 `temperature` 的 MCP 工具（查天气的就是）会让整体扫描误判并悄悄退回缺陷本身，
且只在挂了这类工具的 Agent 上复现。补丁后同步替换 `GetBody`，避免重定向/HTTP2 重发时又发出
未打补丁的字节。

测试断言的是**打到 `httptest.Server` 上的真实字节**，不是 Go 结构体字段——只测结构体会全绿而
缺陷依旧存在。

## 8.5 冒烟测试抓到的缺陷：能力白名单其实有三处

任务书 T023 我写的是"`service.go` 与 `handler.go` 两处判断都要改"。**实际是三处**——
漏掉的是 `dto.go` 里 `addModelRequest.Capability` 的 gin binding 标签
`binding:"required,oneof=chat embedding"`。

这个标签在请求进到 handler 逻辑**之前**就生效，所以另外两处改得再对也没用：
`POST /api/v1/providers/:id/models` 传 `"capability":"rerank"` 直接返回 400，
**rerank 模型根本注册不进去**——而这是它唯一的注册入口（前端的能力下拉暂无该选项）。

值得记住的是：**全套单元测试和集成测试都是绿的**。它们直接调 `service`/`handler`，
绕过了 gin 的 binding 层。这个缺陷是 `/smoke-test` 用真实 HTTP 请求打出来的，
正是 `CLAUDE.md` 那句"项目没有自动化测试，`/smoke-test` 和真实 HTTP 请求验证是唯一的安全网"
的一个实例。

已补 `internal/provider/dto_binding_test.go`：只测 binding 标签本身（不连库、不构造 service），
遍历 `Capability*` 常量断言全部能通过、常见手误拼写（`reranker`/`Chat`/`embeddings`/空串）
全部被拒。以后加第四种能力时忘记同步标签，这组测试会在那一刻就红。

冒烟同时验证了配置校验在真实启动路径上的降级行为：
`HIFY_RAG_RERANK_ENABLED=true` 但没配模型 ID 时，打一条
`WARN config: ... disabling rerank` 然后正常启动，不让进程失败。

## 9. 未验证 / 剩余风险

1. **没有任何真实 rerank 服务被调用过**。`/rerank` 的 HTTP 契约（请求编码、响应解码、5 条校验）
   只用 `httptest.Server` 验证过形状，没有对过任何一家真实供应商（bge-reranker / Cohere / Jina）。
   接入真实服务时首先要验的就是这条链路。
2. **`make eval` 在本仓库是坏的**：`.env` 的值带引号，而 Makefile 的 `-include .env` 不剥引号，
   `strconv.Atoi("\"0\"")` 直接失败。本次是绕过去跑的，没有改 `.env` 也没有改 Makefile——
   `.env` 的注释说引号是给 shell `source` 用的，两边有冲突，需要单独决策。
3. **前端没有 rerank 模型的注册入口**：能力下拉暂未加 `rerank` 选项，只能走 API 注册。
   本次范围明确不含前端改动，这是已知可用性缺口。
4. **`UpdateModelInput` 没有 `Capability` 字段**：PUT 无法修改模型能力，rerank 或其他都一样。
   既有问题，与本功能无关，未动。
5. **快速路径的假阳性词表不追求穷尽**：`其他/其它/吉他` 是显式列出的"假朋友"，漏一个的代价
   只是首轮多付一次改写调用，不是功能错误。
6. **`migrate down` 是 MySQL 与 PostgreSQL 一起退一步**——单独回退某一个库的迁移时要留意，
   本次实施过程中就曾因此意外退掉刚补的 `pg_trgm` 索引（已恢复）。

## 10. 规格制品

完整的 spec-kit 制品在 `specs/001-rag-query-rerank/`：`spec.md`（含三次度量方式修正的记录）、
`research.md`（R1–R9 技术决策及被否决的替代方案）、`plan.md`（宪法关卡表、链路顺序、降级矩阵）、
`data-model.md`、`contracts/`（rerank HTTP 契约 + 内部接口破坏性变更）、`quickstart.md`、`tasks.md`。

项目宪法在 `.specify/memory/constitution.md`（本阶段实施中由 v1.0.0 修订至 v1.1.0）。
