# 010 审核与修订记录

**日期**：2026-09-06
**位置**：Claude 工作目录 `strange-driscoll-87a634`，审核基线 `44f0bca`。
**范围**：spec 与规格检查清单；无业务代码修改、无提交。

## 已修订

| 优先级 | 原问题 | 修订落点 |
|---|---|---|
| P1 | 完整场景不可切与硬上限冲突，超长单段无解 | FR-002/003、US1、SC-001：硬上限优先，段落→句子→字符降级 |
| P1 | 强制章节序号与未知章节不得编造冲突，章节被当作故事时间 | FR-008、SC-012：未知留空，保留原文位置和叙述顺序 |
| P1 | 部分抽取可能被当成全书证据，失败/续跑缺少可观察行为 | FR-010/022/026、SC-010：状态、覆盖、预算与续跑语义 |
| P1 | 新关系路径未约束文档版本、删除及现有查询范围 | FR-023/024、SC-011：旧版本不可见、晚到任务不得复活、范围隔离 |
| P2 | 归一零错误承诺与实验允许误差冲突 | FR-012/013、SC-004/005：固定正反例验收，真实误差单独报告 |
| P2 | 精度口径、未标注范围、失败重试成本容易产生误导数字 | FR-015/016、US4：冻结样本、匹配与去重、全尝试记账、未知费用不填零 |
| P2 | 用户入口/模式未定，性格总结只在动机中出现 | D-001/004：显式开启、既有对话消费；性格总结排除本期验收 |
| P2 | 将 RAG 描述为永远不能综合证据、入库首次调用模型不符合现状 | 问题陈述、D-002、检查清单：改为有限召回覆盖不足、新增生成式抽取 |

## 当前源码核对依据

- `internal/knowledge/chunk.go`：已有按字符数限制与超长结构降级，overlap 也不能突破预算。
- `internal/knowledge/model.go`：默认分块 500 字符，单文档硬上限 2000 块。
- `internal/knowledge/service.go`：已有嵌入调用、文档处理发布与删除路径。
- `internal/conversation/context.go`：既有检索下推 Agent 的知识库及文档范围。

## 复审门槛

1. plan 冻结 D-001～D-004，明确并发、存储、状态、来源校验及预算细节。
2. tasks 覆盖全部 FR/SC，特别是删除与晚到任务、部分完成查询、默认关闭回归。
3. 人工真值集须真实人工确认；不得将模型自标当作人工真值。
4. 本期实验完成后才可报告实际成本与质量；当前文档修订不构成效果验收。

## 第二轮：plan / Phase 1 / 标注审核（基线 03e37bd）

本轮历史结论：**Phase 1 的顺序和语料口径已修订；当时plan尚不能进入业务实现。**
后续设计补齐状态见文末第三轮，不将历史结论当作当前阻塞。
本次仅修文档、提供 AI 标注数据及本地语料副本，没有运行基线测试、下载模型或启动整书抽取。

### 已直接修订

1. **P1 标注材料混入网页内容**：322 段中 C02P030 为零宽空白，C09P048～054 是版权模板/编者说明；保留 314 段，原 ID 不变。逐文件 SHA-256 见初标 manifest。
2. **P1 标注口径错误**：“漏标压低召回率”不成立；原版 `阿贵` 确定别名与 C01P010 相反；出处存在不等于支持关系；跨章去重会掩盖章节覆盖；真值别名可能替系统掩盖误归一。已改标注指南，补疑义列表及章节统计单位。
3. **P1 错把节选当全书**：西游记 1～20 回不满足完整小说验收；T027/T029 改为阿Q完整本测质量和成本，西游节选只补充规模数据。
4. **P2 Phase 1 共库并行风险**：T002/T003 去掉 [P]，顺序记录基线；模型预检与纯分块基线解耦，不把“安装齐全”当测试证据。
5. **P2 章节和分块测试失真**：通用输入不要求从第 1 章连续；节选/缺章仍保留可靠章号；默认配置为 500 字符，1000 是实验选择；overlap 后不能直接拼接等于原文。
6. **P2 模型不确定不等于工程不可测试**：计划补上固定输出下的结构校验、归一、幂等、权限与状态等确定性测试义务；未运行真实检索对照，不宣称 US1 已提升质量。
7. **标注协作已落地**：允许 AI 初标，禁止冒充人工真值。AI 全章初读不保证穷尽；人工需逐段补漏，不能只确认正例。当前 FR-016/SC-007 人工验收未完成。

