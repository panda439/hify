# Quickstart：验证文档处理的「成功但有提示」通道

**Feature**: `007-document-processing-notice` | **Date**: 2026-09-05

每一步都必须**真的跑过并看到输出**才可勾选（宪法第 VI 条）。数据库测试**禁止 skip**。

---

## 0. 前置

```bash
make app-down     # ⚠️ 不能省：容器里的 asynq reconcile 会在测试跑的过程中改文档状态，污染数据
make db-up
```

---

## 1. 改动前基线（**必须在写任何实现代码之前**）

### 1.1 SC-001 的**失败**证据

```bash
# 先只写 SC-001 的验收用例（部分页无文本层的 PDF 入库后，文档列表接口带回缺页信息），
# 不写任何实现
go test ./internal/knowledge/ -race -count=1 -run 'TestIntegrationPartialScanNoticeReachesDocumentList' -v \
  2>&1 | tee /tmp/sc001-before-007.txt
```

**这一步必须看到 `FAIL`。** 一个改动前就能通过的验收标准证明不了任何事。
若它改动前就绿，说明用例没断言到"用户能看见"这件事——**重写用例**，不是往下走。

### 1.2 检索门禁基线（证明本功能没碰检索）

```bash
make eval-retrieval-gate && cp eval/runs/phase6-retrieval-gate-latest.json /tmp/gate-baseline-007.json
```

### 1.3 全量测试基线

```bash
go test ./... -race -count=1 2>&1 | tee /tmp/tests-before-007.txt
```

---

## 2. 夹具清单

| # | 夹具 | 构造要点 | 验证什么 | 对应 |
|---|---|---|---|---|
| **F1** | 部分页无文本 PDF | 5 页里第 2、4 页无文本（006 的 `writeTestPDF` + `pdfLinesFromStrings("", ...)` 已能造） | 文档 `ready`、可检索、**列表接口**带回 `[2,4]` | **SC-001** |
| **F2** | 全页有文本 PDF | 006 的既有夹具 | 写 `NULL`，呈现与改动前逐字节一致 | **SC-004** |
| **F3** | 纯扫描件 | 所有页无文本 | 仍走**失败** `knowledge.pdf_no_text_layer`，不写提示 | **SC-006** |
| **F4** | txt / md | 任意 | 恒 `NULL` | **SC-007** |
| **F5** | 同一文档两个版本 | 先 F1（缺页）后 F2（不缺页） | 提示**消失** | **SC-005** |
| **F6** | 版本竞争 | 同一文档两次处理，只有一次赢下 publishing | 提示属于**赢的那一次** | Edge Cases |

⚠️ **F1 的断言必须走文档列表接口**，不能只查单份文档：漏掉列表那一处 `SELECT` 的话，
单查用例会通过，而用户唯一能看到提示的地方恰恰是列表（data-model §6）。

---

## 3. 验证命令

```bash
make check-deps      # MUST 输出 "OK - no cross-layer or same-layer violations"
go vet ./...         # MUST 无输出
make migrate-up
make sqlc && git diff --stat internal/db/gen    # 生成代码不得手改
go test ./... -race -count=1                     # 全绿，无 skip
cd web && npx tsc --noEmit                       # 前端类型检查
```

**迁移本身要验**：

```bash
docker exec hify-mysql-1 mysql -uroot -phify_root_dev hify \
  -e "SELECT count(*) FROM documents WHERE unextracted_pages IS NOT NULL;"
# MUST 返回 0 —— 存量文档不被追溯标记（FR-015 / SC-007）
```

**门禁必须 IDENTICAL**：

```bash
make eval-retrieval-gate
python3 scripts/compare-retrieval-gate.py /tmp/gate-baseline-007.json eval/runs/phase6-retrieval-gate-latest.json
```

本功能不碰检索链路，所以门禁**逐字节不变**是它的**证明**而不是它的目标。
若它变了，说明有人改到了不该改的地方。

⚠️ **禁止用 `make eval` 当证据**：它每条用例都调真实对话与裁判模型，同一份代码跑两次都不一致。

---

## 4. 逐条成功标准的验证方式

