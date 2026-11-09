package server

// 「行情凭证 → MCP server」同步层的测试。
//
// # 这些测试守的四条不变量
//
//  1. **幂等**：同一 provider 保存 N 次只留一行 mcp_servers（否则 agent 会
//     连上两条，其中一条永远 401）。
//  2. **header 形状**：token 必须变成 `Authorization: Bearer <token>`。
//     env map 就是 header map（见 mcpdiscover.go:36），形状错了对端 401，
//     而本地不会有任何报错 —— 只有这条测试能抓到。
//  3. **匿名不阻断**：没有 token 时仍然建行并尝试连接（`Anonymous: true`，
//     env 是 `{}`）。
//  4. **默认 endpoint 单一来源**：cred.BaseURL 为空时用 dataProviderEndpoints，
//     必须与 meta 里 Fields[].Default 完全一致（两份漂移会让用户看到 A、
//     实际连 B）。
//
// 注意：这些测试**不依赖真实网络**。discover 一定失败（endpoint 是假的或需要认证），
// 但失败路径正是要验证的：行已建好 + 失败被记为 Error 而不是 panic。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// TestMarketProvidersIncludeDataProviders 验证 6 家数据源 provider 都在。
//
// 2026-10-31 新增。断言的 provider 数量从 4 变 10 —— 这不是「改测试绕过」，
// 而是这条需求的核心产出：meta 里必须真的出现这六项，否则前端不渲染它们。
func TestMarketProvidersIncludeDataProviders(t *testing.T) {
	want := []string{"ifind", "fosun_data", "tdx", "neodata", "kuaicha", "westock"}
	have := map[string]*providerMeta{}
	for i := range marketProviders {
		p := &marketProviders[i]
		if p.Purpose == "data" {
			have[p.Provider] = p
		}
	}
	for _, w := range want {
		p, ok := have[w]
		if !ok {
			t.Errorf("marketProviders 里缺 %q（purpose=data），2026-10-31 新增的 6 家数据源", w)
			continue
		}
		if !p.Verifiable {
			t.Errorf("%s 应 Verifiable —— 它走 MCP 同步 + discover，能真返回工具数", w)
		}
		if !p.SupportsBaseURL {
			t.Errorf("%s 应 SupportsBaseURL —— endpoint 存在 base_url 列", w)
		}
		// 每个都必须有 auth_token（secret）+ endpoint（column，带默认值）
		var hasToken, hasEndpoint bool
		for _, f := range p.Fields {
			switch f.Key {
			case "auth_token":
				hasToken = f.Kind == "password" && f.Storage == "secret"
			case "endpoint":
				hasEndpoint = f.Storage == "column" && f.Default != ""
			}
		}
		if !hasToken {
			t.Errorf("%s 的 auth_token 必须是 password+secret", w)
		}
		if !hasEndpoint {
			t.Errorf("%s 的 endpoint 必须 storage=column 且带 default（前端据此预填）", w)
		}
	}
	// 总数：原有 4 项 + 新增 6 项。
	if len(marketProviders) != 11 {
		t.Errorf("marketProviders 应为 11 项（4 既有 + 6 MCP 数据源 + 1 妙想 REST），实际 %d", len(marketProviders))
	}
}

// TestFosunDataIsSeparateProviderKey 验证 fosun_data 是独立 key。
//
// # 为什么这条单独测
//
// 复用 `fosun` 会继承 PEM 的字段布局（server_public_key / client_private_key），
// 用户填的 token 会被塞进 server_public_key 列 —— 存进去了，看起来成功了，
// 但 MCP 那边永远 401。这类「静默错配」只能靠 key 层面隔离避免。
func TestFosunDataIsSeparateProviderKey(t *testing.T) {
	for _, p := range marketProviders {
		if p.Provider != "fosun_data" {
			continue
		}
		for _, f := range p.Fields {
			if f.Key == "server_public_key" || f.Key == "client_private_key" {
				t.Errorf("fosun_data 不该有 PEM 字段 %q —— 那是 fosun/trade 的布局", f.Key)
			}
		}
		return
	}
	t.Fatal("marketProviders 里没有 fosun_data")
}