### plan 必须补齐的实施阻塞项

| 优先级 | 缺口及实际触发 | 所需设计 / 验证 |
|---|---|---|
| P1 | 只列四张表和 documents 状态，没有逐块结果/成功空结果/场景位置的持久化契约；PG 现有 Chunk 只有 ChunkIndex、页码、标题，没有 scene ID | 提供 data-model / contracts：book version、chunk→scene/chapter/source span 映射落点及跨库发布边界。PG 零改动可以作为方案，但须说明 MySQL 映射如何恢复；不能仅口头断言 |
| P1 | 文档 ready 后进程崩溃或入队失败，独立任务永远不开始；Redis 队列不是状态真相 | 持久待处理状态、补偿扫描、版本/租约 claim、心跳和晚到任务 fencing；区分抽取失败与“从未入队”。独立 asynq 任务本身不提供可靠性保证 |
| P1 | 500 块/2 小时预算续跑时是累计还是重置未定；provider 已有 retry，外层再 retry 会放大尝试；失败调用可能无 usage | 指定唯一重试责任层、跨续跑累计预算及主动续跑/预算增加入口；明确 alias 调用预算、请求输出上限、每次物理调用记账与未知用量。测试不能靠外层函数调用次数冒充 HTTP 尝试数 |
| P1 | alias.go 只有“宁可碎”口号，无输入/输出和归一证据协议；当前 JSON 仅含关系 | 设计人物 ID、别名证据、歧义候选、合并/恢复与书内身份更新协议；引用原文存在不能证明两个人就是同一人 |
| P1 | 对话路由、人物歧义、可读来源、预算和前端开关没有具体契约；计划却宣称不改上下文既有路径 | 明确默认关闭/开启的请求字段、上传 DTO/handler/UI、任务注册、查询 Service/对话预算与引用链路。默认关闭可保留旧路径，开启功能必然需新增分支；补入 tasks |
| P2 | T006～T010 在 migration/model 之前修改接入，Phase 2 就声称独立交付却没有完整上传入口；T011 与 T013 的同号 migration 修改策略未说明 | 按 migration→sqlc→model/errors/repository/service/dto/handler/wire→入口顺序重新排任务；纯函数试验可先做，但不是完整 US1 已交付 |
| P2 | 部分关键 FR/SC 没有对应验收任务 | 补 FR-014/023/025/026、SC-004/005/010/011/012 明确用例：空结果、并发续跑、重复消息、同章多变、非法引用、旧版本、跨书同名、预算耗尽、默认关闭 txt/md/PDF 快照 |

### 本轮边界

- 未经核验的本机吞吐、模型优劣与来源许可不作为事实承诺，research 相应降为待验证假设。
- 未擅自制定新的存储架构；上述事项须在同一 plan/tasks 补全再复审，不另建编号。
- 标注详情及复核顺序见 `eval/annotations/aq-ai-v1/README.md`，这不是人工验收通过。


## 第三轮：按用户要求补齐 plan（2026-09-06）

**当前状态**：第二轮列出的设计缺口已给出具体方案，进入实施仍按tasks逐项验证；不是功能验收通过。
新增 data-model.md、contracts/extraction.md、contracts/http-and-chat.md、quickstart.md；
重排tasks为46项并列出FR/SC映射。保留已有AI标注和业务代码原状。

