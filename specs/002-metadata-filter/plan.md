# Implementation Plan: 检索元数据过滤（Metadata Filtering）

**Branch**: `002-metadata-filter` | **Date**: 2026-09-02 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/002-metadata-filter/spec.md`（已过两轮澄清）

## Summary

给 `knowledge.Service.Retrieve` 增加一个**可选、可关闭、默认关闭**的范围限定入参，
支持按 `document_id` 集合与 PDF 页码范围缩小候选来源，且该限定**下推到向量路与关键词路的
召回 SQL**，而不是召回之后在 Go 里筛。过滤是布尔的范围缩小，不参与打分——
`RetrievedChunk.Score` 语义、RRF 融合、Phase 8 准入、Phase 5 去重、Citation 协议全部不变。

**本期不新增任何 migration**：过滤需要的两个列（`document_id`、`page_number`）已经在 `chunks` 表上，
`page_number` 的产出链路（`parse.go` → `chunkPDFPages` → `Chunk.PageNumber` → 落库）也已经完整存在。
起草时"页码在切块时被丢弃"的判断经核查是错的，spec 已更正（见其 Session 2026-09-02）。
因此本功能的实质是**把两个已有的列变成可下推的过滤维度**，不是新建元数据体系。

技术路径详见 [research.md](./research.md)：sqlc 静态 SQL + 可空参数恒真短路谓词（R1）、
NULL 页码靠三值逻辑天然排除（R2）、邻接豁免零代码改动（R3）、
开关关闭时非空过滤器明确报错而非静默忽略（R4）。

## Technical Context

**Language/Version**: Go 1.26.5（GOTOOLCHAIN 管理）+ React 19 / Vite / TS（**本次前端零改动**）

**Primary Dependencies**: Gin、sqlc（PG 查询代码生成）、`lib/pq`（`::text[]` 参数经 `pq.Array`）、
pgvector、pg_trgm。**本期不新增任何依赖**

**Storage**: PostgreSQL/pgvector（`chunks`，**无 schema 变化**）+ MySQL（`documents`，**不参与本期**，
因为两个过滤维度都在 chunks 表上，见 research.md R5）+ Redis（无变化）

**Testing**: `go test ./... -race`、纯逻辑单测（过滤器判空/校验）、
真实 PostgreSQL 的 `Retrieve` 集成测试（**禁止 skip**，宪法第 VI 条）、
确定性检索门禁 `make eval-retrieval-gate`（新增用例 + 空过滤器逐字回归）

**Target Platform**: 单进程 Go 二进制（Linux/macOS），单实例部署

**Performance Goals**: 无新增延迟目标——过滤只减少参与打分的行，两路召回只会更快；
不新增数据库往返（邻接查询不变，无跨库查询）

**Constraints**: 空过滤器/开关关闭时输出与上线前**逐字一致**（FR-013/SC-003）；
过滤必须在召回 SQL 内生效（FR-007/SC-004）；候选不足不得放宽（FR-009）；
邻接块豁免 chunk 级过滤（FR-011）；日志不得含过滤条件取值（FR-018）

**Scale/Scope**: 触及 3 个模块（config、knowledge、以及 conversation/workflow 各一行调用点适配）
+ 1 个 sqlc 查询文件；**0 个 migration**；不触及前端、agent、mcp、auth、user、provider

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| 原则 | 关卡 | 结论 |
|---|---|---|
| I 如实标注 AI 归属 | `docs/` 报告不得描述成用户手写 | ✅ 报告沿用 Phase 1-9 体例，只陈述技术事实 |
| II 规格先行 | spec → clarify → plan → tasks → 实现 | ✅ 两轮澄清已完成并回写 spec；本文件即 plan |
| III 模块分层 | 是否新增跨模块依赖边 | ✅ **零新增依赖边**。knowledge(2) 内部改动 + config(0) 新增字段；conversation(4)→knowledge(2)、workflow(5)→knowledge(2) 都是既有合法方向，只是签名多一个参数。每步跑 `make check-deps` |
| IV 实现顺序固定 | migration→sqlc→模块内顺序 | ✅ 无 migration，从 `sqlc` 起步；knowledge 内按 `model→errors→repository→service` 推进（本期 `dto/handler/wire` 无改动，见下方范围边界） |
| V 确定性优先 | 排序可复现、判定可纯函数单测 | ✅ 过滤不参与排序，两条召回 SQL 的 `ORDER BY ... id ASC` 兜底不变；`IsEmpty`/`Validate` 是纯函数并被单测覆盖 |
| V 确定性优先（续） | 不引入未固定的随机源 | ✅ 本期不引入任何 LLM 调用（spec 已把"LLM 抽取过滤条件"排除出范围） |
| VI 证据式验收 | 有真实命令输出才算完成 | ✅ 验收清单见 [quickstart.md](./quickstart.md)；数据库测试禁止 skip；基线已在改动前跑过并归档 |
| VII 按读者选择语言 | 用户文案中文、注释跟随文件 | ✅ 3 个新错误 Message 中文；`chunks.sql`/`repository.go` 既有中文注释延续中文，`hybrid.go`/`neighbor.go` 若需补注释延续英文 |
| VIII 提交时机归所有者 | 不擅自 commit/push | ✅ 实现阶段只改文件与跑测试 |
| IX 最小范围 | 不夹带无关改动 | ⚠️ 见下方"已知范围边界" |

**已知范围边界（非违规，但需明示）**：

1. **`Service.Retrieve` 签名变更会波及测试替身**。`conversation`、`workflow`、`eval` 下的假
   knowledge service 需要跟着改签名。补签名属于必要连带改动，
   但**除补签名外不得顺手修改这些测试的其他部分**。
2. **本期不提供任何设置过滤器的入口**。没有 HTTP 参数、没有前端、没有 Agent 配置项——
   `dto.go`/`handler.go`/`wire.go` 因此都不改动。这是 spec 明确的 Out of Scope
   （"本期只提供后端能力与契约"）。**后果是这个功能上线后无人可用**，
   它的价值兑现依赖下一期（过滤条件来源）。这是**已知的可用性缺口**，
   在报告里必须如实写明，不能包装成"能力已交付"。
3. **`chunks.sql` 里 `CreateChunk` 的一条过期注释**（声称 page_number 一律传 NULL）本期顺带更正。
   这是范围内的必要澄清——本功能的正确性论证直接依赖"page_number 确实有值"这一事实，
   留着一条断言相反的注释会让后来者对整个设计产生合理怀疑。只改注释文字，不改 SQL 语义。

## Project Structure

### Documentation (this feature)

```
specs/002-metadata-filter/
├── spec.md              # 已完成（两轮澄清后 Status: Clarified）
├── plan.md              # 本文件
├── research.md          # Phase 0：R1-R8 技术判断与被否决方案
├── data-model.md        # Phase 1：类型/SQL/配置/可观测字段
├── contracts/
│   └── internal-contracts.md   # 模块内与跨模块契约（本期无 HTTP 契约）
├── quickstart.md        # 验收步骤
└── tasks.md             # /speckit-tasks 产出
```

本期 `contracts/` 只有一份内部契约文件，**没有 HTTP API 契约**——
因为本期不新增任何 HTTP 端点或参数（见范围边界 2）。

### Source Code (repository root)

```
internal/
├── config/config.go                 # + RAGMetadataFilterEnabled
├── db/
│   ├── pgqueries/chunks.sql         # 两条召回查询各加三行谓词；CreateChunk 注释更正
│   └── gen/                         # sqlc 重新生成（不手改）
├── knowledge/
│   ├── model.go                     # + RetrieveFilter / RetrieveOptions / maxFilterDocumentIDs
│   ├── errors.go                    # + 3 个中文错误
│   ├── repository.go                # searchVectorChunks / searchKeywordChunks 增加 filter 参数
│   ├── service.go                   # Retrieve 签名 + 校验 + 开关 + 两路透传 + 日志字段
│   ├── filter_test.go               # 新增：纯函数单测
│   ├── integration_test.go          # 新增：真实 PG 的下推/豁免/NULL 页码用例
│   └── eval_gate_test.go            # 新增门禁用例 + 空过滤器逐字回归
├── conversation/context.go          # 调用点补 RetrieveOptions{}
└── workflow/executor.go             # 调用点补 RetrieveOptions{}

