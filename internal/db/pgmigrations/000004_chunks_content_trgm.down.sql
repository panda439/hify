-- 只回滚这条迁移自己引入的东西：本阶段建的索引、本阶段设的 GUC 默认值。
-- 不卸载 pg_trgm 扩展本身——CREATE EXTENSION IF NOT EXISTS（up 迁移里）
-- 证明不了这个扩展是本迁移创建的，数据库里完全可能因为其他模块早就装
-- 了它。pg_trgm 一旦启用就是数据库级别的共享能力，其生命周期不归任何
-- 一条迁移独占管理；DROP EXTENSION 会把其他潜在使用方依赖的能力一并
-- 拆掉，这条回滚不做这个越权判断。
DO $$
BEGIN
    EXECUTE format('ALTER DATABASE %I RESET pg_trgm.word_similarity_threshold', current_database());
END
$$;
RESET pg_trgm.word_similarity_threshold;

DROP INDEX IF EXISTS idx_chunks_content_trgm;
