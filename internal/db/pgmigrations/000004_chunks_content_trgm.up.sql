-- Phase 3: Hybrid Search 的关键词检索一路，使用 pg_trgm 做字符级 trigram
-- 相似度匹配——不是 BM25。BM25/tsvector 默认配置 (to_tsvector('english', ...))
-- 依赖分词+词干化，对中文完全无效（没有内置中文分词配置，如 zhparser），
-- pg_trgm 是纯字符 n-gram 相似度，天然对中英文一视同仁，代价是不做真正的
-- 词频/逆文档频率相关性排序，只是"字符序列有多像"，因此仓库里一律称它
-- trigram keyword search / lexical search，绝不称 BM25。
--
-- CREATE EXTENSION IF NOT EXISTS 拿不到"这个扩展是不是本迁移创建的"这个
-- 事实——数据库里可能早就因为别的原因装了 pg_trgm（比如另一个模块也在
-- 用它做模糊匹配）。这就是为什么 000004_chunks_content_trgm.down.sql 不
-- 卸载 pg_trgm 本身，只回滚这条迁移自己引入的东西（索引、GUC 默认值）：
-- pg_trgm 一旦启用就被当成数据库级别的共享能力，其归属不由某一条迁移
-- 独占，回滚不能替其他潜在使用方做出"整个数据库都不再需要它"的判断。
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- word_similarity(query, content) 衡量"query 是否近似 content 内部的一个
-- 子串"，比全串 similarity() 更适合"短关键词 vs 长 chunk 正文"这种场景
-- （全串 similarity 对短 query/长文本几乎恒为低分——共享 trigram 占比被
-- 长文本的总 trigram 数稀释）。见 pgqueries/chunks.sql 的 SearchKeywordChunks。
--
-- gin_trgm_ops 支持 %、<%、%> 三个可索引算子（不支持 <->/<<-> 系列的
-- KNN 排序算子，那是 GiST 独有能力）——SearchKeywordChunks 用 <% 走索引
-- 过滤候选集，ORDER BY 里的 word_similarity() 只对索引已经筛出的候选重新
-- 算精确分数用于排序，不是全表计算。
CREATE INDEX idx_chunks_content_trgm ON chunks USING gin (content gin_trgm_ops);

-- <% 的判定阈值由 pg_trgm.word_similarity_threshold 这个 GUC 控制，默认
-- 0.6——对"短关键词 vs 长 chunk 正文"明显偏严格，几乎没有子串能达到 0.6
-- 的 word similarity，会导致关键词检索退化成"几乎从不命中"。这里在数据
-- 库级别显式设成 0.3：比默认宽松，同时仍然是一个真实下限，避免"所有
-- chunk 都成为候选"（不设下限时 <% 会退化成一个近似恒真的谓词，candidate
-- limit 就成了唯一的过滤手段，不满足"设置合理的最低关键词相似度"的要求）。
--
-- current_database() 不能直接当 ALTER DATABASE 的目标标识符，用 DO +
-- format() 动态拼出真实库名，使这条迁移在 hify 开发库、hify_test_knowledge
-- 等各个测试库上都能正确生效，不用为每个库名单独写一条语句。
DO $$
BEGIN
    EXECUTE format('ALTER DATABASE %I SET pg_trgm.word_similarity_threshold = 0.3', current_database());
END
$$;

-- ALTER DATABASE ... SET 只影响之后新建立的连接——迁移工具和后续在同一个
-- 连接/会话里跑的调用者不应该还看到默认的 0.6，所以当前会话也显式设一次。
SET pg_trgm.word_similarity_threshold = 0.3;