docs/eval-phase10-metadata-filter-report.md   # 阶段报告
```

**不改动**：前端全部、`agent`、`mcp`、`auth`、`user`、`provider`、
`knowledge/{dto,handler,wire,parse,chunk,hybrid,admission,dedup,neighbor,rerank}.go`。
`chunk.go`/`parse.go` 不改是本期的一条关键结论——页码产出链路已经正确，
本期只为它补回归断言（FR-001 修订）。

## 检索链路：过滤插在哪一步（本功能的核心契约）

```
Retrieve(kbIDs, query, topK, opts)
  │
  ├─ 0. 校验 opts.Filter（上限/页码范围/开关）        ← 新增，失败即返回错误，不做任何检索
  │
  ├─ 1. 向量路   SearchVectorChunks(... + filter)     ← 过滤在这里下推
  ├─ 1. 关键词路 SearchKeywordChunks(... + filter)    ← 过滤在这里下推
  │
  ├─ 2. rrfFuse：RRF 融合 → Phase 8 准入 → Phase 5 内容去重   ← 完全不变
  ├─ 3. Phase 9 重排                                          ← 完全不变
  ├─ 4. topK 截断                                             ← 完全不变
  └─ 5. 邻接窗口批量查询                                       ← 完全不变（文档级结构性满足，页码级豁免）
