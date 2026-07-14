-- name: CreateAgentKnowledgeBase :exec
INSERT INTO agent_knowledge_bases (agent_id, knowledge_base_id) VALUES (?, ?);

-- name: DeleteAgentKnowledgeBases :exec
-- Called before re-inserting the full set on update — see
-- agent.Service's replace-all semantics for this association.
DELETE FROM agent_knowledge_bases WHERE agent_id = ?;

-- name: ListKnowledgeBaseIDsByAgent :many
SELECT knowledge_base_id FROM agent_knowledge_bases WHERE agent_id = ?;

-- name: ListAgentIDsByKnowledgeBase :many
-- Reverse lookup for "which Agents use this knowledge base" — surfaced in
-- the knowledge base management UI before disabling one.
SELECT agent_id FROM agent_knowledge_bases WHERE knowledge_base_id = ?;
