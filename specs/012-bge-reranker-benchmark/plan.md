# Implementation Plan: BGE Reranker 真实对照评测

**Branch**: `012-bge-reranker-benchmark` | **Date**: 2026-09-10 | **Spec**: [spec.md](./spec.md)

## Summary

为 Hify 已有 `/rerank` 与 `knowledge.Service.Retrieve` 链路提供一个本地 `BAAI/bge-reranker-v2-m3` sidecar，在 011 固定 MIRACL 中文 Mini 数据上运行“关闭/开启 Rerank”的单变量 A/B。实验分为 1.5 秒部署门禁和 30 秒质量诊断：前者判断当前部署是否可用，后者只判断模型排序效果，不修改生产默认值。

## Technical Context

**Language/Version**: Go（项目现有版本）；Python 3.12（uv 隔离）  
**Primary Dependencies**: Hify provider/knowledge/retrievalbench；FastAPI、Uvicorn、SentenceTransformers/Transformers、PyTorch  
**Storage**: 现有 MySQL/PostgreSQL/Redis；模型与运行缓存写 `eval/cache/`；小报告写 `docs/`  
**Testing**: Go `testing` + race；Python pytest；真实本地 `/health`、`/rerank` 和 50-query benchmark  
**Target Platform**: macOS Apple Silicon，M1 Pro、32GB  
**Project Type**: Go Web 项目附带离线评测 CLI 和本地模型 sidecar  
**Performance Goals**: 1.5 秒部署门禁保持原结论；30 秒质量诊断力求 50 次 Rerank 零失败零降级，并记录稳态 p50/p95  
**Constraints**: 唯一效果变量为 Rerank；不改 011 数据、Embedding、分块、Hybrid 或指标口径；不默认生产启用  
**Scale/Scope**: 50 queries、800 passages、最多 50 个候选/请求

## Constitution Check

- **I 归属**: 文档标注为 AI 辅助学习实验，不包装成用户独立手写的生产系统。
- **II 规格**: spec 已确认；本 plan 后生成 tasks，实施前再次确认。
- **III 分层**: 仅复用 provider → knowledge 既有依赖；Python sidecar 不进入 `internal/`。
- **IV 顺序**: 不新增业务持久化结构；评测类型先于比较/CLI。
- **V 确定性**: A/B、决策、percentile 和响应映射写纯函数测试；固定 tie-break。
- **VI 证据**: Go/Python 测试、真实服务、真实 benchmark、资源快照和检索门禁均要有原始结果。
- **VII 语言**: specs/docs 中文；内部 Go 错误英文；sidecar 错误不泄露路径和凭据。
- **VIII 提交**: 不自动 commit/push。
- **IX 范围**: 不加入第二模型、自动调参、微调、UI 或生产部署。

结论：无宪法偏离。

## Architecture

```text
MIRACL Mini + 011 KB
        │
retrievalbench run（Rerank enabled）
        │
knowledge.Service.Retrieve
        │
Hybrid candidate pool ──> provider.Client.Rerank
                              │ HTTP /rerank
                              ▼
                    local Python sidecar
                    bge-reranker-v2-m3
        │
raw run → score → experiment compare → decision/report
```

## Components

### 1. Local Rerank Sidecar

放在 `eval/rerank-service/`。`uv` 管理 Python 3.12 和锁文件；服务启动时加载固定模型 revision，暴露 `/health` 与 `/rerank`。模型缓存通过 `HF_HOME=eval/cache/rerank-models` 与仓库产物隔离。

Sidecar 只负责 query-passage 打分和模型身份，不访问 Hify 数据库，不实现召回、截断或业务降级。它额外暴露不含正文的累计 `/stats`；benchmark 用 run 前后差值证明真实调用次数并计算稳态延迟。

### 2. Benchmark Runtime Wiring

扩展 `cmd/retrievalbench`：新增 Rerank 预检/配置步骤；通过现有 provider.Service 幂等创建或复用本地无鉴权 provider 与 capability=`rerank` 模型；构建 knowledge service 时显式启用该模型。所有 provider/model/KB ID 写入 gitignored checkpoint，避免重复创建。

基准查询仍只调用 `knowledge.Service.Retrieve`。评测模式必须把 Hify 的生产“失败后继续回答”降级转换为实验失败证据；不能把降级后的 baseline 排名计作 Rerank 成功。