// TestDataProviderEndpointsMatchMetaDefaults 验证默认 endpoint 单一来源。
//
// meta 的 Fields[].Default 给前端预填，dataProviderEndpoints 给后端 fallback。
// 两者不一致时用户会看到「输入框里是 A，实际连的是 B」—— 而且不会有任何报错。
func TestDataProviderEndpointsMatchMetaDefaults(t *testing.T) {
	for _, p := range marketProviders {
		if p.Purpose != "data" || !isSyncedDataProvider(p.Provider) {
			continue
		}
		want := dataProviderEndpoints[p.Provider]
		if want == "" {
			t.Errorf("%s 在 dataProviderEndpoints 里没有默认 endpoint", p.Provider)
			continue
		}
		var got string
		for _, f := range p.Fields {
			if f.Key == "endpoint" {
				got = f.Default
			}
		}
		if got != want {
			t.Errorf("%s: meta default=%q 与 dataProviderEndpoints=%q 不一致", p.Provider, got, want)
		}
	}
}

// TestDataProviderMCPNamesCoverAllProviders 验证映射覆盖。
//
// 少一项 = 那家保存后不同步，而用户看到「保存成功」。
func TestDataProviderMCPNamesCoverAllProviders(t *testing.T) {
	for _, p := range marketProviders {
		if p.Purpose != "data" {
			continue
		}
		if p.Provider == "okx" || p.Provider == "fosun" {
			// 这两家走 REST/CLI，没有 MCP endpoint —— 刻意不同步。
			continue
		}
		// REST 类数据源（妙想）同样不该有 MCP 映射 —— 它由 restDataProviders
		// 显式声明，而不是靠「映射表里碰巧没有」。
		//
		// 这里必须查 restDataProviders 而不是硬编码 "mx"：把「本该跳过」
		// 写在两处（声明 + 测试白名单）会让新增 REST 源时忘改测试而红，
		// 或忘加声明而被这条测试正确抓住 —— 后者才是我们想要的信号。
		if restDataProviders[p.Provider] {
			continue
		}
		if _, ok := dataProviderMCPNames[p.Provider]; !ok {
			t.Errorf("provider %q 是 data 类但没有 MCP name 映射", p.Provider)
		}
	}
	// iFindD-MCP 的大小写是上游自报的名字，错了会对不上号。
	if dataProviderMCPNames["ifind"] != "iFindD-MCP" {
		t.Errorf("ifind 的 MCP 名必须是 iFindD-MCP（上游大小写），实际 %q", dataProviderMCPNames["ifind"])
	}
}

// mcpSyncTestServer 起一个只带 pg 的 Server，并注册 mcp_servers 清理。
func mcpSyncTestServer(t *testing.T) (*Server, func()) {
	t.Helper()
	s, done := credTestServer(t)
	names := make([]string, 0, len(dataProviderMCPNames))
	for _, n := range dataProviderMCPNames {
		names = append(names, n)
	}
	cleanup := func() {
		for _, n := range names {
			_, _ = s.m.pg.Exec(`DELETE FROM mcp_servers WHERE name=$1`, n)
		}
		_, _ = s.m.pg.Exec(`DELETE FROM market_credentials WHERE account_label LIKE 'mcpsync-%'`)
	}
	cleanup()
	return s, func() { cleanup(); done() }
}

