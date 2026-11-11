#!/bin/sh
# ARTEX 守护启动脚本（Linux / macOS / Docker ENTRYPOINT）
#
# 用法：
#   ./start.sh                       前台运行（Ctrl-C 停止）
#   nohup ./start.sh >artex.log 2>&1 &   后台常驻
#   ./start.sh -addr :9000           额外参数原样透传给 artex
#
# 它只做一件事：把 artex 跑起来，进程退出后按退出码决定要不要再拉起。
#
#   0      用户正常停止        → 退出循环
#   75     程序请求重启        → 立刻重跑（页面点了"一键更新"或"回滚"）
#   其他   崩溃                → 退避后重跑（1→2→4…最多 60 秒）
#
# 刻意不在这里做下载、SHA256 校验或换装：那些逻辑在 sh 和 bat 上要写两套，
# 而它们恰恰是最不能出错的一环——一旦换上跑不起来的二进制，本脚本会忠实地
# 反复拉起它，用户只能上机器手工救。所以校验/换装全部留在 Go 里（selfupdate 包），
# 由 artex 自己在启动时完成，脚本保持傻瓜化。
set -u

cd "$(dirname "$0")" || exit 1

# 项目自带工具优先进入 PATH：后端 exec.LookPath("okx") 需要找到
# bin/okx（sidecars/okx 里的 OKX CLI），不依赖机器全局安装。
PATH="$PWD/bin:$PATH"
export PATH

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] 找不到可执行文件 $BIN" >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
akshare_pid=0
stopping=0

# ── 内置 akshare sidecar ─────────────────────────────────────────────
#
# 有 server.py 就拉起；没有（旧镜像/精简部署）静默跳过，不影响 artex 启动。
# 缺了它非加密标的全部报 SIDECAR_UNREACHABLE（crypto 走 OKX，不受影响）。
#
# 监听 127.0.0.1:8791：现在与 artex 同容器，不需要对外暴露，也不该对外暴露。
# AKSHARE_SIDECAR_ADDR 由 compose 传给 artex 时也必须是 127.0.0.1:8791，
# 两侧语义因此一致 —— 合并部署消掉了原来「一侧目标一侧监听」的坑。
AKSHARE_DIR="${AKSHARE_DIR:-./akshare}"
AKSHARE_LISTEN="${AKSHARE_LISTEN_ADDR:-127.0.0.1:8791}"

start_akshare() {
	[ -f "$AKSHARE_DIR/server.py" ] || return 0
	[ -x "$(command -v python 2>/dev/null)" ] || {
		echo "[akshare] 未找到 python，跳过 sidecar（非加密行情将不可用）" >&2
		return 0
	}
	AKSHARE_SIDECAR_ADDR="$AKSHARE_LISTEN" python "$AKSHARE_DIR/server.py" &
	akshare_pid=$!
	echo "[akshare] sidecar 已启动 (pid=$akshare_pid, listen=$AKSHARE_LISTEN)"
}

# 转发停止信号给两个子进程。
#
# Docker 下这是必需的：docker stop 只把 SIGTERM 发给 PID 1（也就是本脚本），
# 不会发给子进程。不转发的话 artex 收不到信号、做不了优雅关闭，10 秒后被 SIGKILL
# 硬杀，正在跑的任务直接断在半路。
forward() {
	stopping=1
	if [ "$child" -ne 0 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
	if [ "$akshare_pid" -ne 0 ]; then
		kill -TERM "$akshare_pid" 2>/dev/null || true
	fi
}
trap forward INT TERM

start_akshare

delay=1
while :; do
	"$BIN" "$@" &
	child=$!

	# 信号会打断 wait 并让它返回 >128。此时子进程其实还在做优雅关闭，
	# 必须再 wait 一次才能拿到它真正的退出码。
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[artex] 已停止"
		exit 0
	fi

	# sidecar 死了要拉起来：它挂了不会让 artex 退出，但非加密行情会全部
	# SIDECAR_UNREACHABLE —— 一个安静的能力退化，比直接崩更难查。
	# 用 kill -0 探活（不发信号，只看进程在不在）。
	if [ "$akshare_pid" -ne 0 ] && ! kill -0 "$akshare_pid" 2>/dev/null; then
		echo "[akshare] sidecar 已退出，重新拉起" >&2
		start_akshare
	fi

	case "$code" in
		0)
			echo "[artex] 正常退出"
			exit 0
			;;
		"$RESTART_CODE")
			# 更新/回滚已就绪：重跑后 artex 会在启动时完成换装（见 selfupdate.Bootstrap）。
			echo "[artex] 请求重启（应用新版本）…"
			delay=1
			;;
		*)
			echo "[artex] 异常退出 (code=$code)，${delay}s 后重启" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
