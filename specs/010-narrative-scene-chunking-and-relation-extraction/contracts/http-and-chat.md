# 010 HTTP、Service 与聊天契约

以下为新增接口设计，均以 `/api/v1` 为前缀；沿用认证中间件、httperr、无成功包壳的 JSON。
写操作要求知识库创建者或 admin；读操作遵循当前认证用户读取有效知识库的规则。
handler检查路径kb与文档实际归属一致；不把传入的kbId直接当文档授权。

## 1. 上传

既有 `POST /knowledge-bases/:id/documents` multipart 增可选字段：
`narrative_mode=false`、`extract_relations=false`、`relation_model_id`。
缺省沿用旧上传；布尔非法返回400；抽取为true且叙事false返回400；开启抽取必须给active chat模型。
叙事true、抽取false无需模型；开关字段写入documents与初始创建同一事务。

现有 Service.UploadDocument保留作为零选项包装，新方法
`UploadDocumentWithOptions(ctx, kbID,userID,role,fileName,fileType,content,UploadOptions)`。
UploadOptions是knowledge领域类型；旧调用方和旧Fake无需修改业务行为，新方法按需补Fake契约。
文档旧字段与值不变；Phase 2 以可选 `is_narrative` / `is_relation_extraction_enabled` 回显已开启标志（false 时省略），不额外逐文档查询job。
上传兼容早期实现同名 `is_*` 字段；与规范字段同时出现须值一致，重复值冲突或非法值均返回400。
Phase 2 抽取尚不可用时显式拒绝 extract_relations=true，不静默接受；relation_model_id 在后续抽取启用入口接入。
抽取详细状态通过下面专门接口读取，避免列表N+1。

## 2. 抽取操作

路径 `/knowledge-bases/:id/documents/:docId/extraction`：

| 方法/后缀 | 输入 | 返回 |
|---|---|---|
| GET | 无 | 200 状态、job_id、document_version、进度、stop_reason、预算、账目摘要 |
| POST /enable | model_id、idempotency_key | 202；仅叙事文档，开启并持久化意图，事实未ready可等待 |
| POST /disable | idempotency_key | 200；关闭并中断，立即不再查询关系；已有成功项保留 |
| POST /pause | idempotency_key | 200；暂停当前run，保留开关；无运行也幂等返回当前状态 |
| POST /resume | idempotency_key，可选additional_chunks/calls/active_seconds/retry_rounds | 202；同run/配置追加额度并续未成功项 |
| POST /restart | idempotency_key、model_id | 202；仅ready，创建新run并替换active指针，旧账目保留 |

resume无预算且耗尽409；运行中resume同键返回此前结果，新键的预算追加可原子接受但不多启动worker。
同幂等键不同body返回409；作业变化后旧键重放返回原操作指向及当前状态，不复活旧作业。
idempotency_key≤128字符，服务端存hash和request_hash；初次enable没有job时在documents事务中
创建等待事实ready的job意图（state=pending、initialization_complete=false），供reconcile补items。
自动上传意图首次建job用保留的upload:<document_version>键，用户键禁止该前缀。
模型变更只允许restart，resume的模型/prompt字段非法；pause/disable不抹掉已有计费尝试。
job版本不匹配或文档非ready时resume返回409；disabled必须先enable再resume。
enable已有paused job仅恢复可见开关，不自动运行，响应明确需要resume。

状态响应含 `state/stop_reason,total_items,succeeded_items,failed_items,has_partial_evidence,`
`confirmed_calls,possible_calls,unknown_usage_attempts,active_ms,wall_ms,cost_kind,cost_amount,`
`remaining_calls,remaining_chunks,remaining_active_ms`；未知值为null，不填零。
PG暂不可读、初始化未完成则total_items为null，不伪称完成0/0。

Service增加：GetExtractionStatus、SetExtractionEnabled、PauseExtraction、ResumeExtraction、RestartExtraction、
ProcessRelationExtraction(jobID)、ReconcileRelationExtractions；入参均本模块领域类型/基础类型。
用户操作携带userID/role；后台worker不伪造用户身份，但必须做文档版本和启用状态检查。

## 3. 同一聊天入口

既有 sendMessageRequest 增可选：

```json
{"content":"李明和小李是什么关系？","relation_query":{"document_id":"文档ID","subject":"李明","object":"小李"}}
```

缺省 relation_query → 现有 StreamMessage 不变。
新增 StreamMessageWithOptions(ctx,userID,conversationID,content,MessageOptions)，旧方法零选项包装。
MessageOptions属于conversation；不得在接口暴露knowledge类型。conversation service完成原会话用户校验，
调用agent.Service获取当前Agent范围，再转成knowledge.RelationQuery领域请求。
subject/object非空≤128 rune；歧义响应可返回候选character_id，后续可额外提交
subject_character_id/object_character_id从下拉候选选定，服务端仍校验属于当前doc/active job且对应此次称呼；
不得仅凭ID绕过范围或采用旧run的人物。doc必须属于Agent KB范围，若Agent DocumentIDs非空还须在该列表中。

knowledge.Service.QueryRelations输入Scope（KBIDs、DocumentIDs）、DocumentID、Subject、Object，
返回status=found/not_found/ambiguous/disabled/incomplete、records、coverage、has_more与来源。
严格区分空KB列表和空DocumentIDs，空KB拒绝；客户端不能指定Scope。
Scope由服务端调用者建立，不接受用户角色为字符串替代资源范围校验。
没有额外ID时，只要称呼命中多个实体或一个歧义别名就返回ambiguous；单一supported候选才解析成功。
读取返回基本文档信息也先过范围校验，不能泄漏范围外人物名/进度。
数据库/PG错误返回error，不映射not_found；中文提示“关系记录暂时无法读取，请稍后重试”。
ambiguous返回书内候选的上下文称谓与出处，用户在当前聊天表单选择带出处的候选；不新增实体编辑界面。

同一规范实体被subject/object解析到时不生成自关系，确定性提示“这两个称呼指向同一人物”并附归一依据。
两人均明确且有多章记录时才进入受限生成；其余按plan确定性反馈。
保持现有SSE事件/消息保存契约，error发生在SSE前按HTTP处理，SSE后按现有带内错误路径。
关系分支成功保存助手正文与实际引用；确定性无结果响应也保存，便于刷新后重放。
不新增消息表列：最终正文已包含覆盖/截断及查询对象，原始请求如需诊断仅存trace安全属性，
不得把原文或人名放入普通日志。关系模式前端通过本轮请求和服务端正文显示，不改旧citation协议。

## 4. 预算、删除和展示验收

开关/状态/人物输入是用户可理解的信息，不向产品UI展示epoch、SHA或数据库表名。
无结果/歧义/禁用/错误为服务端固定中文；completed也展示“已抽取全部可读片段，记录可能有遗漏或错误”。
抽取覆盖和009缺页同时展示；候选或上下文截断强制显示“仅展示部分依据”。
relation来源通过PG批量读取验证；invalid/stale任一则放弃本轮生成并提示数据更新，而非悄悄删掉后声称全量。
权限校验发生在查询与入模前；已送入模型/历史消息不提供跨系统撤回保证。

部署总开关 `NARRATIVE_EXTRACTION_ENABLED=false` 控制创建/执行模型作业；关闭时现有事实检索不受影响，
状态返回功能关闭，关系路径不生成回答。纯叙事分块不依赖总抽取开关，默认上传仍不启用。
配置 `NARRATIVE_EXTRACTION_MODEL_ID` 可作为UI默认选择但不是静默启动许可；request显式抽取仍必需。
