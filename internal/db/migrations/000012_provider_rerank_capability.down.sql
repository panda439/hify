-- 回退前必须确认不存在 capability='rerank' 的行，否则收紧约束会失败——这
-- 是设计上刻意的行为，回退这张表不应该悄悄丢弃已注册的 rerank 模型行。
ALTER TABLE provider_models DROP CHECK chk_provider_models_capability;
ALTER TABLE provider_models
    ADD CONSTRAINT chk_provider_models_capability
    CHECK (capability IN ('chat', 'embedding'));
