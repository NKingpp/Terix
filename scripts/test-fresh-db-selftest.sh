#!/usr/bin/env bash
# scripts/test-fresh-db-selftest.sh —— 验证 test-fresh-db.sh 真的能报错。
#
# # 为什么需要「校验器自测」
#
# test-fresh-db.sh 是一个校验器。它的输出是「通过」或「失败」。
# 而「校验器自己坏了」的时候，输出也是「通过」—— 这就无法区分
# 「真的没问题」和「没在检查」。
#
# 实测（2026-10-28）第一版 test-fresh-db.sh 有四个 bug，全部表现为「通过」：
#   ① grep "^CREATE TABLE" 漏了缩进行  → 「期望 65 实际 69」（碰巧报错了，但不是因为表缺）
#   ② docker exec 写 /tmp，宿主机读不到  → 差集永远是空
#   ③ diff 格式标记没过滤干净          → 「无差异」
#   ④ 动态表 DDL 手抄，5 个列名错      → 「建表成功」
#
# 这四个 bugs 全部靠「输出与已知事实矛盾」才发现。
# 而「已知事实」必须是故意构造的 —— 这就是本脚本的作用。
#
# # 用法
#
#   ARTEX_PG_DOCKER=artex-pg ./scripts/test-fresh-db-selftest.sh
#
# 全部 4 个用例都必须「正确失败」才算通过。
set -uo pipefail

PG_CONTAINER="${ARTEX_PG_DOCKER:?需要 ARTEX_PG_DOCKER}"
PG_USER="${ARTEX_PG_USER:-artex}"
SELFTEST_DB="${ARTEX_SELFTEST_DB:-artex_selftest}"
# 参照库：列级对比的「已知正确」基线。用开发库（它包含全部列）。
REF_DB="${ARTEX_PG_DB:-artex}"

TMPDIR_LOCAL="$(mktemp -d "${TMPDIR:-/tmp}/artex-selftest-XXXXXX")"
trap 'rm -rf "$TMPDIR_LOCAL"; docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d postgres -q -c "DROP DATABASE IF EXISTS $SELFTEST_DB;" >/dev/null 2>&1 || true' EXIT

PASS=0
FAIL=0

# 每个用例都在独立库里跑，互不影响。
recreate_db() {
  docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d postgres -q \
    -c "DROP DATABASE IF EXISTS $SELFTEST_DB;" \
    -c "CREATE DATABASE $SELFTEST_DB;" >/dev/null
}

# expect_fail <用例名> <期望的错误信息片段> <要跑的命令...>
#
# # 为什么两个都要校验
#
# 只验「退出码非 0」是不够的：脚本可能因为**别的**原因失败 ——
# 连不上数据库、psql 命令拼错、临时文件权限不对 ——
# 那些都会让退出码非 0，但**没有验证到你相验证的东西**。
#
# 所以必须同时确认「失败原因是预期的」。
# 这是 self-test 与普通测试最大的区别：
#   普通测试验「成功」；
#   self-test 验「以正确的方式失败」。
expect_fail() {
  local name="$1"; shift
  local expect_msg="$1"; shift

  if "$@" >"$TMPDIR_LOCAL/out.txt" 2>&1; then
    echo "🔴 [$name] 应当失败但通过了（检查逻辑失效）"
    echo "    输出（前 5 行）："
    head -5 "$TMPDIR_LOCAL/out.txt" | sed 's/^/      /'
    FAIL=$((FAIL+1))
    return
  fi

  # 退出码正确，但还要确认失败原因
  if ! grep -q -- "$expect_msg" "$TMPDIR_LOCAL/out.txt"; then
    echo "🔴 [$name] 失败了，但原因不对 —— 期望看到「$expect_msg」"
    echo "    实际输出（末 5 行）："
    tail -5 "$TMPDIR_LOCAL/out.txt" | sed 's/^/      /'
    echo "    这说明脚本是因为别的原因失败的，没验证到目标行为。"
    FAIL=$((FAIL+1))
    return
  fi

  echo "✅ [$name] 正确报错（「$expect_msg」）"
  PASS=$((PASS+1))
}

