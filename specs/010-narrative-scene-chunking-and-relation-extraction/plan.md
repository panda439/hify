# 010 实施计划：叙事分块与人物关系抽取

**修订日期**：2026-09-06。**状态**：设计补齐，待按 tasks 实施与独立验收；不是实现完成。
当前 checkout：`claude/strange-driscoll-87a634`；规格逻辑编号为 010，不要求改分支。
权威顺序：[spec](spec.md) → 本计划及契约 → [tasks](tasks.md)。
research 为调研历史；与本计划冲突时以本计划为准，不能沿用旧“四表/PG 零改动”假设。

## 1. 范围与明确取舍

- 场景分块：用户上传时显式开启，默认关闭。txt、Markdown、可解析 PDF 沿用现有解析能力，不加 OCR。
- 抽取：独立开关，必须先启用叙事模式；事实索引 ready 后，由独立 asynq 作业逐块抽取并归一。
- 消费：沿用聊天页面，增加“人物关系”选项，提交书目与两个人物名称；普通对话默认不自动进入关系路径。
  不新建人物关系管理页面，不做额外 LLM 意图路由。
- 实验：本地 qwen2.5:14b 主跑、7b 对照是既定候选，标签可用性/吞吐必须实测。
  阿Q完整 9 章同时测成本与质量；西游记前 20 回仅为额外长文本实验。
- AI 初标已存在，但人工真值尚未确认。不以 AI 一致性数字冒充 FR-016 人工精确率/召回率。
- 不做图数据库、图算法、完整指代消解、跨书归一、性格总结、自动判断叙事格式。

**方案选择**：来源数据随 PG chunk 发布，而不是额外维护跨库场景映射；作业进度存 MySQL，
Redis 只承载可重复的唤醒消息。相较重算来源/四表 JSON 混存，这多了几张明确职责的表，
但可表达合法空结果、并发 claim、调用账目与证据去重，不引入新的基础设施。

## 2. 当前源码与改动落点

源码确认：`go.mod` 声明 Go 1.25.0（实际运行工具链在 Phase 1 记录）；
`ProcessDocument` 已有 version、lease、PG unpublished→publish→MySQL ready；
`provider.ResolveClient` 返回带自动重试的 Client；`Usage` 零值表示未知；
`conversation` 已有预算、引用和 Agent 文档范围；路由注册在各模块 `wire.go`。

| 层/文件 | 010 改动 |
|---|---|
| MySQL migrations / queries | 000017：documents 模式与抽取模型选择；7 张派生/运行表；sqlc 生成 |
| PG pgmigrations / pgqueries | 000006：chunks 增可空 narrative_metadata；CreateChunk 写入、新增按文档版本游标读取和按 ID 批量核验证据查询 |
| knowledge model/errors/repository/service/dto/handler/wire | 增领域契约、状态、操作与 HTTP 接口，保持层职责 |
| knowledge narrative.go / narrative_test.go | 纯函数结构识别、来源映射、长度降级；不把函数文件当新业务模块 |
| knowledge extract.go / extract_prompt.go / alias.go / relations.go / tasks.go | 作业、结构校验、归一、查询、恢复任务 |
| provider service.go / llm.go / resilience.go / openai_compat.go | 新增单次可审计 ChatOnce 契约；原 Chat/Embed/Stream 的默认重试行为不变 |
| conversation service/model/dto/handler/context/budget 与测试 | 新增可选关系请求；普通分支不变；关系分支的选择、引用、提示与预算 |
| cmd/hify/main.go / config | 开关注入；注册抽取与 reconcile handlers、周期扫描 |
| web/src/lib/knowledge.ts / conversations.ts | 上传选项、抽取状态/续跑、可选关系请求类型 |
| web/src/routes/knowledge-documents-dialog.tsx / chat.tsx | 叙事/抽取开关、状态及续跑；聊天中的书目/人物输入、依据与范围提示 |
| eval / cmd/narrativeeval / Makefile | 数据快照、纯指标重算和本地实验命令；不使用带 LLM 裁判的 make eval 证明回归 |

迁移编号是当前最后编号后的预留值；写 migration 前重新确认没有其他分支占用。
不增加 Go 依赖，不增加模块依赖边：knowledge→provider、conversation→knowledge 已存在。
检索的融合、准入、重排、邻接算法不改；PG 创建查询和新来源查询会改，不能再写“PG 零改动”。

## 3. D-001：分块与来源契约

