package server

// 行情渠道的字段元数据端点。
//
// # 为什么必须由后端给（前端不能硬编码）
//
// `web/src/lib/types.ts` 已有既有约定：secret_keys 由后端按渠道类型给出，
// 前端据此渲染密码框与「留空即不改」提示，不硬编码任何渠道知识。
//
// 凭证场景比通知渠道更严格：前端硬编码「哪些字段是凭据」会导致
// **密钥明文显示在界面上**（而通知渠道的同类漂移只是字段渲染成 text）。
//
// # 三个正交维度，不能合成一个枚举
//
//   - kind      = "password"（脱敏显示） | "text"（明文）
//   - multiline = 是否多行输入框（PEM 需要）
//   - preserve_whitespace = 提交时是否保留原样空白（PEM 需要）
//
// fosun 的 PEM 就是「password + multiline + preserve_whitespace」：
// 脱敏 + 多行 + 不能删换行。合成一个枚举会表达不出来。

import (
	"net/http"
	"strings"
)

// credentialField 是表单里一个字段的描述。
type credentialField struct {
	Key string `json:"key"`
	// Label 是给人看的中文标签。
	Label string `json:"label"`
	// Kind: "password" 走脱敏输入框，"text" 走普通输入框，
	// "checkbox" 走勾选框（值是 "true"/"false" 字符串，存 extra）。
	//
	// # 为什么 checkbox 也用字符串值而不是 bool
	//
	// 所有字段值在下游统一是 string（market_credentials 的 extra 是
	// map[string]string）。为一个控件引入 bool 会迫使 mergeExtra、
	// 前端 buildPayload、verify 三处都加类型分支 —— 而收益只是「少一次
	// 字符串比较」。统一成 string 让「字段值」是一条管道，不按控件分叉。
	Kind string `json:"kind"`
	// Multiline: 多行 textarea（PEM 必须）。
	Multiline bool `json:"multiline"`
	// PreserveWhitespace: 提交时用 normalizePEM 而不是 TrimSpace。
	//
	// **注意语义**：这不是「完全不动空白」。它表示「除了去掉首尾空行
	// 和统一 CRLF→LF 之外，**中间换行一个字都不能删**」。
	// 见 normalizePEM 的注释。
	PreserveWhitespace bool `json:"preserve_whitespace"`
	// Required: 新建时必填。前端据此标星号并在提交前校验。
	//
	// 与 providerMeta.RequiredFields 重复？不是 —— 那个是 key 清单（便于
	// 程序快速判断），这个是字段自身的属性（便于渲染）。两份都从同一份
	// 定义派生。
	//
	// 一致性由 TestMarketProvidersRequiredFieldsMatchFields
	// （server/market_credentials_meta_test.go）双向保证：
	// Fields[].Required → RequiredFields 与反向都检查。
	//
	// ⚠️ 这条注释原先写「有测试保证一致」而那个测试**不存在**（2026-10-29 补上）。
	// 声称有保障时请写清是哪个测试 —— 否则那句话只是愿望，而愿望会被当成事实读。
	Required bool `json:"required"`
	// Storage 说明这个字段的值存在哪。
	//
	//	"secret" → 进 api_key / api_secret / passphrase（只写不读）
	//	"hint"   → 自动从 secret 生成（api_key_hint），客户端不需要传
	//	"extra"  → 进 extra JSONB（**只放非敏感**）
	//	"column" → 进主表的非敏感列（如 base_url）
	//
	// 前端据此决定「提交时要不要带上这个值」：
	//   - secret 字段留空 = 保留旧值（后端有 hint 可回显）
	//   - extra 字段留空 = 显式清空（前端能自己显示旧值，不需要 hint）
	Storage string `json:"storage"`
	// Default 是这个字段的默认值，前端据此预填输入框。
	//
	// # 为什么默认值必须由后端给
	//
	// 数据源类 provider（ifind / neodata / kuaicha …）的 endpoint 是 MCP 的
	// streamable-http URL。它是**后端知识**：前端硬编码一份、后端 sync 里再硬编码
	// 一份，两份必然漂移（用户看到 A 却被连到 B）。
	//
	// 这里给出后，前端预填、后端 fallback 用的都是同一份 `dataProviderEndpoints`。
	//
	// 只用于 `Storage: "column"` 的字段（secret 字段给默认值无意义）。
	Default string `json:"default,omitempty"`
}