// TestSyncMarketCredentialToMCPIdempotent 同一凭证同步两次只留一行。
func TestSyncMarketCredentialToMCPIdempotent(t *testing.T) {
	s, done := mcpSyncTestServer(t)
	defer done()

	label := "mcpsync-idem"
	cred, err := s.m.pg.UpsertMarketCredential(db.MarketCredentialInput{
		Provider: "neodata", Kind: "data", AccountLabel: label,
		APIKey: "tok-abcdefghijklmnop", BaseURL: "http://127.0.0.1:1/proxy/mcp",
	})
	if err != nil {
		t.Fatalf("建凭证: %v", err)
	}
	defer func() { _ = s.m.pg.DeleteMarketCredential(cred.ID) }()

	ctx := context.Background()
	id1, _, err1 := s.SyncMarketCredentialToMCP(ctx, cred)
	if err1 != nil {
		t.Fatalf("首次同步: %v", err1)
	}
	id2, _, err2 := s.SyncMarketCredentialToMCP(ctx, cred)
	if err2 != nil {
		t.Fatalf("二次同步: %v", err2)
	}
	if id1 != id2 {
		t.Fatalf("幂等性破坏：两次同步得到不同 id（%d vs %d）—— 会建出两行 mcp_servers", id1, id2)
	}
	var n int
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM mcp_servers WHERE name=$1`, "neodata").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("mcp_servers 里 neodata 应有 1 行，实际 %d", n)
	}
}

// TestSyncMarketCredentialToMCPBuildsBearerHeader token 变成 Authorization header。
func TestSyncMarketCredentialToMCPBuildsBearerHeader(t *testing.T) {
	s, done := mcpSyncTestServer(t)
	defer done()

	label := "mcpsync-header"
	cred, err := s.m.pg.UpsertMarketCredential(db.MarketCredentialInput{
		Provider: "kuaicha", Kind: "data", AccountLabel: label,
		APIKey: "secret-token-12345", BaseURL: "http://127.0.0.1:1/open/mcp",
	})
	if err != nil {
		t.Fatalf("建凭证: %v", err)
	}
	defer func() { _ = s.m.pg.DeleteMarketCredential(cred.ID) }()

	if _, _, err := s.SyncMarketCredentialToMCP(context.Background(), cred); err != nil {
		t.Fatalf("同步: %v", err)
	}

	var transport, url, env string
	var enabled bool
	if err := s.m.pg.QueryRow(`SELECT transport, COALESCE(url,''), env, enabled
FROM mcp_servers WHERE name=$1`, "kuaicha-search").Scan(&transport, &url, &env, &enabled); err != nil {
		t.Fatal(err)
	}
	if transport != "http" {
		t.Errorf("transport 应为 http（mcphttp 走 streamable-http），实际 %q", transport)
	}
	if url != "http://127.0.0.1:1/open/mcp" {
		t.Errorf("url 应为用户填的值，实际 %q", url)
	}
	if !enabled {
		t.Error("保存凭证后 MCP 行应为 enabled —— 用户填 token 的意图就是要用")
	}
	var headers map[string]string
	if err := json.Unmarshal([]byte(env), &headers); err != nil {
		t.Fatalf("env 不是合法 JSON: %v（%s）", err, env)
	}
	if got := headers["Authorization"]; got != "Bearer secret-token-12345" {
		t.Fatalf("Authorization header 应为 %q，实际 %q —— 形状错了对端会 401 而本地无报错",
			"Bearer secret-token-12345", got)
	}
}

// TestSyncMarketCredentialToMCPAnonymous 无 token 时 env 为 {}，不阻断。
func TestSyncMarketCredentialToMCPAnonymous(t *testing.T) {
	s, done := mcpSyncTestServer(t)
	defer done()

	label := "mcpsync-anon"
	cred, err := s.m.pg.UpsertMarketCredential(db.MarketCredentialInput{
		Provider: "tdx", Kind: "data", AccountLabel: label,
		BaseURL: "http://127.0.0.1:1/txmcp",
	})
	if err != nil {
		t.Fatalf("建凭证: %v", err)
	}
	defer func() { _ = s.m.pg.DeleteMarketCredential(cred.ID) }()

	_, res, err := s.SyncMarketCredentialToMCP(context.Background(), cred)
	if err != nil {
		t.Fatalf("匿名同步不该返回 error（endpoint 可能免凭证）: %v", err)
	}
	if !res.Anonymous {
		t.Error("没有 token 时 res.Anonymous 应为 true")
	}
	var env string
	if err := s.m.pg.QueryRow(`SELECT env FROM mcp_servers WHERE name=$1`, "tdx-finance").Scan(&env); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(env, "Authorization") {
		t.Errorf("匿名时不该有 Authorization，实际 env=%s", env)
	}
	if res.ServerID == 0 {
		t.Error("匿名也应建出 MCP 行（ServerID 不能为 0）")
	}
}

// TestSyncMarketCredentialToMCPUsesDefaultEndpoint base_url 为空时用默认 endpoint。
func TestSyncMarketCredentialToMCPUsesDefaultEndpoint(t *testing.T) {
	s, done := mcpSyncTestServer(t)
	defer done()

	label := "mcpsync-default"
	cred, err := s.m.pg.UpsertMarketCredential(db.MarketCredentialInput{
		Provider: "westock", Kind: "data", AccountLabel: label,
		APIKey: "tok-westock-1234567890",
	})
	if err != nil {
		t.Fatalf("建凭证: %v", err)
	}
	defer func() { _ = s.m.pg.DeleteMarketCredential(cred.ID) }()

	if _, _, err := s.SyncMarketCredentialToMCP(context.Background(), cred); err != nil {
		t.Fatalf("同步: %v", err)
	}
	var url string
	if err := s.m.pg.QueryRow(`SELECT COALESCE(url,'') FROM mcp_servers WHERE name=$1`, "westock-mcp").Scan(&url); err != nil {
		t.Fatal(err)
	}
	if url != dataProviderEndpoints["westock"] {
		t.Fatalf("base_url 为空时应回落到 dataProviderEndpoints，期望 %q 实际 %q",
			dataProviderEndpoints["westock"], url)
	}
}

// TestSyncRejectsTradeKind 交易凭证不该同步。
func TestSyncRejectsTradeKind(t *testing.T) {
	s, done := mcpSyncTestServer(t)
	defer done()
	cred := &db.MarketCredential{ID: 1, Provider: "okx", Kind: "trade"}
	if _, _, err := s.SyncMarketCredentialToMCP(context.Background(), cred); err == nil {
		t.Fatal("kind=trade 应被拒绝 —— 交易通道没有 MCP 对应物")
	}
}

// TestSaveCredentialTriggersMCPSync 保存 handler 会真的建出 MCP 行。
//
// # 为什么必须在 handler 层测
//
// SyncMarketCredentialToMCP 单测通过但 handler 忘了调它，是这类改动的经典漏点：
// 函数写好了、测试全绿、功能没接上。
func TestSaveCredentialTriggersMCPSync(t *testing.T) {
	s, done := mcpSyncTestServer(t)
	defer done()

	label := "mcpsync-handler"
	rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "neodata", "kind": "data", "account_label": label,
		"api_key":  "tok-handler-1234567890",
		"base_url": "http://127.0.0.1:1/proxy/mcp",
	})
	if rec.Code != 200 {
		t.Fatalf("保存: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		ID      int64          `json:"id"`
		MCPSync *MCPSyncResult `json:"mcp_sync"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应: %v（%s）", err, rec.Body.String())
	}
	defer func() { _ = s.m.pg.DeleteMarketCredential(body.ID) }()

	if body.MCPSync == nil {
		t.Fatalf("响应里没有 mcp_sync —— handler 可能没调 syncCredentialBestEffort: %s", rec.Body.String())
	}
	if body.MCPSync.ServerName != "neodata" {
		t.Errorf("mcp_sync.server_name 应为 neodata，实际 %q", body.MCPSync.ServerName)
	}
	if body.MCPSync.ServerID == 0 {
		t.Error("mcp_sync.server_id 为 0 —— MCP 行没建出来")
	}
	var n int
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM mcp_servers WHERE name='neodata'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("保存后 mcp_servers 应有 neodata 行，实际 %d 行", n)
	}
}

