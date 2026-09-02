# Feature Specification: 检索元数据过滤（Metadata Filtering）

**Feature Branch**: `002-metadata-filter`

**Created**: 2026-09-02

**Status**: Clarified（2026-09-02 第二次澄清后，可进入 plan）

**Input**: User description: "RAG 检索链路增加「元数据过滤」能力。当前 `knowledge.Service.Retrieve` 在一个知识库内对全部已发布 chunk 做向量+关键词双路召回，除了 `knowledge_base_id` 和 `embedding_dimension` 之外没有任何缩小范围的手段。两个已知问题：(1) 知识库里文档一多，语义相近但来源不同的片段互相挤占 topK——用户明知答案在某份文档/某个章节里，也没法把检索限定过去；(2) `parse.go` 解析 PDF 时已经逐页重建了文本并带着 1-indexed 页码（`pdfPage.Number`），但这个信号在切块时被丢弃，chunk 落库后无法回答『第 12 页附近说了什么』，引用也无法标到页。目标：给 chunk 增加结构化元数据，并让检索能在**召回阶段**按元数据缩小候选范围。约束：必须可关闭、空过滤器时输出与本功能上线前逐字一致、过滤必须下推到两路召回的 SQL 里（不能召回后再过滤）、不破坏 `RetrievedChunk.Score` 语义与 Citation 协议、不破坏 Phase 4/7 邻接扩展与 Phase 8 准入的既有行为。"

## Clarifications

### Session 2026-09-02（规格起草时的预设决策，实施前需与我确认）

- Q: 元数据存在哪？ → **A: `chunks` 表新增 `metadata jsonb` 列 + GIN 索引**。
  不新增窄列——元数据的键集合会随文件类型演进（PDF 有页码、Markdown 有标题层级、
  未来可能有表格坐标），窄列每加一种就要一次 migration。jsonb + GIN 一次到位。
  文档级属性（`file_name`/`file_type`/`created_by`）**不冗余进 chunk**，
  过滤时 join `documents` 表，避免文档改名后 chunk 侧数据陈旧。
- Q: 过滤条件从哪来？ → **A: 本期只做「调用方显式传入」**——检索入参新增一个过滤器结构，
  由 API 调用方/前端指定。**「让 LLM 从用户问题里自动抽取过滤条件」明确不在本期范围**
  （见 Out of Scope）——那是一次额外的模型调用，且抽错时用户无感知，必须单独做并单独评测。
- Q: 过滤后候选不足怎么办？ → **A: 不自动放宽**。宁可返回少或返回空，也不能悄悄忽略
  用户指定的范围——「我限定了范围但系统偷偷用了范围外的资料」比「没找到」严重得多。
  候选不足必须体现在检索诊断里。
- Q: 邻接块（Phase 4/7）要不要受过滤约束？ → **A: 文档级过滤必须满足，chunk 级过滤豁免**。
  理由与 `neighbor.go` 既有论证同构：邻接块是**上下文补全**，不是检索命中，它的存在意义
  就是补上被切块切断的半句话。一个页码范围过滤不该把答案的后半句挡在外面。
  但跨文档取邻接在任何情况下都不成立，所以文档级过滤照旧生效。

### Session 2026-09-02（实施前澄清，已确认，覆盖上一节的预设）

进入 `/speckit-plan` 前对照真实代码做了一次核查，发现上一节的部分预设与代码库现状不符。
以下四条经所有者确认，**优先级高于上一节的起草时预设**。

**先更正一条事实**：起草时写的「`parse.go` 的 1-indexed 页码在切块时被丢弃」**是错的**。
真实链路是：`parse.go` 的 `pdfPage.Number` → `chunk.go` 的 `chunkPDFPages`
（`chunkPiece{PageNumber: &num}`）→ `model.go` 的 `Chunk.PageNumber` → `CreateChunk` →
`chunks.page_number` 列（migration `000003_chunk_source_metadata`）→ 四条检索查询的 SELECT
列表 → `RetrievedChunk.PageNumber`。页码**早已端到端落库并被 Citation V1 使用**。
（`pgqueries/chunks.sql` 里 `CreateChunk` 那句"page_number 调用方一律传 NULL"的注释是
`000003` 时期的遗留，与 Phase 4 之后的实际代码不符，本期顺带更正。）
因此 US2 的工作量只剩「让已有的页码**可被过滤**」，不含「把页码接上」。

