// 凭证表单的字段规格组装 + 验证状态推导。
//
// # 这个文件的核心约束
//
// **判断「哪些字段算凭据」的知识完全来自后端 meta，这里一个字都不硬编码。**
//
// 依据 `lib/types.ts` 的既有约定：
//   「secret_keys 由后端按渠道类型给出，前端据此渲染密码框与『留空即不改』
//     提示，不硬编码任何渠道知识」
//
// 为什么凭证场景比通知渠道更不能硬编码：
//   notify 漂移 → 字段渲染成 text（体验问题，用户填的 webhook 通常不是高价值凭据）
//   凭证漂移   → API Secret / PEM 私钥明文显示在界面上（安全问题，截屏录屏都会带走）
//   同一类设计缺陷，危害差两个数量级。
//
// 所以本文件的 `PROVIDER_META` **只放展示文案**（label / description / placeholder /
// help），`kind` / `required` / `multiline` 全部从后端 meta 的 fields[] 读。

import type { MarketCredential, MarketCredentialField, MarketProviderMeta } from "@/lib/types";

/** 字段在表单里的最终规格（后端 meta + 前端文案合并结果）。 */
export interface FieldSpec {
  key: string;
  label: string;
  /** 是否脱敏（渲染 type="password" 或用 CSS 遮罩多行）。checkbox = 勾选框。 */
  kind: "text" | "password" | "checkbox";
  /**
   * 预填值。
   *
   * # 为什么必须在这里而不是在页面里写死 map
   *
   * 「用户看到的值」与「实际提交的值」必须同源。若预填写在前端常量里、
   * 后端又有一份 fallback，两边漂移就会出现「页面显示 A、提交空、后端用 B」。
   * 后端 meta 的 fields[].default 是唯一来源，前端只负责显示。
   */
  defaultValue?: string;
  /**
   * 是否多行。
   *
   * 与 kind 正交 —— 四个组合都合法：
   *   text + 单行     → <Input type="text">
   *   text + 多行     → <Textarea>
   *   password + 单行 → <Input type="password">
   *   password + 多行 → <Textarea> + CSS -webkit-text-security（或降级告警）
   *
   * 为什么不能合成一个枚举：fosun 的 PEM 私钥同时需要「多行」和「不回显」。
   * 若只有 text | password | textarea 三选一，就表达不出这个组合，
   * 只能退回「textarea 明文」（泄漏）或「password 单行」（PEM 粘不进去）。
   */
  multiline: boolean;
  /** 新建时必填（来自后端 required_fields）。 */
  required: boolean;
  /** 提交时是否用 normalizePEM（去首尾 + CRLF→LF + 保留中间换行）。 */
  preserveWhitespace: boolean;
  /** 值存在哪（secret / extra / column）。决定提交时放哪个键。 */
  storage: MarketCredentialField["storage"];
  placeholder?: string;
  help?: string;
}

/**
 * 前端的展示层文案。
 *
 * ⚠️ 刻意不包含：
 *   - `secret`      —— 由后端 secret_fields 决定
 *   - `required`    —— 由后端 required_fields 决定
 *   - `multiline`   —— 由后端 fields[].multiline 决定
 *   - `verifiable`  —— 由后端 provider 级 verifiable 决定
 *
 * 加了任何一个就是给「硬编码退路」开门，而凭证硬编码的后果是密钥明文上屏。
 * 见设计文档 §10「已知偏离已裁定」与 §8 Q9。
 */
export interface ProviderCopy {
  /** 中文展示名（后端也给了 label，这里只是兜底文案） */
  label: string;
  /** 卡片/表单顶部的一句说明 */
  description: string;
  /** 「去哪里拿这几个字段」的指引 */
  docHint?: string;
  /**
   * 字段的文案。key 是后端给的字段名。
   *
   * 前端多出的条目会被忽略（后端删了字段），
   * 后端有而前端缺的会退化成「label = key 名」（难看但可用，且报 dev 警告）。
   */
  fieldHints: Record<string, { label?: string; placeholder?: string; help?: string }>;
}

