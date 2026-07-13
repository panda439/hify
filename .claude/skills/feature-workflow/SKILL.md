---
name: feature-workflow
description: Hify 新功能/新模块/新 Phase 开发的标准流程——规划、后端实现顺序、前端实现顺序、验证、验收汇报、git 提交。开始一块新工作时使用；已有功能的小修小补不需要走全套，直接改就行。
disable-model-invocation: false
user-invocable: true
---

# Hify 功能开发标准流程

这是 Phase 0/Phase 1 实际跑下来、验证有效的流程，不是理论上应该怎么做。按这个顺序走，别跳步骤。

## 1. 规划

- 涉及新表/新模块/跨模块调用这类架构决策的，先进 plan mode，把决定写进 `/Users/lishurong/.claude/plans/floofy-churning-rainbow.md`（持久计划文件）。
- 真正需要用户拍板的地方（技术选型、有取舍的架构决策）用 AskUserQuestion 问清楚，不要替他做决定；纯粹的实现细节（文件怎么拆、变量怎么命名）自己判断，别每件小事都问。
- 计划里定下来的、以后每次写代码都要遵守的约定（不是这一次性的决定），同步进 `CLAUDE.md`。计划文件是"这次做了什么决定"的记录，CLAUDE.md 是"以后每次都要照做"的规则，两者不是一回事，别混在一起。

## 2. 后端实现（固定顺序，不要跳着写）

1. migration SQL（跟 CLAUDE.md「数据库层性能规范」：UUIDv7 主键、DATETIME(3)、软删除约定、索引清单）
2. sqlc query 文件 → 跑 `sqlc generate`（在 `internal/db` 目录下）
3. 模块内部固定文件顺序：`model.go → errors.go → repository.go → service.go → dto.go → handler.go → wire.go`
4. 接入 `cmd/hify/main.go` 的 `buildApp`，注意跨模块依赖层级（见 CLAUDE.md 的 5 层依赖图）
5. 每写完一个模块跑一遍：
   ```bash
   gofmt -l .
   go build ./...
   go vet ./...
   ./scripts/check-deps.sh
   ```
   四个都要过，任何一个报错就地修，不要攒到最后一起查。

## 3. 前端实现

1. 缺组件先 `npx shadcn@latest add <component>`
2. 写 `web/src/lib/*.ts` 里的 TanStack Query hook，字段名和后端 DTO 逐个对齐（`snake_case`，别自作主张转 camelCase——历史上这里出过 bug）
3. 写页面/弹窗组件
4. 跑：
   ```bash
   cd web && npx tsc -b --noEmit && npm run build
   ```

## 4. 验证（这一步不能省，历史上靠这一步抓到过真 bug）

- 基础设施 + 服务启动 + 核心检查，直接调用 `/smoke-test` 这个 skill，不要重新发明轮子。
- 新功能特有的检查在 smoke-test 的基础上追加：**必须是真实 HTTP 请求跑一遍正常路径 + 至少一个异常路径**（比如错误输入、鉴权失败、资源不存在），不能只做静态检查就说"验证过了"。
- 前后端一起验证时，两个服务都起（后端 `./bin/hify serve`，前端 `npm run dev -- --port 5173`），**通过 Vite 代理**（`http://localhost:5173/api/...`）打请求，这样测的才是浏览器实际会走的路径，不是直连后端端口。
- 如果依赖外部服务但没有真实凭证（比如 LLM 供应商 API Key），写一个最小的假服务器模拟对方的协议，不要跳过这一步不测。做完记得清理（临时脚本别留在仓库里）。
- 验证过程中发现 bug 就地修，修完重新走一遍这一节，不要带着"应该没问题"的侥幸心理往下走。
- 收尾清理：停掉后台进程、删掉测试过程中产生的数据，`lsof -ti :端口 | xargs -r kill -9` 确认端口释放。

## 5. 验收

- 汇报做了什么、验证到什么深度，明确说清楚哪些是真验证过的、哪些只是类型检查/编译通过（这两者不是一回事，别混着说"已验证"）。
- 前端界面类的东西，起服务给用户自己点一遍——我没有真实浏览器可以驱动，UI 渲染是否正常、点击是否顺手，只有用户亲自试过才算数。
- 用户反馈问题（哪怕是小的，比如文案语言不对）当场修，修完按第4步重新验证一遍再回复。

## 6. Git 提交

这个项目**没有自动化测试**，人工过一遍第4步是唯一的安全网，所以更要靠 git 兜底——一个 Phase/一个完整功能验证通过后，就提交一次：

```bash
git add -A
git status   # 提交前必须看一眼，确认没有 .env、bin/、node_modules、临时测试数据文件混进去
git commit -m "..."
```

不要攒好几个 Phase 一起提交，那样出问题了不好定位是哪一步引入的。

## 已知的结构性限制（不是这个流程能解决的，如实告知用户）

- 没有单元测试/集成测试，回归全靠人工重新跑一遍验证，模块变多之后这里会越来越吃力，值得在纯逻辑重的模块（比如 provider 的 resilience 装饰器）开始补 `_test.go`。
- 外部依赖（LLM 供应商）只用假服务器测过协议层面，没有用真实供应商验证过真实网络环境下的行为。
