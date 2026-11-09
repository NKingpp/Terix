// Package redact 把日志 / 快照 / LLM 记录里的凭证做掩码。
//
// 设计取向：**宁可过度遮盖，不许漏**。误遮一个正常字符串的代价是排查不便；
// 漏一个真密钥的代价是资金可被接管。所以最后一条是宽泛的 base64 兜底。
//
// 本包**只 import stdlib**，且**不得 import 本仓任何其他包**（db / agent /
// intercept / exec 都不行）—— 它要在最底层被所有包调用，反向依赖必成环。
// 这条由 `go list -deps` 断言守住（见《ARCH-W2-W5》§3.2.2 ③「日志如何脱敏」，
// docs 仓库：quantex/ARCH-W2-W5.md）。
//
// 用法：`Mask(s)` 是幂等的 —— `Mask(Mask(x)) == Mask(x)`。这条是硬约束：
// 同一条数据可能被多次过手（写入 → 读出 → 再写入快照），不幂等会导致
// 掩码层层嵌套（`***` → `******` → …），最后看不出原始结构。
package redact

import (
	"regexp"
	"strings"
)

// 掩码字面量。刻意用两种：
//   - maskValue 用于「值」的位置，保留 key 名（看得出这是什么字段被遮了）
//   - maskBlock 用于「整块」的位置（PEM / 长 blob），原文结构已无意义
const (
	maskValue = "***"
	maskBlock = "[REDACTED]"
)

// 敏感 key 名。用于 patternKeyValue 的 key 捕获组。
//
// 覆盖清单（每条都对应一个真实来源）：
//   - api_key / apikey / api-key          通用
//   - secret / secret_key / secretKey     OKX okx-cex-auth、TradingAgents 脚本
//   - passphrase / passwd / password / pwd   通用 + OKX OK-ACCESS-PASSPHRASE
//   - private_key / privateKey            FSOPENAPI_CLIENT_PRIVATE_KEY、session.json
//   - token / access_token / auth_token   Bearer 之外的显式写法
//   - sub_account_id / account_id / accountId   复星 SDK 的账户标识
//     （不是密码，但泄漏它 = 别人可对着你的账户查持仓与下单）
//   - OK-ACCESS-KEY / OK-ACCESS-SIGN / OK-ACCESS-PASSPHRASE  OKX 三件套（HTTP 头）
//   - set-cookie / cookie                 会话
const sensitiveKeyAlt = `api[_-]?key|apikey|secret[_-]?key|secretKey|secret|passphrase|passwd|password|pwd|private[_-]?key|privateKey|access[_-]?token|auth[_-]?token|token|sub[_-]?account[_-]?id|accountId|account[_-]?id|OK-ACCESS-KEY|OK-ACCESS-SIGN|OK-ACCESS-PASSPHRASE|clientPrivateKey`

