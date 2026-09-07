# 010 数据模型与一致性契约

与 [plan](plan.md) 配套；是待实现结构，不是已存在表。

## 1. 通用规则

MySQL 遵循 internal/db/CLAUDE.md：实体主键 CHAR(36) UUIDv7，created_at/updated_at 为 UTC DATETIME(3)，
InnoDB/utf8mb4_0900_ai_ci/ROW_FORMAT=DYNAMIC。状态 VARCHAR(32)+CHECK，不用 ENUM。
派生记录不软删除；不建阻碍日志清理的强级联外键，但每个引用列都显式建索引。
所有内容/配置/唯一键摘要 BINARY(32) SHA-256，避免默认不区分大小写/重音的排序规则合并名字。
文本名仅用于显示/候选查找，不能作为人物唯一键。

## 2. documents 增列（000017）

| 列 | 类型/默认 | 含义 |
|---|---|---|
| is_narrative | TINYINT(1), 0 | 上传固定，已有文档不切换分块模式 |
| is_relation_extraction_enabled | TINYINT(1), 0 | 独立抽取开关，只有叙事文档可开启 |
| relation_model_id | CHAR(36), NULL | 开启抽取时所选 active chat 模型，INDEX |
| active_relation_job_id | CHAR(36), NULL | 当前查询 run，INDEX；关闭时仍可保留指针但数据不可查 |

复用现有 documents.version，不另造相同含义的 content_version；job 另存内容 hash 区分语料。
索引 `(status,is_relation_extraction_enabled,id)` 支持恢复扫描；新字段全零时旧行为保持。

## 3. 7 张 MySQL 表

### relation_extraction_jobs

- id；document_id、knowledge_base_id、document_version；run_number；model_id、config_hash、config_snapshot JSON。
- source_hash NULL（初始化前未知）；state、stop_reason；epoch、lease_until NULL、heartbeat_at NULL；初始化完成标志。
- total_items、succeeded_items、failed_items；approved_item_limit、call_limit、active_ms_limit、retry_rounds。
- reserved_calls、confirmed_dispatches、unknown_attempts；active_ms_used/reserved；started_at/finished_at NULL。
- archived_ledger_summary JSON NULL：过期 attempt 删除前按日批次累加的汇总及 unknown 数。
- operation_key_hash：start/restart 幂等键摘要；operation_request_hash用于同键异body冲突；budget_operations JSON：已接受的追加操作键、额度和结果，
  每 run 最多 100 次控制操作，超过须 restart（旧账目不清零）；更新持有 job 行锁。
- UNIQUE(document_id,document_version,run_number)，UNIQUE(document_id,operation_key_hash)；
  INDEX(knowledge_base_id)、INDEX(model_id)、INDEX(state,lease_until,id)、INDEX(document_id,state,id)。

配置快照包含 model/provider ID、模型名及可获得的 digest、prompt/schema/alias规则版本、分块配置、
请求参数、限额初值。无密钥/Authorization/base URL凭证。限额追加不改变推理 config_hash，有单独操作记录。
模型名相同但服务端实际权重已变，必须 restart；若无法取得权重 digest，报告该复现限制。

### relation_extraction_items

- id、job_id、chunk_id（逻辑引用 PG）、chunk_index、content_hash；state=pending/running/succeeded/failed。
- extract_attempt_count、alias_attempt_count、extract_response JSON NULL、alias_response JSON NULL、last_error_code。
- 引用原始输出的最终校验结果；无效响应仍保存在 attempt，不冒充成功。
- UNIQUE(job_id,chunk_id)，UNIQUE(job_id,chunk_index)；INDEX(chunk_id)、INDEX(job_id,state,chunk_index,id)。

全部 item 来自当前 PG published version，成功空结果也为 succeeded。metadata缺失/损坏不能跳过并减总数，
初始化作业失败并给可操作原因，事实索引不变。

### relation_extraction_attempts

- id、job_id、item_id、epoch、phase=extract/alias、attempt_number；request_hash、max_output_tokens。
- state=reserved/dispatched/completed/failed/unknown/not_dispatched；created/started/finished；elapsed_ms NULL。
- dispatch_confirmed、usage_known；input_tokens/output_tokens NULL；finish_reason NULL、error_code NULL。
- raw_response LONGTEXT NULL（最大 64 KiB）、response_hash；cost_amount DECIMAL(18,8) NULL、currency NULL、
  pricing_version NULL、cost_kind=measured/estimated/unknown/not_applicable。
- UNIQUE(item_id,phase,attempt_number)；INDEX(job_id,created_at,id)、INDEX(item_id)、INDEX(state,created_at,id)。

先记 reserved 再外部调用。服务端返回之前崩溃时 reserved 恢复为 unknown，不能声称没发出。
not_dispatched 可退调用预留，但仍保留控制尝试记录和等待耗时。业务自动重试次数照计，不死循环。
对没有金钱计费的本地 API cost_kind=not_applicable，amount=NULL；硬件电力未测另在报告说明。