/**
 * 只含 A 类通道（需用户提交凭证）。
 *
 * B 类（ifind / tdx）刻意不在这里 —— 它们走 MCP Connector 或本地客户端，
 * 没有可填字段。放进本页只会让用户看到一张空表单或一个「已启用」开关，
 * 然后问「我要填什么」。它们的连通状态属于「数据源健康」，见设计文档 §2bis.3。
 *
 * **若未来 ifind 改成需要用户填 token**，它会从 B 类移到 A 类，那时才加这里。
 * 这个移动需要显式决策，不是「顺手加上」。
 */
// PROVIDER_META 是前端展示文案（label / description / docHint / fieldHints）。
//
// # 为什么 key 是「provider:purpose」两层结构
//
// 后端 marketProviders 里同一个 provider 会出现两次（purpose='trade' / 'data'），
// 因为它们的凭据用途完全不同：
//   - trade：下单通道，要 API Key 或 PEM 密钥对
//   - data ：行情数据源，OKX 公开行情免凭证
//
// 而两者的**文案大部分可以共用**（同一个 provider 的业务背景一样），
// 只有「用途描述」需要区分。所以用嵌套：外层 provider，内层 purpose。
//
// ⚠️ lookup 必须同时给 provider 和 purpose ——— 只给 provider 会取到 trade 那项，
// 于是编辑 data 凭据时看到的是 trade 的字段布局（passphrase 标了必填星号）。
// 用 lib/credential-purpose.ts 的 metaFor() + purposeCopyFor()。
export const PROVIDER_META: Record<string, Partial<Record<"trade" | "data", ProviderCopy>>> = {
  okx: {
    trade: {
      label: "OKX（交易）",
      description: "加密货币交易所实盘交易通道。下单需要「读取 + 交易」权限的 API Key 与 Passphrase。",
      docHint:
        "在 OKX → 账户 → API 页面创建 API Key，权限勾选「读取」+「交易」，并设置 Passphrase。",
      fieldHints: {
        api_key: { label: "API Key", placeholder: "例如 a1b2c3d4-..." },
        api_secret: { label: "API Secret", help: "只在创建时显示一次，请当时保存" },
        passphrase: {
          label: "Passphrase",
          help: "创建 API Key 时你自己设置的口令，不是登录密码",
        },
      },
    },
    data: {
      label: "OKX（行情）",
      description: "OKX 行情数据源。公开接口免凭证，填 API Key 可提高限频配额（可选）。",
      docHint:
        "可选：在 OKX → 账户 → API 页面创建只勾「读取」权限的 API Key，用来提高行情接口的频率上限。",
      fieldHints: {
        api_key: { label: "API Key（可选）", placeholder: "留空则用公开接口（有限频）" },
        api_secret: { label: "API Secret（可选）" },
        passphrase: { label: "Passphrase（可选）", help: "创建 API Key 时你自己设置的口令" },
      },
    },
  },
  // ---- 数据源 MCP（2026-10-31 新增 6 家）----
  //
  // # 为什么这些只有 data 一个 purpose
  //
  // 它们只是数据查询通道，不涉及下单 —— 没有「交易版」。而 PROVIDER_META
  // 是 Partial<Record<"trade"|"data">>，所以只写 data 是合法的。
  //
  // # 为什么 auth_token 的文案强调「可留空」
  //
  // 这 6 个 MCP endpoint 本身可能允许匿名访问（WorkBuddy 侧就是这么用的）。
  // 后端对「没 token」的处理是「发一个不带 Authorization 的请求试试」，
  // 不是报错。所以必填/可选的差异必须体现在文案里 ——
  // 让用户看到「必填」却不知道怎么填，比留空更糟。
  ifind: {
    data: {
      label: "同花顺 iFinD",
      description: "同花顺金融数据 MCP：A 股/港股/美股的行情、财务、公告、研报。",
      docHint: "在 iFinD 开放平台申请授权 Token。留空则尝试匿名连接（多数接口会拒绝）。",
      fieldHints: {
        auth_token: { label: "授权 Token", placeholder: "iFinD 开放平台下发" },
        endpoint: { label: "MCP Endpoint", help: "一般不用改；自建代理时填自己的地址" },
      },
    },
  },
  fosun_data: {
    data: {
      label: "星财富（行情 MCP）",
      description: "复星星财富行情 MCP：港 A 股行情与资讯。与「星财富（交易）」是两套凭证。",
      docHint: "网关可能允许匿名访问，此时 Token 留空即可。需要更高配额时填授权 Token。",
      fieldHints: {
        auth_token: { label: "授权 Token（可选）", placeholder: "留空则匿名连接" },
        endpoint: { label: "MCP Endpoint" },
      },
    },
  },
  tdx: {
    data: {
      label: "通达信",
      description: "通达信行情 MCP：A 股实时行情与历史 K 线。",
      docHint: "公开端点通常免凭证。若你的通达信部署要求 token，在此填入。",
      fieldHints: {
        auth_token: { label: "授权 Token（可选）", placeholder: "公开端点可留空" },
        endpoint: { label: "MCP Endpoint" },
      },
    },
  },
  neodata: {
    data: {
      label: "腾讯 NeoData",
      description: "腾讯金融科技 NeoData：宏观、行业、公司基本面与另类数据。",
      docHint: "在腾讯云 NeoData 控制台申请 API Token。",
      fieldHints: {
        auth_token: { label: "授权 Token", placeholder: "NeoData 控制台下发" },
        endpoint: { label: "MCP Endpoint" },
      },
    },
  },
  kuaicha: {
    data: {
      label: "同花顺快查",
      description: "同花顺快查：企业工商信息、股权结构、司法涉诉等基本面数据。",
      docHint: "在快查开放平台申请 API Token。",
      fieldHints: {
        auth_token: { label: "授权 Token", placeholder: "快查开放平台下发" },
        endpoint: { label: "MCP Endpoint" },
      },
    },
  },
  westock: {
    data: {
      label: "腾讯自选股",
      description: "腾讯自选股 MCP：行情、自选组合与资讯。",
      docHint: "在腾讯自选股开放平台申请 API Token。",
      fieldHints: {
        auth_token: { label: "授权 Token", placeholder: "自选股开放平台下发" },
        endpoint: { label: "MCP Endpoint" },
      },
    },
  },
  // 东方财富妙想：REST API（不是 MCP），Header 是 `apikey` 而不是 Bearer。
  //
  // # 为什么文案要点明「自然语言查询」
  //
  // 它的接口是 `{"toolQuery":"贵州茅台最新价"}` —— 传的是问句，不是
  // symbol/field 结构化参数。用户若按「填个股票代码」的直觉理解这个渠道，
  // 会以为配错了 key（其实 key 没问题，是调用方式不同）。
  mx: {
    data: {
      label: "东方财富妙想",
      description: "东财权威数据库：行情、财务、资金流，以及资讯搜索（自然语言问句）。",
      docHint: "在妙想 Skills 页面（dl.dfcfs.com/m/itc4）获取 API Key。该渠道用问句查询，如「贵州茅台最近5个交易日收盘价」。",
      fieldHints: {
        auth_token: { label: "API Key", placeholder: "妙想 Skills 页面下发" },
        endpoint: { label: "API Base URL", help: "一般不用改；自建代理时填自己的地址" },
      },
    },
  },
  fosun: {
    trade: {
      label: "星财富（交易）",
      description: "港 A 股交易通道。下单需要开通时下发的两个 PEM 文件与账户 ID。",
      docHint: "凭证来自复星 OpenAPI 开通时下发的两个 PEM 文件。",
      fieldHints: {
        server_public_key: { label: "服务端公钥（PEM）" },
        client_private_key: { label: "客户端私钥（PEM）", help: "只在开通时下发一次，丢失需重新申请" },
        apply_account_id: { label: "账户 ID", placeholder: "复星分配的下单账户 ID" },
      },
    },
    data: {
      label: "星财富（行情）",
      description: "港 A 股行情数据源。需要开通时下发的两个 PEM 文件。",
      docHint: "凭证来自复星 OpenAPI 开通时下发的两个 PEM 文件。",
      fieldHints: {
        server_public_key: { label: "服务端公钥（PEM）" },
        client_private_key: { label: "客户端私钥（PEM）", help: "只在开通时下发一次，丢失需重新申请" },
      },
    },
  },
};

