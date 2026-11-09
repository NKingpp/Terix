#!/usr/bin/env python3
"""从 Go 源码提取动态建表的 DDL，输出到 stdout（供 psql 执行）。

# 为什么需要这个工具

llm_records / llm_usage 这两张表由 Go 代码在首次使用时建：
  - db/commands.go:157  const llmRecordsSchema
  - db/llm_usage.go:30  const llmUsageSchema

它们不在 db/schema.sql 里 —— 刻意如此，避免「两处定义同一张表，
改一处忘另一处」。但这让「全新空库能否跑起来」的验证缺了两张表。

scripts/test-fresh-db.sh 需要在建完 schema.sql 后补建这两张。
本工具从 Go 源码直接提取，而不是在 shell 里手抄一份 ——
手抄会随时间漂移（我第一次抄错了 5 个列名，而脚本不报错，看起来是成功的）。

# 用法

    python3 scripts/extract-dynamic-ddl.py | psql -d <fresh_db>

# 输出

三段 DDL 依次拼接，全部幂等（IF NOT EXISTS / ADD COLUMN IF NOT EXISTS）。
"""
import re
import sys

# (文件, 常量名, 用途说明)
SOURCES = [
    ("db/commands.go", "llmRecordsSchema", "llm_records 建表"),
    ("db/commands.go", "llmRecordsMigrate", "llm_records 补列（老库需要，新库为 no-op）"),
    ("db/llm_usage.go", "llmUsageSchema", "llm_usage 建表"),
]


def extract(path: str, name: str) -> str:
    """取出 const <name> = `...` 里的内容。

    Go 的 DDL 用反引号原始字符串，所以内容是原样的 SQL。
    用 re.S 让 . 匹配换行。
    """
    try:
        src = open(path, encoding="utf-8").read()
    except OSError as e:
        sys.exit(f"读不到 {path}: {e}")

    m = re.search(r"const\s+" + re.escape(name) + r"\s*=\s*`(.*?)`", src, re.S)
    if not m:
        # 明确报错而不是静默输出空 —— 静默会让 test-fresh-db.sh 看起来成功。
        sys.exit(f"未能从 {path} 提取 {name}（常量改名了？）")
    return m.group(1)


def main() -> None:
    parts = []
    for path, name, why in SOURCES:
        body = extract(path, name).strip()
        if not body:
            sys.exit(f"{path}:{name} 提取到空内容（{why}）")
        parts.append(f"-- [{why}] from {path}:{name}\n{body}")

    # 断言两张表都出现在提取结果里
    joined = "\n".join(parts)
    for table in ("llm_records", "llm_usage"):
        if f"CREATE TABLE IF NOT EXISTS {table}" not in joined:
            sys.exit(f"提取结果里没有 {table} 的 CREATE TABLE —— 常量内容可能已变")

    print(joined)


if __name__ == "__main__":
    main()
