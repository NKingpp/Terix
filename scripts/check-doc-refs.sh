#!/usr/bin/env bash
# scripts/check-doc-refs.sh —— 检查代码注释里的文档引用是否有效。
#
# # 为什么需要这个脚本
#
# 注释里的路径引用**不参与编译**。文件改名/移动后，引用会静默失效 ——
# 没有编译错误、没有测试失败，只有「读者照着找不到」。
#
# 实测（2026-10-28）：ARTEX/ 里有 8 处 `docs/quantex/xxx.md` 引用，
# 而 `ARTEX/docs/quantex/` **从来不存在**（文档在上一级目录）。
# 失效了很久，没人发现 —— 因为没有任何机制会检查它。
#
# # 检查两类引用
#
#   ① 路径形式：`docs/quantex/ARCH-W2-W5.md` —— 检查文件存在
#   ② 语义形式：`《ARCH-W2-W5》§2.9.4` —— 检查 docs 仓库里有同名文档
#
# # 为什么两类都要
#
# 语义形式（《》+ 章节号）是推荐写法 —— 它不随目录结构失效。
# 但它依赖「文档名正确」，所以仍需检查文档是否真的存在。
#
# # 用法
#
#   ARTEX_DOCS_DIR=../docs/quantex ./scripts/check-doc-refs.sh
#
# 退出码：0 = 全部有效；1 = 有失效引用
set -uo pipefail

# docs 仓库的 quantex 目录。默认按「ARTEX/ 与 docs/ 同级」推断。
DOCS_DIR="${ARTEX_DOCS_DIR:-../docs/quantex}"
SCAN_DIR="${ARTEX_SCAN_DIR:-.}"

if [ ! -d "$DOCS_DIR" ]; then
  echo "⚠️ 找不到 docs 目录：$DOCS_DIR" >&2
  echo "   用 ARTEX_DOCS_DIR 指定（例：ARTEX_DOCS_DIR=../docs/quantex）" >&2
  exit 1
fi

FAIL=0
CHECKED_PATHS=0
CHECKED_NAMES=0

# ---- ① 路径形式：docs/quantex/xxx.md ----
#
# 从代码注释里抓出所有 `docs/quantex/...md`。
# 排除：docs 仓库自身（它就在正确位置）、以及带「docs 仓库」限定的说明句。
echo "=== ① 路径形式的引用 ==="
PATHS=$(grep -rhoE 'docs/quantex/[A-Za-z0-9._-]+\.md' \
  --include=*.go --include=*.md \
  --exclude-dir=docs \
  --exclude-dir=.git \
  "$SCAN_DIR" 2>/dev/null | sort -u)

if [ -z "$PATHS" ]; then
  echo "  （无路径形式的引用 —— 全部改成了《》语义形式，这是期望状态）"
else
  while IFS= read -r rel; do
    [ -z "$rel" ] && continue
    CHECKED_PATHS=$((CHECKED_PATHS + 1))
    base=$(basename "$rel")
    # 路径形式：从 ARTEX/ 看它应该存在，或者从 docs 仓库看它存在。
    if [ -f "$rel" ] || [ -f "$DOCS_DIR/$base" ]; then
      echo "  ✓ $rel"
    else
      echo "  🔴 $rel —— 文件不存在（docs 仓库里也没有 $base）" >&2
      FAIL=$((FAIL + 1))
    fi
  done <<< "$PATHS"
fi

# ---- ② 语义形式：《文档名》 ----
#
# 《ARCH-W2-W5》 这类引用：检查 docs 里有 ARCH-W2-W5.md。
echo ""
echo "=== ② 语义形式《》的引用 ==="
NAMES=$(grep -rhoE '《[A-Za-z0-9._-]+》' \
  --include=*.go --include=*.md \
  --exclude-dir=docs \
  --exclude-dir=.git \
  "$SCAN_DIR" 2>/dev/null | sed 's/《//; s/》//' | sort -u)

if [ -z "$NAMES" ]; then
  echo "  （无语义形式引用）"
else
  while IFS= read -r name; do
    [ -z "$name" ] && continue
    CHECKED_NAMES=$((CHECKED_NAMES + 1))
    if [ -f "$DOCS_DIR/$name.md" ]; then
      echo "  ✓ 《$name》 → $DOCS_DIR/$name.md"
    else
      echo "  🔴 《$name》 —— docs 里没有 $name.md" >&2
      FAIL=$((FAIL + 1))
    fi
  done <<< "$NAMES"
fi

