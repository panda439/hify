-- name: CreateChunk :exec
-- 新版本 chunks 一律以 is_published=false 写入——"新版本写入"这一步不改变
-- 任何已发布的旧版本可见性，发布是 PublishChunkVersion 单独一步。
-- document_name 是处理时刻的 Document.FileName 快照（Citation 用的来源
-- 展示名，见 pgmigrations 000003）。
--
-- page_number/section_title 由调用方按解析器**真实能产出**的信号传入，
-- 拿不到才传 NULL，任何情况下不允许伪造：
--   * PDF 有 page_number + page_end（一个**闭区间**，均为 1-indexed）。
--     006-pdf-layout-chunking 更正：本段原文说"chunkPDFPages 严格按页切块，
--     因此不存在'跨页 chunk 该报哪一页'的问题"——那个问题不是不存在，是被
--     按页硬切**回避**掉了，代价是跨页段落被切成两个都不完整的半截。现在
--     段落跨页合并，一个 chunk 可以覆盖第 3-4 页，page_number 收紧为起始
--     页、page_end 为结束页。**不允许任选一端**（FR-011）；
--     PDF 仍然没有 section_title（US4 若落地会开始产出）；
--   * Markdown 有 section_title（chunkMarkdown 的标题栈），没有 page_number；
--   * txt 两者都没有。
-- 002-metadata-filter 更正：本段原文写的是"当前解析器不产出可靠值，调用方
-- 一律传 NULL"。那是 000003 时期的描述，Phase 4 结构感知切块落地后就已不
-- 符实（page_number 一直在被真实写入并被 Citation V1 读取）。这条注释是
-- 该功能"页码过滤有数据可过滤"这一前提的直接反证，留着会误导后来者，故一
-- 并更正——只改注释文字，SQL 语义未动。
INSERT INTO chunks (
    id, knowledge_base_id, document_id, chunk_index, content,
    content_length, embedding, embedding_dimension, document_version, is_published,
    document_name, page_number, section_title, page_end
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14);

-- name: SearchVectorChunks :many
-- Phase 3 前叫 SearchChunks——引入 SearchKeywordChunks 之后改名以便和它
-- 对称、消歧义，语义完全不变。<=> 是 pgvector 的余弦「距离」（0=同向
-- 2=反向），1 - 距离 = 余弦相似度，和被删掉的 similarity.go 语义完全
-- 一致——分数跨迁移可比。
-- 三个 WHERE 条件都不可省：knowledge_base_id 是 CLAUDE.md 大表查询强过滤
-- 规则；embedding_dimension 过滤是因为混合维度共存一张表，<=> 对不同维度
-- 向量直接报错；is_published 过滤是版本可见性网关——未发布的草稿版本永远
-- 不能被检索命中，即使已经物理写入。::text[] cast 也不可省——sqlc 生成的
-- 代码用 pq.Array 传字符串数组，显式 cast 消除 PG 的类型推断歧义。
-- document_name/page_number/section_title 是 Citation V1 需要的来源
-- metadata，随 chunk 一起返回给 conversation 层，knowledge 自己不解释它们。
-- top_k 由调用方传入——Hybrid Search 场景下调用方传的是 candidateK（比
-- 最终 topK 更宽的候选窗口，给 RRF 融合留排序空间），不是最终 topK 本身。
-- ORDER BY 第二个键 id ASC 是稳定兜底：<=> 距离相同时 PostgreSQL 不保证
-- 返回顺序，而 rrfFuse（hybrid.go）把这个结果切片的位置索引当成 RRF 的
-- rank——距离相同却顺序不定，会让同一批候选在两次调用之间拿到不同的
-- rank，进而拿到不同的 fusionScore，最终排序不稳定。id 是主键，任何两行
-- 都不会相等，兜底到底。
--
-- Phase 4: document_version 加入 SELECT 列表——邻接分块扩展
-- (findPublishedNeighborChunks) 必须用它锁定"同一次处理尝试"，绝不能只凭
-- document_id 就去查前后 chunk_index，否则文档重新处理后旧核心块（本次
-- Retrieve 命中时还是当前发布版本，返回给调用方之间可能被新版本替换）会
-- 意外邻接到新版本的同 index chunk，两个版本的内容被拼接展示。这里只是
-- 把已经存在的列读出来交给 Go 层，不改变检索排序或候选集本身。
SELECT id, knowledge_base_id, document_id, document_version, chunk_index, content,
       content_length, embedding_dimension, created_at,
       document_name, page_number, section_title, page_end,
       (1 - (embedding <=> sqlc.arg(query_embedding)))::float8 AS score
