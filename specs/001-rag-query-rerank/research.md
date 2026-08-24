# Phase 0 研究：RAG 查询优化与结果重排序

**Feature**: `001-rag-query-rerank` | **Date**: 2026-08-24

本文件解决 `plan.md` Technical Context 中标记为待定的问题，每条给出决定、理由和被否决的替代方案。

## R1. 重排序打分接口用什么协议

**Decision**: 走 `POST {provider.base_url}/rerank`，请求
`{"model", "query", "documents": [...], "top_n", "return_documents": false}`，
响应 `{"results": [{"index", "relevance_score"}, ...]}`。用 `net/http` 直接实现，不走 `go-openai` SDK。

**Rationale**: 这套请求/响应形状是当前重排序服务事实上的公共子集——Jina Reranker、SiliconFlow、
Cohere v2、TEI（text-embeddings-inference）、Xinference、vLLM 的 rerank 端点都兼容它，
且它们的 `base_url` 与 chat/embedding 共用同一个前缀。`sashabaranov/go-openai` 没有 rerank 端点，
不存在"复用 SDK"的选项。响应只回 `index + relevance_score`，
天然满足 FR-011 的"结果必须与送入的候选集合一一对应"校验需求。

**Alternatives considered**:
- 用 Cohere 官方 SDK：绑死单一供应商，与 Hify"OpenAI 兼容 + 自填 base_url"的供应商模型冲突。
- 让重排也走 chat 端点（LLM listwise 重排）：不需要新协议，但延迟和成本高一个数量级，
  输出需要额外解析且不稳定；已在 `/speckit-clarify` 中被否决。
- 走 embedding 端点自己算相似度：那只是重算了向量路已有的信号，不构成 cross-encoder，无法解决 US2。

## R2. `rerank` 作为第三类模型能力如何落地

**Decision**: `provider_models.capability` 的 CHECK 约束从 `('chat','embedding')` 扩为
`('chat','embedding','rerank')`（新 migration `000012`），新增常量 `provider.CapabilityRerank`，
并在 `provider.Service.AddModel` / `handler` 的能力白名单里放行。`provider.Client` 接口增加
`Rerank(ctx, RerankRequest) (RerankResult, error)`。

**Rationale**: 与 `embedding` 的既有先例完全同构——`knowledge` 已经通过
`ListModelsByCapability(embedding)` + `GetModel` 校验的方式消费 embedding 模型，
rerank 沿用同一条路径，不需要发明新机制。MySQL 8 支持
`ALTER TABLE ... DROP CHECK / ADD CHECK`，down migration 可原样回退（回退前需确认没有 rerank 行）。

**影响面（必须一并处理）**：`Client` 是接口，加方法会让所有实现失效。生产实现只有
`openAICompatClient` 与 `resilientClient` 两个；测试假实现分布在
`internal/{workflow,knowledge,eval,conversation}/*_test.go` 共 4 处，需同步补一个空实现。

**Alternatives considered**:
- 不入库，rerank 模型走环境变量直配：省一次 migration，但供应商密钥、base_url、熔断/重试
  全部要另起一套，与 `provider` 模块重复；否决。
- 复用 `capability='embedding'` 打标签区分：破坏 `knowledge.validateEmbeddingModel` 的语义，
  会让 rerank 模型出现在知识库的嵌入模型下拉里；否决。

## R3. 查询改写放在 conversation，模型从哪来

**Decision**: 改写用 **当前 Agent 自己的 chat 模型**，通过已有的
`providerSvc.ResolveClient(...)` + `Chat(...)` 调用；配置项 `HIFY_RAG_QUERY_REWRITE_MODEL_ID`
可选覆盖（留空即用 Agent 模型）。改写代码放 `internal/conversation/queryrewrite.go`。

**Rationale**: `assembleContext` 手上已经有 `ag`（Agent）和 `model`，零额外配置即可跑通；
覆盖项留给"想用一个便宜小模型做改写"的部署。分层上 conversation（第4层）依赖 provider（第1层）
是既有合法方向，不新增任何依赖边。

**Alternatives considered**:
- 扩展 `knowledge.Service.Retrieve` 签名让它接收历史：`knowledge` 目前只用 embedding 模型，
  要为它引入 chat 模型和改写职责，且改动公开契约；已在 `/speckit-clarify` 中被否决。
- 强制单独配一个改写模型：多一个必填配置，部署门槛变高；改为可选覆盖。

## R4. 如何避免"每轮都多花一次 LLM 调用"

**Decision**: 移植 research-agent `nodes/query_rewriter.py::_should_skip_rewrite` 的快速路径思路，
在 Go 侧实现为纯函数 `shouldSkipRewrite(query string, hasHistory bool) bool`：
无历史 **且** 问题中不含指代词模式 **且** 问题长度达到最小实词阈值时，原样通过、不调用 LLM。
指代词模式为中文常见指代集合（它/它们/他/她/这个/那个/上述/上面/前者/后者/该/其/这/那）加英文
`it/its/they/them/this/that/these/those`。

**Rationale**: 这条路径覆盖单轮完整提问，是最高频的场景（SC-006 要求 ≥90% 命中），
且是纯函数，可零依赖单测。research-agent 的实测经验表明它能把改写调用量压到很低。

**与 research-agent 的关键差异**：research-agent 在指代不明时会**打断并向用户澄清**
（`needs_clarification`）；Hify 是对话产品，FR-003 明确要求**不打断**——
指代不明时静默退回原问题继续检索并照常回答。因此 `needs_clarification` 在 Hify 的语义是
"放弃改写"，不是"反问用户"。