// providerMeta 是一个 provider 的完整元数据。
type providerMeta struct {
	Provider string `json:"provider"`
	// Purpose 区分「数据源凭据」和「交易通道凭据」：'data' | 'trade'。
	//
	// # 为什么叫 Purpose 而不叫 Kind
	//
	// credentialField.Kind 已经占用了字段名 Kind（"password"/"text"，指输入框类型）。
	// 两者同名但语义完全不同 —— 一个说「怎么输入」，一个说「用来干什么」。
	// 叫 Kind 会让 meta.Kind 和 field.Kind 在同一段代码里指向两件事。
	//
	// DB 列名仍是 kind（表里没有别的 kind，无歧义）。
	Purpose string `json:"purpose"`
	Label   string `json:"label"`
	// SecretFields 是哪些字段是凭据（前端据此渲染密码框 + 「留空即不改」提示）。
	SecretFields []string `json:"secret_fields"`
	// RequiredFields 是新建时必填的。
	RequiredFields []string `json:"required_fields"`
	// Verifiable=false 时前端应隐藏「验证」按钮 ——
	// 而不是点了之后返回 501 才让用户知道。
	Verifiable bool `json:"verifiable"`
	// SupportsBaseURL: 有些 provider 不需要 base_url（OKX 走 CLI）。
	SupportsBaseURL bool              `json:"supports_base_url"`
	Fields          []credentialField `json:"fields"`
}

