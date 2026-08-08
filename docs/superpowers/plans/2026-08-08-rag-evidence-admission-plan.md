# Hify RAG Phase 8：检索证据准入实施计划

对应设计：`docs/superpowers/specs/2026-08-08-rag-evidence-admission-design.md`

## 实施原则

- 严格测试先行：每组行为先增加失败测试，再写最小实现使其通过。
- 只修改 `internal/knowledge` 的候选准入与 Phase 6 检索门禁；不引入 LLM、动态配置或前端变化。
- 固定顺序：RRF 排序 → 来源感知准入 → 精确内容去重 → topK → 邻接批量查询。
- `RetrievedChunk.Score`、Citation、SSE 对外协议保持不变。
- Claude 不执行 commit/push；Codex 完成审核后统一提交。

## Task 1：建立准入纯逻辑测试

涉及文件：

- 修改 `internal/knowledge/hybrid_test.go`
- 可新增 `internal/knowledge/admission_test.go`

先增加并运行失败测试：

1. vector 分数 `< 0.35` 拒绝，`== 0.35` 和 `> 0.35` 通过；
2. keyword 分数 `< 0.45` 拒绝，`== 0.45` 和 `> 0.45` 通过；
3. 双路命中任意一路达标即通过；
4. 两路均未达标拒绝；
5. 未命中某一路时，不能把该路零值当作真实分数；
6. `A(拒绝)、B(通过)、C(通过) + topK=2` 返回 `B、C`；
7. A、B 正文相同：A 排名更高但不达标，B 排名稍低且达标，最终必须保留 B；
8. A、B 正文相同且都达标，仍保留 RRF 排名更高的 A；
9. 准入和内容去重均不得改写保留项的 Score/Citation 元数据。

建议先把准入判断实现为纯函数，输入明确的路径存在标记与原始分数，避免单测依赖数据库。

定向命令：

```bash
go test -count=1 ./internal/knowledge -run 'Test.*Admission|TestRRFFuse.*Admission'
```

## Task 2：在 RRF 内保存来源信号并实施准入

涉及文件：

- 修改 `internal/knowledge/hybrid.go`
- 修改相关调用点与测试

实现要求：

1. 新增包内常量 `vectorAdmissionThreshold=0.35`、`keywordAdmissionThreshold=0.45`；
2. `fusionEntry` 分别保存 vector/keyword 路径是否命中及该路径最高原始相关度；
3. 保持现有 fusionScore、最终 `RetrievedChunk.Score=max(vector, keyword)` 和确定性排序规则；
4. 完整候选排序后先执行准入，再执行 `dedupExactContentChunks`，最后截断 topK；
5. 返回安全的 admission 统计，不暴露 query、正文、embedding 或逐条分数；
6. 不修改 RRF 权重、`rrfK`、`candidateK`。

统计口径固定为：

- `candidate_count_before_admission`：按 ID 融合并排序后的候选数；
- `vector_below_admission_count`：存在向量信号但低于 0.35 的候选数；
- `keyword_below_admission_count`：存在关键词信号但低于 0.45 的候选数；
- `admission_rejected_count`：所有已存在路径均未达标、最终被拒绝的候选数；
- `admitted_anchor_count`：准入、内容去重并截断 topK 后的核心块数。

路径计数可以重叠；`admission_rejected_count` 每个候选最多计一次。代码注释和报告必须明确这个区别。

## Task 3：接入 Retrieve、邻接与安全可观测性

涉及文件：

- 修改 `internal/knowledge/service.go`
- 必要时修改 `internal/knowledge/model.go`
- 修改 `internal/knowledge/neighbor_batch_test.go`

实现要求：

1. `Retrieve` 使用准入后的 anchors 调用 `expandWithNeighborWindow`；
2. 所有候选被拒绝时返回空结果且 batch neighbor 调用次数为 0；
3. 只有通过准入的核心块才能出现在 `buildNeighborRequests` 输入中；
4. 普通邻接查询失败继续降级为核心块，context 错误继续传播；
5. 仅在出现拒绝项时输出安全 debug 日志或 Trace 计数；禁止记录 query、正文、embedding、fingerprint 和候选分数列表；
6. `conversation.selectEvidence` 的 0.2 防御性下游门槛不删除、不提高。

增加 spy 测试：

- 全部拒绝时 0 次 batch call；
- 混合候选时 batch 请求只包含通过准入的 anchor；
- 日志/统计不影响返回排序和字段。

## Task 4：真实数据库集成测试

涉及文件：

- 修改 `internal/knowledge/integration_test.go`

至少覆盖：

1. 非空 KB + 正交向量 + 无关键词命中，`Service.Retrieve` 返回空；
2. vector 0.35 边界通过、低于边界拒绝；
3. keyword 0.45 边界通过、低于边界拒绝；
4. 两路都弱时拒绝，任一路强时通过；
5. topK 前部拒绝项被删除，后续合格候选补位；
6. 高排名不合格重复项不能让低排名合格重复项丢失；
7. 被拒绝候选不触发邻接查询；
8. 跨知识库、跨 embedding 模型组合仍正确。

真实数据库测试必须通过 `-v` 输出确认执行，不能只看整个包 PASS。

## Task 5：扩展 Phase 6 检索门禁

涉及文件：

- 修改 `internal/knowledge/eval_gate_test.go`
- 必要时修改 `internal/eval/retrieval` 的报告/指标，但禁止加入 query、正文或分数

保留原有 6 个 case，并新增：

1. `nonempty_kb_irrelevant_query`：非空 KB 的无关查询返回空；
2. `vector_below_admission`：只有低于 0.35 的向量候选，返回空；
3. `admitted_candidate_backfills_topk`：前部拒绝项不占 topK，后续合格项补位。

负样本必须有直接断言，不能因 `ExpectedConfigured=false` 被聚合指标排除后就失去门禁作用。报告仍只保存白名单字段。

运行：

```bash
make eval-retrieval-gate
```

要求全部 case 真实执行、无 skip；原有 Hit@1、Hit@3、MRR、ContentUniqueRate 均保持 1.0。

## Task 6：文档与完整验证

涉及文件：

- 修改 `README.md`
- 修改 `docs/critical-paths.md`
- 新增 `docs/eval-phase8-evidence-admission-report.md`
- 更新 `.claude/CODEX_CLAUDE_HANDOFF.md` 的 Claude 实施结果，但不提交该文件

报告必须说明：

- 为什么统一 0.2 下游门槛不够；
- vector 0.35、keyword 0.45 的准入语义；
- 为什么顺序是准入 → 内容去重 → topK；
- 无有效证据时模型仍正常回答但无资料注入/引用；
- 两层门槛的职责边界；
- 真实执行、skip 和未验证内容。

完整验证：

```bash
make sqlc                         # 仅当 SQL 被意外改动时应确认无变化；本阶段原则上不改 SQL
make eval-retrieval-gate
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
make check-deps
git diff --check
```

最终检查 `git status --short`：不得出现 `.codex/`、`_to_delete/`、bundle、Git 锁、源码目录内 eval 报告或其它临时产物。

## Codex 审核重点

1. 准入是否使用原始 vector/keyword 信号，而不是 fusionScore；
2. 是否先准入、再内容去重、再 topK；
3. 拒绝项是否真的允许后续合格项补位；
4. 无合格核心块时是否零邻接查询；
5. 无关查询是否不注入知识库内容且不产生 Citation；
6. 统计和报告是否满足隐私白名单；
7. Phase 3-7 既有行为与 Phase 6 门禁是否不回归。
