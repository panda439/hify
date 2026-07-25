DROP INDEX IF EXISTS idx_chunks_document_id_version;
ALTER TABLE chunks DROP COLUMN is_published;
ALTER TABLE chunks DROP COLUMN document_version;
