package server

// 行情渠道凭证的 HTTP API。
//
// # 安全约束（不可妥协）
//
//  1. api_key / api_secret / passphrase **永远不出现在任何 GET 响应、日志、错误信息里**
//  2. 验证失败时 verify_error 必须已脱敏 —— CLI stderr 可能回显 key 片段
//  3. DELETE 直接物理删除，不留审计残留
//
// # 空值语义（与 setLLM 一致）
//
// 三个敏感字段：空串 = 保留旧值；db.ClearMarker = 显式清空；其他 = 新值。
//
// 为什么需要 ClearMarker：前端不回显 key，用户只改 base_url 时那次提交里
// key 字段就是空的。如果空串表示清空，改任何字段都会把 key 清掉。

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/redact"
)

// listMarketCredentials GET /api/market/credentials
func (s *Server) listMarketCredentials(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	list, err := pg.ListMarketCredentials()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// ?kind= 过滤（data / trade）。不传则返回全部。
	//
	// # 为什么在后端过滤而不是让前端筛
	//
	// 前端的两个 Tab 各自只关心一半数据，但它仍需要全量来算「每个 Tab 有几条」。
	// 所以前端通常不传 kind（一次拿全），而 ?kind= 是给「只想拉一类」
	// 的调用方（如脚本）准备的 —— 成本一行，省掉一次无用的序列化。
	if kind := strings.TrimSpace(r.URL.Query().Get("kind")); kind != "" {
		if kind != "data" && kind != "trade" {
			writeErr(w, 400, "kind 必须是 data 或 trade")
			return
		}
		filtered := make([]*db.MarketCredential, 0, len(list))
		for _, c := range list {
			if c.Kind == kind {
				filtered = append(filtered, c)
			}
		}
		list = filtered
	}
	if list == nil {
		list = []*db.MarketCredential{}
	}
	// 响应必须同时带 credentials 与 meta.providers —— 前端 `api.marketCredentials()`
	// 解析的是 `{credentials, meta:{providers}}`（见 web/src/lib/types.ts 的
	// MarketCredentialsResp）。
	//
	// # 为什么也在这里给 meta（明明有独立的 /meta 端点）
	//
	// 前端凭证页加载时只调这一个接口：它需要「列表 + 渠道字段规格」一起到位，
	// 否则表单要先渲染出空渠道下拉、再等第二个请求回来才填充（闪一下）。
	//
	// ⚠️ 这里曾经只返回裸数组 `[]`，而前端按对象解析 → `r.meta?.providers`
	// 恒为 undefined → 渠道下拉永远是空的 → 用户连「新增凭据」都点不出来。
	// 两侧的契约在同一次提交里被改得不一致，而没有任何测试跨这道边界。
	// 所以下面那条 httptest 断言（market_credentials_resp_test.go）测的是
	// **响应形状本身**，不是「函数返回了数据」。
	writeJSON(w, 200, map[string]any{
		"credentials": list,
		"meta":        map[string]any{"providers": marketProviders},
	})
}

