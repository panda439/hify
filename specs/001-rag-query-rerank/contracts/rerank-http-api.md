# 契约：Rerank 服务 HTTP 接口（Hify 作为调用方）

**Feature**: `001-rag-query-rerank`

Hify 通过 `provider.Client.Rerank` 调用外部重排序服务。这是**消费方契约**——Hify 不提供该接口，
而是要求被接入的供应商满足下述形状。已知满足者：Jina Reranker、SiliconFlow、Cohere v2、
TEI（text-embeddings-inference）、Xinference、vLLM。

## 请求

```http
POST {provider.base_url}/rerank
Authorization: Bearer {provider.api_key}
Content-Type: application/json
```

`base_url` 直接取供应商已配置的值（与 chat/embedding 共用），路径拼接为 `strings.TrimSuffix(base_url, "/") + "/rerank"`。

```json
{
  "model": "BAAI/bge-reranker-v2-m3",
  "query": "Hify 文档分块策略的分块大小上限是多少",
  "documents": ["候选片段 1 正文", "候选片段 2 正文"],
  "top_n": 50,
  "return_documents": false
}
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `model` | 是 | `provider_models.model_name` |
| `query` | 是 | 检索问题（可能是改写后的独立问题） |
| `documents` | 是 | 候选片段正文，**顺序即索引**，长度 ≤ `rerankInputLimit`(50) |
| `top_n` | 是 | 取 `len(documents)`，即要求全量打分回传 |
| `return_documents` | 是 | 固定 `false`，避免把正文原样回传，省带宽也减少日志泄漏面 |

## 响应（200）

```json
{
  "results": [
    { "index": 3, "relevance_score": 0.94 },
    { "index": 0, "relevance_score": 0.71 }
  ]
}
```

- `results` 的顺序**不可依赖**：Hify 一律按 `index` 回填到自己的候选列表，再自行排序。
- 允许响应中额外携带 `document`、`usage` 等字段，Hify 忽略。

## Hify 侧的强制校验（FR-011）

收到 200 之后，满足以下**任意一条**即判定响应不可信，**整体丢弃并保持融合排序**，禁止部分采用：

1. `results` 为空，或长度 ≠ 送入的 `documents` 长度；
2. 存在 `index < 0` 或 `index >= len(documents)`；
3. 存在重复 `index`；
4. 缺失任一 `index`（0..n-1 未被完整覆盖）；
5. JSON 解析失败，或 `relevance_score` 非数值。

## 错误处理

| 情况 | Hify 行为 |
|---|---|
| 非 2xx 状态码 | 归类为供应商错误，走既有 `classifyError` + 熔断器计数，本轮降级 |
| 429 / `Retry-After` | 复用 `resilience.go` 既有的 retry-after 处理路径 |
| 超时（`HIFY_RAG_RERANK_TIMEOUT`，默认 1500ms） | 立即降级，不重试 |
| 连接失败 | 立即降级，熔断器计数 |

**任何错误都不得向上冒泡成对话失败**（FR-014）。`Rerank` 方法本身照常返回 error，
由 `knowledge` 的调用点吞掉并降级——错误信息只进 `slog.Warn`，不进用户可见响应。
