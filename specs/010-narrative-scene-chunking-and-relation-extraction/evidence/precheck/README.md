# T041 模型预检

重跑前确认 Ollama 已启动，并执行：

```sh
HIFY_PRECHECK_MODELS=qwen2.5:14b,qwen2.5:7b go test ./internal/knowledge/ -run TestNarrativePrecheck -count=1 -v -timeout 40m
```

未设置 `HIFY_PRECHECK_MODELS` 时测试直接跳过；本次只读取 `ch01.txt`、`ch02.txt`，按 500 rune、0 overlap 分块。
`raw/` 保存每次 Ollama 调用的完整 HTTP 响应及请求 hash/rune 数，不保存 prompt 正文。
`summary.json` 汇总模型 digest、调用阶段、解析/定位结果、耗时、usage 和超时；这些是预检事实，不是准确率。

## 从 raw 重算

本批 raw 已包含 HTTP 状态、`error` 原串、响应 content 和 `finish_reason`，可只读 raw 重算而不发起模型请求：

```sh
GOCACHE="$PWD/.tmp-gocache" HIFY_PRECHECK_RECOMPUTE=1 \
  go test ./internal/knowledge/ -run TestNarrativePrecheck -count=1 -v
```

重算时 `calls` 是 raw 中发起过的调用数；`call_failed` 是 HTTP 层失败数，不进入 JSON 统计；`json_valid` 与 `json_invalid` 的分母是拿到响应的次数。HTTP 错误不会用错误文本参与归类，原始错误仍保存在对应 raw 记录的 `error` 字段；响应 content 以三个反引号开头归入「围栏」，`finish_reason == "length"` 归入「截断」，其余无法由响应事实确定的解析失败归入「其他」。

本批 raw 没有保存模型 digest 字段，因此离线重算时 `summary.json` 的 `digest` 为空；下次真实预检应把 digest 一并写入 raw，避免重算丢失该元数据。

已知局限：只有 2 章；直接调用 Ollama `/v1/chat/completions`，绕过生产 `provider.ChatOnce`；没有人工真值，不能谈 precision、recall 或准确率。