// saveMarketCredential POST /api/market/credentials
func (s *Server) saveMarketCredential(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var body struct {
		// ID 非空 = 按主键更新（允许改 account_label）。
		// 为空 = 按 (provider, account_label) 定位。
		//
		// # 为什么需要这个字段
		//
		// account_label 参与 UNIQUE 约束。用户「重命名」时传的是新 label，
		// 按 label 查会找不到原记录 → 新建一条，旧的还在。
		// 用户以为在重命名，结果多出一条重复的、旧的还占着位置。
		//
		// id 是不变的身份，label 只是它的一个属性。
		ID       *int64 `json:"id"`
		Provider string `json:"provider"`
		// Kind 是凭据用途：'data' | 'trade'。新建时按 meta 的 Purpose 传；
		// 编辑时可以不传（db 层按「空串=没提这件事」处理）。
		Kind         string `json:"kind"`
		AccountLabel string `json:"account_label"`
		APIKey       string `json:"api_key"`
		APISecret    string `json:"api_secret"`
		Passphrase   string `json:"passphrase"`
		// AuthToken 是六家数据源 provider 的凭据字段 key（meta 里
		// `{Key: "auth_token", Storage: "secret"}`），落到 api_key 列。
		//
		// # 为什么需要这个别名
		//
		// 前端的 buildPayload 按 meta 的 fields[].key 逐字段提交，不硬编码任何
		// 渠道知识（见 credentials/page.tsx 的注释）。六家的 key 叫 auth_token，
		// 而 db 的列叫 api_key。只接 api_key 的话 token 会被**静默丢弃** ——
		// 凭证存成空串、MCP 行建出来但没有 Authorization header、远端 401，
		// 而保存接口返回 200。用户看到「已保存」，实际什么都没配上。
		//
		// # 为什么复用 api_key 列而不是加一列
		//
		// 「这条凭证用来认证的那个串」语义相同，只是不同 provider 的叫法不同
		// （okx 叫 api_key、iFinD 叫 auth_token）。加列会让 verify/sync 两处
		// 都要判断「这次该读哪一列」，而它们只需要「那个 token」。
		//
		// 同时传时 api_key 优先（okx/fosun 的老路径不变）。
		AuthToken string `json:"auth_token"`
		BaseURL   string `json:"base_url"`
		// Endpoint 是数据源类 provider 的 meta 字段 key（见 marketProviders
		// 里 `{Key: "endpoint", Storage: "column"}`）。
		//
		// # 为什么两个 JSON 名都要接
		//
		// 前端（web/src/app/(main)/system/credentials/page.tsx）按 meta 的
		// fields[].key 逐字段提交 —— 那六家的 key 叫 `endpoint`，不是 `base_url`。
		// 只接 base_url 会让用户填的 URL 被静默丢弃（base_url 落成空串），
		// 然后同步用默认 endpoint 连 —— 用户明明改了地址却没生效。
		//
		// 两个名字指同一列（base_url）。同时传时 Endpoint 优先：它更具体。
		Endpoint string `json:"endpoint"`
		Enabled  *bool  `json:"enabled"`
		// Extra 是 provider 特有的非敏感字段（如 fosun 的 apply_account_id）。
		//
		// nil（字段省略）= 保留旧值；{} = 显式清空。
		// 这与三个敏感字段的「空串=保留」是不同机制，因为 map 的零值
		// （nil）本身就能表达「未提供」，不必借道哨兵。
		Extra map[string]string `json:"extra"`
		// CreateOnly=true 时撞名报 409（前端「新增」流程）。
		// false/省略 = 覆盖（前端「编辑」流程）。
		CreateOnly bool `json:"create_only"`
	}
	if err := decode(r, &body); err != nil {
		// 注意：这里的 err 可能含请求体片段（json 解码错误会带原始片段）。
		// 过一遍 Mask，防止「api_key 格式不对」时把 key 回显出去。
		writeErrCode(w, 400, "请求体格式错误", "BAD_REQUEST")
		return
	}

	in := db.MarketCredentialInput{
		Provider:     body.Provider,
		Kind:         body.Kind,
		AccountLabel: body.AccountLabel,
		APIKey:       firstNonEmpty(body.APIKey, body.AuthToken),
		APISecret:    body.APISecret,
		Passphrase:   body.Passphrase,
		BaseURL:      firstNonEmpty(body.Endpoint, body.BaseURL),
		Enabled:      body.Enabled,
		Extra:        body.Extra,
		CreateOnly:   body.CreateOnly,
	}

	// id 优先：按主键更新时忽略 create_only（按 id 更新不会跟自己冲突）。
	var cred *db.MarketCredential
	var err error
	if body.ID != nil {
		cred, err = pg.UpdateMarketCredentialByID(*body.ID, in)
	} else {
		cred, err = pg.UpsertMarketCredential(in)
	}
	if err != nil {
		switch {
		case errors.Is(err, db.ErrCredentialExists):
			writeErrCode(w, 409, "该 provider + 账户标签已存在", "CREDENTIAL_EXISTS")
		case errors.Is(err, db.ErrCredentialLabelConflict):
			writeErrCode(w, 409, "该账户标签已被另一条凭证占用", "LABEL_CONFLICT")
		case errors.Is(err, db.ErrCredentialProviderImmutable):
			writeErrCode(w, 400, "凭证的 provider 不可修改（请删除后重建）", "PROVIDER_IMMUTABLE")
		case errors.Is(err, db.ErrCredentialKindImmutable):
			writeErrCode(w, 400, "凭证的用途（kind）不可修改 —— 如需更换用途，请删除后重建", "KIND_IMMUTABLE")
		case errors.Is(err, db.ErrCredentialNotFound):
			writeErrCode(w, 404, "凭证不存在", "CREDENTIAL_NOT_FOUND")
		default:
			writeErrCode(w, 500, redact.Mask(err.Error()), "INTERNAL")
		}
		return
	}
	// 返回的是 GetMarketCredential 的结果 —— 只含 hint，不含全量 key。
	//
	// # 为什么这里要顺带同步 MCP
	//
	// 数据源类 provider（ifind / neodata / …）的凭证对用户的意义不是「存了一条
	// 记录」，而是「这家工具能用了」。而「能用」需要 mcp_servers 里有对应行
	// （见 mcp_credential_sync.go 顶部）。把同步挂在保存路径上，用户只在一个
	// 地方配置，且保存完立刻就知道「连上了没、发现了几个工具」。
	//
	// 同步失败**不改变状态码**：凭证已经落库，返回 500 只会让用户重填一遍。
	if syncRes := s.syncCredentialBestEffort(r.Context(), cred); syncRes != nil {
		// OKX 交易凭证变更后必须刷新 CLI 注入的 key —— 否则用户改了凭证
		// 得重启进程才生效，而「保存成功」的提示会让他以为已经生效了。
		// 这正是「验证通过、交易报错」那类事故的温床。
		s.refreshOkxCredentialIfNeeded(cred)
		writeJSON(w, 200, credentialWithSync(cred, syncRes))
		return
	}
	s.refreshOkxCredentialIfNeeded(cred)
	writeJSON(w, 200, cred)
}

