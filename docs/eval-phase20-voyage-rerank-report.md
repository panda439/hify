# Phase 20：Voyage rerank-3 托管 Rerank 对照评测

状态：1.5 秒部署门禁 `INCONCLUSIVE`；首轮 30 秒质量诊断 `QUALITY_INCONCLUSIVE`；65 秒限速质量诊断 `QUALITY_PASS`。三份结论分别回答不同问题，互不覆盖；012 BGE 的既有结论原样引用，不按本轮新标准回算。

## 结论

- **部署门禁（1.5s，未限速）→ `INCONCLUSIVE`**：50 条里 49 条降级。运行时曾观察到超时及熔断快速失败，但既有 raw run 没有结构化失败分类，不能把终端观察写成可复核的精确次数；成功延迟 p50/p95 不可用。Voyage 账户未添加付款方式，在该账户下无法得到有效部署证据；按用户决策不重跑。
- **质量诊断（30s，未限速）→ `QUALITY_INCONCLUSIVE`**：50 条里 49 条降级。运行时曾观察到 HTTP 429 及熔断快速失败，但既有 raw run 没有结构化失败分类，不能把终端观察写成可复核的精确次数；成功延迟 p50/p95 不可用。
- **质量诊断（30s，65s 限速）→ `QUALITY_PASS`**：50/50 实际应用、零降级、零 429。相对 011 基线，@10 Recall +0.009333、MRR +0.162254、MAP +0.157680、NDCG +0.117428；逐 query 改善 31 / 退化 9 / 不变 10。只回答排序质量，不是部署证据。
- 前两份 `INCONCLUSIVE` 只说明"证据不完整"，不说明 rerank-3 质量好或差。
- 在同一批 50 条查询上，Voyage 限速诊断的 MRR/MAP/NDCG@10（0.789000 / 0.731731 / 0.813876）都高于 012 BGE 30s（0.761556 / 0.697391 / 0.790638），Recall@10 相同。但这是各跑一次、只有 50 条查询、没做显著性检验的结果，只能说在这批数据上更好，不能外推。
- 限速条件下 Hify 逐次调用 p50/p95 为 509/836ms，低于 1.5s。但限速运行每分钟不到 1 次、没有并发，不能据此推断 1.5s 部署门禁会通过。

## 过程与决策记录

- 014 由 Codex 起草 spec/plan/tasks，并把 012/013 改动手工拷进独立 worktree；2026-09-11 17:51 Codex 主线程因用量耗尽（`usage_limit_exceeded`）中断。
- 随后由 Claude Code 接手：先分别提交 012（`2984a43`）和 013（`f567ad3`），再在 014 分支 fast-forward 到 012、合并 013（`5c88ca7`），替换手工拷贝；合并结果与拷贝逐字节一致。
- 首轮两次真实运行后确认账户限流原因，用户决定不添加付款方式，只对质量诊断限速重跑（spec FR-015/FR-016）。

## 门禁快照（T001/T003）

| worktree | 分支 | commit |
|---|---|---|
| `~/go/src/hify` | `012-bge-reranker-benchmark` | `2984a43` |
| `~/go/src/hify-voyage-adapter` | `013-voyage-rerank-adapter` | `f567ad3` |
| `~/go/src/hify-voyage-benchmark` | `014-voyage-rerank-benchmark` | `5c88ca7`（基线 `0492f32`） |

