---

description: "Task list for Agent 文档范围绑定"
---

# Tasks: Agent 文档范围绑定

**Prerequisites**: [plan.md](./plan.md)、[spec.md](./spec.md)

## Phase 1: Setup

- [x] T001 归档改动前基线：`make eval-retrieval-gate` 保存报告；确认 `go test ./... -race`、
  `go vet`、`make check-deps` 全绿

## Phase 2: 数据层

- [x] T002 新增 `internal/db/migrations/000014_agent_documents.{up,down}.sql`，
  表结构照 `agent_knowledge_bases` 的既有形态（复合主键、无独立 id 列、索引齐全）
- [x] T003 在 `internal/db/queries/agents.sql` 加范围的增删查；跑 `make sqlc`
- [x] T004 `make migrate-up` 并确认表结构

## Phase 3: agent 模块（固定顺序）

- [x] T005 `model.go`：`Agent.DocumentIDs`、`CreateAgentInput`/`UpdateAgentInput` 同步字段
- [x] T006 `errors.go`：文档不属于绑定知识库、文档数超上限 两个中文错误
- [x] T007 `repository.go`：范围的读写（跟随既有 `agent_knowledge_bases` 的事务写法）
- [x] T008 `service.go`：FR-004 归属校验、FR-005 上限校验、Edge Case「解绑知识库时清理范围」
- [x] T009 `dto.go` + `handler.go`：请求/响应加 `document_ids`
- [x] T010 集成测试（真实 MySQL，禁止 skip）：范围读写往返、跨库文档被拒、超上限被拒、
  解绑知识库时范围被清理

## Phase 4: 接进对话链路（本期的核心）

- [x] T011 `internal/config/config.go`：`HIFY_RAG_METADATA_FILTER_ENABLED` 默认值改 `true`（FR-007）
- [x] T012 `internal/conversation/context.go`：把 `ag.DocumentIDs` 传进
  `RetrieveOptions.Filter.DocumentIDs`（FR-003）
- [x] T013 集成测试：配了范围的 Agent 只召回范围内文档；未配范围的 Agent 行为不变（FR-008）
- [x] T014 `make eval-retrieval-gate` 与 T001 基线逐字段比对，MUST `IDENTICAL`（SC-002）

## Phase 5: 前端

- [x] T015 `web/src/lib/agents.ts`：类型与请求体加 `document_ids`
- [x] T016 `web/src/routes/agent-form-dialog.tsx`：按知识库分组的文档勾选（FR-009/US2），
  显示文件名，明确提示「不勾选 = 不限定」
- [x] T017 `make web-build` 无 TS 错误

## Phase 6: 验收

- [x] T018 `go test ./... -race`、`go vet`、`make check-deps`
- [x] T019 `make eval` 的**确定性指标**与 `eval/baseline.json` 比对（SC-004）；
  裁判分不作为验收依据
- [x] T020 `/smoke-test`
- [x] T021 浏览器验证 US1/US2
- [x] T022 产出 `docs/eval-phase12-agent-document-scope-report.md`
