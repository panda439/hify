# RAG 优化第七阶段：邻接窗口批量查询（Batch Neighbor Lookup）评估报告

日期：2026-08-08（二次更新：修复 Codex 第一轮审核发现的问题，见第 0 节）。前置阶段：[docs/eval-phase4-neighbor-window-report.md](docs/eval-phase4-neighbor-window-report.md)（邻接分块扩展）、[docs/eval-phase5-content-dedup-report.md](docs/eval-phase5-content-dedup-report.md)（内容去重）、[docs/eval-phase6-retrieval-gate-report.md](docs/eval-phase6-retrieval-gate-report.md)（确定性检索回归门禁）。本阶段不新增检索算法、不改变排序/阈值/预算逻辑，只消除 Phase 4 引入的邻接窗口查询里的 N+1 数据库往返问题。

## 0. 二次更新：修复 Codex 第一轮审核发现的问题

Codex 第一轮审核（`.claude/CODEX_CLAUDE_HANDOFF.md`）确认主体实现正确（`make sqlc` 生成干净、spy 测试证明正常路径只调用一次批量查询、真实 PostgreSQL 多文档批量测试通过、`expandWithNeighborWindow` 组装正确、`make eval-retrieval-gate` 即 Phase 6 门禁 6/6 case 全部真实 PASS 四项指标均为 1.0 无 skip、完整 `go test`/`-race`/`vet`/`check-deps`/`diff --check` 套件全部通过），但发现 1 个阻塞性缺陷，已修复：

1. **`requested` CTE 未去重，重复请求坐标产生重复结果行（正确性缺陷）**：`FindPublishedNeighborChunksBatch`（`internal/db/pgqueries/chunks.sql`）原来的 `requested` CTE 是一个普通 `SELECT`，如果调用方传入的三个并行数组里出现重复的三元组（同一个 `(document_id, document_version, chunk_index)` 坐标被请求两次），`unnest` 产生的 `requested` 行集会保留这个重复，JOIN 到 `chunks` 后同一个 chunk 会被返回两次。Codex 用真实 PostgreSQL 复现：故意把 `chunk_index=1` 的坐标重复请求一次、且乱序排列，得到的结果是 `[dup-0, dup-1, dup-1]`——这不满足 Phase 7 任务原本的验收要求"批量请求中包含重复坐标、乱序坐标时，结果必须正确且确定"："确定地返回重复行"不是"正确"，还会无谓放大 DB 返回行数，并把正确性完全押在调用方（`buildNeighborRequests`）永远提前去重这一个假设上。
   修复：`requested` CTE 的 `SELECT` 改为 `SELECT DISTINCT d.document_id, v.document_version, c.chunk_index`——即使传入的三元组重复、乱序，`requested` 行集里每个坐标也只出现一次，JOIN 后每个匹配的 chunk 只返回一行。`buildNeighborRequests` 原有的 Go 层提前去重完整保留、未做任何减弱（正常生产路径仍然不会往数据库发送冗余坐标）；SQL 层的 `DISTINCT` 是独立于 Go 层之外的第二重、边界防御性保证——即使调用方去重逻辑将来出 bug 或被绕过，这条查询本身的返回结果依然正确，不会把请求里的重复放大成结果里的重复。这不是用 Go 侧后处理掩盖问题，是在 SQL 层直接修正。
   验证方式（红/绿两次真实运行，而非推断）：先把 `TestIntegrationFindPublishedNeighborChunksBatchDuplicateAndOutOfOrderRequestsAreDeterministic` 的期望值改为去重后的 `[dup-0, dup-1]`；用未加 `DISTINCT` 的旧生成代码跑这个改后的测试，确认真的失败（`got [dup-0 dup-1 dup-1], want [dup-0 dup-1]`，和 Codex 报告的症状完全一致）；再用加了 `DISTINCT` 并重新 `sqlc generate` 之后的代码跑同一个测试，确认 PASS。`internal/knowledge/repository.go` 的 `findPublishedNeighborChunksBatch` 文档注释、`internal/db/pgqueries/chunks.sql` 的查询文档注释、`docs/critical-paths.md` 里描述这条测试覆盖范围的一句话，三处此前都错误地宣称/暗示"SQL 不去重、重复请求会产生重复结果"，均已同步改为描述修复后的行为。

