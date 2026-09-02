-- 004-agent-document-scope：Agent 的检索文档范围。
--
-- 语义：一个 Agent 的范围为**空**时不限定（检索该 Agent 绑定的知识库全部内容，
-- 也就是本表出现之前的行为）；非空时只检索这里列出的文档。范围是 Agent 级的
-- 全局列表，不按知识库分组——理由见 specs/004-agent-document-scope/spec.md 的
-- Clarifications（按库分组需要给 002 的 RetrieveFilter 引入分组文档列表、SQL 写成
-- 多组 OR，而收益只在"一个 Agent 绑多个知识库且只想限制其中一部分"这个当前不存在
-- 的场景里）。
--
-- 不冗余 knowledge_base_id：document_id 全局唯一且天然属于某一个知识库，
-- 冗余一列只会制造"文档被移动后两处不一致"的可能（虽然当前不支持移动文档）。
-- 需要按知识库分组展示时，join documents 表即可。
--
-- **故意不建外键、不做级联删除**。这是本表最重要的一条设计约束：
-- 文档被删除时，这里的行**保留**。留着的失效 document_id 匹配不到任何 chunk
-- （002 的 FR-010：引用不存在实体的过滤条件产生"无匹配"而不是报错），
-- 于是该 Agent 检索不到东西——这是正确且可见的结果。
-- 反过来如果级联删除，范围被删空之后，系统再也无法区分"从未限定"和"限定的
-- 文档都被删了"，只能退回成"不限定"，于是 Agent 会**悄悄用起范围外的资料**。
-- 那正是 002 全篇在防的事：宁可检索不到，也不能悄悄放宽用户指定的范围。
-- 代价是可能留下孤儿行，这个代价远小于静默放宽。
--
-- 表形态照抄 agent_knowledge_bases（000003）：纯多对多关联表，不生成独立 id 列，
-- 用两个外键列组成复合主键（见 internal/db/CLAUDE.md 的建表约定）。
CREATE TABLE agent_documents (
    agent_id CHAR(36) NOT NULL,
    document_id CHAR(36) NOT NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (agent_id, document_id),
    KEY idx_agent_documents_document_id (document_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;
