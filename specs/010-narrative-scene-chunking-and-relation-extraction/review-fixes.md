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


## 第四轮：Phase 1 / 2 独立审核（2026-09-06）

**结论：Phase 2 不通过当前 plan 验收；Phase 1 证据未核齐，不能直接判完成。**

### 审核范围与方法

- 开始时HEAD为 `7d29e1e`，已进入Phase 3，并存在未提交的extraction_publish与SQL改动；本轮不修改/验收这些内容。
- 隔离导出 `d09c709`（US1上传收口1d74b02之后的.gitkeep修复），避免编译正在修改的Phase 3文件、也不触碰共享数据库。
- 核对 `d09c709..7d29e1e`：narrative.go / narrative_metadata.go / chunk.go / narrative_upload_test.go没有后续差异。
- 在隔离副本运行narrative_test.go的28个测试及5个上传/分派纯测试，`go test -race -count=1`：**33个通过**。
- 另加8个按既定plan编写的契约测试：**8个失败**。仅放隔离目录，不修改工作目录业务代码或原测试。
- 本轮未运行数据库集成、双门禁、浏览器/真实服务HTTP；httptest是handler测试，不当完整HTTP冒烟。

### 必改项（按优先级）

#### R4-01 [P1] 来源区间不是精确映射，校验器却接受

位置：`internal/knowledge/narrative_metadata.go:126–161`、`chunk.go:807–816`。
输入 `第一章　甲\n甲。\n\n\n\n乙。`，chunk把四个换行合成两个，metadata却把整个chunk映射为
一段连续原文范围。复现输出 `validator=<nil>`，但chunk片段与源区间内容不相等。
引用位于合并点之后时，按rune偏移映射会指错；忽略空白比较不能修复坐标，
也违反plan§3/data-model§4的精确区间与generated_separator约定。

修复：沿分块/拼接保存逐段来源，新增字符单独标generated_separator；可定位segment的长度及逐字文本须匹配。
测试不得用strings.Fields移除全部空白后冒充精确来源验收。对应T010/T011/FR-007/025。

#### R4-02 [P1] overlap原文被丢掉来源，后续跨块证据无法定位

位置：`internal/knowledge/narrative_metadata.go:151–154`及`chunk.go:841–845`。
构造30字符上限、5字符overlap，实际前缀 `甲甲甲甲。\n` 被整体标IsOverlapCopy且DocumentStart/End为空，
连真实复制文字与生成换行都未区分。plan要求复制文字映射回同一原文位置，以便证据去重；
当前实现是“不可引用”，不是“已去重”，会丢掉依赖前块尾部的关系依据。
现有TestOverlapCopyCarriesNoDocumentInterval正好锁住了这个错误契约。

修复：真实复制段保留原文区间，仅生成分隔符无来源；跨overlap引用按源区间去重。
对应T011/FR-002/007/025。

#### R4-03 [P1] Markdown叙事模式既不识别标题包装，又把代码块当故事结构

位置：`internal/knowledge/narrative.go:183–205`、`chunk.go:220`叙事优先分派。
`# 第一章 初见 ... # 第二章 别离`只产1块、章节nil；fenced code内的`第二章`和`***`
却把一个场景切成3块。原因是开启模式直接给通用纯文本正则，没有Markdown行状态。
这会漏掉真实章节、制造假章节并删除代码中的分隔线，不只是标题显示问题。

修复：识别Markdown标题包装，fenced code内禁用章节和分隔符解析，并保留原文映射；
加两个独立反例。对应T009/T011，plan§3第1～2条。

#### R4-04 [P2] 已约定分隔符未实现，无结构文本反而被标为有分隔线

位置：`internal/knowledge/narrative.go:44–45,160–164,chunkNarrative/sceneKey`；metadata.go:145–148。
单个`※`与两个连续空行均只产1块；没有任何结构的普通句子却得到`scene_key=ch0-s1`、
`boundary_kind=divider`。此外scenesWithin直接丢弃分隔行，偏离plan“分隔线附前场景”的正文保存约定。
既有TestNoContentLost先把分隔线从期待值删除，不能验证plan中的无遗漏。

修复：实现约定的边界类型；未知结构chapter/scene语义ID留空，boundary=none，来源位置照常保留。
结构标记来自实际识别结果，不能由“有scene_key”反推。对应T009/T010/T011。