var (
	// 1) PEM 私钥整块。
	//    必须先于其他规则执行：PEM 内部的 base64 会被规则 7 的兜底切成碎片，
	//    切碎后 BEGIN/END 标记就对不上了，反而留下大量无法识别的残片。
	rePEM = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)

	// 2) Authorization: Bearer / Basic。
	//    {20,} 的下界是刻意的：低于 20 字符的东西（"Bearer test"）掩了没意义，
	//    反而会在测试里制造噪音。真 token 长度远超 20。
	//    替换成 "Bearer ***"（保留 scheme），所以二次 Mask 时 "***" 只有 3 字符、
	//    不满足 {20,}，天然幂等。
	reBearer = regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+[A-Za-z0-9._~+/=\-]{20,}`)

	// 3) Cookie / Set-Cookie 头。
	//    整行值遮掉：Cookie 里可能塞多段 key=value，逐段判断容易漏。
	reCookie = regexp.MustCompile(`(?i)\b((?:set-)?cookie)\s*[:=]\s*[^\r\n"']{8,}`)

	// 4) URL 内嵌凭证 scheme://user:pass@host。
	//    Password 允许空（有些 API 用空密码 + token 用户名），但 user 必须有，
	//    否则正常的 scheme://host 会被误伤。
	reURLCred = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/\s:@]{1,64}:[^/\s@]{0,128}@`)

	// 5) key=value / key: "value" 形式的敏感字段。
	//    分两段捕获：$1 = 前缀（含 key 名与分隔符），$2 = 值。
	//    只遮 $2，保留 key 名 —— 「哪个字段被遮了」是排查时的关键信息。
	//
	//    key 名后允许跟 [A-Za-z_]* 后缀：真实字段名有 passphrase_check、
	//    api_key_id、secret_key_v2 这类变体，只认精确 key 会漏掉它们。
	//    后缀不会造成过度匹配 —— 值的下界 {8,} 会把 "token_count":5 这类挡掉。
	//
	//    值下界 {8,}：太短的值（如 "on"/"true"）不是凭证；上界不设（凭证可以很长）。
	reKeyValue = regexp.MustCompile(`(?i)((?:` + sensitiveKeyAlt + `)[A-Za-z_]*["']?\s*[:=]\s*["']?)([^\s"'&,;)\]}\r\n]{8,})`)

	// 5b) 敏感 key 名的独立判定，供 slog 的 kv 序列用。
	//     为什么需要：`slog.Info("x", "api_key", "sk_live_abc")` 拆开后，
	//     值 "sk_live_abc" 单独看是认不出来的（无 key 上下文、不足 64 字符）。
	//     必须由「前一个 key 名」驱动，否则这类日志必漏。
	reSensitiveKey = regexp.MustCompile(`(?i)^(?:` + sensitiveKeyAlt + `)[A-Za-z_]*$`)

	// 6) 复星 SDK 环境变量三件套的赋值形式。
	//    与规则 5 有重叠（FSOPENAPI_CLIENT_PRIVATE_KEY 含 PRIVATE_KEY），
	//    但环境变量名带 FSOPENAPI_ 前缀且值可能是多行 PEM，规则 5 的
	//    「无空白」值捕获接不住多行，故单列一条吃「到行尾」。
	reFosunEnv = regexp.MustCompile(`(?m)^(FSOPENAPI_(?:SERVER_PUBLIC_KEY|CLIENT_PRIVATE_KEY)\s*=\s*)(.+)$`)

	// 7) 兜底：长 base64 / hex blob（>= 64 字符连续）。
	//    这是最后一道网，捕所有规则 1–6 没认出来的密钥形态。
	//    代价：会误伤合法的长哈希（sha256 是 64 hex、git commit 是 40 hex），
	//    但那些在日志里被遮掉不影响排查 —— 需要哈希时看 DB 里的字段，不看日志。
	//    刻意放在最后执行。
	reBlob = regexp.MustCompile(`\b[A-Za-z0-9+/_\-]{64,}={0,2}`)
)

// IsSensitiveKey 判断一个字符串是否是敏感字段名。
//
// 给 slog 的 kv 序列用：`slog.Info("x", "api_key", "sk_live_abc")` 拆开后，
// 值 "sk_live_abc" 无 key 上下文、也不够 64 字符，7 条规则一条都抓不住。
// 必须由前一个 key 名驱动。
func IsSensitiveKey(k string) bool {
	k = strings.TrimSpace(k)
	// 常见包裹形态：`"api_key"` / `api-key:` / `[password]`
	k = strings.Trim(k, `"'[]{}() `)
	k = strings.TrimSuffix(k, ":")
	k = strings.TrimSuffix(k, "=")
	return reSensitiveKey.MatchString(k)
}

// MaskValueForKey 按「已知 key 名」决定是否遮值。
// key 敏感 → 整个值替换为 ***；否则走通用 Mask（值里可能仍含内嵌凭证）。
func MaskValueForKey(key, val string) string {
	if IsSensitiveKey(key) && val != "" {
		// 值本身全遮。不保留前缀字符 —— 凭证的前几位（如 "sk_live_"）
		// 已足以判定「这是哪个服务的 key」，而排查时我们要的是「哪个字段」，
		// 那个信息在 key 名里已经有了。
		return maskValue
	}
	return Mask(val)
}

