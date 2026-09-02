# Implementation Plan: Agent 文档范围绑定

**Branch**: `004-agent-document-scope` | **Date**: 2026-09-02 | **Spec**: [spec.md](./spec.md)

## Summary

新增 `agent_documents` 关联表与配套的 CRUD，让 Agent 能绑定「知识库内的某几份文档」；
`conversation` 组装上下文时把它作为 `RetrieveOptions.Filter.DocumentIDs` 传给 `Retrieve`。
这是 002 的过滤能力第一次进入真实对话链路。

**唯一的行为变更**：`HIFY_RAG_METADATA_FILTER_ENABLED` 默认值 `false` → `true`。
理由与安全性论证见 spec 的「唯一的行为变更」一节。

## Technical Context

**Storage**: MySQL 新增 `agent_documents`（migration `000014`）。PostgreSQL 无变化。

**Testing**: `go test ./... -race`、真实 MySQL 的 agent CRUD 集成测试、
真实 PG+MySQL 的 conversation 端到端检索测试、`make eval-retrieval-gate` 回归、
`make eval` 的确定性指标比对、`/smoke-test`、浏览器验证

**Scale/Scope**: `internal/agent`（全层）、`internal/conversation`（一处调用点）、
`internal/config`（一个默认值）、`web/src`（Agent 表单）。
**不改 `internal/knowledge` 一行**——002 的能力原样复用。

## Constitution Check

| 原则 | 结论 |
|---|---|
| III 模块分层 | ⚠️ **需要论证**，见下方「分层：agent 要不要依赖 knowledge」 |
| IV 实现顺序 | ✅ `migration → sqlc → model → errors → repository → service → dto → handler → wire → buildApp → 前端` |
| V 确定性优先 | ✅ 不新增排序/打分逻辑；无 LLM 参与 |
| VI 证据式验收 | ✅ 数据库测试禁止 skip；开关默认值变更必须有门禁逐字段比对背书 |
| VII 按读者选择语言 | ✅ 新错误 Message 中文；注释跟随所在文件既有语言 |
| IX 最小范围 | ⚠️ 见下方范围边界 |

### 分层：agent 要不要依赖 knowledge

**结论：合法，且依赖边已经存在。**

`agent` 在第 3 层，`knowledge` 在第 2 层，`agent → knowledge` 是自上而下的合法方向。
而且这条边**不是本期新增的**——`agent.Service` 早就依赖 `knowledge.Service`
来校验 `knowledge_base_id` 合法性（见 `internal/CLAUDE.md` 里 Phase 3 把 knowledge
下沉一层的那段论证）。本期只是多调一个方法（按知识库列文档，用于 FR-004 的归属校验）。

`make check-deps` 每步必跑。

### 范围边界

1. **改了一个配置默认值**（FR-007）。这是行为变更，不是范围蔓延——
   不改它就会有静默降级路径（见 spec）。已在 spec 单列一节说明。
2. **`knowledge.Service` 可能需要新增一个方法**：按一组知识库 ID 批量列出文档，
   供 agent 做归属校验。若既有 `ListDocuments(kbID, ...)` 够用则不新增。
   优先复用，不够用时新增的方法只做这一件事。
3. **前端 Agent 表单要改**。这是 FR-009 要求的，不是顺手改。

## 关键实现顺序

```
1. migration 000014_agent_documents        ← 建表
2. queries/agents.sql + make sqlc          ← 生成代码
3. agent: model → errors → repository → service → dto → handler   ← 模块内固定顺序
4. config: 默认值 false → true             ← 行为变更，单独一步以便回滚
5. conversation/context.go: 传 Agent 范围  ← 过滤真正生效的那一行
6. 前端 Agent 表单
```

第 4 步和第 5 步**必须相邻且一起验证**：只做 4 不做 5 是无意义的默认值变更；
只做 5 不做 4 会让配了范围的 Agent 拿到 `ErrMetadataFilterDisabled`。

## Complexity Tracking

| 偏离 | 理由 |
|---|---|
| 改配置默认值 | 见 spec「唯一的行为变更」；不改则存在静默降级路径，与 002 的核心设计冲突 |
| 文档范围是 Agent 级全局列表而非按知识库分组 | 见 spec Clarifications；按库分组需要改 002 的 `RetrieveFilter` 与 SQL，收益只在当前不存在的场景里 |