/**
 * purposeCopyFor 按 (provider, purpose) 取前端文案。
 *
 * 与 credential-purpose.ts 的 metaFor 是同构的 —— 一个查后端 meta（字段规格），
 * 一个查前端文案。拆成两个是因为它们来源不同（网络 vs 代码常量），
 * 而它们都必须带 purpose。
 */
export function purposeCopyFor(
  provider: string,
  purpose: "trade" | "data",
): ProviderCopy | undefined {
  return PROVIDER_META[provider]?.[purpose];
}

/**
 * provider 的展示名；后端没给且前端没配时退化成 provider 本身。
 *
 * ⚠️ purpose 必须传 —— 同一个 provider 的两个用途 label 不同
 * （「OKX（交易）」vs「OKX（行情）」）。不传会退化成后端的 label，
 * 而如果后端也没给，就变成裸 provider 名（用户看不出是哪种用途）。
 */
export function providerLabel(
  provider: string,
  purpose: "trade" | "data" | undefined,
  backendLabel?: string,
): string {
  if (purpose) {
    const copy = PROVIDER_META[provider]?.[purpose];
    if (copy?.label) return copy.label;
  }
  return backendLabel || provider;
}

/**
 * 从后端 meta 拼出最终 FieldSpec 列表。
 *
 * **这是唯一的组装点** —— 页面与表单都只消费它的输出，不再各自判断
 * 「哪些是密码框」。
 *
 * # 两条设计约束
 *
 * 1. **字段集合来自后端，不来自前端。**
 *    顺序按后端 fields[] 的顺序（后端按业务顺序给）。
 *    为什么不用前端 hints 的 key 顺序：后端新增一个字段时，前端若不知道该
 *    字段存在，表单就不渲染它，用户永远填不了。前端 hints 缺失只应导致
 *    「文案是 key 名」（难看但可用），不应导致「字段消失」。
 *
 * 2. **前端 hints 多出的条目被忽略。**
 *    后端删了一个字段，前端 hints 里残留 —— 忽略即可。
 *    反向（后端有前端无）才需要警告。
 */