// refreshOkxCredentialIfNeeded 在 OKX 交易凭证变更后刷新 CLI 的注入凭证。
//
// # 为什么需要这一步（而不是只在启动时注入）
//
// 启动注入解决「进程起来时 key 是旧的」。但用户在凭证页改 key 之后，
// 内存里的 OkxClient 仍是旧值 —— 界面显示「已保存」而交易仍用旧 key，
// 报错还指向 OKX 侧（50119 key doesn't exist），排查时根本想不到是缓存。
//
// # 为什么只处理 okx/trade
//
// 只有 OKX 走 CLI（读环境变量）。其他 provider 要么走 MCP（mcp_servers 行，
// 已在 syncCredentialBestEffort 里同步），要么走 REST（每次调用现读表）。
func (s *Server) refreshOkxCredentialIfNeeded(cred *db.MarketCredential) {
	if cred == nil || cred.Provider != "okx" || cred.Kind != "trade" || s.okxCli == nil {
		return
	}
	// 重新走一遍全局注入：它会挑第一条启用的 okx/trade 凭证。
	// 不直接传 cred，是因为用户可能刚禁用/删除了这条，而仍有另一条可用。
	s.injectOkxCredential(s.okxCli)
}

// firstNonEmpty 返回第一个非空（trim 后）的字符串。
//
// # 为什么不用 strings.TrimSpace(a+b)
//
// 需要保留原始值（含中间空格的 URL 不该被拼坏）。
// 只用于 base_url / endpoint 这种「同一列的两个输入名」。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// credentialWithSync 把凭证 + 同步结果拼成一个响应对象。
//
// # 为什么保持凭证字段在顶层
//
// 前端与既有测试都把响应直接当 MarketCredential 解析（多出的字段会被忽略）。
// 包一层 {"credential": …, "mcp_sync": …} 会打断所有调用方，
// 而这个改动本身与响应结构无关。
func credentialWithSync(cred *db.MarketCredential, syncRes *MCPSyncResult) map[string]any {
	out := map[string]any{"mcp_sync": syncRes}
	raw, err := json.Marshal(cred)
	if err != nil {
		// 极不可能（MarketCredential 全是基本类型）。退化成只返回 id 也不丢信息。
		return map[string]any{"id": cred.ID, "mcp_sync": syncRes}
	}
	_ = json.Unmarshal(raw, &out)
	// 重新赋值：json.Unmarshal 到同一个 map 不会覆盖已有键，但为了可读性显式写一次。
	out["mcp_sync"] = syncRes
	return out
}