// marketProviders 是已支持的 provider 元数据。
//
// # 为什么 okx / fosun 各出现两次
//
// 同一个 provider 有两种用途完全不同的凭据：
//   - purpose='trade' -> 下单通道（要 API Key / 私钥）
//   - purpose='data'  -> 行情数据源（OKX 公开行情免凭证，填 key 只为提高限频配额）
//
// 两者是不同的密钥、不同的权限。混在一起会让「这个 key 能干什么」不明确。
//
// 因此前端任何按 provider 做 key/lookup 的地方都要带上 purpose，
// 否则两个 okx 会取到第一个（见 web/src/app/(main)/system/credentials/page.tsx 的 metaOf）。
//
// # ifind / tdx / akshare 的历史
//
// 早先这几家不在这里：ifind 走 MCP Connector、tdx 走本地客户端、
// akshare 本地 sidecar 免凭证。2026-10-31 起 ifind/fosun_data/tdx/neodata/
// kuaicha/westock 六家的 MCP endpoint 已知，改为「凭证页统一配」——
// 用户填一个 token，后端同步成 mcp_servers 行（见 mcp_credential_sync.go）。
// akshare 仍是本地 Python sidecar，免凭证，不在此列。
var marketProviders = []providerMeta{
	// ---- 交易通道（purpose='trade'）----
	{
		Provider:       "okx",
		Purpose:        "trade",
		Label:          "OKX（交易）",
		SecretFields:   []string{"api_key", "api_secret", "passphrase"},
		RequiredFields: []string{"api_key", "api_secret", "passphrase"},
		Verifiable:     true,
		// OKX 走 REST（此前的 CLI 路径已废弃，见 verifyOkxREST 的注释）。
		// base_url 可留空 —— 留空用 https://www.okx.com。
		SupportsBaseURL: true,
		Fields: []credentialField{
			{Key: "api_key", Label: "API Key", Kind: "password", Required: true, Storage: "secret"},
			{Key: "api_secret", Label: "API Secret", Kind: "password", Required: true, Storage: "secret"},
			{Key: "passphrase", Label: "Passphrase", Kind: "password", Required: true, Storage: "secret"},
			// 模拟盘开关。**必填**（默认勾选）——
			// 实盘/模拟盘的 key 互不通用，OKX 对错配返回 50101
			// 「APIKey does not match current environment」。
			// 不给这个开关，用户拿模拟盘 key 永远验不过，且错误信息看起来像 key 无效。
			{Key: "demo", Label: "模拟盘（Demo Trading）", Kind: "checkbox",
				Required: false, Storage: "extra"},
			{Key: "endpoint", Label: "API Endpoint", Kind: "text", Storage: "column",
				Default: "https://www.okx.com"},
			{Key: "account_label", Label: "账户标签", Kind: "text", Storage: "column"},
		},
	},
	{
		Provider:       "fosun",
		Purpose:        "trade",
		Label:          "星财富（交易）",
		SecretFields:   []string{"server_public_key", "client_private_key"},
		RequiredFields: []string{"server_public_key", "client_private_key"},
		// 暂不支持在线验证。
		//
		// # 为什么 Verifiable: false
		//
		// 验证需要「用表里存的 server_public_key + client_private_key 调 fosun OpenAPI
		// 的只读接口」—— 而那个 API 的签名规范与 endpoint 我们没有（openapi-sdk/ 不存在）。
		//
		// 不要用 mcp__fosunwealth 的接口来「代理验证」：
		// 它的认证走 MCP Connector 自己的凭证，与表里这把密钥无关。
		// 用它验证会得出「验证通过」但实际表里的密钥是错的 —— 那是假验证。
		Verifiable:      false,
		SupportsBaseURL: false,
		Fields: []credentialField{
			{Key: "server_public_key", Label: "服务器公钥 (PEM)", Kind: "password",
				Multiline: true, PreserveWhitespace: true, Required: true, Storage: "secret"},
			{Key: "client_private_key", Label: "客户端私钥 (PEM)", Kind: "password",
				Multiline: true, PreserveWhitespace: true, Required: true, Storage: "secret"},
			// apply_account_id 是非敏感字段（fosun 下单必填），存 extra JSONB。
			{Key: "apply_account_id", Label: "账户 ID", Kind: "text", Storage: "extra"},
			{Key: "account_label", Label: "账户标签", Kind: "text", Storage: "column"},
		},
	},

	// ---- 数据源（purpose='data'）----
	{
		Provider: "okx",
		Purpose:  "data",
		Label:    "OKX（行情）",
		// 数据源凭据全部可选 —— 公开行情接口免凭证，但限频。
		// 填了 key 能提高配额，不填也能用。
		//
		// 因此前端「保存」按钮不能因字段为空而禁用（RequiredFields 为空）。
		SecretFields:    []string{"api_key", "api_secret", "passphrase"},
		RequiredFields:  []string{},
		Verifiable:      true,
		SupportsBaseURL: false,
		Fields: []credentialField{
			{Key: "api_key", Label: "API Key（可选，提高限频配额）", Kind: "password", Storage: "secret"},
			{Key: "api_secret", Label: "API Secret（可选）", Kind: "password", Storage: "secret"},
			{Key: "passphrase", Label: "Passphrase（可选）", Kind: "password", Storage: "secret"},
			{Key: "account_label", Label: "账户标签", Kind: "text", Storage: "column"},
		},
	},
	{
		Provider:        "fosun",
		Purpose:         "data",
		Label:           "星财富（行情）",
		SecretFields:    []string{"server_public_key", "client_private_key"},
		RequiredFields:  []string{"server_public_key", "client_private_key"},
		Verifiable:      false,
		SupportsBaseURL: false,
		Fields: []credentialField{
			{Key: "server_public_key", Label: "服务器公钥 (PEM)", Kind: "password",
				Multiline: true, PreserveWhitespace: true, Required: true, Storage: "secret"},
			{Key: "client_private_key", Label: "客户端私钥 (PEM)", Kind: "password",
				Multiline: true, PreserveWhitespace: true, Required: true, Storage: "secret"},
			{Key: "account_label", Label: "账户标签", Kind: "text", Storage: "column"},
		},
	},

	// ---- 数据源（MCP 直连，2026-10-31 新增 6 家）----
	//
	// # 为什么这六家用「一个 token + 一个 endpoint」
	//
	// 它们的凭证页值就是 MCP 的 streamable-http URL 加一个 Bearer token。
	// 用户能拿到的也就这两样 —— 字段设计必须与「用户手上真有的东西」对齐，
	// 多要一个字段等于逼用户去别处找，于是他会放弃。
	//
	// # 为什么 endpoint 不是必填
	//
	// 有默认值（dataProviderEndpoints）。99% 的用户不改它，
	// 必填会让每次新建都要多复制一次 URL —— 这里必填是纯摩擦。
	//
	// # 为什么 token 在有的家必填、有的家不必填
	//
	// 必填的那几家（ifind/neodata/kuaicha/westock）WorkBuddy 侧也是带 key 调的；
	// 不填的话 discover 一定 401，用户会以为「保存坏了」。
	// fosun_data / tdx 的 endpoint 在实测中允许匿名连（tdx 本地/公开、
	// fosun 的 MCP 网关放行），不填更顺。
	//
	// # 与 okx/fosun(data) 的区别
	//
	// okx/data 和 fosun/data 是「用 API key 调 REST 行情」，这六家是
	// 「用 token 调 MCP」。上半段的 fosun/data 是 PEM 双密钥语义，
	// **不能复用 `fosun` 这个 key**，所以这里叫 fosun_data ——
	// 复用会继承 PEM 的字段布局，用户填 token 会被塞进 server_public_key。
	{
		Provider:        "ifind",
		Purpose:         "data",
		Label:           "同花顺 iFinD",
		SecretFields:    []string{"auth_token"},
		RequiredFields:  []string{"auth_token"},
		Verifiable:      true,
		SupportsBaseURL: true,
		Fields: []credentialField{
			{Key: "auth_token", Label: "授权 Token", Kind: "password", Required: true, Storage: "secret"},
			{Key: "endpoint", Label: "MCP Endpoint", Kind: "text", Storage: "column",
				Default: dataProviderEndpoints["ifind"]},
			{Key: "account_label", Label: "账户标签", Kind: "text", Storage: "column"},
		},
	},
	{
		Provider:        "fosun_data",
		Purpose:         "data",
		Label:           "星财富（行情 MCP）",
		SecretFields:    []string{"auth_token"},
		RequiredFields:  []string{},
		Verifiable:      true,
		SupportsBaseURL: true,
		Fields: []credentialField{
			{Key: "auth_token", Label: "授权 Token（可选，网关允许匿名）", Kind: "password", Storage: "secret"},
			{Key: "endpoint", Label: "MCP Endpoint", Kind: "text", Storage: "column",
				Default: dataProviderEndpoints["fosun_data"]},
			{Key: "account_label", Label: "账户标签", Kind: "text", Storage: "column"},
		},
	},
	{
		Provider:        "tdx",
		Purpose:         "data",
		Label:           "通达信",
		SecretFields:    []string{"auth_token"},
		RequiredFields:  []string{},
		Verifiable:      true,
		SupportsBaseURL: true,
		Fields: []credentialField{
			{Key: "auth_token", Label: "授权 Token（可选，公开端点免凭证）", Kind: "password", Storage: "secret"},
			{Key: "endpoint", Label: "MCP Endpoint", Kind: "text", Storage: "column",
				Default: dataProviderEndpoints["tdx"]},
			{Key: "account_label", Label: "账户标签", Kind: "text", Storage: "column"},
		},
	},
	{
		Provider:        "neodata",
		Purpose:         "data",
		Label:           "腾讯 NeoData",
		SecretFields:    []string{"auth_token"},
		RequiredFields:  []string{"auth_token"},
		Verifiable:      true,
		SupportsBaseURL: true,
		Fields: []credentialField{
			{Key: "auth_token", Label: "授权 Token", Kind: "password", Required: true, Storage: "secret"},
			{Key: "endpoint", Label: "MCP Endpoint", Kind: "text", Storage: "column",
				Default: dataProviderEndpoints["neodata"]},
			{Key: "account_label", Label: "账户标签", Kind: "text", Storage: "column"},
		},
	},
	{
		Provider:        "kuaicha",
		Purpose:         "data",
		Label:           "同花顺快查",
		SecretFields:    []string{"auth_token"},
		RequiredFields:  []string{"auth_token"},
		Verifiable:      true,
		SupportsBaseURL: true,
		Fields: []credentialField{
			{Key: "auth_token", Label: "授权 Token", Kind: "password", Required: true, Storage: "secret"},
			{Key: "endpoint", Label: "MCP Endpoint", Kind: "text", Storage: "column",
				Default: dataProviderEndpoints["kuaicha"]},
			{Key: "account_label", Label: "账户标签", Kind: "text", Storage: "column"},
		},
	},
	{
		Provider:        "westock",
		Purpose:         "data",
		Label:           "腾讯自选股",
		SecretFields:    []string{"auth_token"},
		RequiredFields:  []string{"auth_token"},
		Verifiable:      true,
		SupportsBaseURL: true,
		Fields: []credentialField{
			{Key: "auth_token", Label: "授权 Token", Kind: "password", Required: true, Storage: "secret"},
			{Key: "endpoint", Label: "MCP Endpoint", Kind: "text", Storage: "column",
				Default: dataProviderEndpoints["westock"]},
			{Key: "account_label", Label: "账户标签", Kind: "text", Storage: "column"},
		},
	},
	// ---- 东方财富妙想（REST API，不是 MCP）----
	//
	// # 为什么它和上面六家分开
	//
	// 上面六家是 MCP server（`mcp_servers` 行 + `env` 当 HTTP header）。
	// 妙想是**普通 REST**：`POST https://mkapi2.dfcfs.com/finskillshub/api/claw/query`
	// 加一个 `apikey` header，body 是自然语言问句。
	//
	// 它不需要建 MCP 行 —— 建了反而是错的（MCP 客户端会去 POST 一个不存在的
	// MCP 端点）。所以 SyncMarketCredentialToMCP 要**显式跳过它**（见该函数注释）。
	// 它的消费方是 `agent/mx_client.go`，从凭证表取 key。
	{
		Provider:        "mx",
		Purpose:         "data",
		Label:           "东方财富妙想",
		SecretFields:    []string{"auth_token"},
		RequiredFields:  []string{"auth_token"},
		Verifiable:      true,
		SupportsBaseURL: true,
		Fields: []credentialField{
			{Key: "auth_token", Label: "API Key", Kind: "password", Required: true, Storage: "secret"},
			{Key: "endpoint", Label: "API Base URL", Kind: "text", Storage: "column",
				Default: "https://mkapi2.dfcfs.com/finskillshub/api/claw"},
			{Key: "account_label", Label: "账户标签", Kind: "text", Storage: "column"},
		},
	},
}