长度单位为 rune，沿用知识库 ChunkSize/ChunkOverlap；默认 500、50，单文档最多 2000 块。
叙事模式关闭时直接调用原 chunkDocument，新的 PG 字段为 NULL，旧响应新增字段 omitempty。

1. txt/md 保留正文字符，仅统一 CRLF 为 LF；Markdown fenced code 内不识别场景线，标题保留正文。
   PDF 沿用 stripLayoutNoise 和段落重组，保留实际 page_number/page_end；不以页断开场景。
2. 章节标题采用整行匹配：`第<阿拉伯/中文数字>章/回`及 Markdown 包装；数字支持十百千、两、〇、○、零、位数连写。
   只有能明确解析的序号写 chapter_number；楔子/序等标题可保留 title，序号空。
   通用输入允许第 11 章起步、缺章；重复/逆序给警告并保留原文顺序，不能编造递增章号。
3. 显式场景线为去首尾空白后完整的 `***`、连续至少 3 个 `—`、一个或多个 `※`，或原始正文中至少 2 个连续空行。
   PDF 不从布局生成的空行推断场景，仅识别明确文字分隔线。网页生成脚本的空行不能冒充原作场景。
   章节与显式线切出结构区间；无章内边界的章节标 boundary_kind=chapter_fallback，不能声称语义场景识别成功。
4. 每个明确场景单独开始，不与上一场景拼装。超长依次按段落、句子（。！？!?及后随闭引号）、rune 切分；
   overlap 仅在同场景子块间携带并缩至剩余空间。场景分隔线附前场景，文首线归后场景；标题属于其后场景。
   无可识别结构时使用既有分块内容和顺序，并为每块生成诚实的来源区间，章节和场景语义 ID 留空。
5. narrative_metadata 与片段同一 PG 事务写入，带 schema_version、规范化内容 hash、结构序号、chapter、
   source_segments。每个 segment 将 chunk 的 rune 区间映射到规范化文档 rune 区间及真实页码；
   重叠块重复部分映射到同一源位置，不用 substring 的第一次命中猜重复段落位置。
   PDF 插入的段间分隔字符显式记 generated_separator，不能当模型引用的人物证据。
6. 可识别结构的 scene_key 由文档版本+源起点构成；同一超长场景各块共享它。
   文档内 source_order 始终按源位置，章节编号只用于展示。源引用针对解析后的正文，不承诺 PDF 像素坐标。

来源结构、索引和一致性见 [data-model](data-model.md)。如果旧分块产生的片段含省略/合成分隔字符，来源映射须显式标识而非伪造连续区间；
默认关闭的 txt/md/PDF 内容、顺序和旧元数据做快照回归。

## 4. D-002：独立作业及恢复

### 4.1 发布与调度

documents 保存 is_narrative、is_relation_extraction_enabled、relation_model_id（模型实体 ID，非 API key）。
上传时验证能力；抽取关闭不要求模型可用。事实处理不调用抽取模型。

总抽取开关关闭时不创建或claim新作业，运行中的worker在下次检查取消并保留进度。
每分钟运行一次 ReconcileRelationExtractions：按游标批量扫描 ready、开启抽取的当前文档，
幂等创建/恢复对应作业及全体 chunk_items，再入队 `{job_id}`。documents 的开启标志就是持久意图；
ready 后崩溃/Redis 丢消息不丢意图。初始化所有 item 成功后才允许 claim，无隐藏 500 块截断总数。
扫描也处理过期 lease、无消息的 pending；用户暂停/预算耗尽的作业不得被扫描器自动重启。
扫描每批 100 条，保存本轮游标循环，不能永远只扫描第一页。

旧文档发布版本已由现有 ProcessDocument/RetryDocument 管理。010 不增加 ready 文档“重新分块”入口；
现有重试或未来重新处理只要版本变化，作业就被 superseded。新模型实验通过 restart 创建新 run，
不修改普通文档版本，不混用旧模型的角色/关系。

### 4.2 claim、心跳和逐块事务

作业采用 MySQL CAS claim，epoch 单调增加，lease 180s，工作中每 30s 续租；每次外部调用前再次检查
document.version、开启标志、active_job_id、epoch、lease。一个 job 同时只有一个执行者，逐块顺序处理。
心跳 goroutine 随 context 退出并由工作者等待；取消不得留下裸 goroutine。

单块流程：锁 job/item 并预留预算和 attempt → 事务提交 → 调 ChatOnce → 保存原始响应及账目 →
校验/归一 → **同一 MySQL 事务**提交人物/别名/关系/证据、item succeeded 和完成数。
合法空数组也写 succeeded；非法结构整次不发布，已花调用/时间不回滚。
任何写关系的事务都核对当前 document/version/active job/epoch。lease 失效或删除时旧响应不能发布。

