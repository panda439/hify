# alias protocol v3 smoke

最小真实 14B 请求，验证候选人物 ID 与候选证据引用的边界。

- 候选人物 ID：`char-aq`；候选依据引用：`char-aq#0`。
- 两个输出决策的 `character_id` 都是 `char-aq`，没有把 `#0` 或 `#1` 拼入人物 ID。
- 调用耗时 20,495 ms；完整请求、响应和 usage 在 `response.json`。

这只验证本次提示补丁解决了“证据引用误填人物 ID”的一类协议错误。它不证明
全书别名归一成功率，也不代表关系抽取效果。
