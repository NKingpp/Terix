#!/usr/bin/env bash
# scripts/test-fresh-db.sh —— 全新空库自检（SQLite）。
#
# # 这个脚本回答什么问题
#
# 「拿一个全新空目录启动，服务能不能正常起来？」
# 具体是：schema 全部应用 + 全部 seed 成功 + 关键结构真的建出来了。
#
# # 为什么不能只验"表建出来了"
#
# 「表在但空」比「表不存在」隐蔽得多 —— 查询不报错，只返回空集，
# 闸门全部返回 EXEC_GATE_UNAVAILABLE，看起来像业务 bug。
# 所以必须跑真正的 db.Open()（它包含 seed），并核对行数。
#
# # 与 PG 时代的差异
#
# 不再需要 Docker / PostgreSQL / psql 灌库。
# SQLite 是单文件，这里是"用临时目录建一个全新库"。

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

PASS=0
FAIL=0
ok()  { printf '  [OK]   %s\n' "$*"; PASS=$((PASS+1)); }
bad() { printf '  [FAIL] %s\n' "$*"; FAIL=$((FAIL+1)); }

TMPDIR_DB="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_DB"' EXIT
DB_PATH="$TMPDIR_DB/fresh/quantex.db"

echo "============================================================"
echo " 全新空库自检（SQLite）"
echo " 库文件: $DB_PATH"
echo "============================================================"

# ---------- 1. dbinit：建库 + schema + seed ----------
echo
echo "== 1/3 db.Open()（schema + seed）=="
OUTPUT="$(ARTEX_DB_PATH="$DB_PATH" go run ./cmd/dbinit 2>&1)"
RC=$?
if [ $RC -ne 0 ]; then
  bad "dbinit 退出码 $RC"
  echo "$OUTPUT" | sed 's/^/       /'
  echo
  echo "失败，跳过后续检查。"
  exit 1
fi
echo "$OUTPUT" | sed 's/^/       /'

if echo "$OUTPUT" | grep -q "Open 成功"; then ok "Open 成功"; else bad "Open 未报告成功"; fi
if [ -f "$DB_PATH" ]; then ok "库文件已生成"; else bad "库文件不存在"; fi

# 解析 dbinit 报出的数字并断言
tables=$(echo "$OUTPUT"  | sed -n 's/.*表 \([0-9]\+\) \/.*/\1/p' | head -1)
indexes=$(echo "$OUTPUT" | sed -n 's/.*索引 \([0-9]\+\) \/.*/\1/p' | head -1)
triggers=$(echo "$OUTPUT"| sed -n 's/.*触发器 \([0-9]\+\).*/\1/p' | head -1)
agents=$(echo "$OUTPUT"  | sed -n 's/.*内置 agent \([0-9]\+\) 个.*/\1/p' | head -1)
rules=$(echo "$OUTPUT"   | sed -n 's/.*内置草案规则 \([0-9]\+\) 条.*/\1/p' | head -1)

echo
echo "== 2/3 结构与 seed 断言 =="
if [ "${tables:-0}" -ge 67 ]; then ok "表数 $tables >= 67"; else bad "表数 $tables < 67"; fi
if [ "${indexes:-0}" -ge 120 ]; then ok "索引数 $indexes >= 120"; else bad "索引数 $indexes < 120"; fi
if [ "${triggers:-0}" -eq 25 ]; then ok "触发器数 $triggers == 25"; else bad "触发器数 $triggers != 25"; fi
if [ "${agents:-0}" -ge 17 ]; then ok "内置 agent $agents >= 17"; else bad "内置 agent $agents 过少"; fi
if [ "${rules:-0}" -ge 5 ]; then ok "内置草案规则 $rules >= 5"; else bad "内置草案规则 $rules 过少"; fi

# ---------- 3. 幂等：再跑一次 Open ----------
echo
echo "== 3/3 幂等性（重复 Open 同一库）=="
OUTPUT2="$(ARTEX_DB_PATH="$DB_PATH" go run ./cmd/dbinit 2>&1)"
if [ $? -ne 0 ]; then
  bad "第二次 Open 失败"
  echo "$OUTPUT2" | sed 's/^/       /'
else
  ok "第二次 Open 成功（schema + seed 幂等）"
  agents2=$(echo "$OUTPUT2" | sed -n 's/.*内置 agent \([0-9]\+\) 个.*/\1/p' | head -1)
  if [ "${agents2:-0}" = "${agents:-0}" ]; then
    ok "agent 数量未变（$agents2）"
  else
    bad "agent 数量变化: $agents -> $agents2"
  fi
fi

# ---------- 汇总 ----------
echo
echo "============================================================"
printf ' 结果: %d 通过, %d 失败\n' "$PASS" "$FAIL"
echo "============================================================"
[ "$FAIL" -eq 0 ] || exit 1
