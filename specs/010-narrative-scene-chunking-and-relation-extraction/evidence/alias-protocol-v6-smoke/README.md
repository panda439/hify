# alias protocol v6 smoke

最小真实 14B 请求验证 `alias/v3` 的完整 link 输出契约。

- 候选人物 ID：`char-aq`；候选依据引用：`char-aq#0`。
- 两个决策都输出了正确的 `character_id=char-aq`、`new_group=""`、
  `reason_code=explicit_alias`。
- 每个决策都有一条 `chunk` 与一条 `char-aq#0` 的 supports，文字字段都是
  `quote`，没有使用 `text`。
- 真实调用耗时 27,156 ms；完整请求、响应和 usage 在 `response.json`。

该样本只验证严格别名协议的一条明确场景。它不能说明复杂小说段落的全书归一成功率。