清理：本轮返修没有新增任何未纳入本阶段改动范围的临时产物；`.codex/agents/` 目录的来源说明见交接文档本轮新增小节。

未扩展范围：本轮只修复上述重复坐标去重问题，没有改动批量调用架构、请求排序方式、失败降级策略、检索权重或 Phase 6 门禁阈值——`buildNeighborRequests`/`expandWithNeighborWindow`/`expandWithNeighbors`/`service.findNeighborBatch` 字段设计/失败降级分支本轮零改动。

## 1. 问题背景

Phase 4 的 `expandWithNeighborWindow`（`internal/knowledge/service.go`）按 `(document_id, document_version)` 把核心命中块（anchors）分组，对每一组单独调用一次 `findPublishedNeighborChunks`。一次 `Retrieve` 涉及的核心命中块如果分散在 K 个不同的文档/版本里，邻接窗口这一步就要发生 K 次数据库往返——这是一个 N+1 查询模式：Hybrid Search 本身（向量路 + 关键词路）无论 topK 多大，请求量都是常数（两次查询），但邻接窗口这一步的查询次数会随着核心命中的文档版本分布线性增长，是整条检索链路里唯一一个"结果越分散、往返越多"的环节。

本阶段的目标：把邻接窗口查询改造成正常路径下恒定一次数据库往返，不管有多少个核心块、多少个不同的文档/版本；同时不改变 Phase 3-6 已经锁定的检索结果和门禁指标——这是一次纯粹的查询方式重写，不是检索逻辑变更。

## 2. 设计

### 2.1 Go 层：展平 + 去重 + 一次批量调用

新增 `internal/knowledge/neighbor.go` 的 `buildNeighborRequests(anchors []RetrievedChunk) []neighborRequest`：遍历每个 anchor 想要的邻接坐标（复用 Phase 4 就有的 `neighborIndexesFor` 规则——`chunk_index-1`，仅当非负；`chunk_index+1`，总是包含），用一个 map 以 `(document_id, document_version, chunk_index)` 三元组为 key 去重，产出一个扁平的 `[]neighborRequest`。多个 anchor 想要同一个坐标（相邻的两个核心块，或恰好共享同一个 would-be-neighbor 索引）时，这个坐标在结果里只出现一次——这正是"去重后的请求集合"的字面含义，也是为什么一次批量查询能覆盖任意多个 anchor 而不必让 SQL 自己处理重复输入（SQL 侧的去重责任边界见 2.2 节）。

`service.go` 的 `expandWithNeighborWindow` 不再对 `buildNeighborGroups` 返回的分组做循环，改为：调 `buildNeighborRequests` 拿到扁平请求集合 → 集合非空时调用**一次** `s.findNeighborBatch`（见 2.3 节）→ 把返回的邻接块和 anchors 一起交给 Phase 4 就有的 `expandWithNeighbors` 做两层输出组装。`expandWithNeighbors` 本身一行没有改动——它从"给定 anchors 和一批 neighbors，不关心 neighbors 来自几次查询"这个既有契约天然兼容一次批量调用的结果。

### 2.2 SQL 层：`FindPublishedNeighborChunksBatch`

新增 `internal/db/pgqueries/chunks.sql` 的 `FindPublishedNeighborChunksBatch`，三个等长的并行数组（`document_ids`/`document_versions`/`chunk_indexes`）各自单独 `unnest() WITH ORDINALITY`，再按序数 `ord` 三路 JOIN 拼回同一行，得到一个 `requested(document_id, document_version, chunk_index)` 临时行集，再和 `chunks` 表按这三列做等值 JOIN，`WHERE is_published = true`。