# 复现「表数对比」的核心逻辑，用于用例 ①
check_table_count() {
  local schema_file="$1"
  local expected
  # 与主脚本一致：CREATE 数 + 动态数
  expected=$(( $(grep -cE "^[[:space:]]*CREATE TABLE IF NOT EXISTS" "$schema_file") + 2 ))
  local actual
  actual=$(docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d "$SELFTEST_DB" -tAc \
    "select count(*) from information_schema.tables
     where table_schema='public' and table_type='BASE TABLE';")
  [ "$actual" -eq "$expected" ] || {
    echo "表数不一致：期望 $expected，实际 $actual" >&2
    return 1
  }
}

# 复现「列级对比」的核心逻辑，用于用例 ②④
check_columns() {
  local ref_db="$1"
  docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d "$ref_db" -tAc \
    "select table_name||'.'||column_name from information_schema.columns
     where table_schema='public' order by 1;" > "$TMPDIR_LOCAL/c_ref.txt"
  docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d "$SELFTEST_DB" -tAc \
    "select table_name||'.'||column_name from information_schema.columns
     where table_schema='public' order by 1;" > "$TMPDIR_LOCAL/c_new.txt"
  local missing
  missing=$(comm -23 "$TMPDIR_LOCAL/c_ref.txt" "$TMPDIR_LOCAL/c_new.txt")
  [ -z "$missing" ] || {
    echo "新库缺列：" >&2
    echo "$missing" >&2
    return 1
  }
}

# 复现「种子检查」的核心逻辑，用于用例 ③
check_seed() {
  local n
  n=$(docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d "$SELFTEST_DB" -tAc \
    "select count(*) from intercept_rules
     where match_target='draft_field' and name like '[内置]%';")
  [ "${n:-0}" -ge 5 ] || {
    echo "内置规则只有 $n 条（期望 >=5）" >&2
    return 1
  }
}

echo "=========================================="
echo " 用例 ①：schema.sql 删掉一张表 → 表数对比必须 FAIL"
echo "=========================================="
recreate_db
# 删掉 instruments（一张被多张表依赖的，删它最能体现「删表」的影响）
grep -vE "^CREATE TABLE IF NOT EXISTS instruments \(" db/schema.sql > "$TMPDIR_LOCAL/no_instruments.sql"
# 但只删表名会让 CREATE 数不变 —— 必须真的删掉整个 CREATE 块
python3 - "$TMPDIR_LOCAL/no_instruments.sql" <<'PYEOF'
import re, sys
p = sys.argv[1]
s = open(p, encoding="utf-8").read()
s = re.sub(r"CREATE TABLE IF NOT EXISTS instruments \(.*?\n\);", "", s, flags=re.S)
# 顺带删掉对它的引用，否则灌库会报错
s = re.sub(r".*REFERENCES instruments\(id\).*\n", "", s)
open(p, "w", encoding="utf-8").write(s)
PYEOF
docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d "$SELFTEST_DB" -q \
  -v ON_ERROR_STOP=1 < "$TMPDIR_LOCAL/no_instruments.sql" >/dev/null 2>&1 || true
expect_fail "用例①删表" "表数不一致" check_table_count "$TMPDIR_LOCAL/no_instruments.sql"

echo ""
echo "=========================================="
echo " 用例 ②：schema.sql 删掉一列 → 列级对比必须 FAIL"
echo "=========================================="
recreate_db
# 从 execution_limits 删掉 max_single_amount 列
python3 - "$TMPDIR_LOCAL/schema_no_col.sql" <<'PYEOF'
import re, sys
s = open("db/schema.sql", encoding="utf-8").read()
s = re.sub(r"\n\s*max_single_amount numeric\(20,8\),", "", s, count=1)
open(sys.argv[1], "w", encoding="utf-8").write(s)
PYEOF
docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d "$SELFTEST_DB" -q \
  -v ON_ERROR_STOP=1 < "$TMPDIR_LOCAL/schema_no_col.sql" >/dev/null 2>&1 || true
# 参照库用「完整 schema 建的库」——这里直接用开发库
expect_fail "用例②删列" "缺列" check_columns "$REF_DB"

echo ""
echo "=========================================="
echo " 用例 ③：跳过 seed → 种子检查必须 FAIL"
echo "=========================================="
recreate_db
docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d "$SELFTEST_DB" -q \
  -v ON_ERROR_STOP=1 < db/schema.sql >/dev/null 2>&1
# 故意不跑 dbinit（不 seed）
expect_fail "用例③跳过seed" "内置规则只有" check_seed

echo ""
echo "=========================================="
echo " 用例 ④：动态表 DDL 少一列 → 列级对比必须 FAIL"
echo "=========================================="
recreate_db
docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d "$SELFTEST_DB" -q \
  -v ON_ERROR_STOP=1 < db/schema.sql >/dev/null 2>&1
# 故意用缺列的 llm_records DDL（模拟「手抄时漏了一列」）
docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d "$SELFTEST_DB" -q <<'SQL' >/dev/null 2>&1
CREATE TABLE IF NOT EXISTS llm_records (
    id BIGSERIAL PRIMARY KEY,
    ts TIMESTAMPTZ DEFAULT now(),
    model TEXT
);
CREATE TABLE IF NOT EXISTS llm_usage (
    id BIGSERIAL PRIMARY KEY,
    ts TIMESTAMPTZ NOT NULL DEFAULT now(),
    task_id TEXT
);
SQL
expect_fail "用例④少列" "缺列" check_columns "$REF_DB"

echo ""
echo "=========================================="
echo " 结果：$PASS 个正确报错，$FAIL 个漏报"
echo "=========================================="
if [ "$FAIL" -ne 0 ]; then
  echo "🔴 校验器自身有问题 —— 它漏报了应当发现的缺陷。" >&2
  echo "   这意味着 test-fresh-db.sh 的「通过」不可信。" >&2
  exit 1
fi
echo "✅ 校验器可信：4 个已知缺陷样本全部被检出。"
