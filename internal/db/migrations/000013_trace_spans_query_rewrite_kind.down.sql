-- 回退前必须确认不存在 kind='query_rewrite' 的行，否则收紧约束会失败——
-- 和 000012 收紧 provider_models.capability 的回退是同一个理由：回退不
-- 应该悄悄丢弃已经记录的 query_rewrite span 行。
ALTER TABLE trace_spans DROP CHECK chk_trace_spans_kind;
ALTER TABLE trace_spans
    ADD CONSTRAINT chk_trace_spans_kind
    CHECK (kind IN ('turn', 'retrieval', 'llm_call', 'tool_call'));