export function buildFieldSpecs(pm: MarketProviderMeta, copy: ProviderCopy | undefined): FieldSpec[] {
  // 兼容两种后端形态：
  //   - 新形态：fields[] 每项自带 kind/required/multiline/storage
  //   - 旧形态：只有 secret_fields / required_fields 两个字符串数组
  // 后者用 secret_fields 推导 kind，multiline 一律 false。
  // 兼容旧形态：后端若只给两个字符串数组而没有 fields[]，
  // 就按 secret_fields 推导 kind / storage。这段是**防御性**的 ——
  // 当前后端（market_credentials_meta.go）总是给完整的 fields[]，
  // 但字段缺失时静默渲染出一个空表单比退化成「label 用 key 名」更糟。
  const secretSet = new Set(pm.secret_fields);
  const requiredSet = new Set(pm.required_fields);
  const src: MarketCredentialField[] =
    pm.fields.length > 0
      ? pm.fields
      : Array.from(new Set([...pm.secret_fields, ...pm.required_fields])).map((k) => ({
          key: k,
          label: k,
          kind: secretSet.has(k) ? ("password" as const) : ("text" as const),
          multiline: false,
          preserve_whitespace: false,
          required: requiredSet.has(k),
          storage: secretSet.has(k) ? ("secret" as const) : ("extra" as const),
        }));

  return src.map((f) => {
    const hint = copy?.fieldHints[f.key];
    if (!hint && process.env.NODE_ENV !== "production") {
      // 后端有、前端没配文案：可用但难看。报出来让开发补文案。
      console.warn(`[credentials] provider=${pm.provider} 字段 ${f.key} 缺少前端文案（hints）`);
    }
    return {
      key: f.key,
      label: hint?.label || f.label || f.key,
      kind: f.kind,
      defaultValue: f.default,
      multiline: f.multiline,
      required: f.required,
      preserveWhitespace: f.preserve_whitespace,
      storage: f.storage,
      placeholder: hint?.placeholder,
      help: hint?.help,
    };
  });
}