- 011 manifest / raw run / report SHA256：`737cfb8dfdecdc3a33830e66f24931b85c42c9776ace4762a187e75dce1e9c30` / `10c66e64326d9a63233308147926fe8c828e70343ce20cbcce2ee39c338e6830` / `35d2c666ea587aa42b8508d073977de2bd3d29fce72913b3023cc7dd94d929b6`
- MIRACL Mini queries / documents / qrels SHA256：`581b961248daa371fe618c6d81e87a8804b67f45e171fb5b4d4ee4387995500d` / `940e9dd8e37773f05af9896c0f53bdd4ce91050f3f23dfa288e4110f88b35fe6` / `9aebf3caf49fbfc9574bbb49f0c8c48a5c6ba5fe7e3db03da2186ff5f8f0be6b`；50 queries、800 documents、500 qrels
- 012 BGE 1.5s raw / report / comparison / decision：`b6dbede342c525da6fe02e58729634f377f716ff63c35ed5b9adcdee1262caea` / `e7fda37e2aad6c35396363286b9248cc06b536af98f238de2ff16bbaffc89611` / `0ff73f3bf35be447f1e9614b58a2e20019f6b02944921b51c4480df9af2e70af` / `e2ae2d0e4dd9995d8412d282251e34fd0d8476cb5bb4d7dba3e0d62aad13d51d`
- 012 BGE 30s raw / report / comparison / decision：`3a1eddb3575794a69cfa7075ff1cfe2c2dd528120afd595049f817ad58b77cf7` / `542055bd81a163bf8887c9671e4a1e7a0a847b1770c6ad686e6f459725412ad4` / `ea973d479aedbf9080d5f188a8f96f716675d98a130663a9dc6367442fef1887` / `6b4da81735de76dd09fe2d2e3b355e84ec04d99d5bac8800aad6963bb73433d2`
- 上述制品从主 checkout 复制到 014 worktree（gitignored），逐文件哈希一致；知识库 `01a08adf-576e-72bb-af57-32bb50e67000` 的 800 个存储文件树 SHA256 `64d1b315c5d344df23a8182122c59791a07e50a86a3ba58fc32299e5de7f272a`，两边一致
- Ollama `bge-m3:567m` digest `7907646426070047a77226ac3e684fbbe8410524f7b4a74d02837e43f2146bab`，与 011 指纹一致
- 数据库：Provider `voyage-ai`，`https://api.voyageai.com/v1`，`rerank_format=voyage`，凭据已加密保存（只核对存在性，未读取）；模型 `rerank-3` 启用、`is_default=0`

## 固定实验身份与价格快照（T004）

- 模型 `rerank-3`；请求 `top_k=len(documents)`、`return_documents=false`；响应 `data[]` 与 `usage.total_tokens`
- 价格：$0.05 / 1M tokens；rerank-3 每个账户前 200M tokens 免费。来源 <https://docs.voyageai.com/docs/pricing>，快照日期 2026-09-11，真实运行前固定在代码常量中
- 限流：已添加付款方式的 Tier 1 为 2000 RPM / 2M TPM；运行时观察到本账户未添加付款方式，用户决定不添加并采用 65 秒 benchmark-only 限速

## TDD 与工程验证

- 新增测试先 RED（缺字段/函数导致编译失败）再 GREEN：Voyage `usage.total_tokens` 解码与负值拒绝；knowledge observer 的 token 透传（响应校验失败降级时仍记已计费用量）；托管 Provider/Model precheck（模型名、能力、启用状态、官方 base URL、`rerank_format`、凭据存在性）；托管证据校验（固定模型、50 次全部 applied、零降级、input 与 token 非零、p50/p95 有限且有序、不得混入 sidecar 计数）；列表价估算与免费额度分离；模型无关双门禁决策（含 MAP、边界相等、至少一项提升）；1.5s/30s timeout 固定；付费 raw run 防覆盖；单变量指纹；限速间隔、限速仅限托管质量诊断、限速运行不能作部署证据；T024–T027 的 query 脱敏、成功延迟样本过滤、固定失败分类与聚合
- 变异验证：①把 adapter 回填的 `TotalTokens` 置零 → 解码测试失败；②托管证据接受 0 token → 证据与质量门禁测试失败；③允许限速运行作部署证据 → 决策变 `ADOPT`，测试失败。三者恢复后哈希一致、测试通过
- Phase 5 RED/GREEN 记录：T024 删除实现后运行 `go test ./internal/eval/retrievalbench -run TestSanitizeHostedRun -count=1`，得到 `undefined: SanitizeHostedRun`；恢复最小实现后通过。T025 先运行 `go test ./cmd/retrievalbench -run TestSuccessfulRerankDurations -count=1`，得到 `undefined: successfulRerankDurations`；实现后通过。T026 先运行 provider 分类测试得到固定枚举/函数未定义，先实现 provider 分类，再运行 Hosted 证据未知/负计数测试得到校验失败，补校验后通过。
- 012 本地 sidecar 的证据规则、`DecideRerank`/`DecideQuality` 未改动
- 全量验收（在所有代码改动和限速诊断完成之后运行）：`go vet ./...`、`make check-deps`、`git diff --check` 通过；`go test -race -count=1 ./...` 12 个包全部 ok；`go test -v ./...` 里唯一的 skip 是要设环境变量才会跑的 `TestVoyageRerankLive`（已在 T011 带环境变量单独运行并通过），没有数据库测试被静默跳过；`make eval-retrieval-gate` 14 条子用例全部 PASS、0 skip；011 raw run 纯重算后与历史 report 的 SHA256 一致（`35d2c666ea587aa42b8508d073977de2bd3d29fce72913b3023cc7dd94d929b6`）；限速诊断 raw run 重新 score、重新 gate-decision 的结果与保存的制品字节一致；把限速质量报告当部署门禁候选时，compare 阶段以 `incompatible reports: [run_mode]` 非零退出，不产出决策