// dataProviderEndpoints 是六家数据源的默认 MCP endpoint（裸 streamable-http URL）。
//
// # 为什么放在 meta 文件里而不是 sync 文件里
//
// 它同时被两处使用：
//   - marketProviders 的 Fields[].Default  → 前端预填
//   - SyncMarketCredentialToMCP 的 fallback → 后端建连
//
// 两份硬编码必然漂移（前端预填 A，后端连 B，用户看到自己填的 URL 却被连到别处）。
// 单一 var 保证「用户看到的就是实际连的」。
var dataProviderEndpoints = map[string]string{
	"ifind":      "https://api-mcp.51ifind.com:8643/ds-mcp-servers/hexin-ifind-financial-mcp",
	"fosun_data": "https://aiapi.fosunhanig.com/ai-fin-agent/mcp",
	"tdx":        "https://txmcp.tdx.com.cn:3001/txmcp",
	"neodata":    "https://neodata.tencent.com/proxy/mcp",
	"kuaicha":    "https://bizveris.kuaicha365.com/open/mcp?source=workbuddy",
	"westock":    "https://stockbuddy.qq.com/cgi/cgi-bin/openai/mcp/mcp",
}

// dataProviderMCPNames 是「凭证 provider → mcp_servers.name」的映射。
//
// # 为什么名字要和 WorkBuddy 侧一致
//
// 这些名字对应真实的 MCP server 身份（iFindD-MCP / fosunwealth / tdx-finance …）。
// 用户在「MCP 管理」页看到的就是这些名字；如果这里另起一套（如 mkt_ifind），
// 同一家会在两个列表里各出现一次，用户会分别配两边。
//
// # iFindD-MCP 的大小写是刻意保留的
//
// 上游的 server 自报名字就是 `iFindD-MCP`（大写 D）。改小写虽然不影响本地
// 连接，但会让「用别的客户端连同一家」的人对不上号。
var dataProviderMCPNames = map[string]string{
	"ifind":      "iFindD-MCP",
	"fosun_data": "fosunwealth",
	"tdx":        "tdx-finance",
	"neodata":    "neodata",
	"kuaicha":    "kuaicha-search",
	"westock":    "westock-mcp",
}