FROM chunks
WHERE knowledge_base_id = ANY(sqlc.arg(knowledge_base_ids)::text[])
  AND embedding_dimension = sqlc.arg(embedding_dimension)
  AND is_published = true
--
-- 002-metadata-filter：下面三行是**可选**的检索范围过滤（FR-007 要求过滤
-- 必须在召回阶段下推，不允许"先召回 topK 再在 Go 里筛掉"——后者会让过滤
-- 直接吃掉召回名额：被筛掉的行本来可以让位给范围内排名更后的行）。
--
-- 为什么用"可空参数 + 恒真短路"而不是按过滤组合写多条 SQL、也不在 Go 里
-- 拼 WHERE：拼字符串直接违反"SQL 文本不含任何调用方数据"这条既有约定
-- （FR-016），而按组合分裂查询会让这条查询的 SELECT 列表、ORDER BY 和这一
-- 大段注释被复制成四份必须逐字同步的副本。sqlc.narg 生成可空参数，未指定
-- 该维度时传 NULL，`NULL IS NULL` 为 TRUE 使整个 OR 短路成恒真谓词，等价
-- 于这一行不存在。
--
-- 全部三个参数为 NULL 时（空过滤器 / 功能开关关闭），结果集合与本功能上线
-- 前**逐字一致**：三条谓词都是常量 TRUE，不改变任何一行的去留。行序同样
-- 不受影响——ORDER BY 以 id ASC 收尾（理由见上文关于 RRF rank 稳定性的说
-- 明），最终顺序与 PostgreSQL 因多出常量谓词而可能选择的不同扫描方式无关。
--
-- ⭐ 006-pdf-layout-chunking：页码谓词从「点落区间」改成「**区间相交**」。
-- chunk 现在携带的是一个闭区间 [page_number, page_end]（起始页/结束页），
-- 过滤条件也是一个闭区间 [min, max]，判定标准是**两个区间有交集**。
-- 区间相交的标准形式是 A.start <= B.end AND A.end >= B.start，展开就是下面
-- 那两行——⚠️ **下界谓词作用在 page_end 上、上界谓词作用在 page_number 上，
-- 两行是交叉的**。写成同一列（两行都用 page_number，或两行都用 page_end）
-- 会得到一个语义完全不同、且在跨页行上恒错的条件。这是本次改动最容易写错
-- 的一处，由跨页片段的区间相交用例逐格锁定（contracts §3.3 的表）。
--
-- 对**存量行与全部单页片段**（page_end = page_number）这次改写是**逐字节
-- 等价**的：page_end >= min 就是 page_number >= min。因此没有任何一格是
-- "原本命中的现在不命中"，只有跨页片段上多出三格"原本漏掉的现在命中了"
-- ——那正是想要的：过滤"第 4 页"应当命中一个覆盖 3-4 页的片段，因为它
-- 确实包含第 4 页的内容。完整论证见 data-model.md §7.1。
--
-- 页码过滤对 page_number IS NULL 的行（全部 txt/md chunk，以及 000003 迁移
-- 之前写入的存量行）的行为，靠 SQL 三值逻辑天然正确，**不需要**也**没有**
-- 显式的 IS NOT NULL：`NULL >= 10` 求值为 NULL 而不是 FALSE，此时另一侧
-- `filter_page_min IS NULL` 为 FALSE，`FALSE OR NULL` = NULL，而 WHERE 只
-- 接受 TRUE，该行被排除。这正是"无页码 MUST 视为不匹配，MUST NOT 当作无
-- 元数据即通过"的要求。改写后这条**依然成立且理由不变**，因为 page_end 与
-- page_number 同为 NULL——这个不变量由 pgmigration 000005 的
-- chunks_page_range_valid 约束强制，不是靠约定维持：一旦出现一行
-- page_number 有值而 page_end 为 NULL，下界谓词会排除它而上界谓词会放行，
-- 结果是任何设了 min 的检索都**静默漏召回**这一行，没有报错也没有日志。
-- ⚠️ 禁止把它改写成 COALESCE(page_number, 0) / COALESCE(page_end, 0) 之类的
-- 写法：那等于给一个本来没有页码的 chunk 编造出第 0 页，与"绝不伪造"的既有
-- 约定正面冲突，page_end 完全适用这条禁令、一个字都不放宽。该行为由
-- TestFilterPageRangeExcludesNullPageChunks 锁定（006 给 page_end 侧补了
-- 对称用例，且只给下界/只给上界/闭区间**三种入参各试一次**——只测闭区间
-- 时单侧的错误会被另一侧未改动的谓词掩盖掉）。
--
-- 邻接窗口查询（FindPublishedNeighborChunksBatch）**故意没有**这三行，
-- 那不是遗漏：文档级约束对邻接块是结构性满足的（邻接坐标全部取自已经通过
-- 过滤的 anchors，JOIN 又按 document_id 等值匹配），而 chunk 级的页码过滤
-- 必须被豁免——邻接块是上下文补全而不是检索命中，一个页码范围不该把答案
-- 的后半句挡在外面（FR-011）。
  AND (sqlc.narg(filter_document_ids)::text[] IS NULL
       OR document_id = ANY(sqlc.narg(filter_document_ids)::text[]))
  AND (sqlc.narg(filter_page_min)::int IS NULL OR page_end     >= sqlc.narg(filter_page_min)::int)
  AND (sqlc.narg(filter_page_max)::int IS NULL OR page_number  <= sqlc.narg(filter_page_max)::int)
