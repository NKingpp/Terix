#!/usr/bin/env bash
# 开发环境一键启动（后端 + akshare sidecar + 前端）。
#
# # 与 start.sh 的分工
#
# start.sh 是**生产守护脚本**：它只管 `./artex` 二进制的进程管理
# （退出码 75 = 自更新重启，其他 = 退避重拉），不碰 Docker / 前端。
#
# 本脚本是**开发启动器**：把整条依赖链按正确顺序拉起来，只跑一次不守护。
# 两者职责不重叠，可以并存。
#
# # 为什么需要它
#
# 三个服务有启动顺序依赖（后端要先起来，前端才能取到数据）。
# SQLite 已经消除了"数据库服务没起来"这一类失败，
# 但仍要等后端真正就绪再拉起前端。
#
# # 顺序是硬约束
#
# 数据库（SQLite，本地文件）→ 三个业务服务。
# 从 PG 切到 SQLite 后不再需要 Docker / pg_isready 那一段。
#
# # 数据库为什么不需要"等就绪"
#
# SQLite 是本地文件，没有"服务在启动中"这个状态。
# 唯一的前置条件是库文件可写，由后端首次 db.Open() 自己保证
# （会建目录、应用 schema、seed）。

set -u

ROOT="C:/Users/Administrator/WorkBuddy/亏钱/ARTEX"
DB_PATH="C:/Users/Administrator/WorkBuddy/亏钱/ARTEX/data/quantex.db"
PROXY="http://127.0.0.1:10808"
NODE="C:/Users/Administrator/.workbuddy/binaries/node/versions/22.22.2-6/node.exe"
PY="C:/Users/Administrator/WorkBuddy/亏钱/.venv-akshare/Scripts/python.exe"

say() { printf '\n== %s ==\n' "$*"; }
ok()  { printf '  [OK] %s\n' "$*"; }
bad() { printf '  [FAIL] %s\n' "$*"; }

port_up() { netstat -ano 2>/dev/null | grep ":$1 " | grep -q LISTENING; }
http_code() { curl -s -m "${2:-3}" -o /dev/null -w '%{http_code}' "$1" 2>/dev/null; }

# ---------- 1. 数据库（SQLite） ----------
say "SQLite ($DB_PATH)"
mkdir -p "$(dirname "$DB_PATH")" || { bad "无法创建数据目录，中止"; exit 1; }
ok "数据目录就绪: $(dirname "$DB_PATH")"
if [ -f "$DB_PATH" ]; then
  ok "库文件已存在: $(du -h "$DB_PATH" 2>/dev/null | cut -f1)"
else
  ok "库文件不存在（后端首次启动会创建并 seed）"
fi

# ---------- 3. 后端 ----------
say "后端 API (:8787)"
if port_up 8787; then
  ok "已在运行"
else
  # 代理是必需的：OKX 与妙想域名在本机直连被墙（实测 dial tcp 超时）。
  ( cd "$ROOT" && ARTEX_DB_PATH="$DB_PATH" HTTPS_PROXY="$PROXY" HTTP_PROXY="$PROXY" \
      ./artex-dev.exe > /tmp/be.log 2>&1 & )
  for i in $(seq 1 30); do
    [ "$(http_code http://127.0.0.1:8787/api/health 2)" = "200" ] && { ok "健康检查通过"; break; }
    sleep 2
  done
  port_up 8787 || { bad "后端未起来"; tail -5 /tmp/be.log; }
fi

# ---------- 4. akshare sidecar ----------
say "akshare sidecar (:8791)"
if port_up 8791; then
  ok "已在运行"
else
  ( cd "$ROOT/sidecars/akshare" && "$PY" server.py > /tmp/sc.log 2>&1 & )
  for i in $(seq 1 20); do
    [ "$(http_code http://127.0.0.1:8791/health 3)" = "200" ] && { ok "健康检查通过"; break; }
    sleep 3
  done
  port_up 8791 || { bad "sidecar 未起来"; tail -5 /tmp/sc.log; }
fi

# ---------- 5. 前端 ----------
say "前端 (:3000)"
if port_up 3000; then
  ok "已在运行"
else
  ( cd "$ROOT/web" && "$NODE" node_modules/next/dist/bin/next dev -p 3000 > /tmp/fe.log 2>&1 & )
  for i in $(seq 1 40); do
    [ "$(http_code http://127.0.0.1:3000/login 3)" = "200" ] && { ok "Ready（约 $((i*3))s）"; break; }
    sleep 3
  done
  port_up 3000 || { bad "前端未起来"; tail -5 /tmp/fe.log; }
fi

# ---------- 6. 汇总 ----------
say "状态汇总"
ALL_OK=1
for p in 8787 3000 8791 5433; do
  port_up "$p" && ok "端口 $p" || { bad "端口 $p DOWN"; ALL_OK=0; }
done

if [ "$ALL_OK" = "1" ]; then
  echo ""
  echo "全部就绪  http://localhost:3000/login  账号 ARTEX / terix2026"
  echo "日志: /tmp/be.log  /tmp/fe.log  /tmp/sc.log"
  echo ""
else
  echo ""
  echo "有服务未起来 —— 按上面的日志路径排查"
  echo ""
  exit 1
fi