选择"三个单参数 `unnest` + `WITH ORDINALITY` 拼接"而不是看起来更直接的"多参数 `unnest(a, b, c) AS t(x, y, z)`"，是因为后者在 `sqlc generate` 阶段直接报错——`function unnest(unknown, unknown, unknown) does not exist`：sqlc 的静态 PostgreSQL 类型检查器无法解析多参数 `unnest` 这个多态内建函数的参数类型（这是开发过程中实际跑到的报错，不是猜测的兼容性问题）。`WITH ORDINALITY` 拼接版本每个 `unnest` 调用都是标准的单参数形式，sqlc 能正确推断 `text[]`/`bigint[]`/`int[]` 三个数组各自的类型，`sqlc generate` 顺利生成 `FindPublishedNeighborChunksBatchParams{DocumentIds []string, DocumentVersions []int64, ChunkIndexes []int32}` 和对应的 `pq.Array(...)` 绑定代码。三个数组各自作为独立的 `sqlc.arg(...)::type[]` 参数绑定——不是字符串拼接，SQL 文本本身不包含任何调用方数据。

隔离性和 Phase 4 的单组查询 `FindPublishedNeighborChunks` 完全一致，没有因为改成批量而放松：JOIN 条件同时要求 `document_id`、`document_version`、`chunk_index` 三者都匹配，绝不会把另一个文档、或者同一文档另一次处理尝试（重新处理产生的新/旧版本）里恰好 `chunk_index` 相同的行当成邻接块带回来；`is_published = true` 排除未发布草稿版本；请求数组里某个坐标对应的版本已经被重新处理删除（`DeleteObsoleteChunkVersions` 物理删除旧版本行）时，JOIN 天然匹配不到那一行，返回空——结构性保证，不靠 Go 层二次校验。

去重责任边界（二次更新，见第 0 节）：`requested` CTE 的 `SELECT` 显式加了 `DISTINCT`——即使调用方传入重复、乱序的三元组，`requested` 行集里每个坐标也只出现一次，JOIN 到 `chunks` 后每个匹配的 chunk 只返回一行；这条 SQL 本身不再依赖调用方一定提前去重才能给出正确结果（第 4 节的集成测试直接验证了这一点，故意重复+乱序传同一个坐标，断言最终只得到一行）。`buildNeighborRequests`（Go 层）的提前去重仍然完整保留、是首选机制——它让发给数据库的坐标数量不随重复请求膨胀，这一点没有变；SQL 层的 `DISTINCT` 是在此之外的第二重、独立的边界正确性保证：调用方去重逻辑万一有 bug 或未来出现不经过 `buildNeighborRequests` 的调用路径，这条查询的返回结果依然正确，不会把请求里的重复放大成结果里的重复。这条边界修正是 Codex 第一轮 Phase 7 审核发现的问题，本轮修复（第 0 节有完整背景和红/绿验证记录）。

空数组（调用方没有任何邻接坐标要问）交给 Go 层直接短路返回 `(nil, nil)`，不发起这条查询——`Repository.findPublishedNeighborChunksBatch` 在构造参数数组之前就检查 `len(requests) == 0`。SQL 侧不需要、也没有为空数组写特判：`unnest` 对三个空数组产生零行 `requested`，JOIN 自然返回空结果集，行为本身是对的，只是没必要为了"什么都不查"专门走一次数据库往返。

### 2.3 服务层：`findNeighborBatch` 字段——可注入 spy，不扩大公开 API

任务要求"提供可自动验证的'正常路径只调用一次批量查询'证据……可以抽取最小内部接口/纯编排 helper 注入 spy，但不要扩大公开 `knowledge.Service` API，也不要为了测试引入大型 mocking 框架"。

`service` 结构体（`service.go`）新增一个未导出字段：

```go
findNeighborBatch func(ctx context.Context, requests []neighborRequest) ([]RetrievedChunk, error)
```

这是一个方法值字段，不是把 `s.repo` 的类型换成某个更大的 repository 接口——`service` 结构体里其它每一个方法仍然直接用 `*Repository` 访问数据库，只有 `expandWithNeighborWindow` 通过这个字段间接调用。`wire.go` 的 `NewService` 把它设为 `repo.findPublishedNeighborChunksBatch`，生产路径行为不变；测试（同包，未导出字段可直接访问）构造一个裸的 `&service{findNeighborBatch: spy}`，不需要 mock 框架、不需要一个实现整个 Repository 接口的假对象，`Service` 这个公开接口的方法集合本阶段一个签名都没有变。

