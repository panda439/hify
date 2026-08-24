# Implementation Plan: RAG 查询优化与结果重排序

**Branch**: `001-rag-query-rerank` | **Date**: 2026-08-24 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/001-rag-query-rerank/spec.md`

## Summary

在现有 RAG 链路的两端各加一层：**入口**在 conversation 组装上下文时，把依赖上文的省略式提问
改写成可独立理解的检索问题；**出口**在 knowledge 把候选截断到 topK 之前，用 cross-encoder 式的
rerank 模型按"问题—片段"真实相关度重排候选。两者各自可开关、各自失败即降级到今天的行为，
且都不改动 `RetrievedChunk.Score` 语义、Citation 协议与 SSE 协议。

技术路径（详见 [research.md](./research.md)）：新增 `provider.CapabilityRerank` 第三类模型能力
（migration + `Client.Rerank` + `/rerank` HTTP 适配），改写复用 Agent 自身的 chat 模型并带
"无历史且无指代词就原样通过"的纯函数快速路径。

## Technical Context

**Language/Version**: Go 1.26.5（GOTOOLCHAIN 管理）+ React 19 / Vite / TS（本次前端不改动）

**Primary Dependencies**: Gin、`sashabaranov/go-openai`（chat/embedding，**rerank 端点不经过它**，
用 `net/http` 直连）、`hibiken/asynq`、`sony/gobreaker`（既有 `resilience.go`）

**Storage**: MySQL 8.x（`provider_models.capability` CHECK 约束扩容）+ PostgreSQL/pgvector（chunk，
本次无 schema 变化）+ Redis（本次无变化）

**Testing**: `go test ./... -race`、纯逻辑单测（快速路径判定/改写解析校验/重排排序）、
真实 PostgreSQL + fake provider 的 `Retrieve` 集成测试、`/smoke-test`、`make eval` 固定评测集

**Target Platform**: 单进程 Go 二进制（Linux/macOS），单实例部署

**Project Type**: 模块化单体 Web 服务（`internal/` 六层）

**Performance Goals**: 单轮叠加延迟 p95 ≤ 2s（SC-005）；快速路径命中率 ≥ 90%（SC-006）；
rerank 单次送入候选 ≤ 50 条

**Constraints**: 改写超时 1.5s、rerank 超时 1.5s，超时立即降级；两个开关关闭时输出与上线前逐字一致
（SC-003/FR-018）；日志与 trace 不得含问题原文、片段正文或逐条分数（FR-017）

**Scale/Scope**: 触及 4 个模块（config、provider、knowledge、conversation）+ 1 个 migration；
不触及前端、不触及 workflow/agent/mcp/auth/user

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| 原则 | 关卡 | 结论 |
|---|---|---|
| I 如实标注 AI 归属 | 本功能产出的 `docs/` 报告不得把实现描述成用户手写 | ✅ 报告模板沿用 Phase 1-8，只陈述技术事实 |
| II 规格先行 | spec → plan → tasks → 实现 | ✅ 本文件即 plan，`/speckit-tasks` 之后才动代码 |
| III 模块分层 | 新增依赖边是否合法 | ✅ conversation(4)→provider(1)、knowledge(2)→provider(1) 均为既有合法方向，无新增跨模块边、无同层依赖；仍需每步跑 `make check-deps` |
| IV 实现顺序固定 | migration→sqlc→模块内文件顺序 | ✅ `000012` migration 先行；provider 改动按 `model→service→dto→handler→wire` 推进 |
| V 确定性优先 | 排序可复现、判定可纯函数单测 | ✅ 重排排序键含"原始位置"确定性 tie-break；快速路径判定/改写解析/重排排序均为纯函数；LLM 抖动边界见 research.md R7 |
| V 确定性优先（续） | LLM 参与链路必须有降级/超时/开关 | ✅ FR-013/014/015 全部落到设计，见下文"降级矩阵" |
| VI 证据式验收 | 有真实命令输出才算完成 | ✅ 验收清单见 [quickstart.md](./quickstart.md)，数据库测试禁止 skip |
| VII 中文面向用户 | `apperr.Message` 中文 | ✅ 本功能不新增用户可见错误（全部静默降级），新增注释与文档为中文 |
| VIII 角色分离 | Claude 不执行 commit/push | ✅ 实现阶段只改文件与跑测试，提交由所有者决定 |
| IX 最小范围 | 不夹带无关改动 | ⚠️ 见下文"已知范围边界" |

**已知范围边界（非违规，但需明示）**：

1. `provider.Client` 接口新增 `Rerank` 方法，会让 4 个测试文件里的假 client 编译失败
   （`internal/{workflow,knowledge,eval,conversation}/*_test.go`）。补空实现属于本功能的必要连带改动，
   不算范围蔓延；但**除补方法外不得顺手修改这些测试的其他部分**。
2. 前端模型管理表单的 capability 下拉本次不加 `rerank` 选项（spec 明确"前端不新增界面"）。
   后果：rerank 模型只能通过 `POST /api/v1/providers/:id/models` 直接注册。这是**已知可用性缺口**，
   另立任务处理，不在本次实现。

## Project Structure

### Documentation (this feature)

```text
specs/001-rag-query-rerank/
├── plan.md              # 本文件
├── spec.md              # 需求规格（含 Clarifications）
├── research.md          # Phase 0：R1-R9 技术决策
├── data-model.md        # Phase 1：实体与字段
├── quickstart.md        # Phase 1：验收与运行指引
├── contracts/           # Phase 1：对外/对内契约
│   ├── rerank-http-api.md
│   └── internal-contracts.md
├── checklists/
│   └── requirements.md
└── tasks.md             # 由 /speckit-tasks 生成，本命令不创建
```

### Source Code (repository root)

```text
internal/db/migrations/
├── 000012_provider_rerank_capability.up.sql      # 新增：CHECK 约束扩为 (chat, embedding, rerank)
└── 000012_provider_rerank_capability.down.sql    # 新增：回退

internal/config/
└── config.go                                     # 修改：新增 6 个 RAG 配置项（见 data-model.md）

internal/provider/
├── model.go                                      # 修改：CapabilityRerank 常量
├── llm.go                                        # 修改：RerankRequest/RerankResult + Client 接口加 Rerank
├── openai_compat.go                              # 修改：POST {base}/rerank 的 net/http 实现
├── resilience.go                                 # 修改：resilientClient.Rerank 包一层熔断/重试
├── service.go / handler.go                       # 修改：能力白名单放行 rerank
└── rerank_test.go                                # 新增：请求编码/响应解码/错误分类纯逻辑测试

internal/knowledge/
├── rerank.go                                     # 新增：applyRerank 纯函数 + 响应校验（FR-011）
├── rerank_test.go                                # 新增：纯逻辑测试
├── hybrid.go                                     # 修改：rrfFuse 不再截断 topK，返回完整已准入去重列表
├── service.go                                    # 修改：Retrieve 插入重排步骤 + 截断 + 日志字段扩展
└── wire.go                                       # 修改：NewService 注入 rerank 配置与打分函数

internal/conversation/
├── queryrewrite.go                               # 新增：快速路径判定、提示词、解析、校验、降级
├── queryrewrite_test.go                          # 新增：纯逻辑测试
├── context.go                                    # 修改：Retrieve 之前先改写 + query_rewrite span
└── wire.go                                       # 修改：NewService 注入改写配置

cmd/hify/ (buildApp)                              # 修改：把新配置传进 knowledge/conversation

eval/testset.yaml                                 # 修改：新增多轮省略式追问用例
docs/eval-phase9-query-rerank-report.md           # 新增：阶段报告
README.md / docs/critical-paths.md                # 修改：链路顺序与配置说明
```

**Structure Decision**: 沿用既有模块化单体结构，不新建模块。新增逻辑按"纯函数放独立文件、
网络调用放 service/adapter"的既有惯例落位（与 `admission.go`/`dedup.go` 同构），
保证 FR-005/FR-009/FR-011 的判定逻辑全部可零依赖单测。

## 检索链路的新顺序（本功能的核心契约）

```text
用户消息
  └─ [conversation] 快速路径判定 ──命中──> 原问题
        └─未命中─> 改写 LLM ──失败/超时/歧义──> 原问题（降级）
                      └─成功─> 独立问题
  └─ knowledge.Retrieve(检索问题)
        ├─ 向量路召回 (candidateK)
        ├─ 关键词路召回 (candidateK)
        ├─ RRF 融合排序
        ├─ 来源感知准入（Phase 8，阈值不变）
        ├─ 内容去重（Phase 5）
        ├─ ★ 重排序（前 50 条）──失败/超时/校验不过──> 保持原顺序（降级）
        ├─ topK 截断
        └─ 邻接批量查询 + 二次去重
  └─ [conversation] selectEvidence（0.2 分数线与预算策略不变）
```

**不变量**：`RetrievedChunk.Score` 始终是"向量分与关键词分的较大值"，邻接块继承核心块的 Score；
rerank 分数只存在于 `Retrieve` 内部，不进入 `RetrievedChunk`、不进入 Citation、不落库、不进 SSE。

## 降级矩阵

| 触发条件 | 行为 | 记录 |
|---|---|---|
| 改写开关关闭 | 直接用原问题 | `rewrite.skipped=true`（配置态） |
| 快速路径命中（无历史且无指代词） | 直接用原问题，不调 LLM | `rewrite.skipped=true` |
| 改写 LLM 失败/超时（1.5s） | 用原问题继续检索，本轮正常回答 | `rewrite.degraded=true` + `slog.Warn` |
| 改写返回 `ambiguous=true` | 用原问题（**不打断对话追问**，FR-003） | `rewrite.applied=false` |
| 改写结果校验不过（空/超长/疑似作答） | 用原问题 | `rewrite.degraded=true` |
| 重排开关关闭 / 未配 rerank 模型 | 保持融合排序 | `rerank_enabled=false` |
| 候选数 ≤ 1 | 跳过重排，不发外部请求 | `rerank_applied=false` |
| rerank 调用失败/超时（1.5s） | 保持融合排序，本轮正常回答 | `rerank_degraded=true` + `slog.Warn` |
| rerank 响应含未知/重复/缺失 index | **整体丢弃**，保持融合排序（禁止部分采用，FR-011） | `rerank_degraded=true` + `slog.Warn` |
| 两者同时失败 | 等价于本功能未启用，输出与上线前一致 | 两条降级记录 |

## Complexity Tracking

> 仅在 Constitution Check 出现需要辩护的偏离时填写。

| Violation | Why Needed | Simpler Alternative Rejected Because |
|-----------|------------|-------------------------------------|
| 无 | — | — |

Constitution Check 全部通过，"已知范围边界"两项是明示的连带改动与已知缺口，不构成对宪法条款的偏离。

## Post-Design Constitution Re-Check

Phase 1 设计（data-model / contracts / quickstart）完成后复查：

- 第 III 条：设计未引入任何新的跨模块依赖边，`knowledge` 仍不依赖 `platform/trace`（见 research.md R8）。✅
- 第 V 条：三处判定逻辑（快速路径、改写校验、重排排序与响应校验）在设计中全部为纯函数，
  排序含确定性 tie-break，SC-007 用假 client 可验证。✅
- 第 IX 条：contracts 未扩大对外 API 面——除 `capability` 枚举多一个合法值外，
  没有新增任何 HTTP 端点、没有改动任何响应体结构。✅