// TestSaveTradeCredentialNoMCPSync 交易凭证的响应里没有 mcp_sync 字段。
//
// 有的话前端会显示「同步失败」（其实是不该同步），用户会去修一个不存在的问题。
func TestSaveTradeCredentialNoMCPSync(t *testing.T) {
	s, done := mcpSyncTestServer(t)
	defer done()

	label := "mcpsync-trade"
	rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "okx", "kind": "trade", "account_label": label,
		"api_key": "k1", "api_secret": "s1", "passphrase": "p1",
	})
	if rec.Code != 200 {
		t.Fatalf("保存: %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if id, ok := body["id"].(float64); ok {
		defer func() { _ = s.m.pg.DeleteMarketCredential(int64(id)) }()
	}
	if _, has := body["mcp_sync"]; has {
		t.Errorf("交易凭证不该有 mcp_sync: %v", body["mcp_sync"])
	}
}

// TestSyncErrorIsMasked discover 失败时 Error 已脱敏且不 panic。
//
// 这里用一个必然连不上的地址触发失败，断言：不 panic、返回 nil error、
// res.Error 非空、ServerID 仍非 0（行已建）。
func TestSyncErrorIsMasked(t *testing.T) {
	s, done := mcpSyncTestServer(t)
	defer done()

	label := "mcpsync-err"
	cred, err := s.m.pg.UpsertMarketCredential(db.MarketCredentialInput{
		Provider: "ifind", Kind: "data", AccountLabel: label,
		APIKey:  "sk-should-never-appear-in-error-0001",
		BaseURL: "http://127.0.0.1:1/nope",
	})
	if err != nil {
		t.Fatalf("建凭证: %v", err)
	}
	defer func() { _ = s.m.pg.DeleteMarketCredential(cred.ID) }()

	// 用极短超时避免测试拖 45s。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	id, res, err := s.SyncMarketCredentialToMCP(ctx, cred)
	if err != nil {
		t.Fatalf("MCP 行应已建出，不该返回 error: %v", err)
	}
	if id == 0 {
		t.Fatal("ServerID 为 0")
	}
	if res.Ok {
		t.Fatal("连不上的 endpoint 不该 Ok=true")
	}
	if res.Error == "" {
		t.Fatal("失败时必须有 Error 说明原因")
	}
	if strings.Contains(res.Error, "sk-should-never-appear-in-error-0001") {
		t.Fatalf("token 泄漏进 Error: %s", res.Error)
	}
	if len(res.Error) > 400 {
		t.Errorf("Error 未截断（%d 字节）", len(res.Error))
	}
}