/**
 * 表单里应该渲染的字段。
 *
 * 排除 `storage === "column"` 的字段（如 account_label）—— 它们由表单
 * 固定区块单独渲染（位置固定、有专属布局），不混在动态字段区里。
 */
export function dynamicFieldSpecs(pm: MarketProviderMeta, copy: ProviderCopy | undefined): FieldSpec[] {
  return buildFieldSpecs(pm, copy).filter((f) => f.storage !== "column");
}

/**
 * 验证状态的四种取值。
 *
 * 后端只给三个字段（verified_at / verify_error / updated_at），四态由它们推导。
 */
export type VerifyState = "unverified" | "verified" | "stale" | "failed";

/**
 * 从凭证记录推导验证状态。
 *
 * # `stale` 是最容易被忽略但最重要的一态
 *
 * 没有它会出现这个场景：
 *   用户配好 okx 凭证 → 验证通过 → 三个月后换了 api_secret（怀疑泄漏）
 *   → 保存成功 → 界面仍显示「已验证」绿色徽章 → 用户以为新密钥可用
 *   → **实际下单时认证失败**。
 *
 * `stale` 用琥珀色「已修改，待验证」明确表达「上次验证的是旧凭证」。
 * 靠时间戳比对推出，不依赖额外后端字段。
 */
export function verifyStateOf(c: MarketCredential): VerifyState {
  if (c.verify_error) return "failed";
  if (!c.verified_at) return "unverified";
  // 注意方向：updated_at > verified_at 表示「验证后又改过」→ stale。
  // 用 Date.parse 而不是字符串比较 —— ISO 时区偏移不同时字符串比较会错。
  const verified = Date.parse(c.verified_at);
  const updated = Date.parse(c.updated_at);
  if (Number.isNaN(verified) || Number.isNaN(updated)) return "unverified";
  return updated > verified ? "stale" : "verified";
}

/** 四态的展示配置（文案 + Badge variant + 说明）。 */
export const VERIFY_STATE_UI: Record<
  VerifyState,
  { label: string; variant: "default" | "secondary" | "destructive" | "outline"; hint: string }
> = {
  verified: { label: "已验证", variant: "default", hint: "上次验证通过，且凭证未再修改" },
  stale: {
    label: "已修改，待验证",
    variant: "secondary",
    hint: "凭证在上次验证后被修改过，上次的验证结果不代表当前这份可用",
  },
  failed: { label: "验证失败", variant: "destructive", hint: "上次验证未通过，展开可看具体原因" },
  unverified: { label: "未验证", variant: "outline", hint: "尚未验证过这条凭证" },
};

/**
 * 检测浏览器是否支持多行文本的 CSS 遮罩。
 *
 * Firefox 不支持 `-webkit-text-security`（未标准化）。不支持时：
 *   - textarea 只能明文显示 + 红色常驻警告条
 *   - **保存前弹二次确认**（警告条会被习惯性忽略，必须打断动作）
 *
 * 不能静默降级 —— 用户会以为私钥被遮住了。
 */
export function supportsTextSecurity(): boolean {
  if (typeof CSS === "undefined" || typeof CSS.supports !== "function") return false;
  return CSS.supports("-webkit-text-security", "disc");
}

/**
 * 归一化字段值。
 *
 * # `preserveWhitespace` 的准确语义
 *
 * 不是「完全不处理」，而是「**不要把中间空白折叠 / 删除**」。
 *
 *   - 首尾 trim 一律做（含 PEM）—— 用户粘贴时常带前后空白/空行，
 *     某些 PEM 解析器会因此拒绝
 *   - PEM 还要统一 `\r\n` → `\n`（Windows 粘贴）
 *   - **中间的任何换行都不动** —— PEM 的结构依赖它们
 *
 * 真正要防的错误做法是 `split("\n").map(trim).join("")` 这类「删换行」——
 * 它会把 PEM 压成一行，解析器读不到 base64 的结束位置。
 */
export function normalizeValue(spec: FieldSpec, raw: string): string {
  const trimmed = raw.trim();
  if (!spec.preserveWhitespace) return trimmed;
  return trimmed.replace(/\r\n/g, "\n");
}