## 真实调用证据

### T011 单次中文 smoke

`TestVoyageRerankLive` 经 Hify 真实链路（MySQL Provider/Model → 解密 → registry → adapter）调用 `rerank-3`：相关文档 `0.894531`，无关文档 `0.250000`、`0.249023`；`total_tokens=27`；耗时约 0.83s。

### T012 1.5s 部署门禁（未限速）

- 制品：`eval/runs/miracl-zh-voyage-deployment{,-report,-comparison,-decision}.json`，SHA256 `62de913986cf0c441dce899ec3f584fdf2a2a2f315d1880d112806ad4d00ae1f` / `6484bd474b311d3cacc0b0f7afc5c799c1583521fd4af8574c4920019d26e436` / `5f8e667fc62ecaa6e0603874a6a11af26cadd37e07cfe458ac0a43c07535069b` / `5a1756e75cea8b8ab053c654313b8e6a99e14127caa11d4418efef959e845ccf`
- Hify observer：enabled 50 / applied 1 / degraded 49；input 1759；rerank 耗时合计 5156ms；成功调用延迟 p50/p95 不可用（历史制品无法从聚合数据还原）；tokens 5773；列表价 $0.00028865
- 运行时曾观察到超时与熔断快速失败；当前制品没有结构化失败分类，不能复核精确次数
- @10：Recall 0.976000（Δ0）、MRR 0.626746（Δ0）、MAP 0.577162（Δ+0.003111）、NDCG 0.697535（Δ+0.001087）；逐 query 改善 1 / 退化 0 / 不变 49。增量只来自唯一一次成功应用，不是模型效果证据
- 决策：`INCONCLUSIVE`（candidate run is incomplete）

### T013 30s 质量诊断（未限速）

- 制品：`eval/runs/miracl-zh-voyage-quality{,-report,-comparison,-decision}.json`，SHA256 `f9eff96a02afc673ac23ccae30e31a4c2c8e8d0c6155c909fa8d5fc1e010b3e4` / `4e715293303cd27d147e90d27ab8da468182dc28aa86b19365c2ebe2a664eed4` / `67ae90f13ae07d7a09650ef84bff48279ee2e17519924d29f93a3e8211addb65` / `c7fbdcb712b60131f0f83f65800ae5374fe760815ea60cd3acba33f9eb6d5207`
- Hify observer：enabled 50 / applied 1 / degraded 49；input 1759；rerank 耗时合计 6383ms；成功调用延迟 p50/p95 不可用（历史制品无法从聚合数据还原）；tokens 5773
- 运行时曾观察到 HTTP 429 与熔断快速失败；当前制品没有结构化失败分类，不能复核精确次数
- @10 指标与部署门禁首轮相同；决策 `QUALITY_INCONCLUSIVE`（candidate run is incomplete）

### T022 30s 质量诊断（65s 限速）

- 制品：`eval/runs/miracl-zh-voyage-quality-paced{,-report,-comparison,-decision}.json`，SHA256 `b81709e769894739df833830557a5d4908cf8c51c364ac6e8fc9a4e4c9f50196` / `7c915833c7f2ca9b5cc5608891de645df3d1d0cd9d4300dacbddfaf2055743d3` / `964c6cd78edb713691ad8273c46c44997aa316873e890f50a859e9201a747d5a` / `627971400db6bbc67ee7ed292b2cde3cff35b3900dfe381921bdb7a399b1d81f`
- 限速：相邻 query 起点间隔 65000ms，总等待 3148784ms；运行时间 2026-09-11 18:41:43–19:34:51 CST（`evaluated_at=2026-09-11T10:41:45Z`）；query 阶段总耗时 3185680ms，其中包含限速等待
- Hify observer：enabled 50 / applied 50 / degraded 0，`hify_outcome=all_applied`；input 1759；rerank 耗时合计 27323ms；成功调用延迟 p50/p95 `509/836ms`；结构化失败分类五类均为 0；query 失败 0
- 用量：tokens 238498；列表价 $0.0119249。在每账户 200M 免费额度内，实际扣款未知
- 各 K 相对 011 基线的 delta：

| K | Recall | Precision | MRR | MAP | NDCG |
|---|---|---|---|---|---|
| 1 | +0.155667 | +0.240000 | +0.240000 | +0.240000 | +0.240000 |
| 3 | +0.151333 | +0.100000 | +0.180000 | +0.200000 | +0.184525 |
| 5 | +0.100667 | +0.048000 | +0.172000 | +0.183672 | +0.156797 |
| 10 | +0.009333 | −0.002000 | +0.162254 | +0.157680 | +0.117428 |