记录与状态事务失败时不重复调用已持久化的有效响应；恢复从 raw_response 再做确定性校验和提交。
请求已发出但响应未落盘的崩溃窗口标 outcome_unknown，外部推理可能已执行；
无法保证模型调用恰好一次，但关系发布可做到幂等，未知费用不填零。

### 4.3 状态及用户操作

| 状态 | 条件 / 恢复 |
|---|---|
| 未启用 | documents 抽取关闭，不建新 job；关系查询不返回旧派生数据 |
| pending | 初始化完成、等待 claim；reconcile 可重发消息 |
| running | 持有有效 lease；进度是 succeeded/全部已发布片段数 |
| partial | 有失败块、预算耗尽或连续失败；reason 区分原因；仅用户 resume/restart |
| paused | 用户取消当前运行；取消 context，保留结果；用户 resume |
| failed | 模型配置无效等无法启动，或零成功且不可继续；修复后用户 resume |
| completed | 所有 item succeeded，包括合法空结果；不是关系语义全部正确 |
| superseded | document 版本或 active run 已替换；永不恢复/查询 |

展示层 paused/failed 等对应 spec 的中断/失败，partial 必须显示未完成数量。
PDF 缺页提示继续来自 007～009；“已处理全部可读片段”不能抹掉原文件缺页。
禁用开关使当前 job paused 并立即隐藏关系，重新开启仍通过显式 resume，不由扫描自动重启已暂停 job。

## 5. 超时、重试和账目（D-002）

### 5.1 单一重试责任

provider.Service 新增 `ChatOnce(ctx, providerID, ChatRequest) (ChatAttemptResult, error)`：
保留共享并发限流、速率限制和 breaker，但禁止 wrapper、SDK/HTTP transport 的自动重复发出请求；
适配器验证为一调用至多一次 HTTP dispatch。返回 Dispatched、UsageKnown、Usage、FinishReason、Content，
发生错误也保留可得的 attempt metadata；调用未发出（限流/熔断拒绝）与已发出超时区分。
不得另造未加限流的裸客户端，不变更现有 ResolveClient 默认行为。

knowledge 是抽取唯一自动重试层：每 item 的抽取和归一各至多 3 次尝试（首次+2 次），退避 1s、2s；
网络/5xx/429/超时/非法 JSON 可重试；认证或模型无效直接 failed；取消停止，不自动重试。
asynq MaxRetry(0)。作业恢复读持久计数，不能重置额度。连续 5 个最终失败 item 停止。

### 5.2 固定初始限额及续跑

| 项目 | 初始限额与累计规则 |
|---|---|
| 单次调用 | 60s，包括排队限流、连接及响应；按剩余作业执行预算缩短 deadline |
| 响应 | MaxTokens=2048、解码正文最大 64 KiB、最多 32 人物/64 关系/32 别名提案；超过整次拒绝 |
| 单次输入 | 渲染后的输入上限 12000 rune；当前 chunk 正文优先，不偷偷截断正文；超限 item 明确失败 |
| 文档范围 | 原事实上限 2000 块不变；初始只批准处理最多 500 个不同 item，总数仍展示全部 |
| 调用次数 | 每 job 初始最多 3000 个可能已发出的尝试预留（500×2阶段×3）；归一不需要时零调用 |
| 执行时间 | 每 job 初始 7200s 累计活跃时间，排队/用户暂停不消耗；心跳以DB时间增量结算非调用阶段，调用阶段按预留结算且不双计，崩溃尾段按预留保守扣除 |
| 预算预留 | 开始外部调用前事务预留次数和最长执行时间，完成后按实际耗时结算；未知结局不退次数，未知时间保守结算 |

resume 默认仅续剩余额度，保留成功项。显式 `additional_chunks/calls/active_seconds/retry_rounds`
可由有写权限用户追加预算；每次追加分别最多 500/3000/7200/1，文档 chunk 总额不超过 2000。
追加 retry_rounds 为尚未成功 item 各阶段增加最多 3 次机会，历史计数和费用不清空。
预算未追加却已耗尽返回 409，不能假装续跑成功。restart 必须有 idempotency_key，建新 run，
旧 run superseded、旧数据隐藏，旧账目保留并计入文档总汇总。配置更改通过 restart，resume 不允许变模型/prompt。

### 5.3 报告口径

