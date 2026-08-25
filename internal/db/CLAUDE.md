# 数据库层性能规范（MySQL 8.x，建表时逐字执行）

> 迁移文件在 `migrations/`（MySQL）和 `pgmigrations/`（PostgreSQL + pgvector），
> 查询在 `queries/` / `pgqueries/`，`make sqlc` 生成 `gen/` / `pggen/`。
> 分层与代码组织约定见 `internal/CLAUDE.md`。

## 通用字段约定

- **主键**：单表 `id CHAR(36) NOT NULL PRIMARY KEY`，值用 **UUIDv7**（`google/uuid` 的 `uuid.NewV7()`），不用 UUIDv4/数据库 `UUID()` 函数——UUIDv7 前缀是时间戳、单调递增，InnoDB 按主键聚簇存储，随机的 UUIDv4 会导致插入时随机写入、页分裂，大表（messages/chunks）写入性能会明显劣化。纯多对多关联表（`agent_knowledge_bases`、`agent_mcp_tools`）不单独生成 id 列，用两个外键列组成复合主键 `PRIMARY KEY (a_id, b_id)`。
- **时间戳**：`created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)`，`updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3)`。用 `DATETIME(3)` 不用 `TIMESTAMP`，应用层统一按 UTC 写入，MySQL 连接串固定 `time_zone='+00:00'`。
- **软删除 vs 硬删除**：主数据表（users/model_providers/agents/knowledge_bases/mcp_servers/workflows）用软删除，统一用 `is_active TINYINT(1) NOT NULL DEFAULT 1`，不额外加 `deleted_at`；纯记录/日志型表（messages/workflow_run_steps）不做软删除，要删就是真删。
- **字符集/引擎**：`ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC`。
- 枚举字段用 `VARCHAR(32) NOT NULL CHECK (col IN (...))`，不用原生 `ENUM`；布尔字段用 `TINYINT(1)`，前缀 `is_`/`has_`；小数字段用 `DECIMAL(3,2)` 不用 `FLOAT`；长文本用 `TEXT`/`LONGTEXT`，其余字符串给合理 `VARCHAR` 长度上限；字段默认 `NOT NULL`，只有业务上确实可能不存在才允许 NULL。

建表模板：
```sql
CREATE TABLE example_table (
    id CHAR(36) NOT NULL,                    -- UUIDv7，应用层生成
    -- 业务字段...
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_<fk列名> (<fk列名>)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;
```

## 索引设计原则

每个外键列必须显式建索引（MySQL 不会自动建）；复合索引列顺序按「等值过滤列在前、范围/排序列在后」；只为真实查询模式建索引。逐表索引清单：

```
users              PK(id), UNIQUE(email)
refresh_tokens     PK(id), INDEX(user_id), UNIQUE(token_hash), INDEX(expires_at)
model_providers    PK(id), INDEX(created_by)
agents             PK(id), INDEX(provider_id), INDEX(created_by)
conversations      PK(id), INDEX(agent_id), INDEX(user_id, updated_at DESC)
messages           PK(id), INDEX(conversation_id, created_at, id)
knowledge_bases    PK(id), INDEX(created_by)
documents          PK(id), INDEX(knowledge_base_id, status)
chunks             PK(id), INDEX(knowledge_base_id), INDEX(document_id)
agent_knowledge_bases  PK(agent_id, knowledge_base_id), INDEX(knowledge_base_id)
mcp_servers        PK(id)
mcp_tools          PK(id), INDEX(mcp_server_id)
agent_mcp_tools    PK(agent_id, mcp_tool_id), INDEX(mcp_tool_id)
workflows          PK(id), INDEX(created_by)
workflow_runs      PK(id), INDEX(workflow_id, started_at DESC)
workflow_run_steps PK(id), INDEX(workflow_run_id, started_at)
```

低基数列不单独建索引，并入复合索引做前置过滤列。JSON 列不能直接建索引；真出现按 JSON 内部字段过滤的需求时用 Generated Column + 索引。

## 大表预判和应对策略

`messages`、`chunks`、`trace_spans` 是会持续增长到百万行以上的表。

1. **大表查询必须始终带强选择性过滤条件**——messages 永远 `WHERE conversation_id=?`、chunks 永远 `WHERE knowledge_base_id=?`、workflow_run_steps 永远 `WHERE workflow_run_id=?`、trace_spans 永远 `WHERE conversation_id=?`，禁止不带这类条件的查询。
2. **分区不在当前阶段做**，触发条件：messages 单表超过约 2000 万行，或某查询稳定超过 200ms 再评估按 created_at 做 RANGE 分区。
3. **有 TTL 语义的表必须有清理任务**：`refresh_tokens` 过期/已撤销的行需要定期任务（asynq periodic task）清理。
4. 归档留口子不留实现：日志型大表不建会阻碍未来归档的强级联约束。

## 分页查询注意事项

- **大表（messages/chunks/workflow_run_steps）一律用游标分页，不用 OFFSET/LIMIT**：排序键 `(created_at, id)` 复合，`WHERE conversation_id=? AND (created_at,id) < (:last_created_at,:last_id) ORDER BY created_at DESC, id DESC LIMIT :n`；前端用"加载更多"而不是"共 X 页"。
- **小表（管理后台列表：providers/users/agents/workflows）可以直接用 OFFSET/LIMIT**。
- 任何分页接口的 `LIMIT` 服务端强制设硬上限（如 200）。
