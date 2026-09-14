# reference-eval-smoke

这是 `cmd/narrativeeval` 的端到端冒烟产物，**不是模型效果评测，也不是人工真值**。

- `truth-reference-only.json` 和 `prediction-ai-draft.json` 都由
  `eval/annotations/aq-ai-v1/relations.jsonl` 的 58 条 AI 初标转换而来；
  人物名按同目录的 AI 别名表规范化。
- 两份输入故意相同，所以 `report.json` 的 precision、recall 和有效出处率均为
  1.0。这只证明快照读取、原始材料/账目哈希、关系规范化和计分报告这条链路可跑通。
- `report.json.reference_only=true` 且 `acceptance=false`；不得引用其中任何数值说明
  qwen、Hify 抽取器或全书关系抽取的实际效果。
- 本例使用 `gold_alias_assisted`，因为初标的实体名称已经按 AI 别名表归一；它不能
  替代真正的 `end_to_end` 指标。

复跑：

```sh
make narrative-eval \
  NARRATIVE_TRUTH=specs/010-narrative-scene-chunking-and-relation-extraction/evidence/reference-eval-smoke/truth-reference-only.json \
  NARRATIVE_PREDICTIONS=specs/010-narrative-scene-chunking-and-relation-extraction/evidence/reference-eval-smoke/prediction-ai-draft.json \
  NARRATIVE_EVAL_OUT=specs/010-narrative-scene-chunking-and-relation-extraction/evidence/reference-eval-smoke/report.json \
  NARRATIVE_REFERENCE_ONLY=1
```
