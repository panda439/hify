-- Citation V1 需要的来源快照字段，写在 chunk 行上而不是 join 回 MySQL
-- documents 表——两个原因：(1) chunks/documents 分属两个数据库，检索路径
-- 不能跨库 join；(2) 这是"这个 chunk 当时来自哪份文档的哪一次处理"的快照，
-- 文档改名/重新处理不应该改变已经检索出去、甚至已经被引用保存的历史
-- document_name。
--
-- page_number/section_title 允许 NULL——当前解析器（parse.go）不产出可靠的
-- 页码/章节信息，NULL 就是"这次处理没有这项数据"，不是错误，绝不允许伪造。
--
-- 存量行（这次迁移之前已写入的 chunk）document_name 迁移后为空字符串
-- ''——不跨库回填 MySQL，见 repository.go/service.go 的 fallback 策略：
-- 检索/引用返回时用 document_id 兜底展示，不影响检索可用性；重新处理
-- 该文档后新版本 chunk 才会带上完整 document_name。
ALTER TABLE chunks ADD COLUMN document_name text NOT NULL DEFAULT '';
ALTER TABLE chunks ADD COLUMN page_number integer NULL;
ALTER TABLE chunks ADD COLUMN section_title text NULL;
