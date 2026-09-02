# Quickstart：验证检索元数据过滤

**Feature**: `002-metadata-filter` | **Date**: 2026-09-02

每一步都必须**真的跑过并看到输出**才可勾选（宪法第 VI 条）。数据库测试**禁止 skip**——
skip 等同于未验证。

## 前置条件

```bash
make db-up
```

PostgreSQL（`localhost:5433`）与 MySQL（`localhost:3307`）必须可连。

## 步骤 0：归档改动前基线（必须在写任何代码之前）

```bash
make eval-retrieval-gate && cp eval/runs/phase6-retrieval-gate-latest.json /tmp/gate-baseline.json
```

SC-003 的逐字比对依赖这份基线。比对时 MUST 忽略 `ran_at`（每次运行都不同），
其余字段 MUST 逐字节相同。

## 步骤 1：依赖方向与静态检查

```bash
make check-deps && go vet ./...
```

本期不新增跨模块依赖边，`check-deps` MUST 输出 `OK - no cross-layer or same-layer violations`。

## 步骤 2：sqlc 重新生成

```bash
make sqlc && git diff --stat internal/db/gen
```

`internal/db/gen` 下的改动 MUST 只涉及两条召回查询的参数结构体，
MUST NOT 出现在写路径或邻接查询的生成代码里。**生成代码不得手改。**

## 步骤 3：全量测试

```bash
go test ./... -race -count=1
```

## 步骤 4：空过滤器逐字一致（SC-003 / FR-013）

```bash
make eval-retrieval-gate && python3 scripts/compare-retrieval-gate.py /tmp/gate-baseline.json eval/runs/phase6-retrieval-gate-latest.json
```

MUST 输出 `IDENTICAL`。这是本功能"不成为既有检索质量的风险来源"的唯一硬证据。

**比对规则**（`scripts/compare-retrieval-gate.py`，退出码非 0 即回归）：
基线里的每一个 case 必须存在且逐字段相同，`metrics` 与 `pass` 必须相同，
但**允许新增 case**。

> 为什么不是"整个文件逐字节相同"：`ran_at` 每次运行都不同，而本期按 SC-001/SC-002
> 的要求给门禁新增了两条用例（`filter_scopes_to_document`、`filter_scopes_to_page_range`），
> cases 数组必然变长。SC-003 真正要断言的是"既有行为一个字都没变"，不是"报告文件
> 永不增长"——按后者执行会让任何新增门禁用例都被判成回归，最后逼着人干脆放弃这条断言。
> 新增用例不影响 `metrics`：它们都命中 rank 1，四项指标仍是 1.0。

## 步骤 5：文档级过滤（US1 / SC-001）

```bash
go test ./internal/knowledge/ -race -count=1 -run 'TestIntegrationRetrieveFilterByDocument' -v
```

断言：不带过滤时结果含 A、B 两份文档的片段；限定到 A 后结果中**不含任何 B 的片段**，
且 A 的片段在 A 内部的相对顺序与不带过滤时一致。

## 步骤 6：页码范围过滤（US2 / SC-002）

```bash
go test ./internal/knowledge/ -race -count=1 -run 'TestIntegrationRetrieveFilterByPageRange' -v
```

断言：目标片段在第 N 页，范围含 N 时召回、不含 N 时不召回。

## 步骤 7：下推证明（SC-004 —— 本功能最关键的一条）

```bash
go test ./internal/knowledge/ -race -count=1 -run 'TestIntegrationRetrieveFilterPushedDownToRecall' -v
```

断言形态：构造一个知识库，让目标文档的片段**全部排在全库相似度榜的 candidateK 名之外**。
- 若过滤是"先召回 topK 再筛"，结果为**空**（该文档的片段根本没进候选窗口）。
- 若过滤真的下推进了 SQL，该文档内的片段照常按其**文档内**排名返回。

这条用例是"过滤没有吃掉召回名额"的直接证据，也是它与应用层筛选唯一能被外部观察到的区别。

## 步骤 8：邻接豁免（FR-011）

```bash
go test ./internal/knowledge/ -race -count=1 -run 'TestIntegrationRetrieveFilterExemptsNeighborsFromPageFilter' -v
```

断言：页码范围命中某 anchor 时，它落在页码范围**之外**的邻接块**仍然出现在结果里**
（`NeighborOf != ""`）；同时**不存在**任何来自过滤范围外文档的邻接块。

## 步骤 9：不放宽 / NULL 页码 / 不存在的文档（FR-009 / FR-010 / SC-005）

```bash
go test ./internal/knowledge/ -race -count=1 -run 'TestIntegrationRetrieveFilter(NoAutoRelax|UnknownDocument)|TestFilterPageRangeExcludesNullPageChunks' -v
```

## 步骤 10：开关与校验（FR-013 / FR-015 / research.md R4）

```bash
go test ./internal/knowledge/ -race -count=1 -run 'TestRetrieveFilter(Validate|Disabled)|TestRetrieveFilterIsEmpty' -v
```

断言：开关关闭 + 非空过滤器 → `ErrMetadataFilterDisabled`，**且没有发生任何数据库调用**；
文档数超 50 → `ErrTooManyFilterDocuments`，**不截断**。

## 步骤 10.5：变异测试（确认断言真的有牙齿）

一组"测试全绿"本身不证明任何事——测试可能根本抓不到 bug。至少注入下面两种缺陷各跑一次，
确认对应用例**真的会失败**，跑完务必还原：

| 注入的缺陷 | 应当失败的用例 |
|---|---|
| 让 `repository.go` 的 `filterToPGParams` 恒返回零值（过滤根本不下推） | 全部 7 条过滤集成用例 |
| 给 `chunks.sql` 的页码谓词加上 `OR page_number IS NULL`（"无元数据即通过"）——**两侧各试一次** | `TestFilterPageRangeExcludesNullPageChunks` 的对应子用例 |

> 第二项必须**单侧也试**：只测闭区间时，单侧的错误会被另一侧未改动的谓词掩盖掉，
> 用例照样通过。这个盲区是实际发生过的（见报告第 6.4 节），修正后该用例拆成了
> "闭区间 / 只给下界 / 只给上界"三个子用例。

## 步骤 11：冒烟

```
/smoke-test
```

本期不改动任何 HTTP 端点，冒烟的作用是确认签名变更没有破坏 `buildApp` 装配与既有对话链路。

## 验收清单（全部为"跑过并看到输出"才可勾选）

- [ ] 步骤 0 基线已归档（改动前）
- [ ] `make check-deps` OK
- [ ] `go vet ./...` 无输出
- [ ] `make sqlc` 后生成代码只含预期改动
- [ ] `go test ./... -race -count=1` 全绿
- [ ] 门禁 JSON 与基线（忽略 `ran_at`）逐字节 `IDENTICAL`
- [ ] US1 文档级过滤用例通过
- [ ] US2 页码过滤用例通过
- [ ] SC-004 下推证明用例通过
- [ ] FR-011 邻接豁免用例通过
- [ ] FR-009/FR-010/NULL 页码用例通过
- [ ] 开关与校验用例通过
- [ ] 变异测试：注入缺陷后对应用例确实失败
- [ ] `/smoke-test` 通过
- [ ] `docs/eval-phase10-metadata-filter-report.md` 已产出，且区分"代码验证过"与"效果验证过"
