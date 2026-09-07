# Phase 1 / 2 修复验证（2026-09-06）

本轮在活动 worktree 修复 R4-01～06，验证副本使用独立的 MySQL/PG 数据库前缀，避免其他开发进程重建同名测试库。
没有停止开发服务、调用真实模型、修改用户已有文档或提交代码。

## 可复现证据

- `manifest.json`：被测源码 SHA256、基础提交、测试数量与默认关闭快照 hash。
- `all-tests.jsonl.gz`：修复后的全量 race 原始输出；`http.log`：txt/md/PDF 上传验证。
- `pre010-tests.jsonl.gz`：从 `1f37c1d` 隔离导出的旧代码**补跑**基线，含审核快照测试；不是实施前的历史原始输出。
- `pre-fix-contract-failures.log`：上一轮 8 个明确反例的失败输出；本轮已纳入 narrative_contract_test.go。
- `pre010-*.json` / `fixed-*.json`：新旧检索/上下文门禁报告；相应 `.log` 为执行输出。
- `vet.log` / `dependencies.log`：静态检查与依赖层检查。

默认关闭快照：`narrative_default_snapshot_test.go` 固定 90 份输入 × 7 种 size × 7 种 overlap × 3 种格式，共 13,230 组。
预期 hash 来自 `1f37c1d` 的完整输出，覆盖 Content、顺序、页区间、标题。
检索门禁 14 个既有用例逐字段一致；上下文门禁仅 `ran_at` 不同。

HTTP 测试使用真实 TCP、正式路由/JWT、multipart、文件存储、Redis 入队、MySQL/PG 发布，embedding 为 Fake Provider。
入队后调用与 worker 相同的 ProcessDocument 入口，避免消费共享 Redis 队列；**未宣称独立后台进程/真实模型端到端验收**。
前端用本机 Node 24.19.0 通过 tsc 与 Vite 构建；默认 Node 21.6.0 缺少 styleText 导致初次构建失败。
构建仅有既有大 bundle 提示，产物留在隔离副本，不删除工作目录的 web/dist/.gitkeep。

## 复跑

数据库开启且同包没有其他测试进程时：

```sh
go test ./... -race -count=1 -timeout=3m
go vet ./...
make check-deps
make eval-retrieval-gate
make eval-context-gate
python3 scripts/prepare-aq-corpus.py --source-dir eval/corpus/aq-010
```

测试会重建按包命名的测试库；并发开发时应先复制到隔离目录，将副本 internal/testutil/db.go 中
`hify_test_` 改为本次独占前缀再运行，不能与另一个同包测试进程共用测试库。
前端在 Node 24 环境运行 `npm run build`。

## Phase 1 补录边界

- 源码基线和门禁已从旧提交补跑，旧提交与当前修复的默认路径可比较；不能重建当时未保存的命令历史。
- T004 的构造输入见 narrative_contract_test.go / narrative_default_snapshot_test.go / chunk_test.go 的 crossPageFixture。
  覆盖短场景、长段、重复段、章节缺失/节选、overlap、跨页；测试报告机制结果，不宣称 SC-001 真实语料质量提升。
- `scripts/prepare-aq-corpus.py` 从用户冻结的 9 章快照重建 322 ID / 314 正文 / 8 排除与 source-map.json，
  校验每章/每段 SHA256。两次重建逐字节一致；篡改输入会在写入前被拒绝。
  原网页的不可变 revision 和许可记录尚缺，不能以当前网页覆盖该版本；发布前仍需核验来源许可。
- 构造模型/故障输入现冻结在 `eval/fixtures/narrative-010-v1`，时间是本轮，不能声称实施前已经冻结。
  模型输出解析消费与语义验收留在 T024 / Phase 6。
- 人工标注仍未验收；AI_DRAFT_NOT_HUMAN_GOLD 状态与原引用均保留。

## 已存在的数据

修复后的新上传会生成精确来源。旧叙事文档的 metadata 不会自动变正确；需要重新上传/处理后再用于关系证据。
本轮没有直接改写数据库里的旧来源或 AI 标注，也未把早期 `chapter` 冒充为新 `chapter_fallback`。