ORDER BY embedding <=> sqlc.arg(query_embedding), id ASC
LIMIT sqlc.arg(top_k);

-- name: SearchKeywordChunks :many
-- pg_trgm 字符级 trigram/word-similarity 关键词检索（lexical search）——
-- 明确不是 BM25：BM25 需要真正的分词 + 词频/逆文档频率统计，pg_trgm 只是
-- 字符 n-gram 相似度，中英文都能用但不做语言学分词，也不做真正的相关性
-- 排序模型。见 pgmigrations 000004 的索引和阈值说明。
--
-- 不依赖 embedding，因此没有 embedding_dimension 过滤——这正是关键词检索
-- 在 embedding 服务失败/超时时仍能独立返回结果的原因（见 service.go 的
-- Retrieve：向量一路失败只跳过对应 embedding model 分组，关键词一路继续）。
-- knowledge_base_id / is_published 两个过滤的必要性和 SearchVectorChunks
-- 相同：大表强过滤规则、未发布草稿版本永不可检索。
--
-- query_text <> '' 是防御性的第二道闸——Go 层 Retrieve/searchKeywordChunks
-- 已经在空 query 时直接短路不落库查询，这里再挡一层，防止将来有调用方
-- 绕开 Go 层校验时，<% 空串在语义上退化成"什么都不过滤"从而让全表都
-- 变成候选。
-- <% 走 idx_chunks_content_trgm 这个 GIN 索引过滤候选集（判定阈值见
-- pgmigrations 000004 里对 pg_trgm.word_similarity_threshold 的说明）；
-- ORDER BY 里的 word_similarity() 只对索引已经筛出的候选重新计算一次精确
-- 分数用于排序，而不是对全表算。candidate_k 是硬上限，防止候选集无界增长。
--
-- Phase 4: document_version 同 SearchVectorChunks 的理由——关键词路径命中
-- 的 chunk 同样可能被邻接扩展，必须知道它属于哪一次处理尝试。
SELECT id, knowledge_base_id, document_id, document_version, chunk_index, content,
       content_length, embedding_dimension, created_at,
       document_name, page_number, section_title, page_end,
       word_similarity(sqlc.arg(query_text), content)::float8 AS score
FROM chunks
WHERE knowledge_base_id = ANY(sqlc.arg(knowledge_base_ids)::text[])
  AND is_published = true
  AND sqlc.arg(query_text) <> ''
  AND sqlc.arg(query_text) <% content