// deleteMarketCredential DELETE /api/market/credentials/{id}
func (s *Server) deleteMarketCredential(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "id 必须为正整数")
		return
	}
	if err := pg.DeleteMarketCredential(id); err != nil {
		if err == db.ErrCredentialNotFound {
			writeErr(w, 404, "凭证不存在")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// verifyMarketCredential POST /api/market/credentials/{id}/verify
//
// # 现状：只有 okx 能真验证，且只能验证「CLI 能跑」
//
// OKX 的行情/指标类无需凭证，账户类需 OAuth（`okx-auth login`）。
// 所以我们能验证的是「这个 provider 的 CLI 在当前环境可用」，
// 而不是「这组 key 有效」—— 后者需要 OAuth，本次不做。
//
// 其他 provider 返回 501：不假装能验证，避免用户以为凭证没问题。
func (s *Server) verifyMarketCredential(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "id 必须为正整数")
		return
	}
	cred, err := pg.GetMarketCredential(id)
	if err != nil {
		if err == db.ErrCredentialNotFound {
			writeErr(w, 404, "凭证不存在")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}

	// 数据源类（MCP 直连）：验证 = 重新同步 + 拉工具清单。
	//
	// # 为什么验证和保存走同一条路径
	//
	// 「验证」要回答的问题是「agent 现在能不能调这家」。而那个答案完全由
	// mcp_servers 那一行 + 一次 discover 决定 —— 与保存时做的事一模一样。
	// 另写一套验证逻辑必然会分叉（比如验证用 raw token 调、保存用 header map
	// 调，某天 header 格式改了只有一边跟着改）。
	if cred.Kind == "data" && isSyncedDataProvider(cred.Provider) {
		s.verifyDataCredentialViaMCP(w, r, pg, id, cred)
		return
	}

	// REST 类数据源（妙想）：验证 = 用它自己的 API 打一次轻量查询。
	//
	// # 为什么不复用 MCP 验证路径
	//
	// 妙想没有 MCP 端点（见 restDataProviders 的注释）。把它塞进 MCP 验证会
	// 去 POST 一个不存在的 MCP URL，得到的 404 与「key 无效」完全无关 ——
	// 用户会照着这个错误去换 key，越换越错。
	//
	// 所以验证必须用**真实的业务 API**：一次 `toolQuery` 查询。
	// 那既是「key 有效吗」也是最接近 agent 实际调用形态的探测。
	if cred.Provider == "mx" {
		s.verifyMXCredential(w, r, pg, id, cred)
		return
	}

	if cred.Provider != "okx" {
		writeErr(w, 501, fmt.Sprintf(
			"该渠道（%s）暂不支持在线验证 —— 目前只有 okx 与数据源类有可执行的验证路径",
			cred.Provider))
		return
	}

	// 取全量三元组 + extra（extra 里有 demo 开关）。
	//
	// # 为什么必须用用户填的 key
	//
	// 旧实现跑 `okx account balance` —— 那测的是 **CLI 自己的 OAuth 登录态**
	// （`okx-auth login`），与表里这把 key 完全无关。后果：
	//   - key 完全错误时也可能「验证通过」（CLI 登录着）
	//   - key 完全正确时必然失败（CLI 没登录），报「Not logged in」
	//   - CLI 的版本升级提示会被当成错误落进 verify_error
	//
	// 所以验证必须**用表里存的 key 亲自调一次 OKX REST**。
	apiKey, apiSecret, passphrase, extra, kErr := pg.GetMarketCredentialSecrets(id)
	if kErr != nil {
		writeErr(w, 500, "读取凭证失败")
		return
	}
	demo := strings.EqualFold(strings.TrimSpace(extra["demo"]), "true")

	ok2, verifyErr := verifyOkxREST(r.Context(), verifyOkxParams{
		APIKey: apiKey, APISecret: apiSecret, Passphrase: passphrase,
		Demo: demo, BaseURL: cred.BaseURL,
	})
	if ok2 {
		if err := pg.SetCredentialVerifyResult(id, true, ""); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		// 重新读一次，返回最新的 verified_at。
		updated, err := pg.GetMarketCredential(id)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{
			"ok":           true,
			"api_key_hint": db.MaskAPIKey(apiKey), // 即使成功也只回显 hint
			"credential":   updated,
		})
		return
	}

	// 失败：verify_error 必须脱敏后再存。
	//
	// 这里是最容易漏的一处 —— CLI 的 stderr 可能形如
	// `error: invalid api key sk-abcdef123456`, 直接落库就把 key 存进 DB 了
	// （虽然那张表的这一列不是「敏感列」，但它会被 GET 返回给前端）。
	safeErr := truncateAndMask(verifyErr, 200)
	if err := pg.SetCredentialVerifyResult(id, false, safeErr); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	updated, err := pg.GetMarketCredential(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":           false,
		"api_key_hint": db.MaskAPIKey(apiKey),
		"error":        safeErr,
		"credential":   updated,
	})
}

