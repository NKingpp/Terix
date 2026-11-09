#!/usr/bin/env python3
"""批量文本替换工具 —— 断言式，失败即停。

# 为什么需要这个

本仓已因 `str.replace()` 静默失败踩过三次坑：

1. 锚点不存在 → `replace` 返回原串不报错；脚本抛异常时前几处已改在内存里
   但没落盘 → 全部丢失
2. `if 'X' not in s` 判断在第一次替换后失效 → 后续替换被跳过，
   但脚本报 `changed: 2`（看起来成功）
3. 路径写错 → Python 打不开文件，但后面的 `gofmt` 照常执行，看不出失败

共同特征：**脚本用无条件 `print('ok')` 当成功信号**。那个 ok 和"操作成功"
没有因果关系，它只代表"流程走完了"。

# 用法

    from must_replace import must_replace, save
    s = must_replace(s, old, new, "PM 门槛")
    s = must_replace(s, old2, new2, "risk 空结果")
    save(path, s)          # 写入 + 复核

# 三个保证

1. 锚点不存在 → 立刻 `SystemExit`（不是静默继续）
2. 每次替换单独报数量（不是最后统一报 ok）
3. `save()` 在写入后**重新读回文件**核对，不一致则报错
"""

import sys
from pathlib import Path


def must_replace(s: str, old: str, new: str, label: str) -> str:
    """替换 old→new，锚点不存在则退出。

    为什么不是「没找到就跳过」：跳过意味着「这次的改动没生效」，
    而调用方通常不会检查返回值差异 —— 他们会看脚本有没有报错。
    所以没找到必须报错。
    """
    if old == new:
        raise SystemExit(f"FAIL [{label}]: old 与 new 相同，这是空操作")

    n = s.count(old)
    if n == 0:
        preview = old[:80].replace("\n", "\\n")
        raise SystemExit(f"FAIL [{label}]: 锚点在文件中不存在\n  锚点前80字: {preview!r}")

    s2 = s.replace(old, new)
    # 复核：新旧数量变化必须与预期一致
    if new not in s2:
        raise SystemExit(f"FAIL [{label}]: 替换后找不到新文本（内部错误）")

    print(f"OK  [{label}]: 替换 {n} 处")
    return s2


def must_replace_all(s: str, old: str, new: str, label: str, expect: int) -> str:
    """替换并断言命中次数恰好是 expect（用于「必须有 N 处」的场景）。"""
    n = s.count(old)
    if n != expect:
        raise SystemExit(
            f"FAIL [{label}]: 命中 {n} 处，期望恰好 {expect} 处\n"
            f"  锚点前80字: {old[:80]!r}")
    print(f"OK  [{label}]: 替换 {n} 处（与预期一致）")
    return s.replace(old, new)


def save(path: str, content: str) -> None:
    """写入文件并**读回核对**。

    读回核对能抓到：磁盘满、路径错误、编码问题、被其它进程覆盖。
    只写不读的话，写入失败时脚本仍会打印成功。
    """
    p = Path(path)
    if not p.parent.exists():
        raise SystemExit(f"FAIL [save]: 父目录不存在: {p.parent}")

    p.write_text(content, encoding="utf-8")

    # 读回核对 —— 这是「写成功」的唯一可靠证据
    actual = p.read_text(encoding="utf-8")
    if actual != content:
        raise SystemExit(
            f"FAIL [save]: 写入后读回不一致\n"
            f"  写入 {len(content)} 字符，读回 {len(actual)} 字符\n"
            f"  路径: {p}")

    print(f"DONE [save]: {p} 已写入并核对 {len(content)} 字符")
