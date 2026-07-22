-- 一次 conversation.StreamMessage 调用（用户发一条消息，模型可能经过多轮工具
-- 调用后给出最终回复）算一个 trace；根 span（kind=turn）的 id 直接复用
-- trace_id 本身，子 span（retrieval/llm_call/tool_call）的 parent_span_id
-- 都指向它。和 workflow_run_steps 一样是纯粹的执行轨迹：只在一步真正结束
-- （成功或失败）后 insert 一次，不存在"运行中"的中间行，也不做软删除。
-- attrs 按 OpenTelemetry GenAI 语义约定命名字段（gen_ai.request.model /
-- gen_ai.usage.* 等），为将来可能对接 Jaeger/Langfuse 之类工具保留数据格式
-- 兼容性，但当前阶段只落自己的表，不接任何外部导出。
CREATE TABLE trace_spans (
    id CHAR(36) NOT NULL,
    trace_id CHAR(36) NOT NULL,
    parent_span_id CHAR(36) NOT NULL DEFAULT '',
    conversation_id CHAR(36) NOT NULL,
    kind VARCHAR(16) NOT NULL,
    name VARCHAR(64) NOT NULL,
    status VARCHAR(16) NOT NULL,
    input TEXT NOT NULL,
    output TEXT NOT NULL,
    error_message TEXT NOT NULL,
    attrs JSON NOT NULL,
    started_at DATETIME(3) NOT NULL,
    finished_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    KEY idx_trace_spans_conversation (conversation_id, started_at),
    KEY idx_trace_spans_trace (trace_id, started_at),
    CONSTRAINT chk_trace_spans_kind CHECK (kind IN ('turn', 'retrieval', 'llm_call', 'tool_call')),
    CONSTRAINT chk_trace_spans_status CHECK (status IN ('ok', 'error'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;
