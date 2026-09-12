# Tasks: Voyage Rerank 适配

- [x] T001 固定当前 branch、commit、worktree 状态，确认不包含 012 未提交改动。
- [x] T002 先写 Voyage `top_k` 请求和 `data` 响应测试并验证 RED。
- [x] T003 先写非法 `rerank_format` 创建/更新测试并验证 RED。
- [x] T004 在 `ExtraConfig`、registry 和 HTTP adapter 中实现最小 Voyage 分支。
- [x] T005 实现 `rerank_format` 白名单校验，使目标测试 GREEN。
- [x] T006 复跑既有通用 Rerank 测试，证明 `top_n/results` 行为不变。
- [x] T007 将已存在的 `voyage-ai` Provider 更新为 `rerank_format=voyage`，不回显或重写 Key。
- [x] T008 使用真实 `rerank-3` 经 Hify Provider 客户端验证中文相关性排序。
- [x] T009 运行 `go test -race -count=1 ./...`、`go vet ./...`、`make check-deps`、`git diff --check`。
- [x] T010 对照 FR/SC 和真实 diff 独立验收，记录未验证项；不得提交或推送。