// TestSaveCredentialAcceptsEndpointFieldKey 前端按 meta 的字段 key 提交 `endpoint`。
//
// # 为什么这条要测
//
// 前端（credentials/page.tsx 的 buildPayload）按 fields[].key 逐字段提交，
// 六家数据源的 key 叫 `endpoint` 而**不是** `base_url`。handler 只接 base_url 时，
// 用户填的 URL 会被静默丢弃（base_url 落成空串），同步改用默认 endpoint ——
// 用户明明改了地址却没生效，而两端都显示「成功」。
func TestSaveCredentialAcceptsEndpointFieldKey(t *testing.T) {
	s, done := mcpSyncTestServer(t)
	defer done()

	label := "mcpsync-endpoint-key"
	want := "http://127.0.0.1:1/custom-endpoint"
	rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "tdx", "kind": "data", "account_label": label,
		"endpoint": want,
	})
	if rec.Code != 200 {
		t.Fatalf("保存: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		ID      int64          `json:"id"`
		BaseURL string         `json:"base_url"`
		MCPSync *MCPSyncResult `json:"mcp_sync"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.m.pg.DeleteMarketCredential(body.ID) }()

	if body.BaseURL != want {
		t.Fatalf("提交的 endpoint 应落到 base_url 列，期望 %q 实际 %q", want, body.BaseURL)
	}
	if body.MCPSync == nil || body.MCPSync.URL != want {
		t.Fatalf("MCP 行应连用户填的 endpoint，实际 %+v", body.MCPSync)
	}
}

// TestSaveCredentialAcceptsAuthTokenFieldKey 前端提交的 `auth_token` 落到 api_key 列。
//
// # 为什么这条是本节最重要的一条
//
// 六家数据源的凭据字段 key 是 `auth_token`，db 列是 `api_key`。别名没接住时：
// 凭证存成空串 → MCP 行 env 是 `{}` → 远端 401 → 保存接口仍返回 200。
// 整条链上**没有任何一步报错**，用户看到「已保存」而实际什么都没配上。
// 这是「静默丢弃用户输入」的典型形态，只能靠断言库里的值来抓。
func TestSaveCredentialAcceptsAuthTokenFieldKey(t *testing.T) {
	s, done := mcpSyncTestServer(t)
	defer done()

	label := "mcpsync-authtoken"
	token := "dummy-token-abc123xyz"
	rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "westock", "kind": "data", "account_label": label,
		"auth_token": token,
		"endpoint":   "http://127.0.0.1:1/westock",
	})
	if rec.Code != 200 {
		t.Fatalf("保存: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		ID         int64  `json:"id"`
		APIKeyHint string `json:"api_key_hint"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.m.pg.DeleteMarketCredential(body.ID) }()

	// hint 非空 = api_key 列真的写进去了（hint 由它派生）。
	if body.APIKeyHint == "" {
		t.Fatal("auth_token 没落到 api_key 列 —— 用户填的 token 被静默丢弃")
	}
	// 响应/库里的 hint 不能含完整 token（读写分离）。
	if body.APIKeyHint == token {
		t.Fatalf("api_key_hint 回显了完整 token: %q", body.APIKeyHint)
	}
	// 库里 api_key 列必须是原文（MCP 建连要靠它）。
	var stored string
	if err := s.m.pg.QueryRow(`SELECT COALESCE(api_key,'') FROM market_credentials WHERE id=$1`, body.ID).
		Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != token {
		t.Fatalf("api_key 列应为完整 token，实际 %q", stored)
	}
	// MCP 行的 env 必须带上 Bearer。
	// 用 JSON 解析而不是字符串比较：jsonb 会重排空白（`{"a": "b"}` vs `{"a":"b"}`）。
	var env string
	if err := s.m.pg.QueryRow(`SELECT env FROM mcp_servers WHERE name='westock-mcp'`).Scan(&env); err != nil {
		t.Fatal(err)
	}
	var headers map[string]string
	if err := json.Unmarshal([]byte(env), &headers); err != nil {
		t.Fatalf("env 不是合法 JSON: %v（%s）", err, env)
	}
	if got := headers["Authorization"]; got != "Bearer "+token {
		t.Fatalf("MCP env 的 Authorization 应为 %q，实际 %q", "Bearer "+token, got)
	}
}