// verifyDataCredentialViaMCP 是数据源类凭证的验证实现：同步 MCP 行 + 拉工具清单。
//
// 返回体与 okx 的验证保持一致形态（ok / credential / error），
// 额外带 mcp_sync 与 tool_count —— 前端据此显示「已连接，发现 N 个工具」。
//
// 验证结果写回 verify_error（**成功时清空、失败时记已脱敏原因**），
// 与 SetCredentialVerifyResult 的语义一致：verified_at 只在成功时前进。
func (s *Server) verifyDataCredentialViaMCP(w http.ResponseWriter, r *http.Request,
	pg *db.DB, id int64, cred *db.MarketCredential) {
	_, syncRes, err := s.SyncMarketCredentialToMCP(r.Context(), cred)
	if err != nil {
		// 连 MCP 行都没建出来 —— 这是真错误（DB/映射/参数），记进 verify_error。
		safeErr := truncateAndMask(err.Error(), 300)
		_ = pg.SetCredentialVerifyResult(id, false, safeErr)
		updated, _ := pg.GetMarketCredential(id)
		writeJSON(w, 200, map[string]any{
			"ok":         false,
			"error":      safeErr,
			"credential": updated,
			"mcp_sync":   &MCPSyncResult{ServerName: dataProviderMCPNames[cred.Provider], Error: safeErr},
		})
		return
	}

	if syncRes.Ok {
		if e := pg.SetCredentialVerifyResult(id, true, ""); e != nil {
			writeErr(w, 500, e.Error())
			return
		}
	} else {
		// 行建出来了但 discover 失败（通常是对端 401/超时）。
		// 仍然返回 200 + ok=false：这不是服务端错误，是「这家连不上」。
		_ = pg.SetCredentialVerifyResult(id, false, syncRes.Error)
	}
	updated, _ := pg.GetMarketCredential(id)
	writeJSON(w, 200, map[string]any{
		"ok":         syncRes.Ok,
		"error":      syncRes.Error,
		"tool_count": syncRes.ToolCount,
		"anonymous":  syncRes.Anonymous,
		"credential": updated,
		"mcp_sync":   &syncRes,
	})
}

// verifyMXCredential 用妙想自己的 API 验证 key 是否可用。
//
// # 为什么验证要真发一次业务查询
//
// 「验证」要回答的问题是「agent 现在能不能用它取数」。而那个能力完全由
// 「key 有效 + 端点可达 + 响应能解析」三者共同决定 —— 只有一个真实的
// `toolQuery` 能同时覆盖这三点。
//
// 只检查「key 非空」等于什么都没验；只 ping 域名能通过但 key 是错的。
//
// # 查询内容为什么是「贵州茅台最新价」
//
// 它是最稳定的常识查询：标的长期存在、数据每天都有、返回结构简单。
// 用一个固定查询而不是用户填的标的，是因为**验证的是通道不是数据** ——
// 用户在这里没填标的，也不该为了验证凭证去填一个。
//
// # 失败分类
//
//   - key 无效 → MX_AUTH_FAILED → 「API Key 无效或已过期」
//   - 网络/超时 → 保留原始错误 → 「连不上，可能是网络问题」（不误导用户换 key）
//
// 两者混成一条会让用户按错误方向修。
func (s *Server) verifyMXCredential(w http.ResponseWriter, r *http.Request,
	pg *db.DB, id int64, cred *db.MarketCredential) {
	key, _, _, _, err := pg.GetMarketCredentialSecrets(id)
	if err != nil {
		safeErr := truncateAndMask(err.Error(), 300)
		_ = pg.SetCredentialVerifyResult(id, false, safeErr)
		updated, _ := pg.GetMarketCredential(id)
		writeJSON(w, 200, map[string]any{"ok": false, "error": safeErr, "credential": updated})
		return
	}
	key = strings.TrimSpace(key)
	if key == "" {
		const msg = "API Key 为空 —— 先在妙想 Skills 页面获取 key 后填入"
		_ = pg.SetCredentialVerifyResult(id, false, msg)
		updated, _ := pg.GetMarketCredential(id)
		writeJSON(w, 200, map[string]any{"ok": false, "error": msg, "credential": updated})
		return
	}

	base := strings.TrimSpace(cred.BaseURL)
	if base == "" {
		base = "https://mkapi2.dfcfs.com/finskillshub/api/claw"
	}
	// 只做一次最小查询：验证的是通道，不是数据。
	ok, errMsg := probeMX(r.Context(), base, key, "贵州茅台最新价")

	if ok {
		if e := pg.SetCredentialVerifyResult(id, true, ""); e != nil {
			writeErr(w, 500, e.Error())
			return
		}
	} else {
		_ = pg.SetCredentialVerifyResult(id, false, errMsg)
	}
	updated, _ := pg.GetMarketCredential(id)
	// 注意：这里返回 200 + ok=false（业务失败），不是 5xx ——
	// 「key 无效」是用户可修的状态，不是服务端故障。
	writeJSON(w, 200, map[string]any{
		"ok":         ok,
		"error":      errMsg,
		"credential": updated,
	})
}

