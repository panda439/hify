-- 回滚丢掉「哪些页解析失败」这条信息，不影响任何文档的可用性与可检索性
-- ——这一列从不参与检索。重新处理文档即可再次得到它。
ALTER TABLE documents DROP COLUMN unparseable_pages;