| SC | 怎么验 | 夹具 |
|---|---|---|
| **SC-001** 无需查日志即可知道有内容没进去 | F1 入库 → **走文档列表接口** → 断言带回缺页页码。⭐ 必须先看到改动前 FAIL | F1 |
| **SC-002** 用户能说出下一步动作 | 断言提示文案中同时含**数量**与**页码**，且提到 OCR | F1 |
| **SC-003** 提示不被误判为失败 | 断言带提示的文档 `status === "ready"`；前端断言它与失败的呈现不同、且仍呈现为可用 | F1 + F3 |
| **SC-004** 无提示时呈现逐字节一致 | F2 断言字段为 `null`，前端渲染结果与改动前**逐字符相同** | F2 |
| **SC-005** 提示与当前状态 100% 一致 | F5：先缺页后不缺页，断言提示消失；再断言失败时不显示旧提示 | F5 |
| **SC-006** 纯扫描件仍作为失败呈现 | F3 断言错误码与文案与 006 上线时**完全一致** | F3 |
| **SC-007** 非 PDF 与存量文档 0 条提示 | F4 断言恒 `null`；迁移后 SQL 统计断言为 0 | F4 |

### 4.1 变异测试（确认断言真的有牙齿）

跑完务必还原：

| 注入的缺陷 | 应当失败的用例 |
|---|---|
| `MarkDocumentReady` 不写 `unextracted_pages`（沿用旧 SQL） | SC-001 |
| 写入但**不清除**（无缺页时保留旧值） | SC-005 |
| **只在单查那处** `SELECT` 带出新列，列表那处漏掉 | SC-001（这正是它必须走列表接口的原因） |
| 编码时不排序（依赖来源顺序） | 乱序输入的往返单测 |
| 纯扫描件改判为「成功但有提示」 | SC-006 |
| 前端在 `status !== "ready"` 时也展示该字段 | FR-005 / D7 的断言 |

---

## 5. 冒烟

```
/smoke-test
```

外加一次真实手动路径：**上传一份真实的、部分页是扫描图的 PDF**，
在文档列表里确认提示可见、可读、且不像一个错误。

⚠️ helper 造的 PDF 永远只覆盖 helper 作者想到的情形——006 就是在这一步
撞上了 `rsc.io/pdf` 在真实 PDF 上 panic 的既有缺陷。这一步不能省。

---

## 6. 验收清单

**改动前**

- [ ] `make app-down` + `make db-up` 已执行
- [ ] ⭐ SC-001 用例跑出 **FAIL**，输出归档到 `/tmp/sc001-before-007.txt`
- [ ] 门禁基线归档到 `/tmp/gate-baseline-007.json`
- [ ] 全量测试基线归档

**改动后**

- [ ] `make check-deps` OK；`go vet ./...` 无输出；`tsc --noEmit` 通过
- [ ] `make migrate-up` 成功；存量行 `unextracted_pages IS NOT NULL` 计数为 **0**
- [ ] `make sqlc` 后生成代码只含预期改动，且未手改
- [ ] `go test ./... -race -count=1` 全绿，**无 skip**
- [ ] 门禁比对输出 `IDENTICAL`
- [ ] ⚠️ 全程**未使用** `make eval` 作为证据
- [ ] SC-001 用例现在 PASS（且改动前 FAIL 已在报告里引用）
- [ ] SC-002 ~ SC-007 逐条通过
- [ ] 变异测试：注入的每种缺陷都让对应用例**确实失败**
- [ ] `/smoke-test` 通过 + 真实部分扫描 PDF 手动验证过
- [ ] `docs/eval-phase14-document-processing-notice-report.md` 已产出

**报告必须如实包含的三件事**

1. **本功能不改善处理质量**，不让任何一页变得能提取。它只是把一件已经发生的事说出来。
   不得写成"处理质量提升"。
2. **提示只在文档列表可见**。用户在对话里问到缺失内容时**仍然只会得到"检索不到"**——
   本功能让他能在上传后**预先知道**，不改善那个体验。
3. **存量文档不追溯标记**：`NULL` 同时表示"没缺页"和"不知道"，系统不区分。
