-- 010-narrative-scene-chunking-and-relation-extraction：片段的叙事来源元数据。
--
-- 一个叙事片段需要回答两个检索层面回答不了的问题：它属于哪一章哪一场景，
-- 以及它对应原文的哪一段字节。前者是关系记录挂载的时间坐标，后者是每条
-- 关系证据的出处。两者都不能事后重算——章节要重新解析全书，出处要按内容
-- 回搜（重复段落会全部指向第一次出现的位置，看起来完全正常）。
--
-- ⚠️ 一列 JSONB，不是七个新列。理由不是图省事：这些字段**只有叙事模式产出**，
-- 非叙事片段全部为 NULL。摊成列会给现有全部片段加七个恒为 NULL 的列，
-- 而且每加一种边界类型就要再改一次表结构。它们也从不参与检索谓词——
-- 向量与关键词 SELECT 一个字都不读它。
--
-- ⚠️ **不给这个 JSONB 建任何索引**。当前没有按它过滤的查询；为"以后可能会用"
-- 建 GIN 索引是给每次写入加真实成本换一个不存在的读收益。
-- 需要按 chunk_index 取的路径走下面核对过的既有索引。
ALTER TABLE chunks ADD COLUMN narrative_metadata jsonb NULL;

-- 结构（由 Go 侧纯函数保证并由单测锁定，不写成 CHECK）：
--   schema_version            int
--   normalized_document_hash  text     CRLF 归一化后全文的 hash；坐标系的身份证
--   boundary_kind             text     chapter / divider / none
--   scene_key                 text?    场景未知时为空
--   chapter_number            int?     章节未知时为空，**不填 0**
--   chapter_title             text?
--   source_order              int      全书线性位置，排序用
--   segments                  array    每段 {chunk_start,chunk_end,document_start,
--                                      document_end,page?,is_generated_separator}
--
-- ⚠️ 不写成 CHECK 约束的理由与 000015 的 unextracted_pages 完全相同：
-- 「区间有序且覆盖正文、复制源段长度一致」这类不变量不是 CHECK 能便宜表达
-- 的结构，硬写既脆弱又难读。它们由 internal/knowledge 的纯函数在产出时
-- 保证，并由单测锁定——这不是"约束更弱"，是这些不变量本来就不属于数据库
-- 能便宜表达的那一类。
--
-- ⚠️ is_generated_separator 标记的是**系统拼接进去的分隔字符**（比如合并
-- 段落时补的换行），它不属于原文，因此**不可作为可引用证据**。不标出来的
-- 后果是某条关系的引用落在一个原文里根本不存在的位置上。
--
-- ⚠️ 区间是 0 起半开；page 是实际 1 起页码。两套基准写在同一个结构里容易
-- 混用，所以在这里写死：**只有 page 是 1 起的**。
COMMENT ON COLUMN chunks.narrative_metadata IS
    '010 叙事来源元数据；非叙事片段为 NULL。结构见 000006 迁移注释与 data-model.md §4。';

-- 核对结果（data-model.md §4 要求实施前做这件事）：既有索引只有
-- idx_chunks_document_id_version (document_id, document_version)，**没有**
-- 带 chunk_index 的复合索引。本期新增的两条游标查询都按
-- (document_id, document_version) 定位后 ORDER BY chunk_index, id 翻页，
-- 少了这一列会让每翻一页都对整份文档的全部片段做一次排序——西游记全本
-- 2000 片段量级下是真实开销，且随书变大。
--
-- ⚠️ 这是**唯一**新增的索引，而且是为已经写好的查询建的，不是"以后可能用"。
-- 上面那个 JSONB 列一个索引都不建，理由同上。
CREATE INDEX idx_chunks_document_version_index
    ON chunks (document_id, document_version, chunk_index, id);

-- ⚠️ 故意**不删** idx_chunks_document_id_version，尽管它现在是上面这个索引的
-- 前缀、理论上冗余。删掉它会改变既有检索路径的执行计划，而那不属于本期
-- 范围——"顺手清理一下"造成的无关回归正是这类改动的典型代价。
-- 留一条后续项：确认新索引覆盖全部既有用法后再单独删。