### 3. Experimental Fingerprint and Compare

011 严格 compare 保持不变。新增 experiment compare：先剥离并校验 Rerank 字段是唯一差异，再调用同一指标 delta 逻辑。代码 revision 允许不同但必须显示。

### 4. Decision Gate

纯函数输入 baseline/candidate/comparison/runtime stats：

- complete 且兼容；
- MRR@10 > `0.626746`；
- NDCG@10 > `0.696447`；
- Recall@10 >= `0.976`；
- 50 query、零失败、零降级。

全部满足为 `ADOPT`；有效实验但条件不足为 `DO_NOT_ADOPT`；不完整或不兼容为 `INCONCLUSIVE`。

### 5. Resource Evidence

启动前后采集可用内存、swap 和 sidecar RSS；模型首次加载单独计冷启动。查询延迟从每次真实 Retrieve/Rerank 记录重算 p50/p95。资源采样失败不伪造 0，报告 `unavailable` 并使资源验收未完成。

### 6. Quality Diagnostic

Benchmark 增加显式 `quality_diagnostic` 模式，其超时固定为 30 秒，且写入指纹。该模式仍走同一 provider 和 `knowledge.Service.Retrieve`，不绕开 Hify。只有 Hify observer 证明 50 次全部 applied 时，才计算 `QUALITY_PASS` / `QUALITY_FAIL`；否则为 `QUALITY_INCONCLUSIVE`。

`QUALITY_PASS` 沿用排序门禁：MRR@10 和 NDCG@10 均高于 011，Recall@10 不低于 011。它不能覆盖 1.5 秒部署结论，也不能直接触发 `ADOPT`。

## Error Handling

1. Python/依赖/模型下载失败：停止在 precheck，不执行真实 run。
2. `/health` 身份或许可不符：失败，不接受模型别名猜测。
3. `/rerank` 超时、非 2xx、缺失/重复/越界 index、非有限分数：Hify 可按既有逻辑降级，但 benchmark 标记 incomplete。
4. 011 baseline 缺失或哈希不符：不重新生成冒充历史，报告阻塞。
5. A/B 发现非 Rerank 字段差异：`NON_COMPARABLE` / `INCONCLUSIVE`。
6. 模型效果未过门禁：输出 `DO_NOT_ADOPT`，不修改生产默认配置。

## Project Structure

```text
eval/
├── rerank-service/
│   ├── pyproject.toml
│   ├── uv.lock
│   ├── app.py
│   └── test_app.py
└── cache/                         # 已 gitignore

cmd/retrievalbench/
├── main.go                        # 扩展 precheck/run/experiment compare
└── main_test.go

internal/eval/retrievalbench/
├── model.go                       # Rerank identity/stats/fingerprint/decision
├── compare.go                     # 单变量实验比较
├── compare_test.go
├── decision.go
└── decision_test.go

specs/012-bge-reranker-benchmark/
├── spec.md
├── research.md
├── plan.md
├── data-model.md
├── quickstart.md
├── contracts/local-rerank-service.md
└── tasks.md

docs/
└── eval-phase19-bge-reranker-report.md
```

**Structure Decision**: Sidecar 属于评测工具而非生产业务模块，放 `eval/`；Go 侧只扩展 011 benchmark 与现有内部适配，不新增 HTTP 业务 API。

## Verification

1. Python contract/unit tests先 RED 后 GREEN；固定样本验证相关分高于无关文档。
2. Go compare/decision/runtime stats 纯函数测试先 RED 后 GREEN，含 mutation 证明。
3. Sidecar 真实加载固定 revision，`/health` 与 `/rerank` smoke test。
4. 50-query Rerank run 必须证明真实调用数、零失败/降级并保存资源与延迟。
5. 同一 raw run 两次 score 一致；实验 compare 只接受 Rerank 唯一差异。
6. `go test -race -count=1 ./...`、`go vet ./...`、`make check-deps`、Python tests、`git diff --check`。
7. `make eval-retrieval-gate` 14/14；011 保存 run 纯重算结果不变。
8. 30 秒质量 run 必须独立保存，断言 Hify applied/degraded 真实计数，并验证不修改生产默认值。

## Complexity Tracking

无宪法违规。新增 Python sidecar 是模型运行边界所需的最小独立组件；直接嵌入 Go 或绕过 Hify benchmark 都会降低真实性与隔离性。
