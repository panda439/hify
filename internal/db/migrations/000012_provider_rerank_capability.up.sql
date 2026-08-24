-- 001-rag-query-rerank：供应商模型体系新增第三类能力 rerank（交叉编码器式
-- 重排序模型，走 /rerank 端点，见 internal/provider/llm.go 的 RerankRequest）。
-- 唯一变更是把 capability 的 CHECK 约束从 (chat, embedding) 扩为
-- (chat, embedding, rerank)——列类型 VARCHAR(16)、索引
-- idx_provider_models_provider_capability 均不变，不需要重新生成 sqlc（列
-- 定义没变，只改 CHECK），仍按宪法第 IV 条在这之后跑一次 make sqlc 确认无
-- diff（见 data-model.md §1.1）。
ALTER TABLE provider_models DROP CHECK chk_provider_models_capability;
ALTER TABLE provider_models
    ADD CONSTRAINT chk_provider_models_capability
    CHECK (capability IN ('chat', 'embedding', 'rerank'));
