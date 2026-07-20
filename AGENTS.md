# Hify 项目开发约定

Hify 是一个简化版 Dify（Go + React 单体应用）。这份文档是项目级强约定，不是风格建议——写代码、建表时逐字遵守。完整产品计划见 `/Users/lishurong/.Codex/plans/floofy-churning-rainbow.md`。

技术栈：Go + Gin，MySQL 8.x（业务数据）+ Redis（缓存/限流/asynq 任务队列），React + Vite + TS + Tailwind + shadcn/ui，最终打包成单个 Go 二进制（go:embed 内嵌前端静态资源）。

## 开发流程

新功能/新模块/新 Phase 按 `/feature-workflow` 这个 skill 里的完整流程走（规划→后端→前端→验证→验收→git 提交，含常见坑），这里只放一句话核心顺序方便速查：**migration → sqlc → 模块内 `model→errors→repository→service→dto→handler→wire.go` 固定顺序 → 接入 buildApp → 前端 hook→组件 → `/smoke-test` 冒烟验证 → git commit**。项目没有自动化测试，`/smoke-test` 和真实 HTTP 请求验证是唯一的安全网，不能省。

## 统一响应 / 分页 / 业务异常 / 全局异常处理

- **响应体不包壳（REST 风格）**：成功响应直接返回资源 JSON（`GET /agents/:id` → `{"id":"...",...}`），HTTP 状态码承载真实语义（200/404/409/400...），不搞 Java 那种 `{"code":0,"data":{...}}` 恒 200 包装。错误响应固定 `{"error":{"code":"...","message":"..."}}` 结构。
- **业务异常**：`internal/platform/apperr.AppError`，字段 `Kind`（固定 6 种：`not_found`/`conflict`/`invalid_input`/`unauthorized`/`forbidden`/`rate_limited`，决定 HTTP 状态码）+ `Code`（模块命名空间字符串，如 `"user.email_taken"`）+ `Message`。模块的 `errors.go` 用 `apperr.Conflict("user.email_taken", "...")` 这类构造函数声明具体哨兵错误，不需要中心化的错误码枚举文件（那会破坏"模块只改自己文件"的原则）。**`Message` 是直接展示给用户看的文案，必须是中文**（Hify 是中文产品）；这和 Go 编码规范里"错误信息小写开头"那条不冲突——那条针对的是 `fmt.Errorf` 包装链给日志/开发者看的内部错误字符串，两者面向的读者不同，别混着套用。
- **全局异常处理器**：`internal/platform/httperr.Write(c, err)` 按 `AppError.Kind` 转状态码；`httperr.Wrap(func(c *gin.Context) error) gin.HandlerFunc` 是路由注册用的适配器，handler.go 里的函数直接 `return err`，不用每处手写 `if err != nil { httperr.Write(c, err); return }`。真正的 panic 由 `internal/server/middleware` 的 recover 中间件单独兜底，两条路径互补。handler.go 规则第2步（bind+validate→调用 service→映射 dto 返回）里的"调用 service"如果出错，直接 `return err`，其余步骤照常。
- **分页**：`internal/platform.CursorPage[T]`（`items`+`next_cursor`，用于 messages/chunks/workflow_run_steps 等大表游标分页）和 `internal/platform.OffsetPage[T]`（`items`+`total`+`page`+`page_size`，用于 providers/users/agents/workflows 等管理后台小表）。`platform.ClampLimit` 强制 `LIMIT` 硬上限（`MaxPageLimit=200`）。

## 代码组织规范（分层结构 / 职责边界 / 跨模块调用规则）

Hify 是模块化单体：`internal/` 下每个业务目录（auth, user, provider, agent, conversation, knowledge, mcp, workflow）是一个「模块」。

### 模块内部固定文件结构

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

### 每层职责边界