# ---- ③ 章节号抽查（可选）----
#
# 只对语义引用检查「§X.Y 是否出现在文档里」。
# 这一步能抓住「文档改了章节号但引用没跟着改」。
echo ""
echo "=== ③ 章节号抽查 ==="
SECTION_FAIL=0
while IFS= read -r hit; do
  [ -z "$hit" ] && continue
  name=$(echo "$hit" | sed -n 's/.*《\([A-Za-z0-9._-]*\)》.*/\1/p')
  sec=$(echo "$hit" | sed -n 's/.*§\([0-9][0-9.]*\).*/\1/p')
  [ -z "$name" ] || [ -z "$sec" ] && continue
  doc="$DOCS_DIR/$name.md"
  [ -f "$doc" ] || continue
  if grep -qF "$sec" "$doc"; then
    echo "  ✓ 《$name》§$sec"
  else
    echo "  ⚠️ 《$name》§$sec —— 文档里没找到这个章节号（可能是笔误或文档已改）" >&2
    SECTION_FAIL=$((SECTION_FAIL + 1))
  fi
done < <(grep -rhoE '《[A-Za-z0-9._-]+》§[0-9][0-9.]*' \
  --include=*.go --include=*.md --exclude-dir=docs --exclude-dir=.git \
  "$SCAN_DIR" 2>/dev/null | sort -u)

if [ "$SECTION_FAIL" -eq 0 ]; then
  echo "  （章节号全部命中，或没有可查的）"
fi

# ---- 汇总 ----
echo ""
echo "=========================================="
echo " 路径引用 $CHECKED_PATHS 处，语义引用 $CHECKED_NAMES 处"
echo " 失效：$FAIL 处"
# 章节号警告单独统计 —— 「不阻断」的前提是「有人会看到」。
# 如果警告淹没在输出里，它和没有一样。
echo " 章节号警告：$SECTION_FAIL 处（不阻断，但建议修复）"
echo "=========================================="

# 机器可读的一行，供 CI 记进构建产物看趋势。
#
# 「警告不阻断」的代价是它可能长期累积而不被处理。
# 把它变成一个可追踪的数字，才能发现「文档在重构但引用没跟上」。
echo "DOC_REFS: fail=$FAIL section_warn=$SECTION_FAIL paths=$CHECKED_PATHS names=$CHECKED_NAMES"

if [ "$FAIL" -ne 0 ]; then
  echo "🔴 有失效的文档引用 —— 读者会照着找不到。" >&2
  echo "   修法：改成语义形式（见下），它不随目录结构失效。" >&2
  echo "     改前：见 docs/quantex/ARCH-W2-W5.md §2.9.4" >&2
  echo "     改后：见《ARCH-W2-W5》§2.9.4（docs 仓库：quantex/ARCH-W2-W5.md）" >&2
  exit 1
fi

if [ "$SECTION_FAIL" -ne 0 ]; then
  echo "⚠️ 章节号有 $SECTION_FAIL 处对不上（不算致命，但建议核对）。"
  exit 0  # 章节号漂移不阻断，只提示
fi

# ===========================================================================
# 检查：代码里引用的 KNOWN-LIMITATIONS L<N> 条目是否真实存在
# ===========================================================================
#
# # 为什么需要这一条
#
# 实测（2026-10-28）：`agent/pipeline.go` 有两处写「（见 KNOWN-LIMITATIONS L5）」，
# 而 L5 条目**根本不存在**（文档里 L4 直接跳到 L6）。
#
# 这比「失效的引用」更隐蔽：
#   失效的引用 —— 指向了错的地方（读者去了，发现不对）
#   引用不存在 —— 指向了不存在的地方（读者以为「已经有文档」，根本不去）
#
# 后者制造了「已经解释过」的假象，而实际上什么都没解释。
echo ""
echo "=== 检查代码引用的 L 条目 ==="
L_REF_FAIL=0
LIMITATIONS_MD="${ARTEX_DOCS_DIR:-../docs/quantex}/KNOWN-LIMITATIONS.md"

if [ ! -f "$LIMITATIONS_MD" ]; then
  echo "  ⚠️ 找不到 $LIMITATIONS_MD，跳过 L 条目检查"
else
  # 收集所有被引用的 L 编号（去重）
  REFS=$(grep -rhoE "KNOWN-LIMITATIONS[[:space:]]+L[0-9]+" --include=*.go --include=*.md .. 2>/dev/null     | grep -oE "L[0-9]+" | sort -u -V)

  if [ -z "$REFS" ]; then
    echo "  没有代码引用 L 条目"
  else
    for n in $REFS; do
      if grep -qE "^## ${n}\." "$LIMITATIONS_MD"; then
        echo "  ✓ $n"
      else
        echo "  🔴 $n 被引用，但 $LIMITATIONS_MD 里没有这一条" >&2
        L_REF_FAIL=$((L_REF_FAIL + 1))
      fi
    done
  fi
fi

if [ "$L_REF_FAIL" -ne 0 ]; then
  echo ""
  echo "🔴 有 $L_REF_FAIL 处引用了不存在的 L 条目。" >&2
  echo "   修法：补写那条 L 条目，或删掉引用。" >&2
  echo "   不要留着 —— 它会让读者以为「已经有解释」，从而不去查。" >&2
  exit 1
fi

echo "✅ 全部文档引用有效。"
