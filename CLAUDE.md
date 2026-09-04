# Hify 项目开发约定

Hify 是一个简化版 Dify（Go + React 单体应用）。这份文档是项目级强约定，不是风格建议——写代码、建表时逐字遵守。完整产品计划见 `/Users/lishurong/.claude/plans/floofy-churning-rainbow.md`。

技术栈：Go + Gin，MySQL 8.x（业务数据）+ Redis（缓存/限流/asynq 任务队列），React + Vite + TS + Tailwind + shadcn/ui，最终打包成单个 Go 二进制（go:embed 内嵌前端静态资源）。

**关于项目归属（2026-07-22 用户明确说明）**：这个项目的代码是 Claude Code 写的，不是用户本人手写/设计的——用户是通过指挥 AI 工具完成的这个项目，大部分代码（尤其前端）他本人没有读过。任何面向第三方的材料（简历、面试话术、LinkedIn）都不能把 Hify 描述成用户"亲手设计/实现"的作品，只能用"AI 辅助开发/指挥 AI 构建"这类如实的措辞。这条约束同样适用于任何在本仓库工作、或被要求生成简历/面试材料的对话。

## 分文档导航

细则拆到了子目录，读写对应目录的文件时会自动加载，**不需要手动 Read**：

| 文件 | 覆盖内容 | 何时生效 |
|---|---|---|
| `internal/CLAUDE.md` | 模块文件结构、每层职责边界、跨模块调用规则、依赖注入、统一响应/异常/分页、Go 编码规范、可观测能力 | 改 `internal/` 下的 Go 代码 |
| `internal/db/CLAUDE.md` | 建表字段约定、索引清单、大表策略、分页查询写法 | 建表 / 改 migration / 写 sqlc 查询 |

## 开发流程

新功能/新模块/新 Phase 按 `/feature-workflow` 这个 skill 里的完整流程走（规划→后端→前端→验证→验收→git 提交，含常见坑），这里只放一句话核心顺序方便速查：**migration → sqlc → 模块内 `model→errors→repository→service→dto→handler→wire.go` 固定顺序 → 接入 buildApp → 前端 hook→组件 → `/smoke-test` 冒烟验证 → git commit**。项目没有自动化测试，`/smoke-test` 和真实 HTTP 请求验证是唯一的安全网，不能省。

## 模块分层（架构级硬约束）

Hify 是模块化单体：`internal/` 下每个业务目录（auth, user, provider, agent, conversation, knowledge, mcp, workflow）是一个「模块」。依赖方向禁止成环，分 6 层，只能自上而下依赖：

```
第0层  internal/platform, internal/config, internal/db/gen, internal/user   — 不依赖任何业务模块
第1层  auth, provider, mcp                                                   — 只能依赖第0层
第2层  knowledge                                                             — 只能依赖第0、1层
第3层  agent                                                                 — 只能依赖第0~2层
第4层  conversation                                                          — 只能依赖第0~3层
第5层  workflow                                                              — 可依赖第0~4层所有模块
```

- 只能依赖更低层模块的 `Service` 接口，禁止反向依赖，**禁止同层模块互相依赖**。同层两个模块发现必须互相调用 = 分层分错了，要么合并、要么下沉一层，不允许用事件/回调"假装"没有循环依赖。
- 模块间共享、与具体业务无关的类型放 `internal/platform`，不放在某个业务模块里让别人 import。
- **检查方式**：`make check-deps`（`scripts/check-deps.sh`，用 `go list -deps ./internal/...` 校验层级），每加一个模块跑一次，发现违规即非 0 退出。

> 分层的详细论证（为什么 `user` 下沉到第0层、Phase 3 为什么把 `knowledge` 下沉）和每层的职责边界细则见 `internal/CLAUDE.md`。

## 其他约定

- `Makefile` 提供 `dev`/`build`/`migrate-up`/`migrate-down`/`sqlc`/`check-deps`/`eval` 目标，日常开发用这些命令而不是手写等价命令。
- 容器化运行整套（后端也进容器）用 `app-*` 目标：`app-up`/`app-down`/`app-logs`/`app-seed-admin`。`app`/`migrate` 两个 compose 服务带 profile `app`，默认不启动，不影响 `make db-up` + `make dev` 的本地开发流程。**跑集成测试前必须 `make app-down`**——容器和测试共用同一套 dev 库，容器里的 asynq reconcile 会污染测试数据。
- 前端由 `web/embed.go` 用 `go:embed` 收进二进制，`internal/server/static.go` 负责挂载和 SPA fallback。`web/dist/` 是 gitignore 的构建产物，但 `web/dist/.gitkeep` 必须保留在版本库里——`go:embed` 匹配不到任何文件是编译错误，全新 clone 会编译失败。
- API 统一在 `/api/v1/*`，其余路径走 SPA fallback。
- Redis 只做缓存/限流状态/asynq 任务队列，不存任何"真相"数据。熔断器状态保持进程内，不放 Redis 跨实例共享。