- Q: 页码既然已有专用列，本期元数据存哪？ → **A: 只用现有 `chunks.page_number` 列，本期不引入
  `metadata jsonb`**。上一节主张 jsonb 的理由是"元数据的键集合会演进"，但本期唯一的键就是页码，
  而页码已经有一个填充好、被 Citation 依赖的专用列。此时加 jsonb 只会制造第二个真相来源、
  一次存量回填、以及一个只有单键的 GIN 索引。jsonb 的正确引入时机是**真的出现第二种 chunk 级
  元数据**（Markdown heading path）的那一期——那时它才有第一个非页码的键。符合宪法第 IX 条最小范围。
- Q: FR-002 的「跨页 chunk 记录起止页」怎么处理？ → **A: 承认当前不可达，改为结构性保证的陈述**。
  `chunkPDFPages` 严格按页切块（其 doc 注释："Chunking per page rather than across the whole
  document is deliberate: it's the only way to guarantee PageNumber is never wrong"），
  跨页 chunk 在当前设计下**不存在**；而 FR-005 又禁止改变切块边界。因此「起止页」是一个
  没有输入能触发的字段。本期按单页语义实现，并把跨页要求降级为前瞻约束（见修订后的 FR-002）。
- Q: 本期支持哪些过滤维度？ → **A: 只做 `document_id` 与页码范围**。两者都已经是 `chunks`
  表上的列，两路召回 SQL 可以直接下推，**本期零跨库查询**——`document_id` 根本不需要先查 MySQL
  （见修订后的 Assumptions）。按文件名/类型/上传者过滤才需要跨库，US1/US2 都不需要它。
- Q: `Retrieve` 的签名怎么改？ → **A: 增加一个 options 结构参数**，
  `Retrieve(ctx, kbIDs, query, topK, opts RetrieveOptions)`，两个现有调用方
  （`conversation/context.go`、`workflow/executor.go`）传零值。零值即"不限定"，与 FR-006 的
  空过滤器语义天然一致；只保留一条检索入口，不会出现"有人走了没过滤的老方法"的分叉。

## User Scenarios & Testing *(mandatory)*

### User Story 1 - 把检索限定到指定文档 (Priority: P1)

一个知识库里挂了十几份文档，其中三份是不同版本的产品手册，措辞高度相似。
用户明确知道自己要问的是「2026 版手册」里的内容，但今天检索会把三个版本的相似段落一起召回，
topK 被旧版本挤占，模型据此回答，用户拿到的是过期信息且不容易察觉。

本功能要求：调用方能在检索时指定一个或多个 `document_id`，
系统 MUST 只在这些文档的 chunk 里召回，两路召回都必须受此约束。

**验收**：同一个问题，不带过滤时结果里出现 A/B/C 三份文档的片段；
限定到 A 之后，结果中 MUST NOT 出现任何 B/C 的片段，且 A 的片段排序与不带过滤时
在 A 内部的相对顺序一致（过滤只做范围缩小，不改变打分与融合逻辑）。

### User Story 2 - PDF 页码进入元数据并可被过滤与引用 (Priority: P1)

用户上传一份 200 页的 PDF，问「第三章那几页讲的部署流程」。
页码本身**已经落库**（`chunks.page_number`，见 Session 2026-09-02 的事实更正），
Citation 也已经能标到页；缺的是**用它缩小检索范围**的能力——今天没有任何入参能表达
"只在第 10 到 15 页里找"。

本功能要求：调用方 MUST 能按页码范围过滤，且该过滤 MUST 下推到两路召回 SQL。
无页码的 chunk（非 PDF、或 `000003` 迁移之前入库的存量行）MUST 视为不匹配。

**验收**：一个已知第 12 页含目标内容的 PDF，限定页码范围 `[10, 15]` 时能召回该片段；
限定 `[1, 5]` 时 MUST NOT 召回。

### User Story 3 - 关掉过滤时行为逐字不变 (Priority: P1)

本功能不能成为既有检索质量的风险来源。

**验收**：过滤器为空（或功能开关关闭）时，对固定输入集，
检索输出 MUST 与本功能上线前逐字一致——片段集合、顺序、分数、邻接扩展结果全部一致。
该断言 MUST 由确定性检索门禁覆盖。

### User Story 4 - 过滤生效情况可观测 (Priority: P2)

用户抱怨「我限定了文档怎么还是答不对」时，需要能分清是过滤没生效、
还是过滤生效了但该文档里确实没有答案。

**验收**：每轮检索的诊断信息 MUST 能回答：是否施加了过滤、过滤前后候选数量、
是否因过滤导致候选数为 0。

### Edge Cases

- 过滤器引用了不存在或不属于当前知识库的 `document_id` → MUST 当作无匹配处理（返回空），
  MUST NOT 报错，MUST NOT 悄悄忽略该条件。
