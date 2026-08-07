# Hify RAG 优化阶段一：Eval 基线报告（修订版）

上一版报告有两个问题被审核打回：多轮 case 的 Judge prompt 用的是空的 `tc.Prompt`；10 条 case
从未在同一次运行里跑过。这一版只处理审核指出的这些问题，仍然**没有**碰 Chunk/检索/排序算法。

## 1. 修复内容

### 1.1 多轮 Eval 的 Judge 输入（`internal/eval/judge.go`）

`buildJudgePrompt` 之前直接 `fmt.Fprintf(&sb, "用户问题：%s\n\n", tc.Prompt)`——多轮 case 用 `turns`
字段配置，`Prompt` 是空字符串，Judge 收到的是一句空的"用户问题："，看不到真实问题，更看不到指代
需要的上文。

改法：新增 `formatJudgeTurns(tc TestCase) string`，复用 `runner.go` 里已有的 `caseTurns(tc)`（单轮
退化成 `[]string{tc.Prompt}`，多轮就是 `tc.Turns`）：

- 单轮 case（`len(turns) == 1`）：渲染方式和以前完全一样，`用户问题：<Prompt>`，不引入任何变化。
- 多轮 case：把**全部** turn 按顺序列出来（不是只给最后一句），并且明确标出最后一轮是"下面的‘助手
  最终回复’对应的、真正参与打分的那一轮"。示例（对应 `multi_turn_coreference_chunk_size` 这条 case）：

  ```
  多轮对话（同一个会话里按顺序发生，后面的问题可能用代词或省略指代前面的内容，请结合上下文理解）：
  第 1 轮：知识库创建以后还能修改用的向量模型吗？
  第 2 轮（最后一轮，下面的"助手最终回复"是对这一轮的回复，只评这一轮）：那分块大小呢，也不能改吗？
  ```

  这样 Judge 看得到"分块大小"这个指代依赖的上文（"知识库创建后参数不可修改"这个语境），不会把
  "那分块大小呢"当成一句没头没尾的问题去评分。

打分范围**没有变**：`runCase` 依然只把最后一轮的 `Reply`/`Retrievals`/`Citations` 传给
`computeRAGMetrics` 和 `Judge`（这部分是阶段一第一版就做对的，这次审核也没有要求改）——变的只是
Judge 看到的"用户问题"这部分上下文，不是打分对象。

### 1.2 新增测试

- `internal/eval/judge_test.go`
  - `TestBuildJudgePrompt_SingleTurnRendersPromptAsBefore`：单轮 case 的渲染必须和改动前完全一致
    （回归保护，防止这次改动误伤单轮场景）。
  - `TestBuildJudgePrompt_MultiTurnIncludesFirstAndLastTurn`：直接检查 `buildJudgePrompt` 返回的
    字符串同时包含第一轮和最后一轮的原文，并且不会把 `Prompt` 字段的空值渲染成一句空的"用户问题："。
  - `TestBuildJudgePrompt_MultiTurnLabelsFinalTurnAsScored`：检查 prompt 里有"最后一轮"这个标注，
    不是把几轮问题堆在一起却不说明哪一轮是打分对象。
- `internal/eval/runner_test.go`
  - `TestRunCase_MultiTurnJudgeRequestIncludesFirstAndLastTurn`：这条是审核要求的"直接检查发送给
    Judge 的请求内容"——不只测 `buildJudgePrompt` 这个中间字符串，而是从 `runCase` 完整跑一遍，用
    一个会记录 `req.Messages[0].Content` 的 fake `provider.Client` 截获真正发给 Judge 模型的请求体，
    断言里面同时包含第一轮问题"知识库创建以后还能修改用的向量模型吗？"和最后一轮问题"那分块大小呢，
    也不能改吗？"。

（这几个测试和上一版新增的 `RecallAt1`/`RecallAt3`/多轮 `runCase` 测试一样，**没有被真正编译/跑过**，
见第 3 节——只做了 `gofmt -l` 语法检查，通过。）

