# Phase 12：Agent 文档范围绑定（Agent Document Scope）实施报告

**功能分支**：`004-agent-document-scope` ｜ **规格**：`specs/004-agent-document-scope/`
**实施日期**：2026-09-02
**开发方式**：spec-kit 流程（specify → plan → tasks → implement），AI 辅助开发

## 0. 一句话总结

新增 `agent_documents` 关联表，让 Agent 能把检索限定在所绑定知识库里的某几份文档；
`conversation` 组装上下文时把它作为 `RetrieveOptions.Filter.DocumentIDs` 传给 `Retrieve`。

**这是 002 的过滤能力第一次进入真实对话链路。** 002 交付了能力但没有入口，
003 给了调试面板但不改善对话，本期才真正兑现 002 spec 里那个原始故事：
知识库里挂着三个版本的手册，让 Agent 只用 2026 版。

**唯一的行为变更**：`HIFY_RAG_METADATA_FILTER_ENABLED` 默认值 `false` → `true`（见 §2）。

## 1. 关键设计判断

### 1.1 范围是 Agent 级的扁平列表，不按知识库分组

一个 Agent 绑了两个知识库、却只圈了其中一个库的文档时，另一个库**不参与检索**。

选这个语义是因为另一个方案（按库分别限定：KB1 受限、KB2 不受限）需要给 002 的
`RetrieveFilter` 引入「按知识库分组的文档列表」，召回 SQL 要写成多组 OR，
复杂度显著上升；而收益只存在于「绑多个库且只想限制其中一部分」这个**当前不存在**的场景
（现有 Agent 全部绑 0 或 1 个知识库）。

**这个语义之所以不构成陷阱，靠的是界面**：文档勾选列表按知识库分组、
完整展示当前圈中的全部文档，用户看得见自己圈了什么、漏了什么。

### 1.2 文档被删除时不级联删除范围记录

`agent_documents` **故意不建外键、不做级联删除**。

留着失效的 `document_id`，按 002 的 FR-010 它匹配不到任何 chunk，于是该 Agent 检索不到东西
——这是正确且**可见**的结果。反过来如果级联删除，范围被删空之后，
系统再也无法区分「从未限定」和「限定的文档都被删了」，只能退回成「不限定」，
于是 Agent 会**悄悄用起范围外的资料**。那正是 002 全篇在防的事。

代价是可能留下孤儿行。这个代价远小于静默放宽。

### 1.3 「可见地移除」与「静默清理」的区别

取消勾选某个知识库时，前端会把它名下已选的文档一并移除——**列表当场变短，用户看得见**。
服务端则对「文档不属于已绑定知识库」一律**拒绝**（`ErrInvalidScopedDocument`），
不做静默清理。

这两者不矛盾，区别就在可见性：前端的移除用户看得到，服务端的静默清理用户看不到。
> 规格初稿写的是「保存时一并清理」，实现时改成了「服务端拒绝 + 前端可见移除」，
> 规格已同步更正。

### 1.4 上限常量导出而不是复制

`knowledge.maxFilterDocumentIDs` 导出为 `knowledge.MaxFilterDocumentIDs`，
agent 侧直接引用。两个本该相等的常量各写一个字面量 50 迟早漂移——
agent 侧宽了，用户能存下一个检索时必然被拒的配置；窄了则是无谓地更严。
有一条断言专门锁定它（`TestScopeLimitMatchesKnowledgeFilterLimit`）。

## 2. 唯一的行为变更：过滤开关默认改为开启

**为什么必须改**：`conversation` 在 `Retrieve` 报错时的既有处理是
「记 warn、`candidates = nil`、继续这一轮」——对真正的检索故障这是正确的降级。
但开关关着而 Agent 配了范围时，`Retrieve` 返回 `ErrMetadataFilterDisabled`，
于是这一轮**不带任何知识库资料**就去回答了。用户看到 Agent 凭空作答，
而不是「我被限定在这几份文档里，里面没有」。这是一条静默降级路径，与 002 的设计正面冲突。

**为什么改了是安全的**：002 的 SC-003 已经证明「空过滤器 + 开关开启」时检索输出与
该功能上线前逐字一致。对**没有配置范围**的 Agent（当时的全部 Agent），
这个默认值变更不改变任何行为。本期用两条独立证据复核了这一点，见 §3.2 与 §3.3。

**被否决的方案**：保持默认关闭、改成让这一轮对话直接失败。
为一个部署配置项让整轮对话失败，代价高于收益，且把配置问题变成了终端用户可见的故障。

## 3. 真实测试结果

### 3.1 自动化

```
$ go vet ./...            （无输出）
$ make check-deps         OK - no cross-layer or same-layer violations
$ go test ./... -race -count=1   （全部 ok，无 FAIL）
```