#### R4-05 [P1] US1宣称收口，但PDF叙事支持未交付

位置：`internal/knowledge/service.go:341–342`、前端knowledge-documents-dialog.tsx仅支持txt/md文案。
`validateUploadOptions(FileTypePDF, UploadOptions{Narrative:true})`明确返回“不支持”。
拒绝比静默回退诚实，但plan、spec和T009/T011/T013已经包含可解析PDF跨页叙事。
提交说明把PDF说成独立后续工作，不能替代用户接受的范围调整。

修复：按当前plan接入PDF段落流和实际页码映射并验收；若要延期，先由用户接受缩范围再同步spec/plan/tasks，
此之前不得标完整Phase 2通过。抽取开关暂不展示属于阶段性安排，本轮不要求未完Phase 3假装已可用。

#### R4-06 [P2] 上传API与既定契约不一致，明确请求可能静默关闭

位置：`internal/knowledge/handler.go:159–165`。
契约是`narrative_mode`/`extract_relations`，实现只读`is_narrative`/`is_relation_extraction_enabled`；
按契约上传`narrative_mode=true`得到200但Narrative=false。
无效布尔如`ture`也被静默当false，违背契约400；当前前后端同用实现字段，不能证明外部契约成立。

修复：统一字段并严格解析，缺省=false、合法true/false、有值非法=400；
若保留实现字段须显式兼容或同步经确认的契约，不能吞掉契约字段。对应T012。
默认关闭DTO还新增两个非omitempty字段；需补真实旧JSON快照或明确响应兼容调整，不能称响应逐字节未变。

### Phase 1 与验收证据缺口

- tasks T001～T013当前全部未勾选，也仍留着设计阶段“全部未勾选”的说明；不能靠它判断进度。
- 提交说明自报全量race/vet/双门禁一致，但当前spec/docs没有关联的010改动前原始输出与稳定产物。
  已有docs/eval-phase1-baseline-report.md是此前RAG裁判基线，不是010 Phase1。
- 语料manifest/AI稿已落库；T003要求的重新获取/生成脚本、上游版本及T005冻结故障夹具未在本轮找到对应交付。
  如保存在别处，补路径/hash后再验，不因此断言从未做过。
- 现有TestDefaultOffChunkingIsUnchanged主要查metadata=nil和一个MD标题；没有逐一与改动前内容/顺序快照比较。
  TestNarrativeDocumentProcessesEndToEnd从repo.createDocument起步、使用Fake Provider，不能替代上传API/真实运行服务冒烟。

### 复现证据与复审门槛

隔离目录：`/var/folders/5x/14kbs_jx5yb7xz11hhhdqg4h0000gn/T/hify-010-phase12-review-l_n4xjll`。
`review-existing-tests.log`：33相关测试race通过；`review-contract-tests.log`：8个契约失败；
`internal/knowledge/zz_review_phase12_test.go`保存全部复现输入。
运行：`go test ./internal/knowledge -count=1 -run '^TestReview' -v`（在隔离副本中）。

复审先修R4-01～06、把反例纳入正式测试，再补010 Phase1基线/来源/故障夹具归档；
顺序跑数据库发布与PDF集成、真实上传HTTP、默认关闭快照及双门禁。
修订原先锁住偏差的测试，不能通过放宽spec来迎合测试。已确认偏差修复前，Phase2状态保持未验收。


### 第四轮续查：默认关闭的新旧版本对照

按用户“继续”要求，在两个独立导出副本比较 `1f37c1d`（010业务实现前）与 `d09c709`。

- 90份固定/固定种子构造文本 × 7种ChunkSize × 7种overlap × txt/md/PDF，**13,230组输入全部一致**。
- 比较字段为输出块的Content/PageNumber/PageEnd/SectionTitle及顺序，不仅检查新字段是否nil。
- 覆盖空文本、中英文混合、CRLF、段落、句末标点、无标点长句、MD标题/表格/围栏和PDF文本页。
- 证据：隔离Phase2目录中的 `review-default-comparison.log`、`review-default-run.log`；
  `internal/knowledge/review-default-snapshot.jsonl`为完整快照，
  `internal/knowledge/zz_default_snapshot_test.go`为构造对照输入。
- **结论限定**：上述样本未发现默认关闭分块回归；不能替代完整解析器/真实HTTP/数据库/门禁验收。
  当前六项叙事契约问题仍然成立。默认关闭HTTP JSON新增字段也不在这次纯分块比较内。
