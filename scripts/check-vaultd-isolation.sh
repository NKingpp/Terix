#!/usr/bin/env bash
# scripts/check-vaultd-isolation.sh —— 断言 vaultd 的 import 隔离。
#
# # 为什么这是「编译期」保证而不是「约定」
#
# ARCH §3.2.2 ①「编译期」要求：
#   agent 包与 exec 包之间无 import 边。
#   CI 加 `go list -deps` 断言。
#
# 理由：隔离如果是约定，那「有人不小心 import 了一下」就破了。
# 而 `go list -deps` 是机械检查 —— 它不会忘、不会累、不会妥协。
#
# # 检查什么
#
#   ① cmd/vaultd 的依赖里不得出现 agent / server / llmpool
#   ② exec/vaultd 的依赖里不得出现 agent / server / llmpool
#   ③ agent 的依赖里不得出现 exec（反向：决策进程不能碰执行层）
#
# # 为什么 ③ 也重要
#
# 如果 agent 能 import exec，那 LLM 工具层就能间接调用下单 —— 
# 那「执行能力不作为工具存在」这条保证就失效了。

set -uo pipefail

FAIL=0

# 目标包 → 禁止出现的依赖前缀
check() {
  local pkg="$1"
  shift
  local forbidden=("$@")

  echo "=== 检查 $pkg ==="

  local deps
  if ! deps=$(go list -deps "$pkg" 2>&1); then
    echo "  🔴 无法列出 $pkg 的依赖：$deps" >&2
    FAIL=$((FAIL + 1))
    return
  fi

  for bad in "${forbidden[@]}"; do
    if echo "$deps" | grep -qx "github.com/Autumn-27/artex/$bad"; then
      echo "  🔴 $pkg 依赖了 $bad —— 隔离被打破" >&2
      FAIL=$((FAIL + 1))
    else
      echo "  ✓ 不依赖 $bad"
    fi
  done
}

# ① vaultd 不得依赖决策侧
check ./cmd/vaultd agent server llmpool
# ② vaultd 的库部分同样
check ./exec/vaultd agent server llmpool
# ③ 决策侧不得依赖执行层（反向隔离）
check ./agent exec

echo ""
echo "=========================================="
if [ "$FAIL" -ne 0 ]; then
  echo " 失效：$FAIL 处 —— 隔离被打破" >&2
  echo "=========================================="
  echo "🔴 ARCH §3.2.2 ① 要求编译期隔离。修复方式：删掉那条 import，" >&2
  echo "   或者把需要的能力挪到不破坏隔离的位置。" >&2
  exit 1
fi
echo " 隔离完整（3 组断言全部通过）"
echo "=========================================="
echo "VAULTD_ISOLATION: fail=0"
exit 0
