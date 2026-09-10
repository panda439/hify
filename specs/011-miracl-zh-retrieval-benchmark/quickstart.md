# Quickstart: MIRACL 中文 Mini 检索评测

> 本文件中的命令名是本期契约；实施完成前不可当作已可运行证据。

## 1. 前置检查

```bash
make app-down
make db-up
ollama list
curl -sS http://127.0.0.1:11434/api/tags
```

必须确认 `bge-m3:567m` 实际存在，并记录 digest；不能只根据配置猜模型可用。

## 2. 准备 Mini 数据

```bash
make eval-retrieval-benchmark-prepare
```

预期：50 query、500～1000 documents、所有正 qrel 有对应文档；重复执行固定哈希一致。

## 3. 入库

```bash
make eval-retrieval-benchmark-ingest
```

预期：使用真实 `bge-m3` 正常解析、分块、Embedding、发布；报告 ready/failed document 与 chunk 数。失败不得继续伪装完整运行。

## 4. 运行与计分

```bash
make eval-retrieval-benchmark-run
make eval-retrieval-benchmark-score
```

预期：50 query 均有结果或明确错误；生成 Recall/Precision/MRR/MAP/NDCG @ 1/3/5/10。`score` 不调用数据库或模型。

## 5. 建立和比较基线

首次可靠运行由所有者明确决定是否复制为 baseline，工具不得自动覆盖：

```bash
go run ./cmd/retrievalbench compare \
  --baseline eval/runs/miracl-zh-mini-baseline.json \
  --candidate eval/runs/miracl-zh-mini-latest.json
```

同一报告自比必须输出 `IDENTICAL`。影响口径的指纹不一致必须拒绝比较。

## 6. 完整验收

```bash
make eval-retrieval-gate
go test -race -count=1 ./...
go vet ./...
make check-deps
git diff --check
```

数据库或 Ollama 不可用导致 skip/未运行，必须标记未验证。

