package server

// 「行情凭证 → MCP server」的同步层。
//
// # 为什么需要这一层
//
// 两个系统对「同一家的认证信息」有不同的表示：
//
//	凭证页  market_credentials：一个 auth_token 字符串
//	MCP 行  mcp_servers.env  ：一个 header map（{"Authorization":"Bearer …"}）
//
// MCP 客户端（server/mcpdiscover.go:36）对 http 传输直接把 env 当 HTTP header 用，
// 见 mcphttp.New(ctx, name, url, headers, insecure)。所以「用户填 token」到
// 「agent 能调这家工具」之间缺一次翻译。
//
// # 为什么不在前端翻译
//
// 那会让 token 经过浏览器并出现在第二个配置面（MCP 管理页）。
// 更糟的是「两个入口配同一件事」：用户在凭证页改了 token，MCP 页还是旧的，
// 而两边都显示「已保存」。把翻译放在保存路径上，凭证页成为**唯一配置入口**。
//
// # 为什么不做成后台定时任务
//
// 同步的触发点是「保存」这个动作，是确定的。轮询会引入「保存了但还没生效」
// 的时间窗，而用户点完保存就立刻点验证 —— 那个窗口里的失败会被读成 bug。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

// mcCredSyncTimeout 是「同步 + 工具发现」的总预算。
//
// 45s 与 pgRefreshMCP 同一口径：discover 要跑一次 MCP 握手 + tools/list，
// 远端慢的时候 10s 不够。超时只影响这次的发现结果，MCP 行已经落库。
const mcCredSyncTimeout = 45 * time.Second

// MCPSyncResult 是同步结果，会出现在保存/验证的响应里给前端展示。
//
// # 为什么错误不是 error 而是字符串字段
//
// 同步失败**不应该**让保存失败：凭证已经落库了，用户下次还能重试。
// 如果返回 500，前端会显示「保存失败」，用户会重新填一遍 —— 而实际上
// 凭证早已存好，这只会制造重复提交。所以失败走 Ok=false + Error。
type MCPSyncResult struct {
	// Ok 表示 MCP 行已写入**且**工具发现成功。
	Ok bool `json:"ok"`
	// ServerID 是 mcp_servers 行的 id（即使发现失败也有值 —— 行已经建了）。
	ServerID int64 `json:"server_id"`
	// ServerName 是 MCP 名称（ifind → iFindD-MCP 等）。
	ServerName string `json:"server_name"`
	// URL 是实际连接的 endpoint（用户填的或默认的）。
	URL string `json:"url"`
	// Anonymous 表示这次没有 token，走的是无 Authorization 的连接。
	Anonymous bool `json:"anonymous"`
	// ToolCount 是发现的工具数（失败时为 0）。
	ToolCount int `json:"tool_count"`
	// Error 是失败原因的**已脱敏**文本（空串 = 成功）。
	Error string `json:"error,omitempty"`
}

