# Implementation Plan: 知识库试检索面板

**Branch**: `003-retrieval-playground` | **Date**: 2026-09-02 | **Spec**: [spec.md](./spec.md)

## Summary

给 002-metadata-filter 的过滤能力接上第一个真实调用方：知识库页的「试检索」面板。
新增一个 `POST /api/v1/knowledge-bases/:id/retrieve` 端点，直接复用
`knowledge.Service.Retrieve` 与 `RetrieveOptions`；前端在知识库列表页加一个对话框，
支持勾选文档、填页码范围、看召回结果。

**定位是检索调试工具，不是对话功能**——不调对话模型、不建会话、不写 trace。

## Technical Context

**Language/Version**: Go 1.26.5 + React 19 / Vite / TS / Tailwind / shadcn-ui

**Storage**: 无持久化。**本期不新增 migration。**

**Testing**: `go test ./... -race`、真实 PostgreSQL 的 handler/service 集成测试、
002 的 `make eval-retrieval-gate` 回归、`/smoke-test`、真实浏览器点一遍

**Scale/Scope**: 触及 `internal/knowledge`（dto/handler/wire）+ `web/src`（lib + 一个新对话框）。
不触及 conversation / workflow / agent / provider / config，**不改 `service.go` 的检索逻辑**。

## Constitution Check

| 原则 | 结论 |
|---|---|
| I 如实标注 AI 归属 | ✅ 报告只陈述技术事实 |
| II 规格先行 | ✅ spec → plan → tasks → 实现 |
| III 模块分层 | ✅ **零新增依赖边**，knowledge(2) 模块内部改动；每步跑 `make check-deps` |
| IV 实现顺序固定 | ✅ 无 migration；模块内按 `dto.go → handler.go → wire.go` 推进（`model/errors/repository/service` 本期**不改**），再做前端 |
| V 确定性优先 | ✅ 不新增任何排序/打分逻辑，复用既有 Retrieve；无 LLM 参与 |
| VI 证据式验收 | ✅ 见 [quickstart.md](./quickstart.md)，数据库测试禁止 skip |
| VII 按读者选择语言 | ✅ 新错误 Message 中文、前端文案中文；`handler.go`/`dto.go` 既有注释为英文，新增注释跟随该文件 |
| VIII 提交时机归所有者 | ✅ 实现完等所有者决定 |
| IX 最小范围 | ⚠️ 见下方范围边界 |

**已知范围边界**：

1. **不改 `HIFY_RAG_METADATA_FILTER_ENABLED` 的默认值**（仍为 `false`）。
   后果：面板默认状态下，用户一勾文档就会收到「过滤未启用」的错误。
   这是**有意的**——spec 的 Clarifications 明确不改默认值，静默降级比报错危险得多。
   面板必须把「需要开启哪个环境变量」讲清楚，这属于本期的 FR-013。
2. **不做分页**。试检索的 topK 上限是 50（`clampTopK`），一屏够放，不引入游标分页。
3. **`service.go` 的检索逻辑一行不改**。本期是纯粹的入口层工作，
   这也是 SC-004（门禁既有用例逐字段不变）能够成立的前提。

## 关键设计判断

### 为什么是新端点而不是复用某个既有端点

`Retrieve` 今天没有任何 HTTP 入口——它只被 `conversation` 和 `workflow` 内部调用。
所以这是**新增**而不是**扩展**。用 `POST` 而不是 `GET`：请求体里有文档 ID 数组
（最多 50 个）和问题原文，塞进 query string 既难看又会撞长度限制；
更重要的是**问题原文不应该进 URL**（会落进网关/代理的访问日志），
这与 002 FR-018 不把过滤取值写进应用日志是同一个隐私口径。

> 语义上它是「查询」而非「创建」，用 POST 属于 REST 的常见妥协。
> 这一点在 contracts 里写明，避免后来者误以为它会创建资源。

### 权限

沿用知识库既有模型：登录用户皆可检索（与 `GET /knowledge-bases/:id` 一致），
不做创建者/管理员限制——那是编辑类操作才有的约束（见 `internal/CLAUDE.md` 的权限模型）。

### 错误传达

002 的三个错误都是 `apperr.InvalidInput`，经既有的 `httperr.Wrap` 自动转 400 +
`{"error":{"code":"...","message":"..."}}`。handler 不做任何额外处理，直接 `return err`。
前端按 `code` 区分展示，其中 `knowledge.metadata_filter_disabled` 需要额外提示开关名。

## Complexity Tracking

| 偏离 | 理由 |
|---|---|
| 新端点语义是查询却用 POST | 见上「为什么是新端点」——问题原文不进 URL 的隐私考虑压过 REST 纯洁性 |