// probeMX 打一次妙想的行情查询，返回 (ok, 已脱敏的错误描述)。
//
// 独立成函数是为了能单测（不必构造整个 Server）。
func probeMX(ctx context.Context, baseURL, apiKey, query string) (bool, string) {
	body, _ := json.Marshal(map[string]string{"toolQuery": query})
	url := strings.TrimRight(baseURL, "/") + "/query"

	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		// 不把 apiKey 放进任何错误串。
		return false, "构造请求失败：" + truncateAndMask(err.Error(), 200)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// 网络层失败 ≠ key 无效。文案必须区分，否则用户会去换一个本来正确的 key。
		return false, "连不上妙想 API（网络或 DNS 问题，不是 key 问题）：" + truncateAndMask(err.Error(), 200)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return false, "API Key 无效或已过期（HTTP " + strconv.Itoa(resp.StatusCode) + "）"
	}
	if resp.StatusCode != http.StatusOK {
		return false, "妙想 API 返回 HTTP " + strconv.Itoa(resp.StatusCode) + "：" + truncateAndMask(string(raw), 200)
	}

	// 业务状态：status != 0 表示失败（HTTP 仍是 200）。
	var parsed struct {
		Status  int    `json:"status"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return false, "响应不是合法 JSON：" + truncateAndMask(string(raw), 200)
	}
	if parsed.Status != 0 {
		msg := strings.TrimSpace(parsed.Message)
		if msg == "" {
			msg = "status=" + strconv.Itoa(parsed.Status)
		}
		return false, "妙想返回业务错误：" + truncateAndMask(msg, 200)
	}
	return true, ""
}

// verifyOkxParams 是 OKX REST 验证需要的入参。
//
// 独立成结构体而不是散参数：五个 string 顺序摆错（secret 和 passphrase 对调）
// 不会编译失败，只会得到一个「看起来像 key 无效」的 401 —— 那种错误查起来最费时。
type verifyOkxParams struct {
	APIKey     string
	APISecret  string
	Passphrase string
	// Demo=true 时带 x-simulated-trading: 1，打到模拟盘环境。
	//
	// # 为什么必须显式传而不是猜
	//
	// 实盘与模拟盘的 key **互不通用**，OKX 对错配返回 50101
	// 「APIKey does not match current environment」。
	// 靠 key 长相去猜环境是不可能的（两者格式一致），所以只能由用户声明。
	Demo bool
	// BaseURL 为空时用 https://www.okx.com。
	BaseURL string
}

// verifyOkxREST 用表里存的 API Key 调一次 OKX REST，验证三元组是否可用。
//
// # 为什么弃用 CLI 路径（原 verifyOkxCLI）
//
// 原实现跑 `okx account balance`，那是 **CLI 的 OAuth 登录态**，
// 与用户在凭证页填的 api_key/api_secret/passphrase 毫无关系。实测事故：
//   - 用户填了正确的模拟盘 key，验证却报
//     「Not logged in — run `okx-auth login` first」
//   - 同一段 stderr 里混着 CLI 的版本升级提示
//     「Update available ... 1.4.8 -> 1.5.0」，被当成验证失败原因存进 verify_error
//
// 验证的语义应该是「这把 key 能不能用」，所以必须用它亲自发一次请求。
//
// # 为什么选 /api/v5/account/balance
//
// 它需要签名（能验出签名错）且是只读（不会有副作用）。
// 不能用 /api/v5/public/* —— 那些免签名，任何 key 都能通过，验不出真伪。
//
// 返回 (ok, 已脱敏的错误描述)。
func verifyOkxREST(parent context.Context, p verifyOkxParams) (bool, string) {
	key := strings.TrimSpace(p.APIKey)
	secret := strings.TrimSpace(p.APISecret)
	pass := strings.TrimSpace(p.Passphrase)
	switch {
	case key == "":
		return false, "API Key 为空"
	case secret == "":
		return false, "API Secret 为空"
	case pass == "":
		return false, "Passphrase 为空"
	}

	base := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if base == "" {
		base = "https://www.okx.com"
	}
	const path = "/api/v5/account/balance"

	// OKX 签名：base64(HMAC-SHA256(secret, timestamp + METHOD + requestPath + body))
	// 时间戳格式必须是 ISO8601 带毫秒：2006-01-02T15:04:05.000Z
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	prehash := ts + http.MethodGet + path
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(prehash))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return false, "构造请求失败：" + truncateAndMask(err.Error(), 200)
	}
	req.Header.Set("OK-ACCESS-KEY", key)
	req.Header.Set("OK-ACCESS-SIGN", sign)
	req.Header.Set("OK-ACCESS-TIMESTAMP", ts)
	req.Header.Set("OK-ACCESS-PASSPHRASE", pass)
	if p.Demo {
		req.Header.Set("x-simulated-trading", "1")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// 网络层失败 ≠ key 无效。文案必须区分 —— 否则用户会去换一把本来正确的 key。
		return false, "连不上 OKX（网络或代理问题，不是 key 问题）：" + truncateAndMask(err.Error(), 200)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var parsed struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal(raw, &parsed)

	if resp.StatusCode == http.StatusOK && parsed.Code == "0" {
		return true, ""
	}

	// OKX 的业务错误码里，这几条是「配置问题」而不是「上游故障」，
	// 必须给出可操作的文案，否则用户只能看到一个裸数字。
	switch parsed.Code {
	case "50101":
		// 这条最容易被误读成「key 无效」，实际是环境错配。
		if p.Demo {
			return false, "API Key 与当前环境不匹配：已勾选「模拟盘」但 OKX 认为这是实盘 key。" +
				"请确认 key 来源于 OKX 模拟盘（Demo Trading）页面，或取消勾选「模拟盘」"
		}
		return false, "API Key 与当前环境不匹配：当前未勾选「模拟盘」但 OKX 认为这是模拟盘 key。" +
			"请在「模拟盘（Demo Trading）」处勾选后重试"
	case "50111":
		return false, "API Key 无效（50111）—— 请确认 key 与 passphrase 抄写正确"
	case "50113":
		return false, "签名验证失败（50113）—— 通常是 API Secret 抄错或含多余空格"
	case "50001":
		return false, "API Key 已过期或被禁用（50001）—— 请到 OKX 重新生成"
	case "50102":
		return false, "请求时间戳过期（50102）—— 本机系统时间与标准时间偏差过大，请校准"
	case "50103":
		return false, "Passphrase 不正确（50103）"
	case "50105", "50106":
		return false, "该 API Key 权限不足（" + parsed.Code + "）—— 请在 OKX 勾选「读取」权限"
	}

	msg := strings.TrimSpace(parsed.Msg)
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	if msg == "" {
		msg = "HTTP " + strconv.Itoa(resp.StatusCode)
	}
	return false, "OKX 返回错误（code=" + parsed.Code + "）：" + truncateAndMask(msg, 200)
}

// truncateAndMask 截断 + 脱敏。
//
// # 为什么需要本地补一层 key 形态脱敏
//
// redact.Mask 认的是「带 key 名的赋值形态」（`api_key=xxx`、`"api_key":"xxx"`），
// 而 CLI 的 stderr 常是**裸值形态**：
//
//	error: invalid api key sk-abcdef1234567890abcdef for account
//
// 这种句子里的 key 前面没有 `=` 也没有引号，Mask 认不出来。
// 所以这里额外扫一遍「疑似凭证的裸 token」：长串 + 含 sk-/ghp_/AKIA 等前缀，
// 或者长串本身高熵（字母数字混合且长度超过常见单词）。
//
// # 顺序：先截断到 2 倍上限 → 脱敏 → 再截到目标长度
//
// 先脱敏再截断可能把掩码截成半截（"***ab" 看起来像原文）。
// 先截断再脱敏可能让「截断点正好在 key 中间」使剩余片段不再匹配 ——
// 所以截断留 2 倍余量，让完整的 key 有机会被识别。
func truncateAndMask(s string, limit int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > limit*2 {
		s = s[:limit*2]
	}
	// 先转义输入里本来就有的 <redacted> 字面量。
	//
	// # 为什么需要这一步
	//
	// 如果 CLI stderr 里恰好含 "<redacted>"（可能来自上游自己的脱敏，
	// 也可能是恶意构造），那它会被下游误认为「我们脱敏过了」。
	// 改写成一个不会被误认的形式，让「我们的脱敏」和「原本就有」可区分。
	s = strings.ReplaceAll(s, redactedMark, "<redacted-literal>")
	s = redact.Mask(s)
	s = maskBareTokens(s)
	if len(s) > limit {
		s = s[:limit]
	}
	return s
}

// redactedMark 是脱敏占位符。
//
// # 为什么不是 "***"
//
// "***" 是脱敏常用占位符，**它本身可能出现在正常凭证或 base_url 中**
// （如 `https://api.xxx.com/***/v1`）。替换后再用 strings.Contains 断言
// 「脱敏生效了吗」会命中误报 —— 测试假通过。
//
// redact.Mask 内部用的正是 "***"，所以即使 key 里真含 "***"，
// 脱敏后的文本还会包含 "***"，断言依然是绿的。
//
// `<redacted>` 用尖括号，**几乎不可能出现在 API key / base_url / PEM 里**，
// 语义也明确（「这里被抹掉了」）而不是模糊（「这里有三个星」）。
const redactedMark = "<redacted>"

// maskBareTokens 把「疑似凭证的裸 token」替换成 `<redacted>`。
//
// 判定口径（保守，宁可多遮）：
//   - 已知前缀：sk- / pk- / ghp_ / gho_ / AKIA / xoxb- / xoxp-
//   - 或：长度 >= 32 且同时含字母和数字（高熵 token 的典型形态）
//
// 为什么「宁可多遮」：遮错了只是信息少一点，没遮到是凭证泄露。
func maskBareTokens(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		// 找 token 边界（空白/标点）
		j := i
		for j < len(s) && isTokenChar(s[j]) {
			j++
		}
		if j == i {
			b.WriteByte(s[i])
			i++
			continue
		}
		tok := s[i:j]
		if looksLikeSecret(tok) {
			b.WriteString(redactedMark)
		} else {
			b.WriteString(tok)
		}
		i = j
	}
	return b.String()
}

