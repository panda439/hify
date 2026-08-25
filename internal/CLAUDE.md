# internal/ 代码约定

> 根目录 `CLAUDE.md` 里的分层表和 `make check-deps` 是硬约束，这份是细则。
> 建表 / 改 migration / 写查询另见 `internal/db/CLAUDE.md`。

## 统一响应 / 分页 / 业务异常 / 全局异常处理

- **响应体不包壳（REST 风格）**：成功响应直接返回资源 JSON（`GET /agents/:id` → `{"id":"...",...}`），HTTP 状态码承载真实语义（200/404/409/400...），不搞 Java 那种 `{"code":0,"data":{...}}` 恒 200 包装。错误响应固定 `{"error":{"code":"...","message":"..."}}` 结构。
- **业务异常**：`internal/platform/apperr.AppError`，字段 `Kind`（固定 6 种：`not_found`/`conflict`/`invalid_input`/`unauthorized`/`forbidden`/`rate_limited`，决定 HTTP 状态码）+ `Code`（模块命名空间字符串，如 `"user.email_taken"`）+ `Message`。模块的 `errors.go` 用 `apperr.Conflict("user.email_taken", "...")` 这类构造函数声明具体哨兵错误，不需要中心化的错误码枚举文件（那会破坏"模块只改自己文件"的原则）。**`Message` 是直接展示给用户看的文案，必须是中文**（Hify 是中文产品）；这和 Go 编码规范里"错误信息小写开头"那条不冲突——那条针对的是 `fmt.Errorf` 包装链给日志/开发者看的内部错误字符串，两者面向的读者不同，别混着套用。
- **全局异常处理器**：`internal/platform/httperr.Write(c, err)` 按 `AppError.Kind` 转状态码；`httperr.Wrap(func(c *gin.Context) error) gin.HandlerFunc` 是路由注册用的适配器，handler.go 里的函数直接 `return err`，不用每处手写 `if err != nil { httperr.Write(c, err); return }`。真正的 panic 由 `internal/server/middleware` 的 recover 中间件单独兜底，两条路径互补。handler.go 规则第2步（bind+validate→调用 service→映射 dto 返回）里的"调用 service"如果出错，直接 `return err`，其余步骤照常。
- **分页**：`internal/platform.CursorPage[T]`（`items`+`next_cursor`，用于 messages/chunks/workflow_run_steps 等大表游标分页）和 `internal/platform.OffsetPage[T]`（`items`+`total`+`page`+`page_size`，用于 providers/users/agents/workflows 等管理后台小表）。`platform.ClampLimit` 强制 `LIMIT` 硬上限（`MaxPageLimit=200`）。

## 模块内部固定文件结构

每个业务模块目录固定这套文件（没内容的层也要留空文件占位，禁止合并层）：

```
internal/<module>/
├── handler.go     # HTTP层：Gin handler
├── service.go     # 业务逻辑层：Service 接口 + service 结构体实现（模块对外的唯一契约）
├── repository.go  # 数据访问层：包装 sqlc Queries，DB row <-> 领域struct 转换
├── model.go        # 领域类型：本模块的业务实体 struct（Service 接口方法签名里出现的类型）
├── dto.go          # HTTP 请求/响应 struct，带 validate tag，只给 handler.go 用
├── errors.go        # 模块自定义哨兵错误（var ErrXxx = errors.New(...)），供跨模块 errors.Is 判断
└── wire.go          # NewRepository/NewService/NewHandler 构造函数，供 main.go 组装
```

## 每层职责边界

**handler.go（HTTP 层）**
1. 只允许 import：本模块的 `service.go`/`dto.go`/`errors.go`、`internal/server/middleware`、`internal/platform` 及其子包（`httperr`/`apperr` 用于错误构造、顶层 `platform` 包用于 `OffsetPage`/`CursorPage`/`ClampLimit` 这类共享响应类型）。禁止 import 本模块或其他模块的 `repository.go`。
2. 每个 handler 函数签名固定为 `func (h *Handler) XxxYyy(c *gin.Context) error`（`httperr.HandlerFunc`），路由注册时用 `httperr.Wrap(h.XxxYyy)` 包一层。函数体固定三步：①`c.ShouldBindJSON`/`ShouldBindQuery` 到 dto.go 里的 request struct 并做 validate，失败直接 `return apperr.InvalidInput(...)` ②调用 `h.service.XxxYyy(ctx, ...)`，参数/返回值只能是 model.go 的领域类型，出错直接 `return err`（不在这里处理，见第4条）③把领域类型映射成 dto.go 的 response struct，`c.JSON` 返回，最后 `return nil`。
3. handler.go 里不能出现 SQL、sqlc 生成的类型、其他模块的任何类型。
4. 错误处理见上面「统一响应 / 分页 / 业务异常 / 全局异常处理」一节：handler 只管 `return err`，不手写 `c.JSON(500, ...)` 或直接调用 `httperr.Write`——`httperr.Wrap` 已经在路由层统一接管了。

