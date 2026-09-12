# Implementation Plan: MIRACL 中文真实检索评测

**Branch**: `011-miracl-zh-retrieval-benchmark` | **Date**: 2026-09-10 | **Spec**: [spec.md](spec.md)

## Summary

新增一个不依赖聊天模型的离线检索 benchmark：确定性准备 MIRACL 中文 Mini 数据，经 Hify 正常入库和真实 `bge-m3` Embedding 后，通过 `knowledge.Service.Retrieve` 获取 chunk 结果，折叠为 document ranking，计算标准信息检索指标并支持纯重算和兼容 baseline 对比。

## Technical Context

**Language/Version**: Go（跟随当前 `go.mod`）

**Primary Dependencies**: 现有 MySQL、PostgreSQL/pgvector/pg_trgm、Redis/asynq、Ollama OpenAI-compatible Embedding；不新增评测框架依赖

**Storage**: 隔离的 Hify benchmark 知识库；本地文件缓存与 JSON/JSONL 报告

**Testing**: Go table tests、真实数据库集成、真实 `bge-m3` benchmark、现有检索门禁

**Target Platform**: macOS/Linux 本地开发环境

**Project Type**: Hify 模块化单体中的开发者 CLI/评测工具

**Performance Goals**: 50 query、500～1000 documents；查询阶段可中断并恢复；报告阶段不重新生成 Embedding

**Constraints**: 不调用聊天模型；不提交大语料；不绕过正常入库和 Retrieve；不预设质量阈值

**Scale/Scope**: MIRACL zh Mini，非完整 benchmark、非生产流量

## Constitution Check

- **I 归属**：报告说明 AI 辅助实现及公开数据来源，不包装为个人生产经验
- **II 规格先行**：本目录 spec/plan/tasks 接受后才实施
- **III 分层**：通用纯指标位于 `internal/eval` 下；knowledge 适配只依赖同层内部实现，不新增反向依赖
- **IV 实现顺序**：本期无业务表 migration；先类型与纯指标，再数据准备、knowledge 适配、CLI
- **V 确定性**：数据选择、排序、document 去重、指标与报告均固定 tie-break
- **VI 证据验收**：纯函数测试、真实 DB/模型运行、旧门禁无 skip；skip 不算通过
- **VII 语言**：项目文档与用户错误中文；内部错误链英文
- **VIII 提交**：不 commit/push，等待所有者决定
- **IX 最小范围**：无 UI、CI、调参器、模型微调或完整 MIRACL

结论：无宪法偏离。

## Architecture

```text
MIRACL topics/qrels/corpus
        │ prepare（固定选择 + 校验 + manifest）
        ▼
Mini Dataset（本地 gitignored）
        │ ingest（正常 Upload/Process）
        ▼
Hify benchmark KB + bge-m3 vectors
        │ run（knowledge.Service.Retrieve）
        ▼
Raw Run（chunk ranking + errors）
        │ score（按 document 去重 + qrels）
        ▼
Metric Report
        │ compare（兼容性指纹）
        ▼
Baseline Delta
```

## Components

### 1. `internal/eval/retrievalbench`

职责：dataset manifest/run/report 类型、稳定选择、document ranking、Recall/Precision/MRR/AP/NDCG、聚合、兼容指纹和比较。

约束：纯评分逻辑不依赖数据库、provider 或 knowledge，便于构造反例测试。

### 2. MIRACL 数据准备器

读取固定 revision 的 topics/qrels/corpus，验证 qrels 引用，按 seed 选择 query，完整保留这些 query 的正负 judgment；不足目标时按稳定 hash 补充并标记 unjudged 干扰文档。写入原子临时目录，全部成功后 rename 为正式 Mini 目录并生成 SHA-256 manifest。

不得根据当前模型结果挑选数据。

### 3. Knowledge benchmark adapter

