# Data Model: MIRACL 中文 Mini 检索评测

## DatasetManifest

| 字段 | 含义 |
|---|---|
| schema_version | manifest 格式版本 |
| dataset/revision/language/split/license | 上游身份 |
| selection_seed/query_limit/min_docs/max_docs | 选择配置 |
| query_ids/document_ids | 固定有序 ID |
| qrels_sha256/corpus_sha256/config_sha256 | 完整性与兼容性 |
| created_at | 生成时间，不参与确定性比较 |

## BenchmarkQuery

`query_id`、`text`。ID 唯一、正文非空，至少有一个正 qrel。

## BenchmarkDocument

`source_document_id`、`title`、`text`、`content_sha256`，以及运行后产生的 `hify_document_id` 映射。上游 ID 不因重新入库改变。

## Qrel

`query_id`、`source_document_id`、`relevance`。`relevance > 0` 表示相关，原等级保留给 NDCG。

## RawQueryResult

| 字段 | 含义 |
|---|---|
| query_id | 原始 query ID |
| chunk_hits | Hify 原始返回顺序的 chunk/document ID，不含正文和向量 |
| document_ranking | 首次出现稳定去重后的 document 排名 |
| error | 失败原因；有值时指标计零 |
| elapsed_ms | 单 query 耗时 |

## RetrievalRun

保存 dataset/config/model/retrieval/metric 指纹、全部 `RawQueryResult`、阶段计数和 `complete`。任一 query 未处理或失败时 `complete=false`。

## QueryMetrics

每个 K 保存相关文档总数、返回数、命中数、Recall、Precision、MRR、AP、NDCG。不得只保存百分比而缺失分子分母。

## AggregateMetrics

对所有选中 query 等权平均；同时保存 query 总数、成功/失败数。

## ComparisonReport

保存 baseline/candidate 指纹、兼容性、总体 delta、逐 query improved/regressed/unchanged。`non_comparable=true` 时不得给正式结论。