- 补充核对：testutil使用按包命名的独立测试数据库，不直接用开发库；同一包的并行测试进程仍可能互相重建测试库。
  本轮没有运行这些数据库测试，避免干扰当前Phase3工作，也没有将skip记作通过。


## 第五轮：按用户授权直接修复（2026-09-06）

**当前结论：R4-01～06 已修复并通过复验。Phase 2 的分块/上传机制可用；不等于真实检索效果或整项 010 验收。**
Phase 1 缺失的可复现材料已补录；无法倒推出的历史记录和上游 revision 如实保留限制。
完整源码 hash、测试原始输出与复跑方法见 [修复证据](evidence/phase12-fixes/README.md)。

| 项目 | 已落地修复与回归 |
|---|---|
| R4-01 来源不精确 | 分块、trim、join 逐 rune 传递来源；合成分隔符显式标识；可定位段长度必须一致。纯函数及落库测试均逐字比较，包含空白 |
| R4-02 overlap 丢来源 | 真实复制字保留原始区间；合成连接字单独标记；字符级硬切 overlap 也有来源。重复段不做首个 substring 猜测 |
| R4-03 Markdown | ATX 标题包装、反引号/波浪号围栏状态；围栏内禁止章节/分隔线/空行场景识别；添加短围栏不能关闭长围栏反例 |
| R4-04 边界失真 | 单个/连续 ※、原文两个连续空行；分隔线留在前场景；未知结构 scene_key=nil/boundary=none；章节退化标 chapter_fallback；scene_key=hash+源起点。补两/序及句末闭引号 |
| R4-05 PDF 缺失 | 接入已有去噪与跨页段落重组，逐来源段保留实际页码；PDF 不以布局空行推断场景；UI 支持范围及超长仍拆分的说明已改 |
| R4-06 上传契约 | 规范 narrative_mode/extract_relations；兼容旧别名但拒绝冲突、非法布尔；缺省=false；关闭的响应新字段省略，前端改用规范字段 |

额外发现：全量验证初次复制到了其他进程故意注入的游标变异，不将其计为正式代码缺陷。
恢复正常实现后重跑；修正 extraction_reconcile_test 的重复 job-budget 夹具 ID，并给游标回归加 2 秒上下文超时，
使将来的变异能及时失败而不是等待整个测试超时。其他 Phase 3 业务改动保留。

### 验证和明确边界

- 全量 race / go vet / check-deps 通过，测试计数与零 skip 见 evidence/phase12-fixes/manifest.json。
- 默认关闭 13,230 组与 1f37c1d 一致；14 个检索门禁逐字段一致；上下文门禁除运行时间外一致。
- txt/md/PDF 真实 TCP 上传、JWT、存储、入队、调用 worker 同一处理入口、PG 来源发布通过；embedding 用 Fake Provider。
- 前端 tsc+Vite 在 Node 24.19.0 通过；未改变依赖，未宣称 Node 21 环境可用。
- 语料重建脚本校验 9 章/322 ID，产出 314 正文及源 ID 映射；输入篡改拒绝、两次重建一致。
- 构造模型/故障输入补冻到 eval/fixtures/narrative-010-v1，未把它们冒充真实模型结果或人工真值。
- 修复前的旧叙事元数据需重新上传/处理才能修正；没有自动改写用户数据。
- 未 commit/push；未做独立后台进程、真实模型或人工质量验收。Phase 1 上游不可变版本/发布许可仍待补证。


## 第六轮：第五轮修复的复核与回归修补（2026-09-07）

**结论：第五轮的 R4-01～06 方向正确、测试是往严上改的（逐字比较、overlap 保留真实区间），
但引入了 2 个回归，另有 1 处命名与实现不符。已全部修复。**
`go build` / `go vet ./...` / `go test ./... -race -count=1` / `make check-deps` / 前端 `tsc` + `vite build` 均通过。
本轮未跑数据库集成之外的真实服务冒烟，未运行模型，不构成 010 的功能验收。

### R6-01 [P1] 带空格的场景分隔线不再被识别（第五轮引入）

位置：`internal/knowledge/narrative.go` `sceneDividerPattern`。
为了让单个 `※` 也算分隔线，第五轮把 `※` 从通用字符类里挪出来单开 `※+` 分支，
通用类里就没有 `※` 了，于是 `※ ※ ※`——中文小说里最常见的分隔写法——不再匹配：
两个场景被合成一个，没有任何错误。原有用例只覆盖裸 `\n※\n`，抓不到。