**service.go（业务逻辑层，唯一允许跨模块调用的层）**
5. 文件顶部必须先定义导出接口：`type Service interface { XxxYyy(ctx context.Context, ...) (...) }`。接口方法签名里只能出现：本模块 model.go 的类型、基础类型、`context.Context`、`error`。禁止出现 dto.go 类型、sqlc 生成类型、`*gin.Context`。
6. 具体实现是未导出结构体 `type service struct { repo *Repository; <依赖的其他模块Service接口> }`，通过 `func NewService(repo *Repository, deps...) Service` 构造。依赖其他模块时，构造函数参数类型必须是对方模块导出的 `Service` 接口，不能是对方的具体 struct 指针。
7. 跨模块调用只能调用别的模块的 `Service` 接口方法。禁止 import 别的模块的 `repository.go`、`model.go` 内部字段去拼 SQL 或直接查表。例：conversation 模块创建会话前要校验 agent 存在，只能调 `agentSvc.Get(ctx, agentID)`，不能在 conversation 包里查 `agents` 表。
8. repository 只在本模块内部使用；如果发现一个业务操作需要另一个模块的数据，唯一合法途径是加/复用对方模块 Service 接口的方法，而不是"顺手"多查一张表。

**repository.go（数据访问层）**
9. 只允许被本模块的 `service.go` 调用，禁止被任何其他模块 import（包括同层模块）。
10. 方法返回值统一转换成 model.go 的领域类型再返回给 service 层（不把 sqlc 生成类型直接暴露给 service），转换逻辑写在 repository.go 里。
11. 不允许出现业务规则判断（权限检查、状态机校验、跨表业务一致性判断），只做纯粹的 CRUD/查询组装。业务判断一律放 service.go。

## 跨模块依赖方向：分层的由来与例外

层级表见根目录 `CLAUDE.md`。这里是为什么这么分，以及几条容易踩的细则。

`user` 单独下沉到第0层，不是笔误：登录/鉴权要读 `users` 表校验密码，如果 auth 和 user 同层，会直接违反"禁止同层模块互相依赖"（第12条）。`user` 承载的是被几乎所有模块引用的基础身份数据（`created_by` 之类的字段到处都要用到），把它当作和 `platform` 一样的基础设施对待，比人为拆出一个"auth 专用的用户读路径"更干净。`user` 模块本身仍然遵守第0层"不依赖任何业务模块"的约束，只是被业务模块依赖的位置更靠前。

**Phase 3 设计知识库时发现的修正**：`agent`/`knowledge` 最初被划在同一层（都只依赖 provider），但 `agent_knowledge_bases` 关联表要求 agent 创建/编辑时校验 `knowledge_base_id` 合法（调 `knowledge.Service`），这就是第12条说的"同层模块必须互相调用"——按规则把 `knowledge` 下沉一层，`agent` 相应上移，`conversation`/`workflow` 跟着顺延。这个方向不是任意的：`agent` 本质上是"组合 provider 的模型 + knowledge 的知识库（以后还有 mcp 的工具）"的配置层，天然应该排在这些被组合的资源模块之上，不是反过来。

12. 只能依赖更低层模块的 `Service` 接口，禁止反向依赖，禁止同层模块互相依赖。如果同层两个模块发现必须互相调用，说明分层分错了——要么合并成一个模块，要么把其中一个下沉一层，不允许绕过规则用事件/回调之类的手段"假装"没有循环依赖。
13. 模块间真正共享、和具体业务无关的类型（分页参数、通用错误码枚举等）放 `internal/platform`，不放在某个业务模块里让别人 import。
14. 跨模块调用一律是 Go 接口的同进程直接调用（传 `context.Context`），不做模块内部 HTTP/RPC 自调用；但方法签名只传值类型/领域 struct，不传指针指向对方模块的可变内部状态，为将来万一要拆分服务保留可能性（非近期目标，只是约束写法）。
14a. **横切基础设施包（日志类，非业务实体）不受第7条"只能依赖 Service 接口"约束**——`internal/platform/trace`（trace_spans 记录）是先例：不建 handler/dto/wire.go，直接持有 `*gen.Queries`/`*sql.DB`，业务模块（如 conversation）的 `NewService` 用具体类型（`*trace.Store`）注入，不是接口。判断标准：这个包有没有自己的 CRUD/业务规则、要不要在前端展示——有就走标准五层模块；纯粹"记录了什么、供排查/审计用"就归这一类，参照 `apperr`/`httperr`/`trace` 的先例放 `internal/platform` 下。

## 依赖注入方式（不用 DI 框架）