**handler.go（HTTP 层）**
1. 只允许 import：本模块的 `service.go`/`dto.go`/`errors.go`、`internal/server/middleware`、`internal/platform` 及其子包（`httperr`/`apperr` 用于错误构造、顶层 `platform` 包用于 `OffsetPage`/`CursorPage`/`ClampLimit` 这类共享响应类型）。禁止 import 本模块或其他模块的 `repository.go`。
2. 每个 handler 函数签名固定为 `func (h *Handler) XxxYyy(c *gin.Context) error`（`httperr.HandlerFunc`），路由注册时用 `httperr.Wrap(h.XxxYyy)` 包一层。函数体固定三步：①`c.ShouldBindJSON`/`ShouldBindQuery` 到 dto.go 里的 request struct 并做 validate，失败直接 `return apperr.InvalidInput(...)` ②调用 `h.service.XxxYyy(ctx, ...)`，参数/返回值只能是 model.go 的领域类型，出错直接 `return err`（不在这里处理，见第4条）③把领域类型映射成 dto.go 的 response struct，`c.JSON` 返回，最后 `return nil`。
3. handler.go 里不能出现 SQL、sqlc 生成的类型、其他模块的任何类型。
4. 错误处理见「统一响应 / 分页 / 业务异常 / 全局异常处理」一节：handler 只管 `return err`，不手写 `c.JSON(500, ...)` 或直接调用 `httperr.Write`——`httperr.Wrap` 已经在路由层统一接管了。

**service.go（业务逻辑层，唯一允许跨模块调用的层）**
5. 文件顶部必须先定义导出接口：`type Service interface { XxxYyy(ctx context.Context, ...) (...) }`。接口方法签名里只能出现：本模块 model.go 的类型、基础类型、`context.Context`、`error`。禁止出现 dto.go 类型、sqlc 生成类型、`*gin.Context`。
6. 具体实现是未导出结构体 `type service struct { repo *Repository; <依赖的其他模块Service接口> }`，通过 `func NewService(repo *Repository, deps...) Service` 构造。依赖其他模块时，构造函数参数类型必须是对方模块导出的 `Service` 接口，不能是对方的具体 struct 指针。
7. 跨模块调用只能调用别的模块的 `Service` 接口方法。禁止 import 别的模块的 `repository.go`、`model.go` 内部字段去拼 SQL 或直接查表。例：conversation 模块创建会话前要校验 agent 存在，只能调 `agentSvc.Get(ctx, agentID)`，不能在 conversation 包里查 `agents` 表。
8. repository 只在本模块内部使用；如果发现一个业务操作需要另一个模块的数据，唯一合法途径是加/复用对方模块 Service 接口的方法，而不是"顺手"多查一张表。

**repository.go（数据访问层）**
9. 只允许被本模块的 `service.go` 调用，禁止被任何其他模块 import（包括同层模块）。
10. 方法返回值统一转换成 model.go 的领域类型再返回给 service 层（不把 sqlc 生成类型直接暴露给 service），转换逻辑写在 repository.go 里。
11. 不允许出现业务规则判断（权限检查、状态机校验、跨表业务一致性判断），只做纯粹的 CRUD/查询组装。业务判断一律放 service.go。

### 跨模块依赖方向（禁止环依赖，分 6 层，只能自上而下依赖）

```
第0层  internal/platform, internal/config, internal/db/gen, internal/user   — 不依赖任何业务模块
第1层  auth, provider, mcp                                                   — 只能依赖第0层
第2层  knowledge                                                             — 只能依赖第0、1层
第3层  agent                                                                 — 只能依赖第0~2层
第4层  conversation                                                          — 只能依赖第0~3层
第5层  workflow                                                              — 可依赖第0~4层所有模块
```

`user` 单独下沉到第0层，不是笔误：登录/鉴权要读 `users` 表校验密码，如果 auth 和 user 同层，会直接违反"禁止同层模块互相依赖"（第12条）。`user` 承载的是被几乎所有模块引用的基础身份数据（`created_by` 之类的字段到处都要用到），把它当作和 `platform` 一样的基础设施对待，比人为拆出一个"auth 专用的用户读路径"更干净。`user` 模块本身仍然遵守第0层"不依赖任何业务模块"的约束，只是被业务模块依赖的位置更靠前。

