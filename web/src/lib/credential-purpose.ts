// 凭据用途（purpose）的共享类型与查找工具。
//
// # 为什么单独一个文件
//
// 后端有两处同值域的字段：
//   - MarketCredential.kind      （这条凭据**实际**的用途，来自 DB 列 kind）
//   - MarketProviderMeta.purpose （这个 provider **支持**的用途，来自代码常量）
//
// 名字不同是有意的：前者是数据，后者是能力声明。改名统一会制造新问题 ——
// 若把 MarketCredential 的 JSON tag 改成 purpose，JSON 名就和 DB 列名不一致了。
//
// 但两者的**值域相同**，所以类型可以共用（本文件的 CredentialPurpose）。
// 而「按 provider+purpose 查元数据」这个动作必须集中在一处 ——
// 否则每个调用点都要记得带 purpose（漏了会静默取到 trade 版的字段布局）。

import type { MarketProviderMeta } from "@/lib/types";

// CredentialPurpose 定义在 types.ts（避免循环 import），这里 re-export。
export type { CredentialPurpose } from "@/lib/types";
import type { CredentialPurpose } from "@/lib/types";

/** purpose 的中文标签（Tab 标题、卡片徽章用）。 */
export const PURPOSE_LABELS: Record<CredentialPurpose, string> = {
  trade: "交易通道",
  data: "数据源",
};

/** purpose 的一句话说明（Tab 副标题用）。 */
export const PURPOSE_DESCRIPTIONS: Record<CredentialPurpose, string> = {
  trade: "用于下单。需要交易权限的 API Key 或密钥对。",
  data: "用于行情数据。OKX 公开行情免凭证，填 key 只为提高限频配额。",
};

/**
 * metaFor 按 (provider, purpose) 查 provider 元数据。
 *
 * # 为什么必须带 purpose
 *
 * marketProviders 里 provider 会重复（okx 有 trade 和 data 两项）。
 * 只按 provider 查会取到数组里的第一个 —— 而那是 kind='trade' 那项，
 * 于是用户编辑一条 data 凭据时，看到的字段布局是 trade 的（passphrase 标了必填星号）。
 *
 * 那个错误不会报错、不会崩 —— 只是「表单显示的字段不对」。
 *
 * # 这是**唯一**允许按 provider 取元数据的入口
 *
 * 别处不要直接写 `providers.find((p) => p.provider === x)` ——
 * 包括「顺手带上 purpose 的 find」也不要：那和本函数是同一件事的两份实现，
 * 而两份实现会分叉（其中一份忘了 purpose 检查时，没人会发现）。
 *
 * 唯一性由 `scripts/check-metafor-unique.sh` 在 CI 层保证
 * （该脚本已做过变异验证：插一处裸 lookup → exit=1；删掉 → exit=0）。
 *
 * # 为什么用 filter + hits[0] 而不是 find
 *
 * `find` 的语义就是「取第一个」—— 它主动隐藏了「还有第二个」这个事实。
 * `filter` 的语义是「找出全部」，然后由**我们**决定取第一个，
 * 而那个决定旁边就有一条 `if (hits.length > 1)` 的检查。
 *
 * 判据：选 API 时选「暴露全部」的那个，然后自己决定取哪个 ——
 * 而不是选「已经帮你取好一个」的那个。
 */
export function metaFor(
  providers: MarketProviderMeta[],
  provider: string,
  purpose: CredentialPurpose,
): MarketProviderMeta | undefined {
  const hits = providers.filter((p) => p.provider === provider && p.purpose === purpose);

  // 开发期检查重复项。
  //
  // # 为什么前端也要检查（后端已有 TestMarketProvidersUniqueProviderPurpose）
  //
  // 后端的唯一性保证在**后端**；而前端如果拿到脏数据（旧版后端、mock 数据、
  // 手动构造的测试 fixture），`find` 会静默取第一个 —— 于是字段布局用错，
  // 而那个错误不报错、不崩，只是「表单显示的字段不对」。
  //
  // 这条 console.error 不修问题，但让问题可见。
  if (process.env.NODE_ENV !== "production" && hits.length > 1) {
    console.error(
      `[credentials] (provider=${provider}, purpose=${purpose}) 匹配到 ${hits.length} 项，应唯一 —— ` +
        `find 会静默取第一个，字段布局可能不对`,
    );
  }

  return hits[0];
}

/** provider+purpose 的复合 key（React key / Record 索引 / PROVIDER_META 查找用）。 */
export function purposeKey(provider: string, purpose: CredentialPurpose): string {
  return `${provider}:${purpose}`;
}