| 第二轮缺口 | 当前决策与落点 |
|---|---|
| 来源及逐块持久化 | plan§3、data-model：PG可空metadata随chunk发布；7张MySQL表明确job/item/attempt/人物/别名/关系/证据 |
| ready后丢入队及过期任务 | plan§4：持久启用意图、全体item初始化、分页reconcile、epoch/lease、原始响应回放 |
| 多层重试/预算续跑/未知费用 | plan§5：provider.ChatOnce单次dispatch；knowledge唯一重试层；跨resume累计、显式追加，unknown结果保留 |
| 别名仅口号/无协议 | plan§6、extraction契约：mentions/new_group/有出处的候选决策；无依据不强合，真实语义仍需人工评估 |
| 聊天入口/预算/权限不明 | plan§7、HTTP契约：可选关系模式、书/人物输入及歧义选择、Agent范围校验、专用证据选择共享预算、既有引用落库 |
| migration和层顺序错位 | tasks Phase2：migration→sqlc→model/errors/repository→Service/DTO/handler/wire→入口，US1完整发布验收 |
| 测试覆盖不足 | tasks T001～T046及覆盖表：FR-001～026、SC-001～012均有任务落点 |

设计取舍：放弃旧“4表/PG零改动”限制，不引入新基础设施；选择显式关系模式而非LLM自动路由。
未运行数据库/模型/HTTP测试，所有tasks保持未勾选；人工真值仍待确认，初标身份保持AI。
实施如需更改契约或增加范围，先更新同一套Spec Kit文档，再继续验证。


## 第六轮：Phase 3 独立审核（2026-09-06）

**结论：Phase 3 不通过，不能标为已完成。**
本轮对象是新 worktree `phase3-extraction`，提交 `f046d59`，不是旧 `strange-driscoll-87a634` 的 `5409f58`。
审核中新增的 Phase 4 提交 `2ce52a3` 仅含 extract_prompt，未纳入本轮。

原有全量 `go test ./... -race -count=1` **868 个测试节点通过，无失败/跳过**；
另加 **10 个顶层契约反例，10 个全部失败**（发布守卫另含4个子场景）。
证据与可复制测试见 [phase3-review](evidence/phase3-review/README.md)。
通过的测试证明已有组件的部分行为；不能代替不存在的生产工作循环，也不能排除未覆盖的崩溃窗口。

### 集成状态

- 第四、第五轮在旧 worktree 修复的 Phase 2 来源/Markdown/PDF/上传问题尚未合入本分支。
  该部分历史见 `/Users/lishurong/go/src/hify/.claude/worktrees/strange-driscoll-87a634/specs/010-narrative-scene-chunking-and-relation-extraction/review-fixes.md`。
- 新分支 narrative_metadata 仍是旧来源区间逻辑；不能继承第五轮“Phase 2 修复已验证”的当前分支结论。
- tasks T014～T023 仍未勾选。应先合入并复验 Phase 2 修复，以下问题也需解决后再验收。

### R6-01 [P1] 恢复扫描没有入队，也没有生产执行循环

位置：`internal/knowledge/extraction_reconcile.go:101–108`、`service.go:1441–1442`、`tasks.go`。
扫描 callback 只有 slog.Info，JobsRequeued 却累加；Service 直接返回 repository 结果，没有 asynqClient.Enqueue。
只有 reconcile handler，没有 `{job_id}` 执行任务或 ProcessRelationExtraction。initializeExtractionJob、
newPhaseRunner、findReplayableResponse、fitExtractionInput 等均没有生产调用方。
也没有扫描“ready+开启但尚未建 job”的 documents；ready 后崩溃的持久意图没有恢复路径。

复现：用 nil queue client 调真实 Service，仍返回 `JobsRequeued=3` 且无错误。
`TestWorkLoopStopsCallingTheModelOnceInvalid` 的工作循环写在测试本身，不能证明生产 worker 已实现；
ChatOnce 的 countingClient 也只是内层方法计数，不是 HTTP 接收计数。

修复：完成 plan§4 的文档意图扫描→初始化→实际入队→worker→账目/校验/回放/发布链路，
唯一重试层和预算限制在该链路真实生效；用真实 asynq 消费+Fake Provider 验丢消息、重启、空结果、预算停止。
归一/模型解析可沿 Phase 4 接口继续，但不能用缺少 worker 的组件集合宣称 T015～T021 已交付。

### R6-02 [P1] 发布与调用预留缺少状态/当前 run/lease 守卫

