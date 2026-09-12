# Specification Quality Checklist: MIRACL 中文真实检索评测

- [x] 数据源、语言、split、query/doc 规模与公开标注边界明确
- [x] Mini 子集与完整 MIRACL 排行榜不可比的边界明确
- [x] fake 回归门禁与真实 Embedding 效果评测职责分离
- [x] chunk 结果折叠为 document ranking 的口径明确
- [x] Recall/Precision/MRR/MAP/NDCG 的 K、分母和失败处理明确
- [x] baseline 兼容性与拒绝比较条件明确
- [x] 下载、入库、运行、重算可恢复且不自动覆盖 baseline
- [x] 缓存、许可、敏感信息与大文件提交边界明确
- [x] 首版排除 CI、UI、LLM Judge、自动调参和完整 MIRACL
- [x] 验收包含真实模型证据且 skip 不算通过

