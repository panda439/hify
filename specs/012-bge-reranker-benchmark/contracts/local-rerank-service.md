# Contract: 本地 Rerank Sidecar

## GET /health

成功响应：

```json
{
  "ready": true,
  "model": "BAAI/bge-reranker-v2-m3",
  "revision": "resolved-model-revision",
  "license": "Apache-2.0",
  "runtime": {
    "python": "3.12.x",
    "torch": "resolved-version",
    "transformers": "resolved-version"
  }
}
```

模型未加载完成时返回 HTTP 503 和 `ready=false`。不得返回本机绝对缓存路径、token 或环境变量。

## GET /stats

返回进程启动以来的无正文计数：`request_count`、`success_count`、`failure_count`、`candidate_count_total`、稳态延迟样本与当前/峰值 RSS。Benchmark 在 run 前后各读取一次，以差值证明 Hify 的 50 条查询实际调用了模型。不得返回 query、passage 或逐条分数。

## POST /rerank

复用 [001 Rerank HTTP contract](../../001-rag-query-rerank/contracts/rerank-http-api.md)。请求包含 `model`、`query`、`documents`；响应必须为每个输入文档返回且只返回一次原 index 与有限数值 `relevance_score`。

固定样本预检必须证明：相关 passage 分数高于无关 passage，且结果不是恒定分数或输入原顺序复制。
