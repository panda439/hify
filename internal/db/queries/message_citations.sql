-- name: CreateMessageCitation :exec
-- 单条插入，由 repository.go 在写 assistant message 的同一个 MySQL 事务里
-- 循环调用（一轮 turn 最多 maxTopK=50 条，批量不值得单独写一条多值 INSERT）。
INSERT INTO message_citations (
    message_id, ref, knowledge_base_id, document_id, document_name,
    chunk_id, chunk_index, quote, score
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListCitationsByMessageIDs :many
-- 历史消息接口的批量加载入口——一次查询覆盖一页消息里所有 assistant
-- message 的 citations，避免按每条消息各查一次（见 CLAUDE.md N+1 规则）。
-- ORDER BY message_id 让调用方按 message_id 分组后，组内已经是 ref 的字符
-- 序（S1 < S10 < S2 ...）——repository.go 按数字排序重排，不依赖这里的顺序。
SELECT message_id, ref, knowledge_base_id, document_id, document_name,
       chunk_id, chunk_index, quote, score, created_at
FROM message_citations
WHERE message_id IN (sqlc.slice('message_ids'))
ORDER BY message_id;