新增断言：
- `internal/agent/service_test.go`（**该模块此前一个测试都没有**）：
  空范围永远合法、范围内文档合法、跨知识库文档被拒、不存在的文档被拒、
  超上限**拒绝而非截断**、恰好等于上限合法、上限常量与 knowledge 侧一致。
- `internal/conversation/integration_test.go`：Agent 的 `DocumentIDs` 确实被下推进
  `RetrieveOptions`；未配置范围的 Agent 传的是**空**过滤器。
  > 这一环特别值得断言：范围没传下去时，检索照常返回结果、对话照常完成，
  > 只是范围没生效——没有任何报错能暴露它。

### 3.2 确定性检索门禁（SC-002）

```
$ make eval-retrieval-gate && python3 scripts/compare-retrieval-gate.py ...
IDENTICAL（14 个既有用例逐字段一致，metrics/pass 未变）
```

### 3.3 `make eval` 的确定性指标（SC-004）

测试集里的 Agent 不配置文档范围，因此这几项必须与基线完全一致：

| 指标 | 基线 | 本期 |
|---|---|---|
| `retrieval_hit` / `recall_at_1` / `recall_at_3` / `mrr` | 0.917 | **0.917** |
| `expected_document_cited` | 0.917 | **0.917** |
| `citation_requirement_met` | 0.923 | **0.923** |

全部一致——**默认值变更未改变检索行为**，这是 §2 那条安全性论证的机器验证。

> 裁判分**不作为验收依据**。理由见 `docs/eval-environment-setup.md` §7.1：
> 同一份代码两次跑分可差 4 分。

### 3.4 真实对话端到端（SC-001）

在真实运行的服务上建了三个只有文档范围不同的 Agent，问同一个问题「忘记密码怎么办？」：

| Agent | 召回来源 |
|---|---|
| 不限定 | `customer_service_faq.txt`, `hify_test_doc.txt` |
| 限定到 `customer_service_faq.txt` | 只有 `customer_service_faq.txt` |
| 限定到 `hify_test_doc.txt` | 只有 `hify_test_doc.txt` |

保存时的校验（真实 HTTP）：

| 输入 | 响应 |
|---|---|
| 文档属于另一个知识库 | 400 `agent.invalid_scoped_document` |
| 文档不存在 | 400 `agent.invalid_scoped_document` |
| 51 份文档 | 400 `agent.too_many_scoped_documents`（**不截断**） |

### 3.5 浏览器（US2）

Agent 编辑表单中「限定检索文档（可选）」按知识库分组、显示文件名、
已配置的文档正确回填勾选，提示文案完整（「不勾选 = 不限定」+ 未被选到文档的知识库
不参与检索 + 最多 50 份）。只在勾选了至少一个知识库后才出现。

## 4. 未验证 / 剩余风险

1. **孤儿行没有清理机制**。文档删除后 `agent_documents` 里的行会留下（这是有意的，见 §1.2），
   但没有任何界面提示「这个 Agent 的范围里有 N 份文档已被删除」。
   用户看到的是 Agent 突然什么都不知道，需要自己去排查。**这是本期最明显的可用性缺口**，
   规格已明确列入 Out of Scope，应另立任务。
2. **多知识库场景未经真实验证**。§1.1 那个「圈了 KB1 的文档导致 KB2 不参与检索」的语义，
   只有单元/集成测试覆盖，没有在真实多库 Agent 上点过——因为当前没有这样的 Agent。
3. **`agent_documents` 无外键约束**，因此 `document_id` 的合法性完全靠 service 层校验。
   绕过 API 直接写库可以插入任意值。与仓库既有的 `agent_knowledge_bases` 同一取舍。
4. **本期没有效果度量**。与 002/003 同理：过滤是布尔的范围缩小，不改变打分。
   §3.4 证明的是**机制成立**（范围确实生效），不是「限定范围让回答质量提升了多少」——
   后者需要一份带范围标注的语料，本仓库没有。

## 5. 改动清单

**新增**：`internal/db/migrations/000014_agent_documents.{up,down}.sql`、
`internal/db/queries/agent_documents.sql`、`internal/agent/service_test.go`、
`specs/004-agent-document-scope/*`、本报告。

**修改**：`internal/agent/{model,errors,repository,service,dto,handler}.go`、
`internal/knowledge/model.go`（仅导出一个已有常量）、
`internal/config/config.go`（默认值）、`internal/conversation/context.go`（一处调用点）、
`web/src/lib/{agents,knowledge}.ts`、`web/src/routes/agent-form-dialog.tsx`、
`internal/db/gen/*`（sqlc 重新生成，未手改）、`internal/conversation/integration_test.go`。

**未修改**：`internal/knowledge` 的检索逻辑一行未动——002 的能力原样复用，
这也是 §3.2/§3.3 两条回归断言能够成立的前提。
