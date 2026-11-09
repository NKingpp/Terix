#!/usr/bin/env bash
#
# check-metafor-unique.sh —— 断言「按 provider 查 providerMeta」只出现在
# credential-purpose.ts 里。
#
# # 为什么需要这个脚本
#
# marketProviders 里 provider 会重复（okx 有 trade/data 两项）。
# 任何 `providers.find((p) => p.provider === x)` 都会静默取第一个（trade 版），
# 于是用户编辑一条 data 凭据时，看到的字段布局是 trade 的
# （passphrase 标了必填星号）—— 不报错、不崩、类型也对。
#
# 所以只能靠「唯一入口」来防：所有 lookup 走 metaFor(providers, provider, purpose)。
# 而「唯一入口」如果只靠 grep 维护，就可能忘 —— 所以变成 CI 检查。
#
# # 为什么这是静态检查而不是测试
#
# web/ 目前没有测试框架（只有 biome lint + tsc）。
# 为了验证一处 10 行的 lookup 逻辑引入 @testing-library/react + jsdom
# 成本远高于收益。
#
# # 变异验证记录（2026-10-29）
#
# 按「检查脚本必须先证明它会报警」这条规则做过：
#   ① 在 web/src 下插一处 `providers.find((p) => p.provider === x)`
#   ② 脚本报错，exit=1，并指出该文件
#   ③ 删掉探针
#   ④ 脚本通过，exit=0
#
# 用法：
#   ./scripts/check-metafor-unique.sh

set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

ALLOWED="credential-purpose.ts"

# 匹配形态：
#   providers.find((p) => p.provider === ...)
#   providers.find((p) => p.provider === ... && p.purpose === ...)
#
# ⚠️ 第二种（带 purpose 的 find）在语义上是安全的，但我们仍不许 ——
#    因为「带 purpose 的 find」和 metaFor 是同一件事的两份实现，
#    而两份实现会分叉（其中一份忘了加 purpose 检查时，没人会发现）。
#    唯一入口的价值就在于「只有一份」。
HITS=$(grep -rnE '\.find\(\s*\(p\)\s*=>\s*p\.provider\s*===' \
  --include="*.ts" --include="*.tsx" web/src 2>/dev/null \
  | grep -v "$ALLOWED" || true)

if [ -n "$HITS" ]; then
  echo "🔴 发现 credential-purpose.ts 之外的「按裸 provider 查 providerMeta」："
  echo ""
  echo "$HITS"
  echo ""
  echo "   marketProviders 里 provider 重复（okx 有两个 purpose），"
  echo "   裸 provider lookup 会静默取第一个 → 字段布局用错。"
  echo "   那个失效不报错、不崩、类型也对。"
  echo ""
  echo "   修法：改用 metaFor(providers, provider, purpose)"
  exit 1
fi

echo "✅ provider lookup 唯一入口检查通过"