## 3. 失败降级策略

Phase 4 的失败隔离粒度是"每个 (document_id, document_version) 分组独立失败、独立降级"——一个分组的查询失败只丢那个分组的邻接块，其它分组和全部 anchors 不受影响。Phase 7 把 K 次查询合并成 1 次之后，这个"分组级降级"就不再有意义了：只有一次调用，要么整体成功、要么整体失败，不存在"部分分组失败"的中间态。

失败降级因此简化为整批粒度，但保留的契约和 Phase 4 完全一致（只是把"每个分组"换成"这一次批量调用"）：

- 普通错误（连接失败、超时之外的驱动层错误）：`expandWithNeighborWindow` 记录一条不含 query/正文的 `slog.Warn`（只有错误本身、anchor 数量、请求坐标数量），返回全部 anchors、零邻接块、`nil` error——`Retrieve` 不会因为邻接窗口这个增强功能失败而整体失败。
- `context.Canceled`/`context.DeadlineExceeded`：必须原样向上传播，不能被当成"这次批量查询就是没查到"而吞掉——复用 Phase 4 就有的 `classifyRetrieveErr`，判断逻辑完全不变（先看 `ctx.Err()`，再看 `errors.Is(err, context.Canceled/DeadlineExceeded)`）。

`internal/knowledge/neighbor_batch_test.go` 的纯逻辑 spy 测试和 `integration_test.go` 里复用/更新的两个真实数据库测试（`TestIntegrationExpandWithNeighborWindowDegradesToAnchorsOnOrdinaryFailure`/`TestIntegrationExpandWithNeighborWindowPropagatesContextCancellation`）分别覆盖了这两条路径——后两个是 Phase 4 就有的测试，本阶段唯一的改动是把手写的 `&service{}` 字面量补上 `findNeighborBatch` 字段（因为 `expandWithNeighborWindow` 现在通过这个字段而不是 `s.repo` 直接访问数据库），断言和触发条件完全没变。

## 4. 修改/新增文件

- `internal/db/pgqueries/chunks.sql`：新增 `FindPublishedNeighborChunksBatch`（见 2.2 节）；`FindPublishedNeighborChunks`（Phase 4 的单组查询）文档注释更新，说明它不再是 `service.go` 的生产路径，但保留供 `integration_test.go` 里若干 Phase 4/5 测试直接调用，驱动 `expandWithNeighbors` 独立于 `Service.Retrieve`/MySQL 验证。
- `internal/db/pggen/chunks.sql.go`、`internal/db/pggen/querier.go`：`sqlc generate` 自动生成，新增 `FindPublishedNeighborChunksBatch`/`FindPublishedNeighborChunksBatchParams`/`FindPublishedNeighborChunksBatchRow`，未手改。
- `internal/knowledge/neighbor.go`：新增 `neighborRequest` 类型和 `buildNeighborRequests` 纯函数（见 2.1 节）；`buildNeighborGroups`/`neighborGroupKey`（Phase 4）保留不变，文档注释更新说明它们不再是生产路径。
- `internal/knowledge/neighbor_test.go`：新增 `TestBuildNeighborRequestsFlattensAndDedupsAcrossAnchors`/`TestBuildNeighborRequestsSingleAnchorPerDocument`/`TestBuildNeighborRequestsEmptyAnchors` 三个纯逻辑测试。
- `internal/knowledge/repository.go`：新增 `findPublishedNeighborChunksBatch` 方法（见 2.2 节），`findPublishedNeighborChunks`（Phase 4 单组查询）保留不变。
- `internal/knowledge/service.go`：`service` 结构体新增 `findNeighborBatch` 字段（见 2.3 节）；`expandWithNeighborWindow` 重写为单次批量调用（见 2.1、2.3 节）。
- `internal/knowledge/wire.go`：`NewService` 把 `findNeighborBatch` 设为 `repo.findPublishedNeighborChunksBatch`。
- `internal/knowledge/neighbor_batch_test.go`（新增）：6 个 spy 回归测试，覆盖"正常路径只调用一次"（多文档多版本、单 anchor 两种场景）、"空 anchors 零次调用"、"普通错误降级为 anchors-only"、"`context.Canceled` 传播"、"成功批量结果正确组装"。
- `internal/knowledge/integration_test.go`：新增 6 个真实 Postgres 集成测试（见第 5 节）；`TestIntegrationExpandWithNeighborWindowDegradesToAnchorsOnOrdinaryFailure`/`TestIntegrationExpandWithNeighborWindowPropagatesContextCancellation`（Phase 4）的手写 `&service{}` 字面量补上 `findNeighborBatch` 字段，断言不变。
- `README.md`：「RAG 全流程」段落里"邻接分块扩展"后追加"批量邻接查询"描述，补上本报告链接。
- `docs/critical-paths.md`：链路 3 描述里的 Phase 4 邻接查询步骤更新为批量查询，覆盖状态一栏追加 Phase 7 的单测/集成测试条目。
- 新增本文件 `docs/eval-phase7-batch-neighbor-report.md`。