位置：`extraction_publish.go:312–335`、`relation_extraction.sql:154–161,273–278,310–318`。
verifyJobSourceStillCurrent 声称校验 active run，但锁文档查询根本不取 active_relation_job_id，
实际仅检查版本/ready 与 job.State!=superseded。最终 BumpJobItemOutcome 只看 id+epoch。
AddJobCallReservation 同样仅检查 epoch 和额度，不要求 job running、有效租约或文档启用。

复现：分别替换 active_job_id、关闭抽取、暂停 job、使 lease 过期，**四种情况均能发布成功**；
暂停 job 后仍能预留下一次调用。没有新 worker 抢占时，epoch 相同不能代表租约仍有效。

修复：所有 dispatch admission / 业务发布用同一套 document→job→item 锁与当前状态校验；
核对 enabled、active_job_id、version、epoch、lease、item所属及状态；锁定 job 后再锁 item。
迟到调用的账目仍可结算，业务发布须拒绝，不能把“记账允许”混成“继续调用允许”。

### R6-03 [P1] 预留和次数计数分开提交，崩溃后永久撞重复编号

位置：`extraction_retry.go:173–185`、`extraction_ledger.go:94–144`。
reserveExtractionAttempt 已提交 attempt_number=1 后，才单独 BumpItemAttemptCount；中间崩溃时
attempt 已存在，item.extract_attempt_count 仍为0。恢复将旧 attempt 改判 unknown，也不会修复计数。

复现：模拟此提交窗口后 runPhase 继续从1预留，MySQL返回 `uk_rea_item_phase_attempt` 重复键，
新调用次数为0，自动恢复仍会反复撞相同错误。

修复：同一事务锁 job/item、分配并递增阶段次数、预留额度、插入 attempt；恢复读取持久事实，
不能把两条提交之间的空窗留给重新编号。补三阶段崩溃点与并发重复任务测试。

### R6-04 [P1] 连续心跳失败超过 TTL，keeper 仍声称有效

位置：`extraction_lease.go:170–190`。
每次续租 error 都直接 continue，没有按最后一次成功续租计算失效期限；也没有给单次续租设置
剩余租约期限内的超时。数据库长期故障时 Valid 可一直为true，旧 worker 仍可能继续调用。

复现：TTL=20ms，续租每次报错，150ms后仍 `Valid=true`，Lost未关闭。

修复：保存最近确认的到期时间，查询/续租不得越过该期限；到期立即标失效并取消工作 context。
续租SQL也不能让已过期的旧 owner 在没有新claim时自行复活。验证短暂故障与超过TTL两种边界。

### R6-05 [P1] 初始化只检查 schema 标号，坏来源也可建作业

位置：`extraction.go:168–176`。
decodeNarrativeMetadata 只解JSON并检查schema_version，没有调用覆盖/区间校验器或验证文档hash。
因此“能解JSON”被当成了“每个chunk有可用来源”。

复现：将两个已发布chunk的metadata改成 `{"schema_version":1}`，初始化仍成功。

修复：按正文长度验证完整来源映射、合法结构顺序、hash一致性；缺失/损坏/跨文档不一致整批拒绝。
须在合入上一轮精确来源修复后验证，不能只加schema字段或空对象兜底。

### R6-06 [P1] 已明确截断的响应仍被当作成功回放

位置：`extraction_ledger.go:155–168`、`relation_extraction.sql:286–290`。
64KiB截断只写 error_code=response_truncated，state仍completed；FindReplayableAttempt完全不看
error_code/finish_reason，返回被截掉内容的响应作为可回放结果。runPhase对原始completed响应也直接accept。

复现：落盘64KiB+1字节响应，再查回放，found=true。

修复：截断/finish_reason=length必须保留调用账目但标为不可接受结果；回放选择必须排除此类响应，
首次与恢复路径共享同一校验，不因重启改判。测试应穿过完整worker而非仅测capRawResponse长度。

### R6-07 [P2] 清理永远反复扫已空的第一批，后续作业饿死

位置：`relation_extraction.sql:399–412`、`extraction_archive.go:236–278`。
ListDeadJobsWithDerivedRows并未过滤“还有派生记录”的作业，没有游标/清理标记；清理后保留jobs账目是正确的，
但下次扫描依然命中同一批最小ID，排序后面的job永久进不来。

