# Phase 18：MIRACL 中文 Mini 真实检索评测报告

日期：2026-09-10。数据来源为 MIRACL v1.0 中文 dev，Apache-2.0；本报告只表示固定 50 query / 800 document Mini 子集表现，不等同完整 MIRACL 排行榜或生产中文 RAG 效果。

固定选择 seed=11；manifest 固定字段：500 qrels、qrels SHA-256 `651a380bb5c48b72e8a4b14f7a6ca122f33c9b4758c16840f0642c0fc121d983`，corpus SHA-256 `a59920984ee748f565aafce79d4ce30808adecb6e42aec3c68b7f4c4c9a9e298`。上游 topics/qrels/corpus 来源和下载缓存只保留在本地 gitignored 路径。

## 机制验证

- 本轮审核修复保留 TDD 证据：RED 阶段执行 `go test ./internal/eval/retrievalbench ./cmd/retrievalbench`，因阶段类型与 fingerprint 构造函数尚不存在而退出 1；GREEN 阶段同命令及新增比较测试通过，随后 `go test -race -count=1 ./...` 通过。
- 纯函数测试覆盖 document 首次出现去重、空/失败 query、单/多正例、分级 relevance、Recall/Precision/MRR/AP/NDCG，以及 compare 兼容性。
- prepare 从标准 topics/qrels TSV 与 corpus JSONL.GZ 读取，保留正负 judgment，按 seed hash 补足 unjudged 文档；两次 prepare manifest 与内容哈希一致。
- score 从 raw run 纯重算，不调用模型或数据库；两次重算逐字节一致；同报告 compare 为 `IDENTICAL`。

## 真实 Mini 效果

真实运行使用 Hify 正常 `UploadDocument` → `ProcessDocument` → Embedding 路径与 `knowledge.Service.Retrieve`，Ollama `bge-m3:567m`，digest `7907646426070047a77226ac3e684fbbe8410524f7b4a74d02837e43f2146bab`，维度 1024。服务地址只保存脱敏标识 `sha256:b26993598dffd1f1`；隔离知识库 ID 为 `01a08adf-576e-72bb-af57-32bb50e67000`。

阶段证据：prepare 800 documents/50 queries、2 ms；Embedding 800 documents/822 chunks、81623 ms；入库 800 ready/0 failed/822 chunks、89933 ms；查询 50/50、3681 ms。本地 Embedding 成本标记 `not_applicable`，不写入虚构美元金额。分块指纹为 `{"chunk_size":500,"chunk_overlap":50}`，检索指纹为 `{"top_k":10,"rerank_enabled":false,"metadata_filter_enabled":true}`。

| K | Recall | Precision | MRR | MAP | NDCG |
|---:|---:|---:|---:|---:|---:|
| 1 | 0.237667 | 0.460000 | 0.460000 | 0.460000 | 0.460000 |
| 3 | 0.525000 | 0.380000 | 0.583333 | 0.447778 | 0.515064 |
| 5 | 0.738000 | 0.332000 | 0.606333 | 0.508622 | 0.598649 |
| 10 | 0.976000 | 0.222000 | 0.626746 | 0.574051 | 0.696447 |

制品位于 gitignored 的 `eval/cache/miracl-zh-mini/`、`eval/runs/miracl-zh-mini-latest.json` 和 `eval/runs/miracl-zh-mini-report.json`，不保存 embedding 向量。

## 边界与下一步

Mini 子集缩小了检索空间，未评审文档按不相关计但不代表人工判错；不得与完整 MIRACL 或生产中文 RAG 横向比较。首次入库受本地 provider 默认 300 RPM 限制产生 500 个失败文档，随后在同一 checkpoint 恢复完成；这证明失败保留和恢复路径，但不构成质量阈值。后续优化应在同一 manifest、模型 digest、chunk 与检索配置下做 baseline 对照，并分别报告真实效果与机制回归。