- @10 绝对值：Recall 0.985333、MRR 0.789000、MAP 0.731731、NDCG 0.813876；逐 query（K=10 互斥分类）改善 31 / 退化 9 / 不变 10。Precision@10 −0.002 不在门禁标准内
- 决策：`QUALITY_PASS`（all fixed gates passed）。限速记录已写入 raw run 和 report；同一份报告作为部署证据会被判 `INCONCLUSIVE`

## 同表对比（T015）

| 运行 | applied / degraded | Recall@10 | MRR@10 | MAP@10 | NDCG@10 | 延迟口径 | 结论 |
|---|---|---|---|---|---|---|---|
| 011 无 Rerank | — | 0.976000 | 0.626746 | 0.574051 | 0.696447 | — | 基线 |
| 012 BGE 1.5s | 0 / 50 | 0.976000 | 0.626746 | 0.574051 | 0.696447 | sidecar 稳态 p50/p95 7840/11654ms | `INCONCLUSIVE` |
| 012 BGE 30s | 50 / 0 | 0.985333 | 0.761556 | 0.697391 | 0.790638 | sidecar 稳态 p50/p95 3073/4330ms | `QUALITY_PASS`（012 标准） |
| 014 Voyage 1.5s | 1 / 49 | 0.976000 | 0.626746 | 0.577162 | 0.697535 | Hify 成功调用 p50/p95 unavailable | `INCONCLUSIVE` |
| 014 Voyage 30s 未限速 | 1 / 49 | 0.976000 | 0.626746 | 0.577162 | 0.697535 | Hify 成功调用 p50/p95 unavailable | `QUALITY_INCONCLUSIVE` |
| 014 Voyage 30s 限速 65s | 50 / 0 | 0.985333 | 0.789000 | 0.731731 | 0.813876 | Hify 逐次调用 p50/p95 509/836ms | `QUALITY_PASS`（014 标准） |

- 012 BGE 30s 与 014 Voyage 限速诊断都是 50/50 实际应用：Voyage 的 MRR/MAP/NDCG@10 分别高 0.027444 / 0.034340 / 0.023238，Recall@10 相同；逐 query 改善/退化 31/9 对 28/14。各跑一次、50 条查询、未做显著性检验
- 延迟口径不同：BGE 是本机 sidecar 的稳态推理耗时，Voyage 是 Hify 侧整次调用耗时（含网络、重试等待），不能直接横比
- 012 BGE 的质量结论按 012 当时标准（MRR、NDCG 提升且 Recall 不降，未含 MAP），未按 014 新标准回算

## 用量与费用

- tokens：smoke 27 + 部署门禁 5773 + 未限速质量诊断 5773 + 限速质量诊断 238498 = 250071；按 $0.05/1M 计列表价合计约 $0.0125
- 计数口径：Hify 收到的成功响应 `usage.total_tokens` 之和；超时或 429 的请求在 Voyage 侧是否计费无法从响应得知，未计入
- 列表价只是估算；账户免费额度剩余量 API 不暴露，不能写成实际扣款

## 边界与未验证项

- 1.5s 部署门禁在当前账户限流下拿不到有效证据，本轮未重跑；添加付款方式后需另开一轮，使用新文件名
- 三份 Voyage raw run 已通过临时副本离线脱敏并原子替换：`queries[].text` 全部为空；脱敏前后离线 score 的 report 字节一致，comparison/decision 字节一致；未执行 `run` 或 live test。历史两份不完整运行的成功延迟标记为 unavailable；限速完整运行保留 509/836ms。report、comparison、decision、费用证据和运行日志都不含 query 或文档正文，Hify 日志只记 `input_count` 与固定失败分类
- 限速诊断只回答排序质量；query 阶段总耗时包含限速等待，不代表线上延迟
- MIRACL Mini 只有 50 条查询，qrels 覆盖有限，结论不外推到完整 MIRACL 或 Hify 线上流量
- `internal/platform/trace/span.go`、`internal/workflow/integration_test.go` 的 gofmt 问题在 `0492f32` 就存在，014 未改动
- 014 worktree 里保留了一个 stash（`014-import-backup-before-merge`，Codex 手工拷贝的备份，已核对与合并结果一致）；删除操作被权限拦截，可手动 `git stash drop`
- 未提交、未推送、未启用默认 Rerank
