-- 001-rag-query-rerank US3/T034：新增 kind='query_rewrite' 子 span（见
-- internal/platform/trace/span.go 的 KindQueryRewrite），parent_span_id 指
-- 向本轮根 span，和既有的 retrieval/llm_call/tool_call 是同一层级的兄弟
-- span。列类型 VARCHAR(16) 不变，只把 CHECK 约束的合法取值集合扩一项，同
-- 000012 收紧 provider_models.capability 的先例一致的做法。
ALTER TABLE trace_spans DROP CHECK chk_trace_spans_kind;
ALTER TABLE trace_spans
    ADD CONSTRAINT chk_trace_spans_kind
    CHECK (kind IN ('turn', 'retrieval', 'llm_call', 'tool_call', 'query_rewrite'));
