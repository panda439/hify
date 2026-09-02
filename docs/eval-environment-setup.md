# Eval 环境搭建（真实向量）

**最后更新**：2026-09-02

`make eval` 与 `eval/testset.yaml` 依赖一组**只存在于本地数据库里的 id**
（provider、模型、知识库、文档、Agent）。这些 id 不可能提交进仓库，
所以数据库一旦重建，`testset.yaml` 里的 id 全部失效。这份文档记录如何把它们重新建出来。

> **`eval/baseline.json` 是 gitignored 的本地产物**，每个 checkout / 每台机器各有一份。
> 它不随仓库分发，也不应该被提交——基线只有在**同一套数据、同一套 id** 下才有可比性。

---

## 0. 为什么必须用真实 embedding

2026-09-02 之前，本地唯一可用的 embedding 模型是 `mock-embedding-model`，
指向 `http://127.0.0.1:8090/v1` 的 mock server，库里存量向量是 **32 维**——
不是任何真实 embedding 模型的输出维度。而那个 mock server 早已不再运行。

后果是：**此前每一次 `make eval`，向量召回一路要么用的是假向量、要么直接降级跳过**
（日志里是 `query embedding failed ... 无法连接到供应商` 随后熔断），
真正在支撑检索的只有 pg_trgm 关键词一路。在那种状态下测出来的任何分数都不能说明检索质量。

这也是 Phase 1-11 全部报告里反复出现的那句「本期结论是机制证明，不是真实效果幅度」的根源。

---

## 1. 准备 Ollama（一次性）

选 Ollama 而不是在线 API 的理由：免费、离线、不需要 API key，
且与 `~/AI-self-study/research-agent` 005 期已经验证过的方案一致（同一个模型、同一个维度）。

```bash
brew install ollama          # 已装可跳过
ollama serve                 # 后台常驻；macOS 上装完通常已自启
ollama pull bge-m3:567m      # 约 1.1GB，1024 维，bert 架构，capabilities=["embedding"]
```

验证 OpenAI 兼容端点可用（hify 的 provider 层走的就是这个）：

```bash
curl -sS http://127.0.0.1:11434/v1/embeddings -H "Content-Type: application/json" \
  -d '{"model":"bge-m3:567m","input":["test"]}' | head -c 200
```

期望看到 `"data":[{"embedding":[...]` 且向量长度为 **1024**。

---

## 2. 在 Hify 里注册 provider 与模型

`$TOKEN` 是 admin 登录拿到的 access_token。

```bash
# provider：auth_type 必须是 none —— Ollama 不需要认证，
# 传 api_key 反而会因为缺 auth_type 被 binding 拦下（400 provider.invalid_request）
curl -sS -X POST http://127.0.0.1:8080/api/v1/providers \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"ollama-local","base_url":"http://127.0.0.1:11434/v1","auth_type":"none"}'

# 模型：capability=embedding，维度 1024
curl -sS -X POST http://127.0.0.1:8080/api/v1/providers/<PROVIDER_ID>/models \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"model_name":"bge-m3:567m","capability":"embedding","embedding_dimension":1024}'
```

**不需要任何代码改动**——Ollama 暴露的是 OpenAI 兼容接口，hify 的 provider 层本就是 OpenAI 兼容的。

---

## 3. 建知识库并上传语料

知识库的 embedding 模型**创建后不可修改**（见 `internal/knowledge/model.go` 的
`KnowledgeBase` 注释），所以换 embedding 只能新建库，不能改旧库。

```bash
curl -sS -X POST http://127.0.0.1:8080/api/v1/knowledge-bases \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"产品文档知识库（真实向量）","embedding_model_id":"<MODEL_ID>",
       "chunk_size":500,"chunk_overlap":50}'
```

语料是两份 txt，在主 checkout 的 `data/knowledge/<旧KB_ID>/` 下：

- `hify_test_doc.txt`（Hify 密码重置流程说明）
- `customer_service_faq.txt`（Hify 客服常见问题手册）

```bash
curl -sS -X POST http://127.0.0.1:8080/api/v1/knowledge-bases/<KB_ID>/documents \
  -H "Authorization: Bearer $TOKEN" -F "file=@<路径>/customer_service_faq.txt"
```