- 过滤器指定的文档处于 `pending`/`processing`/`failed` 状态 → 沿用既有「只检索已发布版本」的语义。
- 页码过滤作用在非 PDF 文档上 → 该文档的所有 chunk 无页码元数据，MUST 视为不匹配，
  MUST NOT 当作「无元数据即通过」。
- 老数据没有 metadata 列内容（本功能上线前入库的 chunk）→ 见 FR-014 的回填要求。
- 过滤后候选数低于 Phase 3 的 `candidateK()` → 融合与准入照常执行，MUST NOT 因候选少而跳过准入阈值。

## Requirements *(mandatory)*

### Functional Requirements

**元数据产出与存储**

- **FR-001**: 系统 MUST 在切块阶段为每个 chunk 产出结构化元数据，与 chunk 一同落库。
  **本期该要求已由既有代码满足**（`chunkPDFPages` → `Chunk.PageNumber` → `chunks.page_number`），
  本期 MUST NOT 改动这条产出链路，只 MUST 为其补上"回归断言"，防止后续改动把它退化掉。
- **FR-002**: PDF 文档的 chunk MUST 携带其来源页码（已满足，见 FR-001）。
  **跨页语义（修订）**：`chunkPDFPages` 按页切块，跨页 chunk 在当前设计下不存在，
  因此本期页码 MUST 按**单页**语义实现与断言，MUST NOT 引入一组恒相等的起止页字段。
  这是一条**前瞻约束**：将来若有任何改动允许一个 chunk 跨页，那次改动 MUST 同时引入起止页表示，
  MUST NOT 让某个页码代表一个它并不完整覆盖的 chunk。
- **FR-003（修订，本期不兑现）**: 「新增一种元数据不需要改表」这一目标本期**明确不实现**。
  本期唯一的 chunk 级元数据是页码，它已有专用列。引入 `metadata jsonb` 的正确时机是第二种
  chunk 级元数据（Markdown heading path）真正出现的那一期——理由见 Session 2026-09-02。
  本期 MUST NOT 新增 jsonb 列，MUST NOT 新增 GIN 索引。
- **FR-004**: 文档级属性（文件名、文件类型、上传者、上传时间）MUST NOT 冗余存储在 chunk 上，
  过滤时 MUST 从文档表取，保证文档属性变更后过滤结果随之变化。
- **FR-005**: 元数据 MUST NOT 参与 embedding 计算，MUST NOT 改变现有 chunk 正文内容与切块边界
  （本功能上线前后，同一份文档切出的 chunk 正文 MUST 逐字一致）。

**过滤语义**

- **FR-006**: 检索入参 MUST 支持一个可选的过滤器；过滤器为空时 MUST 等价于当前无过滤行为。
- **FR-007**: 过滤条件 MUST 在向量路与关键词路的召回 SQL 中下推执行，
  MUST NOT 采用「先召回 topK 再在应用层筛掉」的实现——后者会让过滤直接吃掉召回名额。
- **FR-008**: 多个过滤条件之间 MUST 是「与」关系；同一条件的多个取值 MUST 是「或」关系。
- **FR-009**: 系统 MUST NOT 在候选不足时自动放宽或忽略任何已指定的过滤条件。
- **FR-010**: 引用了不存在实体的过滤条件 MUST 产生「无匹配」，MUST NOT 报错，MUST NOT 被静默丢弃。
- **FR-011**: 邻接块扩展 MUST 继续满足文档级过滤条件，MUST NOT 受 chunk 级过滤条件约束
  （理由见 Clarifications；邻接块是上下文补全而非检索命中）。
- **FR-012**: 过滤 MUST NOT 改变 `RetrievedChunk.Score` 的语义、RRF 融合逻辑、
  Phase 8 准入阈值与 Phase 5 去重行为——它只缩小候选来源，不参与打分。

**兼容、回填与开关**

- **FR-013**: 本功能 MUST 可整体关闭，关闭时检索链路 MUST 与上线前逐字一致。
- **FR-014（修订，本期无回填命令）**: 本期不新增任何元数据列，因此不存在"新列需要回填"的问题。
  存量语义 MUST 如下明确：`000003` 迁移之前入库、以及全部非 PDF 的 chunk，其 `page_number`
  为 NULL，这类 chunk 在页码过滤下 MUST 视为不匹配（与 Edge Cases 里"页码过滤作用在非 PDF 文档上"
  同一口径），在**无过滤**检索下 MUST 照常可被命中。NULL 页码 MUST NOT 被伪造成任何数值；
  存量 PDF 想获得页码的唯一正当路径是重新处理该文档（既有的 RetryDocument）。
