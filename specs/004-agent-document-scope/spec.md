# Feature Specification: Agent 文档范围绑定（Agent Document Scope）

**Feature Branch**: `004-agent-document-scope`

**Created**: 2026-09-02

**Status**: Draft

**Input**: 002 交付了检索范围过滤能力，003 给它做了「试检索」调试面板。但**普通对话仍然用不到它**——
`conversation` 调 `Retrieve` 时传的是零值 `RetrieveOptions{}`。本期让 Agent 能绑定
「知识库内的某几份文档」，把过滤真正接进对话链路，兑现 002 spec 里那个原始故事：
知识库里挂了三个版本的产品手册，用户只想让 Agent 用 2026 版。

## Clarifications

### Session 2026-09-02

- Q: 文档范围存哪？ → **A: 新建 `agent_documents(agent_id, document_id)` 关联表**。
  `document_id` 全局唯一且天然属于某一个知识库，不需要再冗余 `knowledge_base_id`。
  沿用 `agent_knowledge_bases` 的既有形态（复合主键、无独立 id 列）。
- Q: 一个 Agent 绑了两个知识库，却只给其中一个圈了文档，另一个怎么办？
  → **A: 范围是 Agent 级的全局列表，非空即「只检索这些文档」**，
  没有被圈到文档的知识库因此不参与检索。
  **这个语义之所以不令人意外，靠的是界面**：文档勾选列表按知识库分组、
  完整展示当前生效的全部文档，用户看得见自己圈了什么、漏了什么。
  被否决的方案是「按知识库分别限定」（KB1 受限、KB2 不受限）——
  它需要给 002 的 `RetrieveFilter` 引入「按知识库分组的文档列表」，
  SQL 要写成多组 OR，复杂度显著上升；而收益只在「一个 Agent 绑多个知识库
  且只想限制其中一部分」这个当前不存在的场景里（现有 5 个 Agent 全部绑 0 或 1 个库）。
- Q: 被圈进范围的文档被删除了怎么办？ → **A: 不级联删除范围记录**。
  留着那条失效的 `document_id`，按 002 的 FR-010 它匹配不到任何东西。
  **绝不能级联删除**：范围记录被删空之后，系统无法区分「从未限定」和「限定的文档都被删了」，
  于是会退回成「不限定」——这正是 002 全篇在防的那件事：**悄悄放宽用户指定的范围**。
  宁可检索不到，也不能让 Agent 用范围外的资料回答。
- Q: 过滤开关默认关闭，Agent 配了范围但开关没开怎么办？
  → **A: 把 `HIFY_RAG_METADATA_FILTER_ENABLED` 的默认值改成 `true`**。见下方「唯一的行为变更」。

## 唯一的行为变更：过滤开关默认改为开启

**这是本期唯一一处会改变现有部署行为的改动，必须显著标注。**

**为什么必须改**：`conversation` 在 `Retrieve` 返回错误时的既有处理是
「记一条 warn，然后 `candidates = nil` 继续这一轮」（`context.go`）——这对真正的检索故障
是正确的降级（一次检索失败不该让整轮对话失败）。但如果开关关着而 Agent 配了文档范围，
`Retrieve` 会返回 `ErrMetadataFilterDisabled`，于是这一轮**不带任何知识库资料**就去回答了。
用户会看到 Agent 凭空作答，而不是「我被限定在这几份文档里，里面没有」。
这是一条**静默降级**路径，与 002 的整体设计正面冲突。

**为什么改了是安全的**：002 的 SC-003 已经证明——**空过滤器 + 开关开启**时，
检索输出与该功能上线前**逐字一致**（确定性门禁的既有用例逐字段比对，`IDENTICAL`）。
因此对**没有配置文档范围**的 Agent（当前全部 Agent），开关从 `false` 改成 `true` 不改变任何行为。
它改变的只有一件事：配了范围的 Agent 现在能真正用上范围，而不是拿到一个错误。

**取舍**：也考虑过「保持默认关闭，改成让这一轮对话直接失败」，
但为了一个配置项让整轮对话失败，代价高于收益，且它把一个部署配置问题变成了终端用户可见的故障。

## User Scenarios & Testing *(mandatory)*

### User Story 1 - 把 Agent 限定到指定文档 (Priority: P1)

知识库里挂着三个版本的产品手册，措辞高度相似。用户希望某个 Agent 只用 2026 版回答。

**验收**：给 Agent 配置文档范围为「2026 版」后，同一个问题的回答与引用
MUST 只来自该文档；不配置范围时行为与本期上线前一致。

### User Story 2 - 范围要看得见 (Priority: P1)

Agent 表单里必须能看清当前圈了哪些文档，否则「只圈了 KB1 的文档导致 KB2 整个不参与检索」
这个语义会变成一个陷阱。

**验收**：Agent 表单中，文档勾选列表 MUST 按知识库分组展示，
MUST 显示文件名而非 ID，且 MUST 明确提示「不勾选 = 不限定」。