为公开 dataset document ID 建立稳定映射，使用独立知识库和正常文档处理链路生成真实向量。入库状态可恢复：已 ready 且内容哈希一致的文档跳过；失败文档明确记录并阻止 run 被标为 complete。

查询只调用 `knowledge.Service.Retrieve`。邻接 chunk 不单独作为文档排名项。

### 4. `cmd/retrievalbench`

建议子命令：

```text
prepare  下载/生成 Mini 数据与 manifest
ingest   建立或恢复真实 bge-m3 知识库
run      执行查询并保存 raw run
score    从 raw run 纯重算指标
compare  比较两份兼容报告
```

具体 flags 在 Luna 实施前先由失败的 CLI 解析测试固定；敏感配置只从现有环境读取，报告不保存凭据。

### 5. Makefile 与制品

新增 `eval-retrieval-benchmark-*` 薄入口，不复制 CLI 逻辑。生成内容统一放 `eval/cache/miracl-zh-mini/` 与 `eval/runs/`，前者整体 gitignore。

## Data Flow and Failure Handling

1. prepare 下载失败/校验失败：删除临时目录，不发布 manifest
2. ingest 某文档失败：保存 checkpoint，允许重试；不得开始 complete run
3. run 某 query 失败：记录错误并计零，报告 `complete=false`
4. score 输入缺 query/qrels 或重复冲突：非零退出，不生成成功报告
5. compare 指纹不同：非零退出并列出不兼容字段
6. Ctrl-C：已完成阶段产物保留，原子写入的当前文件不留下半成品

## Metric Semantics

- 评分前按 `document_id` 首次出现去重，再截取 K
- relevance > 0 为相关；NDCG 使用原始 relevance 等级
- Precision@K 分母固定为 K，返回不足 K 的位置按不相关计，和标准 P@K 口径一致
- AP@K 分母使用 `min(正相关文档总数, K)`；MAP 为 AP 的 query 均值
- 所有 query 等权；失败 query 指标为零且 run incomplete
- 未评审文档按标准 qrels 口径视为不相关，同时在报告披露 pooling 局限

## Baseline Compatibility Fingerprint

至少包含：dataset 名称/revision、query IDs、document IDs、qrels hash、正文 hash、选择配置、chunk 配置、Embedding 模型名/digest/维度、检索配置、K、指标版本、代码 revision。

代码 revision 不同允许比较（否则无法比较改动），但必须显示；其余影响结果口径的字段不同则默认拒绝。允许显式 override 只用于诊断，输出必须醒目标记 `non_comparable=true`，不得产生正式 delta。

## Project Structure

```text
cmd/retrievalbench/
└── main.go

internal/eval/retrievalbench/
├── model.go
├── dataset.go
├── metrics.go
├── report.go
└── compare.go

internal/knowledge/
└── benchmark_adapter.go       # 最小内部适配；不得扩大生产 HTTP API

eval/
├── benchmarks/miracl-zh-mini.yaml
├── cache/miracl-zh-mini/      # gitignored 生成内容
└── runs/

scripts/
└── （仅在 Go 无法可靠完成上游下载时才增加薄下载脚本）
```

## Verification Strategy

1. 指标纯函数：手算构造样本逐项相等，突变排名能使指标按预期失败
2. dataset：重复 prepare 哈希一致；缺文档/qrel/重复冲突全部失败
3. adapter：真实 MySQL/PostgreSQL 与本地 bge-m3，确认正常入库、恢复、document 映射和 Retrieve
4. benchmark：50 query 完整运行并纯重算一致
5. compare：自比 IDENTICAL，单排名突变产生预期 delta，不兼容报告拒绝
6. 回归：`make eval-retrieval-gate` 基线 IDENTICAL，再跑全量 race/vet/deps/diff

## Complexity Tracking

无宪法违规。独立 CLI 是开发者工具，不进入业务 API；拆阶段是为了避免每次失败重做真实 Embedding。