### narrative_characters

- id、job_id、display_name VARCHAR(128)、first_source_order BIGINT、identity_evidence JSON、has_ambiguity。
- 创建人物实例，不 UNIQUE(name)；INDEX(job_id,first_source_order,id)。
- identity_evidence 保存至多 2 组简短身份依据及源引用，不存无限增长的人物传记。

### narrative_aliases

- id、job_id、character_id NULL、surface VARCHAR(128)、surface_hash、state。
- evidence JSON（当前/既有原文位置、引用、判断规则、模型提案）；first_source_order；decision_key_hash。
- state=proposed/supported/ambiguous/rejected；supported 才可关联单个人物；ambiguous 不自动消解。
- UNIQUE(job_id,decision_key_hash)，INDEX(character_id)、INDEX(job_id,surface_hash,state,id)。

不同人物可有相同 surface_hash；严禁名称唯一约束。后来的歧义标记保留历史实体指向，查询时返回歧义提示。
去重键包括 mention来源、候选实体和规则版本，不能只由名字组成。

### narrative_relations

- id、job_id、subject_id、object_id、relation_type、is_directed、relation_key_hash。
- first_source_order；chapter_number NULL、chapter_title NULL（首证据展示快照，不充当唯一故事时间）。
- UNIQUE(job_id,relation_key_hash)；INDEX(subject_id)、INDEX(object_id)、
  INDEX(job_id,subject_id,object_id,first_source_order,id)。

记录单位包含规范主体/客体、类型、源场景或无结构时源证据位置；无向关系按实体 ID 排序。
跨章同一类型新增记录，同章不同场景新增；同场景相同三元组聚合多证据，变化类型分别保留。
关系历史不能按当前状态覆盖。评价按章节聚合是指标视图，不改变存储记录。

### narrative_relation_evidence

- id、job_id、relation_id、chunk_id、document_version、source_order、source_start/end、quote TEXT。
- source_segments JSON（跨段/页证据映射）；evidence_key_hash。
- UNIQUE(relation_id,evidence_key_hash)；INDEX(job_id)、INDEX(relation_id)、INDEX(chunk_id)。

evidence_key 以规范文档源区间+quote hash计算；不同 overlap chunk 中同一出处归为一个证据。
不要用 chunk_id 作为证据唯一性的全部依据。跨库不建 FK；发布和读取分别做实际 parent/version 检查。

## 4. PG chunks（000006）

新增 `narrative_metadata JSONB NULL`，不修改向量或检索得分。关闭模式 NULL。
结构为 schema_version、normalized_document_hash、boundary_kind、scene_key NULL、chapter_number NULL、
chapter_title NULL、source_order、segments 数组；每段含 chunk_start/end、document_start/end、page NULL、
is_generated_separator、is_overlap_copy。复制字符保留同一源区间，只有生成字符没有源区间。均为 0 起半开 rune 区间；page 为实际 1 起页码。

boundary_kind 新写入 none/divider/chapter_fallback；chapter 仅兼容早期值。scene_key 为规范文本 hash + 场景源起点。
PDF 坐标基于去噪后重组的段落流，新增段间/页间字符标 generated_separator；每个真实来源段记录实际页码。

metadata 验证由纯函数负责：范围非负、start<end、chunk区间有序且覆盖正文、复制源段长度一致；
生成分隔字符显式标志，不计作可引用证据。scene未知时 key 空；章节未知时序号空。
CreateChunk 同事务写 metadata，默认参数 NULL；查询新增：

- ListPublishedNarrativeChunks(document_id,version,after_chunk_index,limit)：ORDER BY chunk_index,id，limit≤200。
- GetPublishedNarrativeChunksByIDs(document_id,version,ids)：ids≤200，过滤 is_published；批量核验证据。

已有 `(document_id,document_version,chunk_index)` 查询路径的索引在实施前核对；缺少则在 000006 补复合索引，
不为 JSON 盲建索引。现有向量/关键词 SELECT 和排序无需读取该 JSON，不改变其结果集合。

## 5. 事务与读可见性

全局锁顺序 document→job→item→人物/关系；计数的更新与结果发布同事务。
item成功唯一约束+epoch使重复任务不产生双份关系；epoch仅限制数据发布，不代表外部推理没有重复。
初始化 item 前后验证 ready/version 和 PG总数，若文档已改版本则 superseded，不提交初始化完成。

启用开关和 active_job 指针、作业创建、restart旧job失效在同一 MySQL 事务；后入队可失败，reconcile恢复。
清理attempt先把聚合计数写job再删除，事务中保存游标/批次标识，重复清理不重复累计。
查询费用=未归档attempt+job归档汇总；不得既用全生命周期计数又加汇总导致重复。
