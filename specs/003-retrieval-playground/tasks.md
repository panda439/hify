---

description: "Task list for 知识库试检索面板"
---

# Tasks: 知识库试检索面板

**Input**: Design documents from `/specs/003-retrieval-playground/`

**Prerequisites**: [plan.md](./plan.md)、[spec.md](./spec.md)、[contracts/](./contracts/)

**Organization**: 后端 → 前端 → 验收。后端完成即可用 curl 独立验证，不依赖前端。

---

## Phase 1: Setup

- [x] T001 确认改动前工作区全绿：`go test ./... -race -count=1`、`go vet ./...`、`make check-deps`
- [x] T002 归档 002 的门禁基线（SC-004 用）：`make eval-retrieval-gate` 并保存报告

## Phase 2: 后端（US1-US3 的能力基础）

模块内顺序（宪法第 IV 条）：`dto.go → handler.go → wire.go`。
**`model.go`/`errors.go`/`repository.go`/`service.go` 本期一行不改**——这是 SC-004 成立的前提。

- [x] T003 在 `internal/knowledge/dto.go` 新增 `retrieveRequest` 与 `retrieveResponse`
  （含 `chunkResult`），字段严格照 [contracts/retrieval-http-api.md](./contracts/retrieval-http-api.md)。
  `page_number` MUST 是 `*int` 以便序列化成 `null`
- [x] T004 在 `internal/knowledge/handler.go` 新增 `Retrieve` handler：
  bind → 组装 `RetrieveOptions` → 调 `h.service.Retrieve` → 映射 dto。
  错误一律 `return err`（FR-006，不转换不吞掉）。`query` 为空返回 `ErrInvalidRequest`
- [x] T005 在 `internal/knowledge/wire.go` 注册 `kbs.POST("/:id/retrieve", ...)`
- [x] T006 集成测试（真实 PostgreSQL，**禁止 skip**）：
  文档过滤生效、页码过滤生效、空过滤器正常、三个过滤错误如实返回、知识库不存在返回 404
- [x] T007 `go vet ./...`、`make check-deps`、`go test ./... -race`

**Checkpoint**: 后端可用 curl 独立验证

## Phase 3: 前端

- [x] T008 在 `web/src/lib/knowledge.ts` 新增 `useRetrieveProbe` mutation 与相关类型
- [x] T009 新建 `web/src/routes/knowledge-retrieval-dialog.tsx`：
  问题输入、已就绪文档的勾选列表（显示文件名，FR-009）、页码上下界、topK、发起检索
- [x] T010 结果区：命中与邻接块**视觉区分**（FR-011/US4），显示来源文档名与页码；
  页码为空时显示「—」而不是「0」或空白
- [x] T011 空结果与错误处理（US3/FR-013）：区分「过滤后无候选」「知识库无已就绪文档」
  「开关未启用」三种情形，全部中文；开关未启用时 MUST 说明环境变量名
- [x] T012 前端校验（Edge Cases）：问题为空不发请求；页码非正数或起始>结束就地拦下
- [x] T013 在 `web/src/routes/knowledge.tsx` 加入口按钮（FR-008）
- [x] T014 面板顶部加一句定位说明（FR-012）：这是检索调试工具，不产生对话
- [x] T015 `npm run build`（或 `make web-build`）通过，无 TS 错误

## Phase 4: 验收

- [x] T016 `make eval-retrieval-gate` 与 T002 基线比对，MUST `IDENTICAL`（SC-004）
- [x] T017 `/smoke-test`
- [~] T018 **部分完成，如实记录**：SC-001（限定文档）已在**真实运行的服务**上用 HTTP 验证通过；
  SC-002（限定页码）**只在集成测试层面验证**，未能在真实服务上验证——开发环境的 embedding
  供应商连不上，库里唯一的 PDF 处于 failed 状态、零分片，没有带页码的数据可供检索。
  详见报告第 4.1 节。前端只做了 `tsc -b && vite build` 构建验证，未做浏览器点击验证
- [x] T019 产出 `docs/eval-phase11-retrieval-playground-report.md`，
  区分「代码验证过」与「效果验证过」

---

## Dependencies

```
Phase 1 → Phase 2（后端，可独立验证）→ Phase 3（前端）→ Phase 4（验收）
```
