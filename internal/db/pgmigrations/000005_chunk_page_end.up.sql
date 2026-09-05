-- 006-pdf-layout-chunking：片段的页码从「单值」变成「区间」。
--
-- 改动前 PDF 按页独立分块，一个片段不可能跨页，所以 page_number 一个值就够
-- 了。跨页段落合并之后这个前提没了：一个片段可以同时覆盖第 3 页和第 4 页，
-- 这时任选一端当作它的页码就是不诚实的引用（spec 的 FR-011）。因此新增
-- page_end，page_number 的语义收紧为「起始页」——**不改名**，改名是纯成本
-- （DB + sqlc 生成代码 + dto.go + 前端四处），收益只有命名美观。
--
-- 不变量（Go 侧与本约束共同保证，data-model.md 的 C1/C2）：
--   C1  page_end IS NULL  ⟺  page_number IS NULL
--   C2  两者都有值时 page_number <= page_end
-- C3（两端都落在文档实际页数内）**数据库检查不到**——DB 不知道一份文档有
-- 几页——只能由 layout.go 的纯函数在产出时断言，见 layout_test.go。
--
-- ⚠️ 下面三步的顺序不可调换：先加列、再回填、最后加约束。约束在回填之前加
-- 会立刻拒绝所有 page_number 有值的存量行。
--
-- ⚠️ 沿用既有禁令：**禁止用 COALESCE(page_number, 0) / COALESCE(page_end, 0)
-- 一类写法给 NULL 页码兜底**。那等于给一个本来没有页码的片段编造出「第 0
-- 页」，与「绝不伪造来源信息」正面冲突（000003 的注释已写明这条对
-- page_number 成立，page_end 完全适用，一个字都不放宽）。
ALTER TABLE chunks ADD COLUMN page_end integer NULL;

-- 回填**故意不加** WHERE page_number IS NOT NULL：page_number 为 NULL 的行
-- （全部 txt/md chunk，以及 000003 之前写入的存量行）被写成 page_end = NULL，
-- 正好满足 C1。回填后 page_end = page_number，而新的区间相交谓词
-- （page_end >= min AND page_number <= max）在这种行上与旧的点落谓词
-- （page_number >= min AND page_number <= max）逐字节等价——这就是存量数据
-- 检索行为完全不变的全部理由（research.md R2、data-model.md §7.1）。
UPDATE chunks SET page_end = page_number;

-- ⭐ 两个条件必须写在**同一个** CHECK 里用 AND 连接，不能拆成两个独立约束。
-- PostgreSQL 的 CHECK 只在结果为 FALSE 时拒绝，NULL 视为通过；拆开之后
-- 「page_number IS NULL 而 page_end = 4」这一格会从第二个约束里逃逸掉
-- （FALSE OR NULL = NULL → 通过）。合成一条时它是 FALSE AND NULL = FALSE，
-- 才真的被拒绝。逐格真值表见 data-model.md §6。
--
-- page_number >= 1 这个下界是安全的：parse.go 只产出 1..NumPage，
-- filter_test.go 里的 0 与 -1 是**过滤入参**的校验用例，不落库。
ALTER TABLE chunks ADD CONSTRAINT chunks_page_range_valid
  CHECK ((page_number IS NULL) = (page_end IS NULL)
         AND (page_end IS NULL OR (page_number >= 1 AND page_number <= page_end)));
