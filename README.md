# Hify

[![CI](https://github.com/panda439/hify/actions/workflows/ci.yml/badge.svg)](https://github.com/panda439/hify/actions/workflows/ci.yml)

一个从零实现的简化版 LLM 应用开发平台（对标 Dify 的核心能力），Go + React。不依赖任何 Agent 编排框架——LLM 供应商抽象、RAG 流水线、Agent 工具调用循环、工作流引擎全部手写，用来把这些系统的工作原理真正吃透。

## 核心能力

- **LLM Provider 抽象层**：统一 `Client` 接口（Chat / ChatStream / Embed），OpenAI 兼容协议适配多家供应商；API Key AES-256-GCM 加密落库。
- **弹性调用装饰器**：per-provider 熔断（gobreaker）、并发限流 + Redis 令牌桶、指数退避重试（流式场景只重试首连，断流绝不重试以避免向客户端重复推送）、空闲超时。
- **RAG 全流程**：文档解析分块（含 PDF 逐字形位置重建）→ 批量 embedding → **PostgreSQL + pgvector** 在库内打分/排序/topK（无维度声明 vector 列支撑混合维度知识库）→ 检索结果注入对话上下文并通过 SSE 暴露调试信息。
- **Agent 工具调用循环**：OpenAI 风格 function calling，流式 tool_calls 按 Index 合并分片，MCP（stdio + SSE）工具发现/同步/调用，最大迭代保护。
- **SSE 流式架构**：断线时部分内容仍落库；goroutine / 信号量泄漏防护（trySend + context 联动）。
- **简化工作流引擎**：DAG 校验（无环/可达性）、条件分支（expr-lang）、模板变量渲染、执行轨迹持久化——一个迷你版工作流编排器。
- **JWT + RBAC**、asynq 异步任务队列、sqlc 生成的类型安全 SQL。

## 技术栈

Go 1.26 / Gin / MySQL 8 / PostgreSQL 17 + pgvector / Redis / asynq / sqlc · React / TypeScript / Vite / @xyflow/react

## 快速开始

```bash
cp .env.example .env        # 按需修改
make db-up                  # docker compose 起 MySQL/PG/Redis
go run ./cmd/hify migrate up
make dev                    # 后端（air 热重载）
make web-dev                # 前端
```

## 测试

```bash
make test        # vet + 全部测试（容器没起时集成测试自动 skip）
make test-race   # 加 race detector
```

测试策略围绕 [docs/critical-paths.md](docs/critical-paths.md)——9 条"改造时容易出问题"的核心链路清单：纯逻辑用 characterization test 锁行为（弹性装饰器的故障注入、tool_calls 分片合并等），跨库契约用真实 MySQL + pgvector 集成测试验证（`internal/testutil` 每包独立测试库，支持并行）。CI 里用 service containers 跑全量。

## 架构决策记录

关键设计取舍（为什么 pgvector 而不是专用向量库、为什么无维度声明的 vector 列、跨库删除为什么靠顺序而不是分布式事务、流式重试的边界在哪）散落在各模块的包注释和 `docs/` 里，整理中。