--
-- 002-metadata-filter：与 SearchVectorChunks 完全相同的三行可选过滤谓词，
-- 完整论证（为什么恒真短路、为什么全 NULL 时逐字一致、NULL 页码为什么天然
-- 被排除且禁止 COALESCE、邻接查询为什么故意不加）见 SearchVectorChunks 里
-- 的对应段落，不在此重复。
--
-- 关键词路特有的一点：这三个谓词是 GIN 索引（idx_chunks_content_trgm）已经
-- 用 `<%` 筛出候选集之后的**残余谓词**，只在至多 candidate_k 数量级的行上
-- 求值，因此不需要为它们单独建索引。两路都下推是 FR-007 的硬要求——只在
-- 向量路过滤会让关键词路把范围外的片段重新带回候选池。
  AND (sqlc.narg(filter_document_ids)::text[] IS NULL
       OR document_id = ANY(sqlc.narg(filter_document_ids)::text[]))
  AND (sqlc.narg(filter_page_min)::int IS NULL OR page_end     >= sqlc.narg(filter_page_min)::int)
  AND (sqlc.narg(filter_page_max)::int IS NULL OR page_number  <= sqlc.narg(filter_page_max)::int)
ORDER BY word_similarity(sqlc.arg(query_text), content) DESC, id ASC
LIMIT sqlc.arg(candidate_k);

-- name: CountChunksByKnowledgeBase :one
-- 只数已发布的——未发布的草稿版本不该出现在"这个知识库有多少分片"的统计里。
SELECT COUNT(*) FROM chunks WHERE knowledge_base_id = $1 AND is_published = true;

-- name: DeleteChunksByDocument :exec
-- 整份文档删除用（DeleteDocument）：不分版本、不看发布状态，全部清空。
DELETE FROM chunks WHERE document_id = $1;

-- name: DeleteChunksByDocumentVersion :exec
-- 精确删除某一个 version 的 chunks——CAS 发布网关被拒绝（version 已过期）
-- 时，worker 用它清理自己刚写入、永远不会被发布的那批草稿行。
DELETE FROM chunks WHERE document_id = $1 AND document_version = $2;

-- name: DeleteObsoleteChunkVersions :exec
-- 发布步骤的"清理旧版本"半部分：删掉除了刚发布的这个 version 之外的所有
-- 行。和下面 PublishChunkVersion 一起在同一个 PG 事务里调用（见
-- repository.go 的 publishDocumentVersion），保证 PG 内部不会出现"新旧
-- 同时可见"或者"新旧都不可见"的中间态。
DELETE FROM chunks WHERE document_id = $1 AND document_version <> $2;

-- name: PublishChunkVersion :exec
-- 发布步骤的"发布新版本"半部分。
UPDATE chunks SET is_published = true WHERE document_id = $1 AND document_version = $2;

-- name: CountChunksByDocumentVersion :one
-- reconciliation 恢复卡在 publishing 的文档时用它现查这次尝试实际发布了
-- 多少行，写回 MySQL 的 chunk_count——reconciliation 和原 worker 不是同一
-- 次调用，没有 worker 当时算出来的 len(pieces) 可用。
SELECT COUNT(*) FROM chunks WHERE document_id = $1 AND document_version = $2 AND is_published = true;