// Mask 对 s 做掩码。幂等：Mask(Mask(x)) == Mask(x)。
//
// 顺序是硬约束：PEM 必须最先（避免被 blob 兜底切碎），blob 必须最后（避免
// 抢在结构化规则前把 key=value 的值切走、丢掉 key 名的上下文）。
func Mask(s string) string {
	if s == "" {
		return s
	}
	// 快速排除：不含任何可疑标记的短串直接返回。
	// 依据：上述 7 条规则的触发都需要至少一个标记字符。
	// 这条是性能主路径 —— 绝大多数 activity 的 summary/detail 是短中文。
	if len(s) < 20 && !strings.ContainsAny(s, ":=") {
		return s
	}

	s = rePEM.ReplaceAllString(s, maskBlock)
	s = reBearer.ReplaceAllString(s, "${1} "+maskValue)
	s = reCookie.ReplaceAllString(s, "${1}: "+maskBlock)
	s = reURLCred.ReplaceAllString(s, "${1}"+maskValue+"@"+"")
	// 环境变量：值可能是多行，单独处理，替换成单行标记
	s = reFosunEnv.ReplaceAllString(s, "${1}"+maskBlock)
	s = reKeyValue.ReplaceAllString(s, "${1}"+maskValue)
	s = reBlob.ReplaceAllString(s, maskBlock)
	return s
}

// MaskJSON 对一段 JSON 字节做掩码。
//
// 为什么不 json.Unmarshal → 遍历 → 再 Marshal：
//  1. 会丢字段顺序与空白（快照要可复现，字节变了 checksum 就变了）；
//  2. 大 payload（traffic 体可达数 MB）反序列化再序列化耗时是字符串替换的数十倍；
//  3. 遇到畸形 JSON（源站返回的半截体）会整体失败，而掩码是「必须尽力做」的。
//
// 安全性：所有替换字面量（*** / [REDACTED]）都不含 " \ 或控制字符，
// 因此不会破坏 JSON 的字符串转义结构。MaskJSON 后仍可 json.Valid。
func MaskJSON(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	return []byte(Mask(string(b)))
}

// MaskIfEnabled 供「有开关」的落点使用（如 llmrec 的记录开关）。
// 关闭时不复制、不扫描，零开销。
func MaskIfEnabled(s string, enabled bool) string {
	if !enabled {
		return s
	}
	return Mask(s)
}

// 以下为 traffic 专用的端点判定。放在本包是因为它决定「要不要遮」，
// 与「怎么遮」是同一决策的两半；拆开会让两处漂移。

// tradingURLPrefixes 是需要遮的交易类端点（按 URL path 前缀）。
// 判定用 path 前缀而不是域名：同一家券商（复星）的行情与交易走同一域名，
// 域名级判定无法区分。
var tradingURLPrefixes = []string{
	"/api/v1/trade/",     // 复星：下单/撤单/改单/订单列表/订单详情/资金流水
	"/api/v1/account/",   // 复星：账户列表（含 sub_account_id）
	"/api/v1/portfolio/", // 复星：持仓/资金汇总
	"/api/ops/v1/trade/",
	"/api/ops/v1/account/",
	"/api/ops/v1/portfolio/",
	"/api/v5/trade/",   // OKX REST：下单/撤单/改单
	"/api/v5/account/", // OKX REST：余额/持仓/配置（含 passphrase 校验路径）
	"/api/v5/asset/",   // OKX REST：资金划转
}

// ShouldRedactURL 判断一条记录是否需要遮。
//
// 取舍：**行情类不遮**。理由是可复现性 —— point-in-time 快照要求
// 「同一 (source, endpoint, params, collected_at) 可重放」，若把行情响应里的
// 数字或长哈希遮掉，重放就不可比。行情请求本身不带凭证（凭证在 header，
// 而 header 由规则 2/3 单独遮），所以不遮 body 是安全的。
//
// 但**交易类的响应体必须遮**：复星 get_order_detail 的 raw_ack 会回显
// sub_account_id，OKX 的部分错误响应会回显 passphrase 校验失败详情。
func ShouldRedactURL(rawURL string) bool {
	// 快速判定：只看 path 部分，忽略 query（query 里也可能有敏感参数，
	// 但那些由 reKeyValue 在 body/整行层面遮）。
	p := rawURL
	if i := strings.Index(p, "://"); i >= 0 {
		p = p[i+3:]
		if j := strings.IndexAny(p, "/?#"); j >= 0 {
			p = p[j:]
		} else {
			p = "/"
		}
	}
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	for _, pre := range tradingURLPrefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// MaskForURL 是 traffic 落库点的入口：交易类端点全遮，其余原样返回。
// 返回 (result, redacted) 便于调用方在快照上打 `degraded`/`redacted` 标记。
func MaskForURL(rawURL string, body []byte) ([]byte, bool) {
	if !ShouldRedactURL(rawURL) {
		return body, false
	}
	return MaskJSON(body), true
}