**Phase 3 设计知识库时发现的修正**：`agent`/`knowledge` 最初被划在同一层（都只依赖 provider），但 `agent_knowledge_bases` 关联表要求 agent 创建/编辑时校验 `knowledge_base_id` 合法（调 `knowledge.Service`），这就是第12条说的"同层模块必须互相调用"——按规则把 `knowledge` 下沉一层，`agent` 相应上移，`conversation`/`workflow` 跟着顺延。这个方向不是任意的：`agent` 本质上是"组合 provider 的模型 + knowledge 的知识库（以后还有 mcp 的工具）"的配置层，天然应该排在这些被组合的资源模块之上，不是反过来。

12. 只能依赖更低层模块的 `Service` 接口，禁止反向依赖，禁止同层模块互相依赖。如果同层两个模块发现必须互相调用，说明分层分错了——要么合并成一个模块，要么把其中一个下沉一层，不允许绕过规则用事件/回调之类的手段"假装"没有循环依赖。
13. 模块间真正共享、和具体业务无关的类型（分页参数、通用错误码枚举等）放 `internal/platform`，不放在某个业务模块里让别人 import。
14. 跨模块调用一律是 Go 接口的同进程直接调用（传 `context.Context`），不做模块内部 HTTP/RPC 自调用；但方法签名只传值类型/领域 struct，不传指针指向对方模块的可变内部状态，为将来万一要拆分服务保留可能性（非近期目标，只是约束写法）。

### 依赖注入方式（不用 DI 框架）

15. `main.go` 里手写一个 `buildApp(cfg Config) (*gin.Engine, error)`，按第0→4层顺序，逐模块 `NewRepository → NewService → NewHandler`，上一层的 Service 作为下一层的构造参数传入。不引入 wire/fx 等 DI 框架——单人项目手写组装更直观、编译期报错更直接。
16. 模块目录名 = package 名，不加 `xxx_service`/`xxx_repo` 后缀；接口固定叫 `Service`（调用方通过 `agent.Service` 这种带包名的写法引用，不会和别的模块的 `Service` 撞名）。

### 检查方式

单人项目没有 CI，但违反第 3 层依赖方向的问题必须能低成本发现：`make check-deps` 用 `go list -deps ./internal/...` 拉出每个包的 import 列表，按上面的层级表校验有没有反向/同层依赖，发现即报错退出非 0。每加一个模块跑一次。

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

## 数据库层性能规范（MySQL 8.x，建表时逐字执行）

### 通用字段约定
- **主键**：单表 `id CHAR(36) NOT NULL PRIMARY KEY`，值用 **UUIDv7**（`google/uuid` 的 `uuid.NewV7()`），不用 UUIDv4/数据库 `UUID()` 函数——UUIDv7 前缀是时间戳、单调递增，InnoDB 按主键聚簇存储，随机的 UUIDv4 会导致插入时随机写入、页分裂，大表（messages/chunks）写入性能会明显劣化。纯多对多关联表（`agent_knowledge_bases`、`agent_mcp_tools`）不单独生成 id 列，用两个外键列组成复合主键 `PRIMARY KEY (a_id, b_id)`。
- **时间戳**：`created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)`，`updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3)`。用 `DATETIME(3)` 不用 `TIMESTAMP`，应用层统一按 UTC 写入，MySQL 连接串固定 `time_zone='+00:00'`。
- **软删除 vs 硬删除**：主数据表（users/model_providers/agents/knowledge_bases/mcp_servers/workflows）用软删除，统一用 `is_active TINYINT(1) NOT NULL DEFAULT 1`，不额外加 `deleted_at`；纯记录/日志型表（messages/workflow_run_steps）不做软删除，要删就是真删。
- **字符集/引擎**：`ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC`。
- 枚举字段用 `VARCHAR(32) NOT NULL CHECK (col IN (...))`，不用原生 `ENUM`；布尔字段用 `TINYINT(1)`，前缀 `is_`/`has_`；小数字段用 `DECIMAL(3,2)` 不用 `FLOAT`；长文本用 `TEXT`/`LONGTEXT`，其余字符串给合理 `VARCHAR` 长度上限；字段默认 `NOT NULL`，只有业务上确实可能不存在才允许 NULL。

