-- 回滚会丢掉"哪些页没进去"这条信息，但**不影响任何文档的可用性与可检索性**
-- ——这一列从来不参与检索，只用于展示。重新处理文档即可再次得到它。
ALTER TABLE documents DROP COLUMN unextracted_pages;