每次调用账目记录阶段、输入 hash、模型/adapter/config digest、请求上限、开始/结束、结果、
已知输入输出 token、UsageKnown、估算/未知标志；失败及归一调用全部计入。
正常完成且所有 dispatch 已确认时可报确切调用数；崩溃窗口存在则报告 confirmed 与 possible 范围，
不能编造精确数。用量缺失不按零求和掩盖，汇总同时返回 unknown_attempt_count。

报告分别给首次启动到终态的墙钟时间（含排队/暂停）、累计活跃时间、模型调用耗时；
本地 API 计费标“不适用”，设备/电力金钱成本“未测”，不得声称总经济成本为零。
若报告价格估算，记录币种/价格日期/计价来源；不同模型 tokenizer 的用量不能直接当另一模型真实费用。
既有 embedding 成本排除且注明；在线对话生成成本单列，不混入离线抽取账目。

## 6. 抽取和别名归一

完整 JSON、验证次序和身份协议见 [extraction](contracts/extraction.md)。

第一阶段逐块提取人物 mention 和关系候选；第二阶段只为有多个称呼/已有身份候选的块运行归一，
两阶段都在同一 item 下计账。第一阶段成功响应可以复用，第二阶段失败只重试归一；
归一耗尽时整 item 失败，候选不对外发布，防止未完成块被算成功。

候选只在同文档/run 内选择：最多 32 个人物候选，每个最多 2 条已确认身份依据，按精确名称命中、
源顺序和 ID 稳定排序；只按名称检索候选，不按名称直接合并。超过候选窗口标歧义，不猜一个。
同场景连续、无冲突的明确全名复现可作为上下文依据；跨章需模型给出两侧身份支持原文或明确别名语句。
通用称呼、同名冲突、指代、否定/假设姓名不能自动合并；无法可靠判定就创建独立人物实例。

归一保留每条证据及 proposed/supported/ambiguous/rejected 状态。支持的是模型判断附原文，
不是语义真值；纯函数门禁只能排掉缺依据/同名强合/非法指向，真实误合并仍由人工集度量。
不用一个覆盖全书且会越来越长的别名表塞进 prompt，不将 AI 自报 confidence 当可靠性证明。

已有实体不原地合并或重写历史关系；新 mention 只关联一个已有实体或新建实体。
后发现冲突时将受影响别名标 ambiguous，不继续自动复用，历史记录保留并向查询展示歧义；
需彻底纠正时 restart 重抽，本期不加人工实体编辑 UI。

## 7. D-004：关系查询、引用和预算

契约见 [http-and-chat](contracts/http-and-chat.md)。
聊天页用户切到“人物关系”，选 Agent 范围内一本书并输入两个人名；正常问题文本仍保留在同一消息中。
不增加自动分类模型，普通消息完全保留现有 StreamMessage 路径。

knowledge 查询先检查有效知识库/文档、当前 ready/version/active run/开启标志，再做人物解析，
必须使用 conversation 从 Agent 获取的 KB/DocumentIDs 范围，不能信任客户端提交的范围。
现有读取规则是认证用户读有效资源，写入要求创建者或 admin；不擅自把读取改成仅创建者可读。
空 Agent KB 范围表示没有可查询知识库；DocumentIDs 空在非空 KB 范围内才表示全部文档。

同名多个实体返回 ambiguous，不强选、不交 LLM 猜；不存在或无关系返回“当前抽取记录中未找到”；
未启用/未完成/查询故障分别提示，故障不伪装“无关系”。这些响应通过确定性文本走既有消息/SSE持久化。
关系查询不触发在线抽取，不回退到一般模型凭记忆断言人物关系。

有记录时：按源顺序选择，优先为每个不同关系类型与方向各留最早证据，再补各章证据，
选择后按 document source_order、relation ID 排序；同章变化不按章节号挤掉。
最多 200 条候选、最终最多 12 条证据；SQL查询带 document/job/人物 pair 强过滤和 LIMIT+1，
has_more 标明数据库截断。合成后的关系描述、引用、覆盖/截断提示共同进入既有 2000-token
RAG 字符估算上限和模型 ContextWindow 预算，不伪造 similarity score 以绕过 ragMinSimilarityScore。
新增专用 selectRelationEvidence；不调用向量证据准入函数，不占两份独立 RAG 预算。

关系分支关闭本轮工具循环及 query rewrite；Agent system prompt、用户问题和证据使用原预算框架，
保留输出预留；整条证据放不下则舍弃，不能裁成半句后继续声称原文支持。
覆盖提示和截断提示是服务端稳定附加文本，并计入预算，不能仅靠 prompt 期待模型记住。
不足以放入一条证据时返回确定性“当前上下文预算不足以展示依据”，不生成无依据关系结论。