建表模板：
```sql
CREATE TABLE example_table (
    id CHAR(36) NOT NULL,                    -- UUIDv7，应用层生成
    -- 业务字段...
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_<fk列名> (<fk列名>)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;
```

### 索引设计原则
每个外键列必须显式建索引（MySQL 不会自动建）；复合索引列顺序按「等值过滤列在前、范围/排序列在后」；只为真实查询模式建索引。逐表索引清单：

```
users              PK(id), UNIQUE(email)
refresh_tokens     PK(id), INDEX(user_id), UNIQUE(token_hash), INDEX(expires_at)
model_providers    PK(id), INDEX(created_by)
agents             PK(id), INDEX(provider_id), INDEX(created_by)
conversations      PK(id), INDEX(agent_id), INDEX(user_id, updated_at DESC)
messages           PK(id), INDEX(conversation_id, created_at, id)
knowledge_bases    PK(id), INDEX(created_by)
documents          PK(id), INDEX(knowledge_base_id, status)
chunks             PK(id), INDEX(knowledge_base_id), INDEX(document_id)
agent_knowledge_bases  PK(agent_id, knowledge_base_id), INDEX(knowledge_base_id)
mcp_servers        PK(id)
mcp_tools          PK(id), INDEX(mcp_server_id)
agent_mcp_tools    PK(agent_id, mcp_tool_id), INDEX(mcp_tool_id)
workflows          PK(id), INDEX(created_by)
workflow_runs      PK(id), INDEX(workflow_id, started_at DESC)
workflow_run_steps PK(id), INDEX(workflow_run_id, started_at)
```

低基数列不单独建索引，并入复合索引做前置过滤列。JSON 列不能直接建索引；真出现按 JSON 内部字段过滤的需求时用 Generated Column + 索引。

### 大表预判和应对策略
`messages`、`chunks` 是仅有的两张会持续增长到百万行以上的表。
1. **大表查询必须始终带强选择性过滤条件**——messages 永远 `WHERE conversation_id=?`、chunks 永远 `WHERE knowledge_base_id=?`、workflow_run_steps 永远 `WHERE workflow_run_id=?`，禁止不带这类条件的查询。
2. **分区不在当前阶段做**，触发条件：messages 单表超过约 2000 万行，或某查询稳定超过 200ms 再评估按 created_at 做 RANGE 分区。
3. **有 TTL 语义的表必须有清理任务**：`refresh_tokens` 过期/已撤销的行需要定期任务（asynq periodic task）清理。
4. 归档留口子不留实现：日志型大表不建会阻碍未来归档的强级联约束。

### 分页查询注意事项
- **大表（messages/chunks/workflow_run_steps）一律用游标分页，不用 OFFSET/LIMIT**：排序键 `(created_at, id)` 复合，`WHERE conversation_id=? AND (created_at,id) < (:last_created_at,:last_id) ORDER BY created_at DESC, id DESC LIMIT :n`；前端用"加载更多"而不是"共 X 页"。
- **小表（管理后台列表：providers/users/agents/workflows）可以直接用 OFFSET/LIMIT**。
- 任何分页接口的 `LIMIT` 服务端强制设硬上限（如 200）。

## 其他约定

- `Makefile` 提供 `dev`/`build`/`migrate-up`/`migrate-down`/`sqlc`/`check-deps` 目标，日常开发用这些命令而不是手写等价命令。
- API 统一在 `/api/v1/*`，其余路径走 SPA fallback。
- Redis 只做缓存/限流状态/asynq 任务队列，不存任何"真相"数据。熔断器状态保持进程内，不放 Redis 跨实例共享。