-- name: FindPublishedNeighborChunks :many
-- Phase 4 引入、Phase 7 起不再是 service.go 邻接窗口扩展的调用路径——见下面
-- FindPublishedNeighborChunksBatch 的 doc 注释：expandWithNeighborWindow 现
-- 在把所有核心命中块的邻接坐标一次性展平成一个批量查询，不再按
-- (document_id, document_version) 分组循环调用这条按单一分组查询的版本。
-- 这条查询本身没有删除——它的语义（单个 document_id+document_version 分组
-- 内按 chunk_indexes 取邻接块）仍然正确，且 integration_test.go 里若干 Phase
-- 4/5 的真实 Postgres 测试继续直接调用它来驱动 expandWithNeighbors，不经过
-- Service.Retrieve 整条链路，独立验证核心/邻接去重和跨版本隔离逻辑；保留
-- 这条查询让那些测试不必跟着批量化重写。真正的生产路径请看
-- FindPublishedNeighborChunksBatch。
--
-- 四个过滤条件里 document_id + document_version 缺一不可：只有这两个一起
-- 锁定"同一次处理尝试"的 chunk 集合，绝不会把另一个文档、或者同一文档另
-- 一次处理尝试（重新处理产生的新/旧版本）里恰好 chunk_index 相同的行当成
-- 邻接块带回来——这是防止"文档重新处理后邻接块串版本"的唯一防线，
-- Go 层不做二次校验（也没有足够信息做二次校验：Go 层只知道自己要哪些
-- index，不知道 PG 里实际还剩哪些行）。is_published = true 和
-- SearchVectorChunks/SearchKeywordChunks 同一个理由：未发布的草稿版本永
-- 远不可检索，邻接块也不能是例外。如果调用方传入的 document_id +
-- document_version 组合对应的版本已经被重新处理删除（见 pgmigrations
-- 000002 的发布流程：DeleteObsoleteChunkVersions 物理删除旧版本行），这
-- 条查询会因为 document_version 这个条件天然匹配不到任何行，返回空集合
-- 而不是退回去匹配新版本的相同 index——不需要额外代码保证这一点，是
-- WHERE 条件本身的结构性保证。
--
-- chunk_indexes 由调用方保证只含非负整数（chunk_index=0 没有前块，调用方
-- 不会把 -1 放进这个数组）——SQL 侧不需要也不做负数过滤，ANY 对一个不含
-- 负数的数组天然不会匹配到任何 chunk_index（chunk_index 本身也不可能是
-- 负数，chunkDocument 生成时从 0 开始递增）。
--
-- ORDER BY chunk_index ASC, id ASC：chunk_index 是主排序键（邻接窗口要按
-- 文档内的自然顺序展示），id ASC 是稳定兜底——chunk_index 在同一个
-- (document_id, document_version) 下语义上唯一，理论上不会真正撞车，但保
-- 留这个兜底和 SearchVectorChunks/SearchKeywordChunks 的约定一致，不留下
-- 一个"这条查询没有稳定排序保证"的例外。
SELECT id, knowledge_base_id, document_id, document_version, chunk_index, content,
       content_length, embedding_dimension, created_at,
       document_name, page_number, section_title, page_end
FROM chunks
WHERE document_id = sqlc.arg(document_id)
  AND document_version = sqlc.arg(document_version)
  AND is_published = true
  AND chunk_index = ANY(sqlc.arg(chunk_indexes)::int[])
ORDER BY chunk_index ASC, id ASC;

