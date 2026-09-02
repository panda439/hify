#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""比对两份确定性检索门禁报告，用于 002-metadata-filter 的 SC-003 回归断言。

用法：compare-retrieval-gate.py <baseline.json> <current.json>

判定规则（比"整个文件逐字节相同"更精确，理由见下）：
  1. 基线里的**每一个 case** 必须在当前报告里存在，且逐字段完全相同；
  2. metrics 与 pass 必须完全相同；
  3. 当前报告允许**新增** case——门禁数据集本来就会随功能增长。

为什么不直接比整个文件：`ran_at` 每次运行都不同，而新增门禁用例（本期新增了
filter_scopes_to_document / filter_scopes_to_page_range 两条）会让 cases 数组
变长。SC-003 真正要断言的是"既有行为一个字都没变"，不是"报告文件永远不增长"——
后者会让任何新增门禁用例都变成"回归"，从而逼着人放弃这条断言。
"""
import json
import sys


def load(path):
    with open(path, encoding="utf-8") as f:
        return json.load(f)


def canon(obj):
    return json.dumps(obj, sort_keys=True, ensure_ascii=False)


def main():
    if len(sys.argv) != 3:
        print(__doc__)
        return 2
    base, cur = load(sys.argv[1]), load(sys.argv[2])
    bc = {c["name"]: c for c in base["cases"]}
    nc = {c["name"]: c for c in cur["cases"]}

    problems = []
    for name, case in bc.items():
        if name not in nc:
            problems.append("既有用例消失了: %s" % name)
        elif canon(case) != canon(nc[name]):
            problems.append("既有用例结果改变了: %s" % name)
    if base["metrics"] != cur["metrics"]:
        problems.append("metrics 改变了: %s -> %s" % (base["metrics"], cur["metrics"]))
    if base["pass"] != cur["pass"]:
        problems.append("pass 改变了: %s -> %s" % (base["pass"], cur["pass"]))

    added = sorted(set(nc) - set(bc))
    if problems:
        print("REGRESSION")
        for p in problems:
            print("  -", p)
        return 1
    print("IDENTICAL（%d 个既有用例逐字段一致，metrics/pass 未变）" % len(bc))
    if added:
        print("新增用例（允许）: %s" % ", ".join(added))
    return 0


if __name__ == "__main__":
    sys.exit(main())