引用沿用 message_citations 存储已有 chunk/document/name/quote 字段，Score=0 表示不适用，
前端关系模式不把它画成相似度。原文页码/章节及顺序以关系响应和最终正文展示；
现有 Citation DTO page/section 恒空的历史路径不顺手改造。引用只包含本轮实际送入模型的证据。
入模前批量核对 PG published chunk 当前版本/quote，并再次核对 MySQL文档与 active job；变化则取消本轮关系答案。
并发删除后已开始的模型调用无法回收已发送文本；按入模前校验时刻定义本轮快照。后续查询不返回删除数据。
既有历史消息引用快照的保留政策不由 010 扩大为“删除历史聊天”。

## 8. 删除、跨库边界和清理

PG 来源 metadata 跟随 chunks 原有发布事务，不另做第二份 MySQL 来源真相。
MySQL 只在看到 ready 当前版本和完整 published chunks 后建立 job/items；准备失败不会修改事实 status。
查询串联 MySQL有效文档/run 校验及 PG已发布来源校验，不宣称跨库强事务。

写关系时锁 document→job→item，操作端删除/关闭/restart 也遵守同一锁顺序。
删除 documents 沿用原硬删除；job/关系等没有强级联外键，失去有效 parent 后即不可见。
每分钟清理扫描每批 100 个过期 job（已不存在文档、非当前版本、superseded），物理删除
其 evidence/relations/aliases/characters/items；attempts 可保留 30 天审计，随后按 created_at/id 游标清理。
未删除文档的所有 run 账目在 30 天内用于汇总；到期清理前保存聚合账目到 job，避免费用消失；
已删除文档的最终清理连 job 一起删除。权限失败不向外透露被删文档信息。

## 9. 验证与交付阶段

1. Phase 1：工作目录/配置基线、顺序运行共库测试、语料 hash/清洗映射、纯分块样本基线；不运行整书模型。
2. Phase 2：migration→sqlc→领域层→上传入口→分块及 PG 来源发布，关抽取验证 US1；默认关闭快照与原门禁不变。
3. Phase 3：ChatOnce、MySQL作业/账目、幂等、恢复/删除；用受控 adapter 与真实 DB 验证错误窗口。
4. Phase 4：抽取/归一 schema 与纯函数测试，串入 item 事务；模型是假输出时也必须覆盖所有工程约束。
5. Phase 5：HTTP/聊天/UI/预算/引用/权限及真实 HTTP 冒烟。
6. Phase 6：人工真值冻结之后再跑真实模型对照；AI稿只能先做开发一致性检查；保存原始输出、ledger、模型配置与哈希。
7. 最终：go test（含 race）、go vet、check-deps、两条确定性门禁、smoke，数据库 skip 视为未验证；不自动提交。

条目映射见 tasks 覆盖表；指标重算给定同一快照必须相同。真实模型不要求逐字节重放，
工程测试不能因模型不确定而免除。质量阈值本期不预设，但指标及人工真值未交齐不得标本期完成。

## 10. 宪法与复杂度检查

| 原则 | 设计落实 / 验证边界 |
|---|---|
| I 归属 | 代码和初标均 AI 辅助；人工审核需真实署名；模型效果与机制验证分报 |
| II 规格先行 | spec→plan/data-model/contracts→tasks；本轮只改规格文档 |
| III 分层 | 沿用现有依赖边，跨模块通过 Service；provider不依赖knowledge |
| IV 层顺序 | 迁移/sqlc 后按 model/errors/repository/service/dto/handler/wire；纯函数测试先行 |
| V 确定性 | 原文顺序+ID兜底，JSON/来源/预算/状态纯函数测试，LLM有开关/限额/降级 |
| VI 证据 | 设计检查不等于 go test/HTTP/真实模型通过；tasks 仍未勾选 |
| VII 中文 | UI/prompt/规格中文，错误链英文；代码注释随文件 |
| VIII 提交 | 仅所有者明确要求时 commit/push |
| IX 范围 | 7 表和PG可空列服务已授权的恢复/来源要求；无图结构新平台、无其他功能重构 |

复杂度调整理由：四表无法明确记录合法空块、重试与证据多出处；选择 7 表而非把所有状态塞入
四表 JSON，便于索引、唯一约束和事务验收。PG一列比跨库独立来源表更少一致性窗口。
这些是补齐现有 FR 的实现选择，不增加新的用户目标；若实现需新增范围，先回到 spec。