// isTokenChar 判断字符是否可能是 token 的一部分。
func isTokenChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
		c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '+'
}

// looksLikeSecret 判断一个 token 是否像凭证。
//
// # 判定分三档，从强到弱
//
//  1. **已知前缀**（sk-/pk-/sec-/pass-/ghp_/AKIA/...）——
//     无论长度，命中即遮。这些前缀的语义就是「这是密钥」。
//  2. **字母数字混合 + 长度 >= 20** —— 中等熵。
//     覆盖 `sec-charlie-9999` 这类「有明确前缀语义但我们没想到」的。
//     阈值 20 而不是 32：实测 API secret 常见 16-32 位，
//     32 会漏掉一半以上（这是本测试抓到的真实漏洞）。
//  3. **纯长串 >= 40** —— 兜底高熵（base64/PEM 片段）。
//
// # 为什么「宁可多遮」
//
// 遮错了只是错误信息少一点信息，没遮到是凭证泄露进 DB 且被 GET 返回。
// 代价不对称，所以阈值倾向保守。
func looksLikeSecret(tok string) bool {
	if len(tok) < 12 {
		return false
	}
	// 第 1 档：已知前缀
	for _, p := range []string{
		"sk-", "pk-", "sec-", "pass-", "key-", "tok-",
		"ghp_", "gho_", "AKIA", "xoxb-", "xoxp-",
	} {
		if strings.HasPrefix(tok, p) {
			return true
		}
	}
	// 以下两档都要先看是否字母数字混合
	var hasLetter, hasDigit bool
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			hasLetter = true
		case c >= '0' && c <= '9':
			hasDigit = true
		}
	}
	// 第 2 档：中等长度 + 混合
	//
	// **阈值 16 而不是 20/32**：实测 API secret 常见 16-32 位。
	// 定在 20 会让 16-19 位的从缝里漏过去（这是测试抓到的真实漏洞：
	// `sec-charlie-9999` 长 16，既无已知前缀也不够 32，被原样存进了 verify_error）。
	//
	// 为什么不降到 8：正常 ID、时间戳、UUID 片段都是 16 位左右的字母数字，
	// 降到 8 会把它们也遮掉，错误信息就没用了。
	//
	// **低于 16 位的凭证必须靠已知前缀覆盖，不能靠长度猜** ——
	// 长度判定的信噪比在 16 以下会崩。
	if len(tok) >= 16 && hasLetter && hasDigit {
		return true
	}
	// 第 3 档：长串兜底（不要求混合，base64 可能全字母）
	return len(tok) >= 40
}