// SyncMarketCredentialToMCP 把一条 purpose='data' 的行情凭证同步成一个 MCP server 行，
// 然后立刻拉一次工具清单。返回 MCP server 的 id（新建或更新）。
//
// # 幂等
//
// 按 name upsert（db.UpsertMCPByName）。同一 provider 保存 N 次只有一行，
// env/url 被覆盖成最新值。不这么做的话「先存了个错 token，再存对的」
// 会留下两条 mcp_servers 行 —— 而 agent 会连上两条，其中一条永远 401。
//
// # 匿名也能连
//
// 没有 token 时**不中断**，而是发一个不带 Authorization 的请求。
// 理由：这六家的 endpoint 本身可能允许无凭据访问（实测 fosun/tdx 就放行），
// 而且「先试试能不能通」比「服务端替你判定必须有 key」更有信息量 ——
// 后者的判定一定会在某家（今天还免凭证、明天开始要）变成错的。
//
// # 失败不阻断保存
//
// 见 MCPSyncResult 的注释。这里返回 error 是为了让调用方能区分
// 「MCP 行都没建出来」（真错误，如 DB 挂了）与「建出来了但 discover 失败」
// （正常，远端不通/需认证）。前者返回非 nil error，后者返回 nil error
// 但 result.Ok=false。
func (s *Server) SyncMarketCredentialToMCP(ctx context.Context, cred *db.MarketCredential) (int64, MCPSyncResult, error) {
	var res MCPSyncResult
	if cred == nil {
		return 0, res, errors.New("凭证为空")
	}
	if cred.Kind != "data" {
		// 交易凭证没有 MCP 对应物（okx CLI / fosun OpenAPI）。
		return 0, res, fmt.Errorf("只有 purpose='data' 的凭证才同步 MCP，收到 kind=%q", cred.Kind)
	}
	// REST 类数据源（妙想）不该建 MCP 行 —— 它没有 MCP 端点。
	// 返回 (0, 零值 res, nil)：**不是错误**，调用方据此跳过同步即可。
	//
	// # 为什么返回 nil error 而不是一个「跳过」错误码
	//
	// 调用方（saveMarketCredential）对 error 的处理是「记进 verify_error 并提示用户」。
	// 而「这个 provider 本来就不用同步」不是需要用户知道的事 —— 告诉他们
	// 「妙想不需要 MCP」只会制造疑问。返回 nil 让保存路径当它没发生过。
	//
	// ⚠️ 但 res 是零值（Ok=false），所以调用方**不能**把零值 res 当成
	// 「同步失败」去显示。见 syncCredentialBestEffort 的处理。
	if restDataProviders[cred.Provider] {
		return 0, res, nil
	}
	name, ok := dataProviderMCPNames[cred.Provider]
	if !ok {
		return 0, res, fmt.Errorf("provider %q 没有 MCP 映射", cred.Provider)
	}
	if s == nil || s.m == nil || s.m.pg == nil {
		return 0, res, errors.New("PostgreSQL 未连接")
	}

	// 全量 secret：**唯一**读 token 的地方，读完只用于构造 header。
	// 绝不能把 token 放进 res / 日志 / 错误（见 db.GetMarketCredentialSecrets 的注释）。
	token, _, _, _, err := s.m.pg.GetMarketCredentialSecrets(cred.ID)
	if err != nil {
		return 0, res, fmt.Errorf("读取凭证失败: %w", err)
	}
	token = strings.TrimSpace(token)

	url := strings.TrimSpace(cred.BaseURL)
	if url == "" {
		// 用户没填也没预填（脚本调用/旧数据）→ 用默认 endpoint。
		// 与 meta 的 Fields[].Default 同源（dataProviderEndpoints），不会漂移。
		url = dataProviderEndpoints[cred.Provider]
	}
	if url == "" {
		return 0, res, fmt.Errorf("provider %q 没有默认 endpoint，且凭证未填 base_url", cred.Provider)
	}

	// headers 就是 mcp_servers.env —— http 传输下它被直接当 HTTP header 用。
	headers := map[string]string{}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	envRaw, mErr := json.Marshal(headers)
	if mErr != nil {
		return 0, res, fmt.Errorf("序列化 MCP header 失败: %w", mErr)
	}

	row := &db.MCPServer{
		Name:      name,
		Transport: "http",
		Env:       json.RawMessage(envRaw),
		URL:       url,
		// 凭证页保存即代表「我要用这家」，所以建出来就是启用状态。
		// 停用请到 MCP 页关 —— 那里是「开关」，凭证页是「配置」。
		Enabled: true,
	}
	id, err := s.m.pg.UpsertMCPByName(row)
	if err != nil {
		return 0, res, fmt.Errorf("写入 MCP server 失败: %w", err)
	}
	row.ID = id

	res.ServerID = id
	res.ServerName = name
	res.URL = url
	res.Anonymous = token == ""

	// discover：拉工具清单写 mcp_tools_cache。失败只记账，不返回 error ——
	// 行已经建好了，用户可以在 MCP 页重试。
	dctx, cancel := context.WithTimeout(ctx, mcCredSyncTimeout)
	defer cancel()
	if dErr := s.discoverAndCacheMCP(dctx, row); dErr != nil {
		// 脱敏：远端错误里可能回显 Authorization 片段。
		res.Error = truncateAndMask(dErr.Error(), 300)
		return id, res, nil
	}
	tools, _ := s.m.pg.MCPToolNames(id)
	res.ToolCount = len(tools)
	res.Ok = true
	return id, res, nil
}

// syncCredentialBestEffort 是「保存凭证后顺带同步」的包装。
//
// # 为什么吞掉 error
//
// 保存路径已经成功（凭证落库）。这里任何失败都不该把 200 变成 500 ——
// 那会让用户以为凭证没存住。失败原因通过返回的 *MCPSyncResult.Error
// 传给前端展示（一个不阻断的黄色提示）。
//
// provider 不在映射里 / kind != data 时返回 nil（调用方据此不往响应里塞字段）。
func (s *Server) syncCredentialBestEffort(ctx context.Context, cred *db.MarketCredential) *MCPSyncResult {
	if cred == nil || cred.Kind != "data" || !isSyncedDataProvider(cred.Provider) {
		return nil
	}
	_, res, err := s.SyncMarketCredentialToMCP(ctx, cred)
	if err != nil {
		// 连 MCP 行都没建出来（DB 错误等）。仍然不阻断保存，
		// 但要把原因说清 —— 否则前端只看到「没同步」，无从排查。
		return &MCPSyncResult{
			ServerName: dataProviderMCPNames[cred.Provider],
			Error:      truncateAndMask(err.Error(), 300),
		}
	}
	return &res
}
