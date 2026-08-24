# Quickstart：验证 RAG 查询优化与结果重排序

**Feature**: `001-rag-query-rerank` | **Date**: 2026-08-24

本文件是**验收指引**，不含实现代码。实现步骤见 `tasks.md`。

## 前置条件

- MySQL 8.x、PostgreSQL(pgvector)、Redis 已启动（`/smoke-test` 会拉起）。
- 已有一个绑定知识库、且知识库内至少有一篇已发布文档的 Agent。
- 验证重排需要一个可用的 rerank 服务（SiliconFlow / Jina / 本地 TEI 均可）。
  没有真实服务时，纯逻辑与集成测试用假 client 覆盖，但**报告中必须如实注明哪些项未用真实服务验证**。

## 步骤 1：迁移与依赖检查

```bash
make migrate-up && make sqlc && make check-deps
```

预期：迁移到 `000012`；`make sqlc` 无 diff（列定义未变，只改 CHECK）；`check-deps` 0 退出。

## 步骤 2：全量测试

```bash
go test ./... -race -count=1 && go vet ./...
```

预期：全绿。数据库集成测试**不得出现 skip**——出现 skip 视为未验证（宪法第 VI 条）。

## 步骤 3：注册 rerank 模型（前端暂无入口，用 API）

```bash
curl -sS -X POST "$HIFY/api/v1/providers/$PROVIDER_ID/models" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"model_name":"BAAI/bge-reranker-v2-m3","capability":"rerank","is_active":true}'
```

预期：`201`，返回体 `capability` 为 `rerank`。传 `"capability":"reranker"`（非法值）应得 `400` 且 message 为中文。

## 步骤 4：开关关闭时的基线一致性（SC-003 / FR-018）

在 `HIFY_RAG_QUERY_REWRITE_ENABLED=false`、`HIFY_RAG_RERANK_ENABLED=false` 下跑**确定性**门禁
（无 LLM、无裁判，因此可以真的比对"逐字一致"）：

```bash
make eval-retrieval-gate
```

预期：`eval/runs/phase6-retrieval-gate-latest.json` 除 `ran_at` 外与改动前逐字节相同，
`metrics` 四项（HitAt1 / HitAt3 / MRR / ContentUniqueRate）全部不变、`pass: true`。
不一致即说明"topK 截断从 rrfFuse 移到 Retrieve"的改造不是行为等价的，必须先修这个再继续。

> **不要用 `make eval` 验证这一条**：它每条用例都调真实对话模型 + 裁判模型，
> 同一份代码跑两次都不会一致。`make eval` 的用途是步骤 5/6 的相对提升度量。

**已知环境坑**：`make eval` 目前会因为 `.env` 的值带引号而失败
（`-include .env` 不剥引号 → `strconv.Atoi("\"0\"")`）。绕过办法是自己剥掉引号后
直接 `go run ./cmd/evalrunner`。这是既有问题，与本功能无关。

## 步骤 5：查询优化的效果（US1 / SC-001 / SC-006）

打开 `HIFY_RAG_QUERY_REWRITE_ENABLED=true`，在同一会话里连发两条消息：

1. 「Hify 的文档分块策略是什么？」
2. 「那它的上限呢？」

预期：
- 第二轮返回的引用与第一轮主题一致（而非空引用或跑题引用）。
- `query_rewrite` span 中 `rag.rewrite.applied=true`，且 attrs 里**看不到**任何问题原文。
- 单轮完整提问（如「Hify 用什么数据库？」）的 span 中 `rag.rewrite.skipped=true`，未产生 LLM 调用。

## 步骤 6：重排的效果（US2 / SC-002）

打开 `HIFY_RAG_RERANK_ENABLED=true` 并配好 `HIFY_RAG_RERANK_MODEL_ID`：

```bash
make eval
```

预期：SC-001 提升 ≥ 30 个百分点、SC-002 提升 ≥ 15 个百分点；未达标不算完成，
需在报告中给出实际数字并说明差距原因，**禁止**只写"效果有提升"。

## 步骤 7：降级路径（US3 / SC-004）

逐项验证 `plan.md` 的降级矩阵，至少覆盖：

1. 把 rerank 模型的 base_url 改成一个不可达地址 → 对话仍正常回答，日志出现 `rerank_degraded=true`。
2. 把改写超时设为 `1ms` → 对话仍正常回答，出现 `rag.rewrite.degraded=true`。
3. 用假 client 返回带重复 index 的 rerank 响应 → 顺序与关闭重排时**完全一致**（整体丢弃，非部分采用）。

## 步骤 8：确定性（SC-007）

用固定打分的假 client 重复执行同一次 `Retrieve` 20 次，断言 20 次结果的 chunk ID 序列逐字相同。

## 步骤 9：冒烟与报告

```bash
make check-deps && go test ./... -race -count=1
```

然后跑 `/smoke-test`，最后产出 `docs/eval-phase9-query-rerank-report.md`：
沿用 Phase 1-8 报告的结构，写清实际命令输出、达标/未达标数字，以及**未用真实服务验证的项**。

## 验收清单（全部为"跑过并看到输出"才可勾选）

- [ ] `make migrate-up` / `make sqlc`（无 diff）/ `make check-deps`
- [ ] `go test ./... -race -count=1` 全绿，无 skip
- [ ] `go vet ./...` 无输出
- [ ] 双开关关闭下 `make eval` 与基线逐字一致
- [ ] US1 两轮对话人工验证通过
- [ ] US2 `make eval` 达到 SC-001 / SC-002 门槛
- [ ] 三条降级路径逐条验证通过
- [ ] 20 次确定性测试通过
- [ ] `/smoke-test` 通过并清理测试数据
- [ ] 阶段报告已写，含未验证项的如实说明