15. `main.go` 里手写一个 `buildApp(cfg Config) (*gin.Engine, error)`，按第0→4层顺序，逐模块 `NewRepository → NewService → NewHandler`，上一层的 Service 作为下一层的构造参数传入。不引入 wire/fx 等 DI 框架——单人项目手写组装更直观、编译期报错更直接。
16. 模块目录名 = package 名，不加 `xxx_service`/`xxx_repo` 后缀；接口固定叫 `Service`（调用方通过 `agent.Service` 这种带包名的写法引用，不会和别的模块的 `Service` 撞名）。

## Go 编码规范（命名 / 异常处理 / 日志 / 并发）

**命名**
1. 包名全小写、不加下划线/驼峰、不用复数，且是有具体含义的名词，禁止 `util`/`common`/`misc` 这类无意义包名。
2. 单方法接口以"方法名+er"命名（`Reader`/`Formatter`），不加 `I` 前缀或 `Interface` 后缀。
3. 标识符名字长度和作用域大小成正比：循环内临时变量用 `i`/`j` 短名，包级导出标识符用完整清晰的名字。
4. 错误变量固定叫 `err`；哨兵错误用 `Err` 前缀且放包级；自定义错误类型以 `Error` 结尾。
5. Getter 不加 `Get` 前缀（`user.Name()` 不是 `user.GetName()`），Setter 才用 `Set` 前缀。
6. 可见性只靠首字母大小写控制，不用命名后缀模拟"private"，该 unexported 就直接小写。

**异常处理**
7. `error` 必须是函数最后一个返回值，调用后立即检查；忽略 error 必须有注释说明为什么安全，禁止裸的 `_ = err`。
8. 用 `fmt.Errorf("...: %w", err)` 包装保留错误链，不用 `%v`。
9. 判断错误类型统一用 `errors.Is`/`errors.As`，禁止字符串比较或裸类型断言。
10. `panic` 只用于真正不可恢复的程序错误，不能代替业务错误返回；每个独立 goroutine 入口必须有 `recover`，一个 goroutine panic 不能带崩整个进程。
11. 错误信息小写开头、不加句末标点。
12. 需要调用方区分处理路径时定义哨兵错误或自定义错误类型，禁止让调用方 parse 错误字符串分支判断。

**日志**
13. 统一用 `log/slog` 结构化日志，禁止业务代码用 `fmt.Println`/`log.Println` 输出到生产日志。
14. 日志字段用结构化 key-value，禁止把变量拼进消息字符串本身。
15. 级别要用对：Debug=调试细节，Info=正常业务事件，Warn=预期内但需关注，Error=需要人介入的失败；不允许什么都打 Error。
16. 密码/token/API Key/身份证号等敏感信息永远不进日志（含 Debug 级别），只记录标识符不记录内容本身。

**并发**
17. 每个启动的 goroutine 必须明确谁负责等待它结束（`sync.WaitGroup`/`errgroup.Group`），禁止"发射后不管"的裸 `go func(){}()`。
18. 共享可变状态必须有明确同步原语保护（`Mutex`/`RWMutex`/channel），不能假设"实践中不太会冲突"；能用 channel 通信替代共享内存加锁的地方优先用 channel。
19. `context.Context` 必须是函数第一个参数、固定命名 `ctx`，不放进 struct 字段长期持有；所有可能阻塞的操作必须接收并尊重 `ctx` 的取消/超时。
20. 把 `-race` 跑测试当成标准流程的一部分，不要等生产环境出现偶发诡异 bug 才想起查数据竞争。

## 可观测能力（Trace/Span + Eval Harness）

- 每次 `conversation.Service.StreamMessage` 调用是一个 trace，落 `trace_spans` 表：根 span（`kind=turn`，直接复用 trace_id 作为自己的 id）+ 子 span（`retrieval`/`llm_call`/`tool_call`，`parent_span_id` 指向根 span）。记录逻辑在 `internal/platform/trace`（横切基础设施包，见上面 14a 条），业务模块拿到的是具体类型 `*trace.Store`，不是 Service 接口。
- `trace_spans.attrs` 字段名照抄 OpenTelemetry GenAI 语义约定（`gen_ai.request.model`/`gen_ai.usage.input_tokens`/`gen_ai.usage.output_tokens`/`gen_ai.response.finish_reasons`/`gen_ai.tool.name`，常量定义在 `internal/platform/trace/attrs.go`），新增 span 类型或属性时沿用这套命名，不要另起一套 key 风格。
- `provider.Message`/`provider.ChatChunk` 的 `Usage` 字段是 best-effort——不是所有 OpenAI 兼容供应商都返回 token 用量，零值就是"没有"，不是错误，不用额外做校验或告警。
- eval harness（`internal/eval` + `cmd/evalrunner`，跑 `make eval`）读 `eval/testset.yaml` 固定测试集，用 trace 数据核实"评分标准里要求的工具调用是否真的发生"，裁判打分结果存本地 JSON 文件（`eval/runs/*.json`、`eval/baseline.json`），不进数据库、不接 API——这是开发者自用的回归工具，不是给最终用户看的功能。