-- name: FindPublishedNeighborChunksBatch :many
-- Phase 7: 邻接窗口批量查询（Batch Neighbor Lookup）——这条查询取代了
-- FindPublishedNeighborChunks 在 service.go 里被循环调用的用法：
-- expandWithNeighborWindow 不再按 (document_id, document_version) 分组、每
-- 组发一条 SQL，而是把所有核心命中块需要的邻接坐标（document_id +
-- document_version + chunk_index 三元组，见 knowledge/neighbor.go 的
-- buildNeighborRequests）去重展平成三个等长的并行数组，一次性传给这一条
-- 查询——正常成功路径无论有多少个核心块、多少个不同的文档/版本，只发生
-- 一次数据库往返。
--
-- 三个并行数组各自单独 unnest() WITH ORDINALITY，再按序数 ord 三路 JOIN 拼回
-- 同一行——这等价于"多参数 unnest(a,b,c) AS t(x,y,z)"想表达的并行数组展开，
-- 但用的是 sqlc 的 PostgreSQL 类型检查器能正确识别的单参数 unnest 形态（多
-- 参数 unnest 在 sqlc 的静态 catalog 里无法解析参数类型，会在 sqlc generate
-- 阶段直接报 "function unnest(unknown, unknown, unknown) does not exist"）。
-- 拼出来的 requested(document_id, document_version, chunk_index) 再和
-- chunks 表按这三列做等值 JOIN——这是 PostgreSQL 里"传入一组元组、按元组
-- 匹配"的标准参数化写法，不是字符串拼接：三个数组各自作为独立的
-- sqlc.arg(...)::type[] 参数绑定，SQL 文本本身不包含任何调用方数据。
--
-- 隔离性和 FindPublishedNeighborChunks 完全一致，不因为改成批量就放松：
-- JOIN 条件同时要求 document_id、document_version、chunk_index 三者都匹配，
-- 绝不会把另一个文档、或者同一文档另一次处理尝试（重新处理产生的新/旧版
-- 本）里恰好 chunk_index 相同的行当成邻接块带回来。is_published = true 过
-- 滤未发布草稿版本的理由同上——邻接块不能是例外。请求数组里某个三元组对
-- 应的版本已经被重新处理删除（DeleteObsoleteChunkVersions 物理删除旧版本
-- 行）时，JOIN 天然匹配不到那一行，不会退回去匹配新版本的相同 index——
-- 结构性保证，不靠 Go 层二次校验。
--
-- 调用方（Repository.findPublishedNeighborChunksBatch）在正常生产路径上已
-- 经保证三个数组等长、且已经去重（buildNeighborRequests 用一个 map 收集
-- 三元组，同一个三元组只出现一次）——这仍然是首选：提前去重让发给数据库
-- 的坐标数量不随重复请求膨胀。但这条查询本身不能把"调用方永远会提前去
-- 重"当成正确性前提：requested CTE 显式加了 DISTINCT，即使调用方（或未来
-- 某个不经过 buildNeighborRequests 的调用路径）传入重复甚至乱序的三元组，
-- 同一个坐标在 requested 里也只会出现一次，JOIN 到 chunks 后每个匹配的
-- chunk 只返回一行——这是 SQL/repository 边界上的防御性去重，不是替代 Go
-- 层去重（Go 层提前去重仍然保留，理由见上一段），而是"调用方去重逻辑万一
-- 有 bug 或被绕过时，这条查询的返回结果依然正确、不会把请求里的重复放大
-- 成结果里的重复"这一条独立的正确性保证（Codex 第一轮 Phase 7 审核发现：
-- 去重前的版本对重复请求坐标返回重复结果行，不满足"批量请求包含重复/乱
-- 序坐标时结果必须正确且确定"的验收要求，已在此修复）。空数组（调用方没
-- 有任何邻接坐标要问）交给 Go 层直接短路返回，不发起这条查询——SQL 侧不
-- 需要、也不做空数组特判：unnest 对三个空数组产生零行 requested，JOIN 自
-- 然返回空结果集，行为本身是对的，只是没有必要为了"什么都不查"专门走一次
-- 数据库往返。
--
-- ORDER BY 用 document_id、document_version、chunk_index 依次排序、id ASC
-- 稳定兜底——和 FindPublishedNeighborChunks 一样，调用方（neighbor.go 的
-- expandWithNeighbors）按 (document_id, document_version, chunk_index) 三元
-- 组 key 在结果里查找匹配项，不依赖这条查询返回的顺序本身有业务含义，这里
-- 排序只是为了让同一份请求集合两次调用返回确定性一致的行序，方便测试断言
-- 和排查问题。
WITH requested AS (
    SELECT DISTINCT d.document_id, v.document_version, c.chunk_index
    FROM unnest(sqlc.arg(document_ids)::text[]) WITH ORDINALITY AS d(document_id, ord)
    JOIN unnest(sqlc.arg(document_versions)::bigint[]) WITH ORDINALITY AS v(document_version, ord)
      ON v.ord = d.ord
    JOIN unnest(sqlc.arg(chunk_indexes)::int[]) WITH ORDINALITY AS c(chunk_index, ord)
      ON c.ord = d.ord
)
SELECT ch.id, ch.knowledge_base_id, ch.document_id, ch.document_version, ch.chunk_index, ch.content,
       ch.content_length, ch.embedding_dimension, ch.created_at,
       ch.document_name, ch.page_number, ch.section_title, ch.page_end
FROM chunks ch
JOIN requested r
  ON ch.document_id = r.document_id
 AND ch.document_version = r.document_version
 AND ch.chunk_index = r.chunk_index
WHERE ch.is_published = true
ORDER BY ch.document_id ASC, ch.document_version ASC, ch.chunk_index ASC, ch.id ASC;
