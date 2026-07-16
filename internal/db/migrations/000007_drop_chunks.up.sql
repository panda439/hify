-- chunks 已整表迁往 PostgreSQL（pgvector，见 pgmigrations/000001）。
-- 运行顺序警告：存量数据必须先跑 `hify backfill-chunks` 再执行本迁移——
-- 本文件直接 drop 源表。chunks 是派生数据，顺序搞错的兜底是重新上传文档。
DROP TABLE IF EXISTS chunks;