## 2. 重新运行 10 条 Eval case——结论：做不到，阶段一在这一项上仍未完成

审核要求"修复后必须在当前单路向量检索版本上运行全部 10 条，生成同一时间、同一代码版本下的完整优化前
baseline"。**这件事我做不到，如实说明如下，不能宣布 baseline 已建立。**

运行 `make eval` 需要同时具备：

1. 真实运行的 MySQL + Postgres(pgvector) + Redis（`docker-compose.yml` 定义的那一套）；
2. 一个已经在这套服务里配置好、挂载了知识库的 Agent，知识库里的文档已经上传并处理成"已就绪"状态；
3. 一个真实可用、能实际发请求的模型供应商（embedding + 对话模型都要），用来跑 `Retrieve`/`StreamMessage`；
4. 一个已注册的、可以当裁判的模型 id（`--judge-model-id`）。

这次会话能用的两个执行环境都不满足：

- 云端容器：`docker ps`/`docker info` 显示装了 docker 客户端，但 `/var/run/docker.sock` 不存在——没有
  docker daemon，起不了 `docker compose up`；即便能起，拉镜像、连任何真实模型供应商也需要出网，而这
  个容器的出网白名单不包含这些域（和第 3 节的 Go 模块代理阻塞是同一类限制）。也没有任何模型供应商的
  API key。
- 用户本机的沙箱（`device_bash`）：这是一个隔离的本地 Linux VM，不是用户 Mac 的真实 shell——即使用户
  Mac 上确实跑着 `docker-compose` 起的 MySQL/Postgres/Redis（监听在用户 Mac 的 localhost 上），这个
  VM 也够不到那些端口（这是已知限制：这类沙箱连不到用户机器的 localhost），而且这个 VM 里同样没有出
  网权限，没装 Go。

所以**没有办法在本次会话里真实跑一次覆盖全部 10 条 case 的评测**，`eval/baseline.json` 目前仍然只是
原来那 5 条 case 的结果（`ran_at: 2026-07-26T07:06:38Z`，本阶段没有改动这个文件），新增的 5 条
（`exact_error_message_billing`/`synonym_model_unavailable`/`no_answer_in_knowledge_base`/
`similar_docs_temp_password_cannot_skip`/`multi_turn_coreference_chunk_size`）**依然没有任何真实运行
结果**。第 4 节的 baseline 表如实标注了这一点，不是把旧 5 条和编造的新 5 条拼在一起。

**结论：阶段一"建立 10 条 case 完整优化前 baseline"这一验收项没有完成。** 要完成它，需要在一台真正能
连到 MySQL/Postgres/Redis 和真实模型供应商的机器上（大概率是用户本机，或者一个有权限的 CI/开发环境）
执行：

```bash
make db-up
# 确认 Agent 019f9d3a-a045-719a-ae79-270a0bbbd739 和它挂载的知识库（019f5f88-ba7e-...）里的文档处理状态是"已就绪"
make eval JUDGE_MODEL_ID=<已注册的裁判模型 UUID> EVAL_USER_ID=<已存在的用户 UUID>
```

跑完之后把 `eval/runs/<timestamp>.json` 确认为一次真实的"当前单路向量检索"结果后，手动复制成新的
`eval/baseline.json`，覆盖阶段一遗留的这个缺口。

## 3. 关于"审核方已经实际执行并通过"——复现结果和一个需要澄清的问题

审核方说已经用下面三条命令在"当前机器"跑通过：

```bash
env GOCACHE=/tmp/hify-go-build-cache go test ./...
env GOCACHE=/tmp/hify-go-build-cache go test -race ./...
env GOCACHE=/tmp/hify-go-build-cache make check-deps
```

我在本次会话能访问的云端容器里，用完全相同的命令重新跑了一遍，结果和审核方说的不一致，如实汇报：

**`go test ./...`（含 `-race` 那条）**：仍然失败，报错和之前完全一样——