## R5. 改写输出如何解析与校验

**Decision**: 提示词要求模型只输出一个 JSON 对象
`{"standalone_question": "...", "ambiguous": true|false}`；解析用 `encoding/json`，
容忍 ```` ```json ```` 围栏与前后空白。校验（任一不过即退回原问题）：
`ambiguous == true`；`standalone_question` 去空白后为空；长度 > 200 runes；
长度 > `max(3 × 原问题长度, 60)` runes（防止模型开始作答而不是改写）。
解析 + 校验实现为纯函数 `parseRewriteResult` / `validateRewrite`，不依赖网络。

**Rationale**: Hify 的 `provider.ChatRequest` 没有 structured-output/JSON-mode 抽象
（research-agent 靠 LangChain 的 `with_structured_output`），最小可靠做法就是"提示 + 宽容解析 + 严格校验"。
把不可信输出挡在纯函数里，是 FR-005 与宪法第 V 条（判定逻辑可纯函数单测）的直接落点。

**Alternatives considered**:
- 给 `provider.ChatRequest` 加 `response_format` 支持：范围扩张，且不是所有 OpenAI 兼容供应商都实现；否决（另立任务）。
- 让模型直接输出改写后的裸文本：无法表达"指代不明、放弃改写"，也难以区分"模型开始作答"；否决。

## R6. 重排在检索链路中的插入点

**Decision**: 固定顺序为
`两路召回 → RRF 融合排序 → 来源感知准入 → 内容去重 → **重排序** → topK 截断 → 邻接批量查询`。
`rrfFuse` 不再自己做 topK 截断，改为返回"已准入、已去重的完整有界候选列表"，
截断交给 `Retrieve` 在重排之后执行。

**Rationale**: 与 Phase 8 准入层同样的论证——放在截断之后就没有意义（被截掉的候选已经没机会翻身，
US2 的验收场景 1 直接失败）；放在邻接查询之后则会为被淘汰的候选白白查一次邻接（FR-012）。
放在准入与去重之后，保证送进 rerank 的都是合格且无重复的候选，既省 token 又避免重复内容互相挤占。

**送入上限**：`rerankInputLimit = 50`。候选集上界是 `2 × candidateK ≤ 200`，
全量送 rerank 的延迟和成本不可接受；只对融合排名前 50 个重排，其余保持原相对顺序排在其后。

**Alternatives considered**:
- 在准入之前重排：低质量候选会消耗 rerank 配额，且准入门槛是按召回来源的原始信号定义的，
  与 rerank 分数不同量纲，顺序反过来会让 Phase 8 的语义失效；否决。
- 用 rerank 分数替代准入门槛：属于改动既有阈值语义，spec 的 Assumptions 已明确本次不改；否决。

## R7. 确定性如何保证与验证

**Decision**: 重排后的排序键为 `rerankScore 降序 → 重排前的原始位置升序`，用 `sort.SliceStable`
在"原始位置"上稳定化；`RetrievedChunk.Score` 不被写入 rerank 分数（FR-008），
rerank 分数只存在于 `Retrieve` 内部的一个未导出切片里。SC-007 的 20 次重复一致性测试
用**固定打分的假 client** 验证。

**Rationale**: 宪法第 V 条要求的是"相同输入产出相同顺序"。外部 rerank 服务本身可能有微小抖动，
这不在 Hify 的控制范围内，也不是本条要约束的对象——要约束的是"给定同一组分数，
Hify 的排序结果必须逐字相同"。因此确定性测试必须打在假 client 上，
真实模型的抖动通过 `temperature=0`（改写）与固定 `top_n`（重排）尽量压低，并在报告中如实说明边界。

## R8. 可观测落点

**Decision**:
- 查询改写：在 conversation 侧新增一个 `query_rewrite` 子 span（沿用 `trace.Store`，
  与 `retrieval`/`llm_call` 同级），attrs 只放 `rewrite.skipped`（是否走快速路径）、
  `rewrite.applied`、`rewrite.degraded`、`rewrite.duration_ms`。
- 重排序：**不**引入 trace 依赖到 `knowledge`（该模块目前不依赖 `platform/trace`，
  为一个统计项加一条依赖不划算），沿用 Phase 8 已有的 `slog.Debug`
  "retrieval candidate admission and dedup" 结构化行，扩展
  `rerank_enabled` / `rerank_applied` / `rerank_degraded` / `rerank_input_count` /
  `rerank_duration_ms` 五个字段。

**Rationale**: FR-016 要求"能看出发生了什么"，没要求两者都进 trace 表。改写发生在 conversation，
那里本来就在写 span，顺手；重排发生在 knowledge，那里本来就在打结构化日志，也顺手。
两边都严格遵守 FR-017：只记计数、时长、布尔状态，绝不记问题原文、片段正文或逐条分数。

## R9. 效果如何度量

**Decision**: 扩展 `eval/testset.yaml`，新增一组"多轮省略式追问"用例（每条含前置轮次与省略式追问），
跑 `make eval` 得到 SC-001/SC-002 的前后对比；基线取本功能实现前的一次完整评测结果，
产出 `docs/eval-phase9-query-rerank-report.md`，与 Phase 1-8 的报告格式保持一致。

**Rationale**: 项目已有的固定评测集 + 本地 JSON 结果文件就是既定的 RAG 回归工具，
不需要引入新的评测框架。SC-003（关闭时逐字一致）用同一套评测集在"双开关关闭"下跑一次比对即可。