// restDataProviders 是「走普通 REST、**不建 MCP 行**」的数据源 provider。
//
// # 为什么需要这个集合（而不是靠 `dataProviderMCPNames` 里没有就跳过）
//
// 两者语义不同，混用会导致「加漏一个就是静默 bug」：
//   - `dataProviderMCPNames` 里没有 → 现在 `SyncMarketCredentialToMCP` 会报
//     「未知 provider」并 return error。这对**新加的 MCP 源**是对的（逼你补映射）。
//   - 但 `mx` 是**故意不该建 MCP 行**的（它没有 MCP 端点）。如果只在
//     dataProviderMCPNames 里查不到，它会被当成「配置漏了」而报错 ——
//     用户保存妙想凭证时看到一个与事实无关的错误。
//
// 所以显式列出「REST 类」，让跳过成为**声明**而不是**遗漏的副产品**。
// 新增 REST 源时加到这里；新增 MCP 源时加到 dataProviderMCPNames。
var restDataProviders = map[string]bool{
	"mx": true,
}

// isSyncedDataProvider 判断某个 provider 是否走「凭证 → MCP 同步」路径。
//
// 只有 purpose='data' 且在 dataProviderMCPNames 里的才同步。okx/fosun(data)
// 不走 —— 它们用 REST/CLI，没有 MCP endpoint。
func isSyncedDataProvider(provider string) bool {
	_, ok := dataProviderMCPNames[provider]
	return ok
}