```
go: downloading go1.25.0 (linux/amd64)
go: download go1.25.0: golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64: reading https://proxy.golang.org/golang.org/toolchain/@v/v0.0.1-go1.25.0.linux-amd64.zip: 403 Forbidden
	server response: Host not in allowlist: proxy.golang.org. Add this host to your network egress settings to allow access.
```

退出码是 1（不是我之前一版报告里误判的那样——之前把输出接了 `| head` 导致 `$?` 读到的是 `head` 的退出码，这次改成不接管道直接看真实退出码，确认是失败）。`GOCACHE` 控制的是编译产物缓存，和这里卡住的模块下载（`GOMODCACHE`/网络）是两件事，改 `GOCACHE` 的路径不会绕开这个网络限制——这也是我重新验证之后能确认的。

**`make check-deps`**：命令本身退出码是 0，但我核实过之后发现这是一个**假阳性**，不是真的通过：

`scripts/check-deps.sh` 里核心逻辑是 `while ... done < <(go list -json ./internal/... | jq ...)`。当
`go list -json` 因为同样的 403 网络问题失败时，它不往 stdout 输出任何内容（只往 stderr 打错误），于是
这个 `while` 循环因为读到空输入直接正常结束，`fail` 变量维持初始值 `0`，脚本打印"check-deps: OK - no
cross-layer or same-layer violations"——但它实际上什么依赖关系都没检查过。我单独验证了这一点：

```bash
$ go list -json ./internal/... 2>/tmp/err.log | wc -l
0
$ cat /tmp/err.log
go: downloading go1.25.0 (linux/amd64)
go: download go1.25.0: ... 403 Forbidden ...
```

`go list` 真实是失败的、零行输出，`check-deps.sh` 没有对这种情况做防御（没检查 `go list` 自身的退出
码），所以"退出码 0"并不代表依赖分层规则被验证过，是脚本的一个健壮性缺口造成的误报。这个问题不在这次
审核要求修的范围内（这次只让修 Judge 输入和补 baseline），我没有动 `scripts/check-deps.sh`，只是如实
指出，避免"退出码 0"被当成"依赖分层检查通过了"。

**结论**：我没能在自己能访问的环境里复现审核方所说的"已经实际执行并通过"，尤其是 `go test`/`go test
-race` 这两条，我这边是确定性的失败（403，和上一版报告描述的原因完全相同）；`make check-deps` 表面
退出码是 0，但已经证明是空输入导致的假阳性，不能当作真正验证过。如果审核方是在另一台有 Go 模块代理
出网权限、且模块缓存已经预热过的机器上跑的（比如用户本机），那和这次会话能用的两个环境（云端容器、
`device_bash` 本地沙箱）都不是同一个环境——这两个环境目前都没有下载 Go 模块的出网权限，也没有预置的
模块缓存或 vendor 目录。我不会把"审核方说通过了"直接改写成"已验证通过"写进报告，因为那不是我自己验证
出来的结果，而且我这边用同样的命令重复得到的是相反的结果；但也把审核方的说法如实记录在这里，供参考。

如果需要真正在这次会话里跑通这三条命令，需要这个云端容器获得 Go 模块代理（`proxy.golang.org` 或一个
可用镜像）的出网权限，或者把已经下载好的 `$GOMODCACHE` 内容传进来。

## 4. 10 条 case 的完整 baseline

**如实说明：这不是"10 条 case 的完整 baseline"，因为做不到（见第 2 节）。** 下面分两部分列：

### 4.1 已有真实结果的 5 条（来自 `eval/baseline.json`，`ran_at: 2026-07-26T07:06:38Z`，未改动）

| Case | Judge 分数 (1-5) | RetrievalHit | MRR | Recall@1 | Recall@3 | CitationRequirementMet | ExpectedDocumentCited |
|---|---|---|---|---|---|---|---|
| forgot_password | 3 | true | 1.0 | true | true | true | true |
| login_rate_limit | 5 | true | 1.0 | true | true | true | true |
| supported_file_formats | 5 | true | 1.0 | true | true | true | true |
| immutable_knowledge_config | 5 | true | 1.0 | true | true | true | true |
| conversation_privacy | 5 | true | 1.0 | true | true | true | true |