复现：两个到期superseded job，第一个无派生数据、第二个有2个人物，批大小1，连续清理3次仍剩2个人物。

修复：按游标遍历或事务性记录清理完成，并在删除前锁定/复检仍可清理的状态，避免用户续跑与清理竞态。
测试超过一批、空首批与并发恢复，不只验证单个job删完。

### R6-08 [P1] 费用归档既覆盖旧金额，又把DECIMAL写成字节数组文本

位置：`extraction_archive.go:159–168`。
CostAmount直接 `fmt.Sprint(sum.CostAmount)`，没有加prev.CostAmount；MySQL驱动的DECIMAL返回[]byte，
最终存成 `[49 46 ...]`，不是金额。jobLedgerTotals不返回金额，原“总费用不变”测试只比token/次数，漏过此错。

复现：分两批各归档1.25 USD，预期2.50，实际 `CostAmount="[49 46 50 53 48 48 48 48 48 48]"`。

修复：用精确decimal解析与累加，保留currency/pricing_version/cost_kind及未知口径；不得混币种相加。
归档、汇总读取还需同一一致性快照/锁，避免并发归档时把同一批同时计入live与archive。
本地Ollama不计金额不能掩盖契约中estimated/measured金额路径损坏。

### R6-09 [P2] ChatOnce的timeout不包含并发槽/限流等待

位置：`internal/provider/chatonce.go:88–112`。
先 acquire(ctx)/checkRateLimit(ctx)，之后才创建callCtx并启动计时。并发槽占满时，
调用方给20ms timeout也会等待外层context；ctx无deadline时没有这个参数承诺的上限，等待时间也不进账。

复现：占住唯一并发槽，timeout=20ms、外层deadline=200ms，实际约201ms才返回。

修复：从入口创建总deadline并贯穿排队/限流/dispatch，分别如实记录未dispatch与已dispatch时间；
补真实HTTP单请求计数，检查超时后台goroutine仍占用适当资源且有等待/回收策略。

### T014～T023 当前验收状态

| 任务 | 本轮结论 |
|---|---|
| T014 | 部分：单次装饰器已有；等待超时错误，真实HTTP计数证据不足 |
| T015 | 部分：初始化函数有；无生产入口，坏metadata可进入 |
| T016 | 部分：lease CAS/心跳有；到期失效和调用前守卫不足，工作循环只在测试里 |
| T017 | 部分：账目事务有；缺生产dispatch串联，预留/阶段计数非原子 |
| T018 | 部分：retry helper有；崩溃恢复重复编号，连续失败停止未接入生产 |
| T019 | 部分：预算/输入限制helper有；生产worker未接通，不能称实际调用受限 |
| T020 | 部分：结果事务有；当前run/lease/暂停守卫缺失，截断响应误回放 |
| T021 | 未交付：周期任务注册有，但无ready文档意图恢复/真正入队/执行handler |
| T022 | 部分：版本/删除/显式superseded有；当前run和启用/租约检查不足 |
| T023 | 部分：归档/删除有；扫描饿死和金额损坏已复现 |

### 修复顺序与复审门槛

1. 合入Phase2修复，完成真正的Phase3 worker与入队/恢复链路，不另建状态或绕过既定Spec Kit。
2. 先修统一守卫、预留计数原子性和lease到期，再接模型调用；账目结算不受旧epoch丢弃。
3. 修来源校验、响应回放、清理扫描及金额归档，把上述反例纳入正式回归。
4. Fake Provider + 真实MySQL/PG/Redis验证丢消息、崩溃窗口、暂停/替换run、空结果、预算耗尽与重启。
5. 再运行全量race、vet、双门禁；提供生产调用链和真实队列证据，不用测试内手写循环冒充。

本轮在隔离副本进行反例验证，工作目录仅补审核记录/证据和tasks状态说明；未修改Phase3业务实现，
未替换正在推进的Phase4代码，未commit/push。上述缺口属于待完成的Phase3实施，不是假装已完成后的小修收口。