```

**第 2 步及之后一行代码都不改**，这是 FR-012（过滤不参与打分）在实现层面的直接体现，
也是"过滤只缩小候选来源"这句话的可验证含义。

## 降级与失败矩阵

| 情形 | 行为 | 依据 |
|---|---|---|
| 空过滤器 + 开关关闭 | 与上线前**逐字一致** | FR-013 / SC-003 |
| 空过滤器 + 开关开启 | 与上线前**逐字一致**（三条谓词恒真） | FR-006 / research.md R1 |
| 非空过滤器 + 开关关闭 | 返回 `ErrMetadataFilterDisabled`，**不检索** | research.md R4（FR-009 优先于静默降级） |
| 过滤器超限/页码非法 | 返回中文错误，**不截断**、**不检索** | FR-015 |
| 过滤后候选不足 | 照常融合、照常走准入阈值，**不放宽过滤** | FR-009 / Edge Cases |
| 过滤后零候选 | 返回空结果 + `filter_zero_candidates=true` | FR-017 / US4 |
| document_id 不存在/不属于该知识库 | 无匹配（空结果），**不报错** | FR-010 |
| 页码过滤遇到 NULL 页码 chunk | 不匹配（三值逻辑天然排除） | FR-014 修订 / research.md R2 |

## Complexity Tracking

| 偏离 | 为什么需要 | 更简单的方案为何被否决 |
|---|---|---|
| FR-017 的"过滤**前**候选数量"不记录 | 拿到它必须把两路召回各跑两遍（带过滤 + 不带过滤） | 那正是 FR-007 禁止的"先召回再过滤"形态的成本。US4 要区分的两件事（过滤没生效 vs 过滤生效但没答案）用 `filter_applied` + 过滤后各路计数 + `filter_zero_candidates` 已能回答。详见 data-model.md §5 |
| `Service.Retrieve` 签名变更波及 3 个模块的测试替身 | 新增 `RetrieveWithFilter` 方法可以零波及 | 但那会在 Service 接口上留下两个语义重叠的检索入口，长期看是分叉风险——澄清时所有者已选定 options 结构方案 |

## Post-Design Constitution Re-Check

设计完成后重新对照，结论不变：

- **第 III 条**：零新增依赖边，`make check-deps` 在每个任务后跑。
- **第 IV 条**：无 migration，`sqlc` 起步，模块内 `model → errors → repository → service` 顺序不变。
- **第 V 条**：过滤不参与排序；`IsEmpty`/`Validate` 为纯函数并有单测；无 LLM 参与。
- **第 VI 条**：改动前基线已跑（`go vet` 干净、`check-deps` OK、`go test ./... -race` 全绿、
  门禁 12/12 通过并归档 JSON），改动后逐项复跑并比对。
- **第 IX 条**：三条范围边界已在上方明示，其中"本期无人可用"这一条必须进报告。
