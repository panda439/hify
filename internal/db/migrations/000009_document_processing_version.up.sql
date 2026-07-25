-- version 是"当前处理尝试"的身份编号，不是重试计数——只在发起一次新尝试
-- 时才递增（人工重试 / reconciliation 认领卡死任务），正常的
-- pending -> processing -> ready 全程不变。ProcessDocument 的所有状态转换
-- 都是带 version 的 CAS（WHERE id=? AND version=? AND status=...），version
-- 不匹配即视为过期任务，静默退出，防止旧 worker 覆盖新一轮处理的结果。
ALTER TABLE documents ADD COLUMN version BIGINT NOT NULL DEFAULT 1 AFTER chunk_count;
