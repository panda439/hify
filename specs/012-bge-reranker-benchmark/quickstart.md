# Quickstart: BGE Reranker 真实对照评测

> 本文件描述验收顺序；命令参数由实施阶段的测试固定后补齐。不要把示例输出当作真实结果。

## 1. 前置检查

- 确认当前分支为 `012-bge-reranker-benchmark`
- 确认 011 Mini 数据、raw run 和 baseline 报告存在且哈希未变
- 确认 Ollama `bge-m3:567m` digest 与 011 一致
- 记录可用内存、swap、磁盘空间；不要同时运行无关大型生成模型

## 2. 准备本地 Rerank 服务

使用 `uv` 的 Python 3.12 隔离环境安装锁定依赖，将模型缓存根放入 `eval/cache/rerank-models/`。`HF_HOME` 只表示 Hugging Face cache root，sidecar 会把 `CrossEncoder` 的 `cache_dir` 指向其 `hub/` 子目录，避免重复下载。Xet 大文件通道不稳定时，安装、下载和服务启动都必须显式设置 `HF_HUB_DISABLE_XET=1`，走官方标准 HTTP：

```sh
make eval-rerank-install
make eval-rerank-download
make eval-rerank-service
```

等价的固定下载方式为：

```sh
HF_HOME="$PWD/eval/cache/rerank-models" \
HF_HUB_DISABLE_XET=1 \
uv run --project eval/rerank-service --python 3.12 python -c \
  'import os; from sentence_transformers import CrossEncoder; CrossEncoder("BAAI/bge-reranker-v2-m3", revision="953dc6f6f85a1b2dbfca4c34a2796e7dde08d41e", cache_dir=os.path.join(os.environ["HF_HOME"], "hub"))'
```

启动后依次验证：

1. `/health` 的模型名、revision、许可和 runtime
2. 固定 query + 相关/无关 passage 的 `/rerank` 完整响应
3. 相关 passage 分数高于无关 passage

## 3. 配置 Hify 实验模型

通过 benchmark setup 命令幂等创建或复用本地 provider 与 `capability=rerank` 模型。只把生成的 ID 放入 gitignored checkpoint，不保存 token。

## 4. 运行真实 A/B

保持 011 知识库和全部召回配置不变，开启 Rerank 后运行 50 条查询，再从 raw run 纯重算指标。任何 query 超时、失败或降级都使 run incomplete。

## 5. 比较与决策

实验 compare 必须先证明唯一差异是 Rerank，再输出各 K delta 和逐 query 分类：

- `ADOPT`: MRR@10、NDCG@10 上升，Recall@10 不下降，50 条零失败零降级
- `DO_NOT_ADOPT`: 实验完整但未满足门禁
- `INCONCLUSIVE`: 实验不完整或指纹不兼容

## 6. 回归与报告

运行 Go/Python 全部测试、依赖检查、静态检查和 14 条确定性检索门禁；确认关闭 Rerank 后 011 纯重算逐字段不变。最终报告必须分开写模型真实效果、机制证明、资源/延迟和 MIRACL qrels 边界。

## 7. 30 秒质量诊断

使用显式 `quality_diagnostic` 模式重跑同一 50-query benchmark，超时固定为 30 秒。输出使用独立文件名，不覆盖 1.5 秒制品。

```sh
make eval-rerank-quality-run RERANK_USER_ID="$HIFY_BENCHMARK_USER_ID" RERANK_MODEL_ID="$HIFY_RAG_RERANK_MODEL_ID"
make eval-rerank-quality-score
make eval-rerank-quality-decision BASELINE=eval/runs/miracl-zh-mini-report.json
```

质量 raw/report/decision 默认写入 `eval/runs/miracl-zh-rerank-quality*.json`。

仅当 `hify_applied_count=50`、`hify_degraded_count=0`时输出 `QUALITY_PASS` 或 `QUALITY_FAIL`；其他情况输出 `QUALITY_INCONCLUSIVE`。无论质量结论如何，Hify 生产默认超时仍为 1.5 秒。
