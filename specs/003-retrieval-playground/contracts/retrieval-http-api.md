# HTTP 契约：试检索端点

**Feature**: `003-retrieval-playground` | **Date**: 2026-09-02

## `POST /api/v1/knowledge-bases/:id/retrieve`

在**单个**知识库内做一次检索，返回召回的片段。**这是一次查询，不创建任何资源**——
用 POST 只是因为请求体里有文档 ID 数组和问题原文，且问题原文不应进 URL（会落进访问日志）。

**鉴权**：需要登录（`RequireAuth`）。不限创建者/管理员——与 `GET /knowledge-bases/:id` 一致。

### 请求

```json
{
  "query": "部署流程是怎样的",
  "top_k": 5,
  "document_ids": ["01930f...", "01930g..."],
  "page_min": 10,
  "page_max": 15
}
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `query` | 是 | 问题原文，空字符串视为非法请求 |
| `top_k` | 否 | 缺省 5；沿用 `clampTopK`，超过 50 静默收敛到 50 |
| `document_ids` | 否 | 最多 50 个（002 的 `maxFilterDocumentIDs`）；省略/空数组 = 不限文档 |
| `page_min` / `page_max` | 否 | 1-indexed 闭区间；两端都可单独省略 |

三个过滤字段全部省略时，等价于 002 的空过滤器 —— 行为与该功能上线前逐字一致。

### 响应 200

```json
{
  "chunks": [
    {
      "id": "01930h...",
      "document_id": "01930f...",
      "document_name": "部署手册-2026.pdf",
      "page_number": 12,
      "content": "……",
      "score": 0.83,
      "is_neighbor": true,
      "neighbor_of": "01930i..."
    }
  ],
  "hit_count": 3,
  "neighbor_count": 2,
  "filter_applied": true
}
```

| 字段 | 说明 |
|---|---|
| `page_number` | **可为 null**——txt/md 与 000003 迁移前的存量行没有页码。null 是"没有这项数据"，不是错误 |
| `is_neighbor` | 邻接块（002 FR-011：豁免页码过滤）。前端必须据此在视觉上区分 |
| `neighbor_of` | 该邻接块是为哪个命中块补充上下文的；`is_neighber=false` 时为空串 |
| `hit_count` / `neighbor_count` | 命中数与邻接数，供 US3 区分"过滤生效但没答案" |
| `filter_applied` | 本轮是否施加了过滤 |

`chunks` 为空数组时 HTTP 仍是 200——"没找到"不是错误。

### 错误

| HTTP | code | 触发条件 |
|---|---|---|
| 400 | `knowledge.invalid_request` | `query` 为空、请求体不是合法 JSON |
| 400 | `knowledge.too_many_filter_documents` | `document_ids` 超过 50（002 FR-015，**不截断**） |
| 400 | `knowledge.invalid_page_range` | 页码非正数，或 `page_min > page_max` |
| 400 | `knowledge.metadata_filter_disabled` | 指定了过滤条件但 `HIFY_RAG_METADATA_FILTER_ENABLED=false` |
| 404 | `knowledge.not_found` | 知识库不存在 |

后三个直接来自 002，handler **原样 `return err`**，不做转换、不吞掉、不降级成空结果（FR-006）。

### 明确不做的事（FR-005）

不调用任何对话模型、不创建会话或消息、不写 `trace_spans`。
它只是 `knowledge.Service.Retrieve` 的一层 HTTP 包装。
