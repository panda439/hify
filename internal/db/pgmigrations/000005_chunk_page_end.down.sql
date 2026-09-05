-- ⚠️ 这次回滚是**不可逆的信息损失**：跨页片段「还覆盖到第 4 页」这件事没有
-- 任何别处存着，删列即丢失。page_number 本身不受影响（它仍然是一个有效的
-- 起始页），所以回滚不会让检索坏掉，只会把跨页信息退回改动前的精度——
-- 一个覆盖 3-4 页的片段重新表现为「第 3 页」。要恢复精度只能重新处理文档。
--
-- 先删约束再删列：约束引用了 page_end，反过来会被 PostgreSQL 拒绝。
ALTER TABLE chunks DROP CONSTRAINT IF EXISTS chunks_page_range_valid;
ALTER TABLE chunks DROP COLUMN page_end;
