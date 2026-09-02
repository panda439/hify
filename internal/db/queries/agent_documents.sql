-- 004-agent-document-scope：Agent 的检索文档范围。语义、以及"为什么不建外键、
-- 不级联删除"的完整论证见 migrations/000014_agent_documents.up.sql 的注释。
-- 写入沿用 agent_knowledge_bases 的 replace-all 语义（先全删再全插，同一事务内）。

-- name: CreateAgentDocument :exec
INSERT INTO agent_documents (agent_id, document_id) VALUES (?, ?);

-- name: DeleteAgentDocuments :exec
-- 更新时先清空再整体重插——与 DeleteAgentKnowledgeBases 同一套 replace-all 语义。
DELETE FROM agent_documents WHERE agent_id = ?;

-- name: ListDocumentIDsByAgent :many
-- ORDER BY document_id 是确定性兜底：这个集合会变成 RetrieveFilter.DocumentIDs
-- 进而成为召回 SQL 的 IN 条件。条件本身的顺序不影响匹配结果，但顺序稳定能让
-- 日志、诊断和测试断言可复现，不依赖 MySQL 的返回顺序（宪法第 V 条）。
SELECT document_id FROM agent_documents WHERE agent_id = ? ORDER BY document_id;