// listMarketCredentialMeta GET /api/market/credentials/meta
func (s *Server) listMarketCredentialMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"providers": marketProviders})
}

// normalizePEM 归一化 PEM 文本。
//
// # 三步，只有第三步是必须的
//
//  1. **去掉首尾空白**（用 TrimSpace）—— 安全且必要。
//     用户从文件粘贴时常带首尾空行，某些解析器会因此拒绝。
//     注意：TrimSpace 只去**首尾**，中间的行结构不受影响。
//  2. **\r\n → \n**（Windows 粘贴会带 CRLF）—— 必要。
//     PEM 解析器对 CRLF 的容忍度不一致，统一成 LF 最稳。
//  3. **不动中间的任何换行** —— PEM 的结构依赖它们
//     （`-----BEGIN ...-----` 与 base64 行之间必须有换行）。
//
// # 常见错误做法
//
// `strings.Split(s, "\n")` + 逐行 trim + `strings.Join(lines, "")`
// （去掉换行拼成一行）会彻底破坏 PEM —— 解析器读不到 base64 的结束位置。
func normalizePEM(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return s
}

// normalizeSecretField 按字段元数据归一化一个提交值。
//
// preserve_whitespace=true → normalizePEM；否则 TrimSpace。
//
// 注意：**空值语义（保留/清空）的判断在调用方**，这里是「已经决定要写入新值」
// 之后的格式化。所以空串传进来会原样返回空串（调用方已判过）。
func normalizeSecretField(v string, preserveWhitespace bool) string {
	if preserveWhitespace {
		return normalizePEM(v)
	}
	return strings.TrimSpace(v)
}