修复：`※` 同时保留在 `※+` 分支和通用字符类里。
顺带修了一个**更早就存在**的同类缺口：空白从 `\s` 改成 `[\s\p{Zs}]`——
Go 的 RE2 里 `\s` 不含全角空格 U+3000，而中文排版的 `※　※　※` 用的正是全角空格。
回归：`TestSpacedAsteriskDividerForms`（8 种正例 + 5 种反例 + 端到端场景数）。

### R6-02 [P1] 引号感知的断句被 `BoundaryKind` 当成了开关（第五轮引入）

位置：`internal/knowledge/narrative.go` `chunkNarrativeWithOptions`。
`keepClosingQuotes`（句末 `”`/`」` 跟着它闭合的那句走）被接到了
`scene.BoundaryKind != boundaryNone` 上。结果同一段对白，只是加不加章节标题，
断句结果完全不同——没识别出结构的文本每一块都从一个孤零零的 `”` 开头。
影响范围包括无章节的节选、全书无章节的作品，以及**所有认不出章回的 PDF**
（`chunkNarrativePDF` 传 `blankScenes=false`，无章节必然是 `boundaryNone`）。
引号属于哪一句是行文的性质，与这份文档碰巧有没有标题无关。

修复：叙事模式下恒为 `true`。
回归：`TestClosingQuoteHandlingDoesNotDependOnStructure`（有无标题逐块比对 + 块首闭引号断言）。

### R6-03 [P1] 恢复扫描从不重新入队，字段名与日志却说它入队了

位置：`internal/knowledge/extraction_reconcile.go`、`tasks.go`、`cmd/hify/main.go`。
`ReconcileResult.JobsRequeued`、`jobs_requeued` 日志和"只把作业重新丢进队列"的注释
都声称作业已被重新入队，但实现里 `visit` 只有一句 `slog.Info`，
全仓库不存在抽取的 asynq task type。第二轮的 P1 缺口
（"ready 后崩溃 → 作业永远不开始"）**没有关掉**。

修复（**只改名与说明，不假装补上功能**）：
`JobsRequeued` → `JobsNeedingRecovery`；日志显式带 `jobs_requeued=0` 并注明未接入；
逐条日志从 Info 降到 Debug（扫描不夺租约，同一条作业每轮都会被扫到，
分钟级周期打 Info 会把日志刷成噪音）；`reconcileRelationExtractions`、
`NewRelationExtractionReconcileHandler` 和 main.go 的 cron 注册处都标了 🚧 未完成及后续落点。
cron 保留注册（本身空跑，让恢复路径与 Phase 3 其余部分一起上线并被观察）。
**抽取的入队与整条恢复链路仍未交付**，`validateUploadOptions` 的
`ErrRelationExtractionUnavailable` 也仍在，抽取现在一条作业都建不出来。

### R6-04 [P2] `validateNarrativeMetadata` 只有测试在调

位置：`internal/knowledge/service.go` `ProcessDocument`。
第五轮的来源修复很依赖这个校验器，但线上没有任何东西挡着一份坐标错位的元数据被发布。
这类错误没有运行时症状：块照样嵌入、照样召回，只是引用指向原文的错误位置且看上去合理。

修复：新增 `validateNarrativePieces`，在分块之后、**花嵌入的钱之前**逐块自检，
不过就走 `failDocument`（用户看得见、可重试）。刻意用非 apperr 的内部哨兵
`errNarrativeMetadataInvalid`，走 `userFacingFailureMessage` 兜底给用户通用提示，
细节进 `slog.Error`。回归：`TestNarrativePiecesPassTheValidator`（真实输出通过 + 篡改区间必须被拒）。

### R6-05 [P2] `is_narrative` 无人消费

`web/src/routes/knowledge-documents-dialog.tsx` 文档列表加「场景分块」标记。
这个开关上传时写定、之后不可改，而它决定了引用能不能定位到原文位置——
界面上完全看不出来是不对的。

### 复核后判定「不是缺陷」的一项