上传后轮询文档状态直到 `ready`，记下每份文档的 `id`。

> 同目录下还有一份 `hify_test.pdf`，它在**任何** embedding 配置下都处理失败
> （`ErrEmptyContent`，多半是没有文本层的扫描件）。它不被 `testset.yaml` 引用，
> 处理失败不影响 eval，不用管它。

---

## 4. 建 Eval Agent

```bash
curl -sS -X POST http://127.0.0.1:8080/api/v1/agents \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"RAG Eval - 真实向量","model_id":"<CHAT_MODEL_ID>","temperature":0,
       "system_prompt":"请只根据检索到的知识库资料准确回答；资料不足时明确说明不知道；引用事实时使用提供的来源标记",
       "knowledge_base_ids":["<KB_ID>"]}'
```

`temperature: 0` 不可省——评测要的是可复现，不是多样性。

---

## 5. 同步 id 到 testset

把 `eval/testset.yaml` 里的 `agent_id` 与 `expected_document_ids` 全部替换成上面拿到的新 id
（当前共 14 处 agent_id、16 处 document_id）。

## 6. 跑评测并建立基线

```bash
go run ./cmd/evalrunner --testset eval/testset.yaml \
  --judge-model-id <JUDGE_MODEL_ID> --user-id <USER_ID>

# 确认结果合理之后，把它固化成本地基线
cp eval/runs/<最新一次>.json eval/baseline.json
```

之后 `make eval` 会自动和 `eval/baseline.json` 做回归对比。

---

## 7. 这套东西能证明什么、不能证明什么

### 7.1 实测：同一份代码跑两次，哪些指标稳、哪些不稳

2026-09-02 在完全相同的代码、数据和 id 下连跑两次，结果如下：

| 指标 | 第 1 次 | 第 2 次 | 稳定？ |
|---|---|---|---|
| `retrieval_hit` | 0.917 | 0.917 | ✅ |
| `recall_at_1` / `recall_at_3` | 0.917 | 0.917 | ✅ |
| `mrr` | 0.917 | 0.917 | ✅ |
| `expected_document_cited` | 0.917 | 0.917 | ✅ |
| `citation_requirement_met` | 0.923 | 0.923 | ✅ |
| **裁判平均分** | **4.571** | **4.643** | ❌ |

三条用例的裁判分在两次之间发生变化，其中一条从 **5 分掉到 1 分**
（`hard_multi_turn_ellipsis_locked_params`）、另两条分别从 3→5 和 2→5。

**结论比"`make eval` 不可信"更精确**：

- **确定性检索指标**（`retrieval_hit`/`recall_at_*`/`mrr`/`expected_document_cited`）
  **是稳定的**——它们只取决于检索链路本身，不经过裁判模型。
  这几项拿来做回归对比是有意义的。
- **裁判分是不稳定的**。它同时受被测模型和裁判模型两层采样影响，
  同一份代码两次跑出的差异可以大到 4 分。**任何基于裁判分的"提升了 X"都不成立**，
  除非做多次重复取分布，而本项目没有。

### 7.2 边界

**能**：在这个 14 条用例的固定测试集上，当前系统的检索命中率与引用正确性（见 7.1 的稳定项）。

**不能**：
- **不能证明"某次改动带来了多少提升"**，除非改动前后用的是**同一套** id、同一份基线。
  换过 embedding、重建过知识库之后，新旧基线之间没有可比性——
  2026-09-02 这次换库就使 8 月 24 日的旧基线彻底作废。
- **不能支撑统计结论**。14 条用例、两份 txt 语料，样本量太小。
- **不能用来证明"行为未变"**。理由见 7.1：裁判分本身就不稳定。
  证明行为未变的是 `make eval-retrieval-gate`（确定性门禁，无 LLM、无裁判），
  这一条是宪法「技术栈与工程约束」里的硬规定。

> **给未来的自己**：主 checkout 里那份 `eval/baseline.json`（2026-08-24）是在
> mock 32 维向量下跑出来的，已经没有任何参考价值，别拿它跟现在的结果比。
