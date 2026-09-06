# Phase 3 独立审核证据

审核对象：`claude/010-phase3-extraction` 的 `f046d59`，覆盖 T014～T023。
审核期间 checkout 已继续提交 Phase 4 的 `2ce52a3`（仅 extract_prompt.go / test），不纳入此次验收。
从 f046d59 导出隔离副本；只替换副本 testutil 的测试库前缀，避免与开发者共用同包测试库。
未停止开发服务，未调用真实模型，未覆盖正在实施的 Phase 4。

- `baseline-tests.jsonl.gz`：原有全量 race 测试通过，868 个测试节点，无失败、无跳过。
- `contract-tests.jsonl.gz`：新增 8 个顶层反例全部失败；其中发布守卫包含 4 个失败子用例。
- `additional-contract-tests.jsonl.gz`：费用归档、续租连续失败两条追加反例全部失败。
- `failures.txt`：上述失败诊断的精简提取；两个 `*_test.go.txt` 保存完整复现测试。
- `manifest.json`：对象和顶层测试计数。顶层数量与包含子用例的 go test 节点数不同，不混计。

复跑：将两个 .go.txt 分别复制为隔离副本 internal/knowledge/zz_phase3_review_test.go、
internal/provider/zz_phase3_review_test.go，然后运行：

```sh
go test ./internal/knowledge ./internal/provider -race -count=1 -timeout=90s -run '^TestReviewP3' -v
```

复现文件故意放在 evidence 中，未将预期失败的审核夹具直接加入正在实施的主测试目录。
本轮给出审核与返工依据，未将缺失的生产工作循环临时拼成另一套 Phase 4 实现。
