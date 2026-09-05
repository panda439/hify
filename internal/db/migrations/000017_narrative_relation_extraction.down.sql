-- 逆序删表：先删引用方，再删被引用方（虽然本期故意不建跨表外键，
-- 顺序仍按依赖方向写，便于人读）。
DROP TABLE IF EXISTS narrative_relation_evidence;
DROP TABLE IF EXISTS narrative_relations;
DROP TABLE IF EXISTS narrative_aliases;
DROP TABLE IF EXISTS narrative_characters;
DROP TABLE IF EXISTS relation_extraction_attempts;
DROP TABLE IF EXISTS relation_extraction_items;
DROP TABLE IF EXISTS relation_extraction_jobs;

ALTER TABLE documents DROP CONSTRAINT chk_documents_relation_requires_narrative;
ALTER TABLE documents
    DROP KEY idx_documents_relation_recovery,
    DROP KEY idx_documents_active_relation_job_id,
    DROP KEY idx_documents_relation_model_id;
ALTER TABLE documents
    DROP COLUMN active_relation_job_id,
    DROP COLUMN relation_model_id,
    DROP COLUMN is_relation_extraction_enabled,
    DROP COLUMN is_narrative;