`Recall@1`/`Recall@3` 是这次改造之后才有的字段，`baseline.json` 是旧格式跑出来的、没存这两个字段——
上面表里这两列是我用 `internal/eval/metrics.go` 里 `recallAtK` 的逻辑，对 `baseline.json` 里已经存
的 `retrievals`/`expected_document_ids` 原始数据手工重新计算的（5 条 case 的 `expected_document_ids`
命中都发生在 rank=1，所以 `Recall@1`/`Recall@3` 都是 true），**不是重新跑了一次 eval**，只是用同一份
已有的真实检索结果重新算了一遍确定性指标，没有引入任何新的、未经真实运行验证的数字。

### 4.2 从未真实运行过的 5 条

| Case | Judge 分数 | RetrievalHit | MRR | Recall@1 | Recall@3 | CitationRequirementMet | ExpectedDocumentCited |
|---|---|---|---|---|---|---|---|
| exact_error_message_billing | 无 | 无 | 无 | 无 | 无 | 无 | 无 |
| synonym_model_unavailable | 无 | 无 | 无 | 无 | 无 | 无 | 无 |
| no_answer_in_knowledge_base | 无 | 无 | 无 | 无 | 无 | 无 | 无 |
| similar_docs_temp_password_cannot_skip | 无 | 无 | 无 | 无 | 无 | 无 | 无 |
| multi_turn_coreference_chunk_size | 无 | 无 | 无 | 无 | 无 | 无 | 无 |

这 5 行全部是"无"，不是 0 或 false——0/false 会被误读成"跑了但结果是负面的"，这五条从来没有被
`evalrunner` 真实执行过，没有任何数字可以填。

## 5. 仍然存在的真实阻塞

1. **10 条 case 的完整同版本 baseline 没有建立**——需要真实 MySQL/Postgres/Redis + 真实模型供应商 +
   已处理就绪的知识库，这次会话的两个执行环境都不具备，见第 2 节。这是阶段一验收里唯一"没做完"的部分。
2. **`go build`/`go test`/`go test -race` 在本次会话能访问的环境里无法通过**——不是编译产物缓存
   （`GOCACHE`）的问题，是 Go 模块下载需要访问 `proxy.golang.org`，这次会话的云端容器和本机沙箱都没
   有这个出网权限，也没有预置模块缓存/vendor 目录。这次改动（`judge.go`/`judge_test.go`/
   `runner.go`/`runner_test.go`/`metrics.go`/`metrics_test.go`/`model.go`/`report.go`）**只做了
   `gofmt -l` 的语法和格式检查**（全部通过），类型正确性和单测是否真的跑绿都还没有被真正验证过。
3. **`make check-deps` 的"通过"是假阳性**，见第 3 节——`go list -json` 因为同样的网络问题静默失败、
   零输出，脚本没有检测这种情况，误报"OK"。这次改动没有涉及 `internal/eval` 以外任何模块的 import
   关系，理论上不会触发真正的分层违规，但这句话本身也没有被真正的 `check-deps` 运行验证过，只是基于
   "我没有改动任何跨模块 import"这个事实的推断。
4. **`_to_delete/` 目录清理不了**：`_to_delete/git_index.lock`（git 在这个挂载目录上跑命令时留下的
   残留锁文件）和 `_to_delete/hify_stage.tar.gz`（我第一轮往这个云端容器搬代码时用的中转压缩包，内容
   就是这个仓库当时的一份快照，不包含任何这次改动之外的东西）——两个我都确认不再需要，但通过设备桥
   在这个挂载目录上执行 `rm` 一律返回 `Operation not permitted`（挂载文件系统本身的限制，我这边没有
   任何权限层面能绕过的办法）。这两个文件仍然留在 `/Users/lishurong/go/src/hify/_to_delete/`，需要你
   自己在 Finder/Terminal 里删除；它们都不在 `git status` 的已跟踪改动里（`git status --short` 里是
   `??` 未跟踪状态），不会被误提交，但建议手动清理掉避免误加进以后的 commit。