### User Story 3 - 不配置范围的 Agent 行为逐字不变 (Priority: P1)

**验收**：没有文档范围的 Agent，其检索输出 MUST 与本期上线前逐字一致；
确定性检索门禁的既有用例 MUST 逐字段不变。

### Edge Cases

- 范围内的文档被删除 → 范围记录保留，该文档匹配不到内容；若范围内文档全被删除，
  该 Agent 检索结果为空，**MUST NOT 退回成不限定**。
- 范围内的文档被重新处理（`RetryDocument`）→ `document_id` 不变，范围继续有效。
- 范围里的文档不属于该 Agent 绑定的任何知识库 → 该条件匹配不到内容（002 的 FR-010），
  不报错。保存时 MUST 校验并拒绝这种配置。
- 文档数超过 002 的 50 份上限 → 保存时 MUST 拒绝，MUST NOT 静默截断。
- Agent 解绑了某个知识库，但该库的文档还在范围里 →
  **服务端 MUST 拒绝**（`ErrInvalidScopedDocument`），**MUST NOT 静默清理**。
  静默清理等于替用户改了他配置的范围，与本期其余部分的原则冲突。
  **前端 MUST 在取消勾选知识库时就可见地移除对应文档**——列表当场变短，
  用户看得见范围变了，因此不会撞上这个错误。
  「可见地移除」与「静默清理」的区别就在这里：前者用户看得到，后者看不到。

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: MUST 新增 `agent_documents` 关联表，记录 Agent 与文档的范围绑定。
- **FR-002**: Agent 的创建与更新 MUST 支持设置文档范围；空列表 = 不限定。
- **FR-003**: `conversation` 组装上下文时 MUST 把 Agent 的文档范围作为
  `RetrieveOptions.Filter.DocumentIDs` 传给 `Retrieve`，MUST NOT 另起检索路径。
- **FR-004**: 保存文档范围时 MUST 校验每个 `document_id` 都属于该 Agent 绑定的知识库之一；
  不满足 MUST 返回明确错误，**MUST NOT 静默剔除**不合规的文档。
- **FR-005**: 文档范围的条数 MUST 不超过 002 的 `maxFilterDocumentIDs`(50)；
  超出 MUST 拒绝，MUST NOT 截断。
- **FR-006**: 删除文档 MUST NOT 级联删除范围记录（理由见 Clarifications）。
- **FR-007**: `HIFY_RAG_METADATA_FILTER_ENABLED` 默认值 MUST 改为 `true`。
- **FR-008**: 未配置文档范围的 Agent，其检索行为 MUST 与本期上线前逐字一致。
- **FR-009**: Agent 表单 MUST 提供按知识库分组的文档勾选界面，MUST 显示文件名。
- **FR-010**: 检索诊断 MUST 能反映本轮是否因 Agent 范围而施加了过滤
  （沿用 002 已有的 `filter_applied` / `filter_document_id_count` 字段，不新增）。

### Key Entities

- **Agent 文档范围（Agent Document Scope）**：Agent 与文档的多对多绑定。
  空集合是合法且默认的取值，语义为「不限定」。

## Success Criteria *(mandatory)*

- **SC-001**: 真实对话中，给 Agent 配置文档范围后，回答的引用只来自范围内文档；
  不配置时引用可来自该知识库任意文档。
- **SC-002**: 确定性检索门禁既有用例逐字段不变（SC/FR-008 的机器验证）。
- **SC-003**: 集成测试覆盖：范围生效、范围为空等价于不限定、
  跨知识库文档被拒绝保存、超上限被拒绝、文档删除后范围不退化为不限定。
- **SC-004**: `make eval` 的确定性检索指标（`retrieval_hit`/`recall_at_*`/`mrr`/
  `expected_document_cited`）在本期前后不变——测试集里的 Agent 不配置范围，
  因此这几项 MUST 与 `eval/baseline.json` 一致。**裁判分不作为验收依据**
  （理由见 `docs/eval-environment-setup.md` §7.1：同一份代码两次跑分可差 4 分）。

## Assumptions

- 002 的 `RetrieveFilter` / `RetrieveOptions` 语义不变，本期只是它在对话链路上的第一个调用方。
- 页码范围不进入 Agent 配置——按页限定一个 Agent 的长期行为没有合理场景，
  它属于单次查询（003 的试检索面板已覆盖）。
- 前端沿用 Agent 表单既有形态，不引入新页面。

## Out of Scope

- **按知识库分别限定文档范围**（见 Clarifications 的否决理由）。
- 在 Agent 上配置页码范围。
- 由 LLM 从用户问题中自动抽取过滤条件（002 起就明确排除，仍然排除）。
- 对话时临时覆盖 Agent 的文档范围。
- 文档被删除时对引用了它的 Agent 做提示或告警。