`boundary_kind` 是**整段 body 一个值**而不是逐场景一个值。初看像是失真
（一章中间有分隔线时，该章第一个场景也被标 `divider`），复核后认为当前实现是对的：
分隔线把 body 切成若干段，所以有分隔线的 body 里每个场景都至少有一端紧贴真实分隔线。
反过来逐场景按"谁起的头"标，才会把这一章的第一个场景标成 `chapter_fallback`，
而那个值的含义是"没做到场景识别、只退到章的粒度"——与事实相反。
已在 `scenesWithinOptions` 就地写下理由，避免以后被当成 bug"修掉"。

### 本轮边界

- 未 commit / push；未运行真实模型、真实浏览器或人工质量验收。
- 未重新审计第五轮的 evidence 产物，`evidence/phase12-fixes/` 保持原状——
  其中的测试计数与快照是修复前的运行结果，不覆盖本轮改动。
- tasks.md 的勾选状态未改：R6-01/02 说明 T009/T011 的用例仍有盲区，
  但本轮没有重跑第五轮那套完整证据，不据此改判进度。

## 第七轮：Phase 4 与运行侧落地（2026-09-07）

**结论：抽取从"一条作业都建不出来"变成了可以端到端跑完一份文档。**
`go build` / `go vet ./...` / `go test ./... -race -count=1` / `make check-deps` 全过，
数据库集成测试真实执行（MySQL + PG 容器在跑，不是 skip）。
**仍然没有跑过真实模型**，全部验证用的是可编程的假模型；真实效果、成本、
精确率/召回率仍归 Phase 6，本轮不做任何效果声称。

### 落地的内容

| 任务 | 内容 |
|---|---|
| T024 | `extract_prompt.go`：严格 JSON（重复 key / 尾随对象 / 未知字段 / 围栏 / 截断全部整次拒绝）+ 封闭 8 类关系 + 规模上限 |
| T025 | 引用按 rune 精确定位（occurrence 重叠计数，口径写进指令）、映射回原文区间、跨拼接分隔符拒绝、证据按原文区间去重 |
| T026 | `alias.go`：候选选取（32/每人 2 条依据，排序无相似度）、决策校验、误合并的全部拒绝规则 |
| T027（半） | 第一阶段响应复用、归一独立重试、ambiguous 保留独立人物、零调用路径 |
| T021 | `extraction_runner.go` + asynq task type + main.go 注册 + 恢复扫描入队 + 上传开关放行 |

### 几条值得单独记下来的判断

- **校验放在重试循环里面**。放外面的话一次漏字段的输出直接判 item 失败，
  而契约给的是至多 3 次。这次调用确实发生过，照常记账——一次输出不合法的
  调用和一次成功的调用花的钱一样多。三次都不合法时 `last_error_code` 记
  `extract_invalid` 而不是 `model_gave_up`：前者改 prompt/schema，后者查模型
  和超时，两者的下一步不同。
- **预算耗尽和连续失败停成 paused**，不留在 running。留着的话恢复扫描每分钟
  把它捡回来，每次都在同一处因为同样的理由停下——一个不花钱但永不停歇的循环。
- **认不出来的错误让整个作业停下**，不逐个把 item 标失败。基础设施问题
  （数据库、编码）会把 500 个 item 一个个"失败"掉，最后产出一份看起来像
  "模型抽不出东西"的报告。
- **上传开关的拒绝理由换成"没有配置抽取模型"**，并且 `HIFY_RELATION_EXTRACTION_MODEL_ID`
  故意没有默认值：默认指向对话用的模型，会让一次上传悄悄开始烧几个小时的 GPU。
- 别名的犹疑词表（`大约`/`可能`/`不是`…）是**固定词表，不是语义理解**。
  它挡得住原文明写的犹疑（"阿Q大约是阿贵，也未可知"——这正是标注指南点名的
  反例），挡不住要读懂上下文才能判断的假设句。这条必须写进报告。

### 本轮边界

- 未跑真实模型、未做人工标注、未产出任何效果数字。
- item 之间**串行**，没有并发：并行要让三重预算闸门、连续失败计数、租约
  有效性全变成并发安全的判断，而它们判断错了都只表现为数字不对。
- T022（模型/源版本固定、删除守卫、数据库故障注入）、T023（清理与归档）未做。
- Phase 5 全部未做：没有 enable/disable/pause/resume/restart 接口，
  停成 paused 的作业目前**没有界面可以让用户恢复**——只能改数据库。
  这是本轮留下的最明显的缺口。
- 未 commit 之外的动作：没有 push，没有跑 smoke。