- **FR-015**: 过滤器 MUST 有数量上限——`document_id` 列表 MUST 有一个明确的条数上限，
  超限 MUST 是一个明确的失败，MUST NOT 静默截断（静默截断等于悄悄放宽用户指定的范围，违反 FR-009）。
  页码范围 MUST 校验上下界关系与非负性。
- **FR-016**: 过滤条件 MUST 被当作数据处理，MUST NOT 以字符串拼接方式进入 SQL。

**可观测**

- **FR-017**: 每轮检索 MUST 记录：是否施加过滤、过滤条件的**种类与数量**、
  过滤前后各路候选数量、是否因过滤导致零候选。
- **FR-018**: 上述记录 MUST NOT 包含用户问题原文、片段正文或过滤条件的具体取值
  （取值可能含文件名等可识别信息），沿用 Phase 9 既有的日志脱敏口径。

### Key Entities

- **Chunk 元数据（Chunk Metadata）**：随 chunk 落库的结构化附加信息。本期的具体载体是
  **既有的 `chunks.page_number` 列**（不新增载体，见 Session 2026-09-02）。
  只用于过滤与引用展示，不参与向量化，不参与打分。NULL 表示"这次处理没有这项数据"，不是错误。
- **检索过滤器（Retrieval Filter）**：一次检索请求携带的范围限定。空值是合法且默认的取值，
  语义为「不限定」。与「知识库 ID」不是一回事——后者是既有的强制隔离边界，不属于本功能。
- **检索诊断（Retrieval Diagnostics）**：沿用 Phase 9 已有概念，本功能为其新增
  过滤相关的计数与状态字段，仍然只含计数与状态，不含内容。

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 确定性检索门禁中存在一组用例：同一问题在不带过滤时召回到 A、B 两份文档的片段，
  限定到 A 之后结果中不含任何 B 的片段，且可重复复现。
- **SC-002**: 确定性检索门禁中存在一组用例：目标片段位于 PDF 第 N 页，
  页码范围包含 N 时召回、不包含 N 时不召回，且可重复复现。
- **SC-003**: 空过滤器 / 功能关闭时，固定输入集的检索输出与上线前逐字一致（回归断言）。
- **SC-004**: 过滤条件下推验证——存在一条断言证明过滤发生在召回阶段而非应用层筛选
  （例如：限定到只含 1 条匹配的文档时，仍能拿到该文档内 topK 个候选，
  而不是「全库 topK 里恰好属于该文档的那几条」）。
- **SC-005（修订）**: 本期无回填命令（FR-014），原幂等断言不适用。替代断言：存在一组用例证明
  `page_number IS NULL` 的 chunk（非 PDF / 存量行）在**页码过滤下不被召回**、
  在**无过滤下正常被召回**——即"无元数据"既不被当作通过、也不被当作永久失效。

## Assumptions

- 本期不引入新的外部模型调用——元数据在切块阶段由确定性代码产出，过滤由 SQL 执行。
- `chunks` 在 PostgreSQL、`documents` 仍在 MySQL 的现状不变。**本期不产生任何跨库查询**：
  本期的两个过滤维度 `document_id` 与 `page_number` 都已经是 `chunks` 表自己的列，
  直接作为参数下推到 PG 的两路召回 SQL 即可。起草时设想的"先查 MySQL 得到 document_id 集合再
  IN 下推"只有在按文件名/类型/上传者过滤时才需要，而那些维度已被本期排除（见 Out of Scope）。
- 每知识库 5000 chunks 的软上限不变。本期不新增 GIN 索引（FR-003 修订）；
  `document_id` 已有 btree 索引 `idx_chunks_document_id`，页码过滤在此规模下是候选集上的
  廉价残余谓词，不需要为它单独建索引——这一判断 MUST 在 plan/research 中给出依据。
- Markdown 标题层级（heading path）作为元数据是自然的下一步，也是 `metadata jsonb` 的正确
  引入时机，但**本期只做页码**，保持范围最小、可评测。

## Out of Scope

- **由 LLM 从用户问题中自动抽取过滤条件**（"去年的手册里怎么说"→ 时间过滤）。
  这是独立的一期，需要独立评测与降级策略。
- Markdown 标题层级、表格坐标、图片位置等其他元数据类型。
- 前端交互界面。本期只提供后端能力与契约。
- 基于元数据的**排序加权**（如「新文档优先」）。过滤是布尔的，加权会改变打分语义，
  与 FR-012 冲突，必须单独立项。
- HNSW/IVFFlat 索引。与本功能无关，且既有取舍（见 `000001_chunks.up.sql` 注释）不变。
