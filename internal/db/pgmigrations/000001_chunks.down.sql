-- 只删表不删 extension：vector 扩展是库级共享设施，回滚本迁移不该连带
-- 破坏未来可能依赖它的其他对象；重复 CREATE EXTENSION IF NOT EXISTS 也无害。
DROP TABLE IF EXISTS chunks;