未改动：`internal/knowledge/neighbor.go` 的 `expandWithNeighbors`（组装/排序/去重规则）、`internal/knowledge/dedup.go`、`internal/knowledge/hybrid.go`（RRF 融合）、`internal/eval/retrieval`（Phase 6 门禁的指标/阈值定义）、6 个 Phase 6 门禁 case 的场景定义和期望值——本阶段任务明确要求不改变这些。

## 5. 测试

### 5.1 纯逻辑（不依赖数据库）

- `neighbor_test.go`：`buildNeighborRequests` 的展平/去重/多文档多版本/单 anchor/空输入。
- `neighbor_batch_test.go`：`expandWithNeighborWindow` 的 spy 回归——
  - `TestExpandWithNeighborWindowCallsBatchExactlyOnceAcrossMultipleDocumentVersions`：3 个 anchor 跨 3 个不同文档版本，`findNeighborBatch` 恰好被调用 1 次。
  - `TestExpandWithNeighborWindowCallsBatchExactlyOnceForSingleAnchor`：单 anchor 同样恰好 1 次。
  - `TestExpandWithNeighborWindowSkipsBatchCallOnEmptyAnchors`：空 anchors，0 次调用。
  - `TestExpandWithNeighborWindowDegradesToAnchorsOnlyOnGenericBatchError`：普通错误，返回全部 anchors、`nil` error、恰好 1 次调用（无重试）。
  - `TestExpandWithNeighborWindowPropagatesContextCanceledFromBatchCall`：`context.Canceled` 正确传播。
  - `TestExpandWithNeighborWindowAssemblesSuccessfulBatchResult`：成功批量结果通过 `expandWithNeighbors` 正确组装（烟雾测试，不重复 `expandWithNeighbors` 自身已有的详尽断言）。

### 5.2 真实 PostgreSQL 集成测试（本沙箱可直接跑，不需要 MySQL）

- `TestIntegrationFindPublishedNeighborChunksBatchAcrossMultipleDocumentsAndVersions`：2 个文档、其中一个还有一个未发布的同 index 干扰版本，一次批量调用正确返回 3 条请求坐标对应的行，不串文档/版本。
- `TestIntegrationFindPublishedNeighborChunksBatchIsolatesAcrossVersions`：显式请求已被重新处理删除的旧版本坐标，返回空；对照组请求当前版本坐标，正确返回。
- `TestIntegrationFindPublishedNeighborChunksBatchExcludesUnpublished`：未发布草稿即使坐标精确匹配也不返回。
- `TestIntegrationFindPublishedNeighborChunksBatchDuplicateAndOutOfOrderRequestsAreDeterministic`：乱序 + 重复坐标经 `requested` CTE 的 `DISTINCT` 去重后，产生确定性且唯一（不含重复行）的结果，验证 2.2 节（二次更新后）描述的 SQL 层防御性去重行为——本轮修复目标，见第 0 节的红/绿验证记录。
- `TestIntegrationFindPublishedNeighborChunksBatchEmptyRequestsNeverQueriesDatabase`：用一个真实拒绝连接的 `*sql.DB` 证明空请求集合从未触达数据库（如果触达了，这个测试会因为连接失败而报错，而不是返回空结果）。
- `TestIntegrationExpandWithNeighborWindowProducesCorrectResultAgainstRealPostgres`：端到端，`expandWithNeighborWindow` 通过真实 `Repository` 驱动，2 个文档各自的核心块 + 邻接块，最终两层排序输出和 Phase 4 逐组查询时期望的结果完全一致。

### 5.3 真实测试结果（云端沙箱，PostgreSQL 可用，MySQL 不可用）

```
go build ./...                                                    # 通过
go vet ./...                                                      # 通过
go test -count=1 ./...                                            # 全部 PASS
go test -race -count=1 ./...                                      # 全部 PASS，无 race
go test -count=1 -v ./internal/knowledge -run 'TestBuildNeighborRequests'          # 3/3 PASS
go test -count=1 -v ./internal/knowledge -run 'TestExpandWithNeighborWindow'       # 6/6 PASS
go test -count=1 -v ./internal/knowledge -run 'TestIntegration.*Neighbor|TestIntegrationExpandWithNeighbor'
  # 全部 PASS，含本阶段新增的 6 个真实 Postgres 批量查询测试
  # TestIntegrationRetrieveNeighborDedupPrefersCoreOverDuplicateNeighborContentEndToEnd — SKIP（需要 MySQL，本沙箱按约定 SKIP，Phase 4 起就是如此，非本阶段引入）
make check-deps                                                   # OK - no cross-layer or same-layer violations
git diff --check                                                  # 无输出
gofmt -l .                                                        # 无本阶段改动文件相关问题（既有的 internal/workflow/integration_test.go 未格式化和本阶段无关，未处理）
```

## 6. 未验证内容与剩余风险

- **本沙箱没有 MySQL**，`TestRetrievalGatePhase6`（Phase 6 门禁）和唯一一个需要真实 `Service.Retrieve` 全链路（含 MySQL `knowledge_base` 查询）的邻接测试本轮都只验证了正确 SKIP，没有重新跑通真实 MySQL+PostgreSQL 全链路。本阶段的改动只涉及 `expandWithNeighborWindow` 内部如何查询邻接块（一次批量 vs 多次分组查询），不涉及 `rrfFuse`/`dedupExactContentChunks`/`expandWithNeighbors` 的排序或去重规则本身，理论上不应该改变 Phase 6 门禁的 6 个 case 的最终结果（Hit@1/Hit@3/MRR/ContentUniqueRate 仍应为 1.0）——但这是理论推断，不是真实执行证据。需要 Codex（或任何有完整 MySQL+PostgreSQL docker 环境的机器）跑一遍 `make eval-retrieval-gate` 或 `go test -v -race -count=1 -run TestRetrievalGatePhase6 ./internal/knowledge/` 确认真的 PASS、6 个 case 全部无 skip、四项指标仍为 1.0，以及跑一遍 `TestIntegrationRetrieveNeighborDedupPrefersCoreOverDuplicateNeighborContentEndToEnd` 确认端到端邻接去重行为不回归。
- 本阶段没有做任何性能基准测试（benchmark/压测）——"优化前最多 N 次查询、优化后正常路径 1 次"这个结论是从代码结构上论证的（`buildNeighborRequests` 产出单个扁平集合、`expandWithNeighborWindow` 只有一处调用点、spy 测试直接断言调用次数），不是从实测延迟/QPS 数字得出的，本阶段任务本身也没有要求性能基准。
- `internal/eval/retrieval`（Phase 6 门禁的指标/阈值定义包）和 6 个门禁 case 本阶段零改动，未重新验证，未扩大范围。
