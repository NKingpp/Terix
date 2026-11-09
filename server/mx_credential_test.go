package server

// 妙想（mx）凭证接线的验收测试。
//
// # 这组测试守的是什么
//
// 妙想与前面六家 MCP 数据源走**同一条凭证通路、不同的消费方式**：
//   - 六家 MCP：凭证 → `mcp_servers` 行（env 当 header）→ MCP 客户端
//   - 妙想 REST：凭证 → 保留在 `market_credentials` → `agent/mx_client.go` 直连
//
// 两类混在一起的失败方式很隐蔽：把妙想塞进 MCP 同步，会去 POST 一个
// 不存在的 MCP 端点，得到 404 —— 而 404 与「key 无效」毫无关系。
// 用户按那个错误去换 key，越换越错。
//
// 所以这里钉三件事：① 它不建 MCP 行 ② 它的验证走自己的 API ③ 失败分类能区分
// 「key 错」和「网不通」。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// TestMXProviderIsDeclaredAsREST 妙想必须声明为 REST 类，不进 MCP 映射。
//
// # 为什么是两条断言而不是一条
//
// 只查 `restDataProviders["mx"]` 为真，无法发现「同时也被误加进
// dataProviderMCPNames」的情况 —— 那时两条路径都会执行，最后一次写覆盖前一次。
// 所以必须双向断言。
func TestMXProviderIsDeclaredAsREST(t *testing.T) {
	if !restDataProviders["mx"] {
		t.Fatal("mx 必须在 restDataProviders 里 —— 否则它会被当成了 MCP 源")
	}
	if _, ok := dataProviderMCPNames["mx"]; ok {
		t.Fatal("mx 不该出现在 dataProviderMCPNames 里 —— 它没有 MCP 端点，" +
			"同步会去 POST 一个不存在的 URL，得到的 404 与 key 无效无关")
	}
	if isSyncedDataProvider("mx") {
		t.Error("isSyncedDataProvider(mx) 应为 false —— 它不参与 MCP 同步")
	}
}

// TestMXProviderInMeta 妙想必须在 provider 列表里且字段完整。
func TestMXProviderInMeta(t *testing.T) {
	var mx *providerMeta
	for i := range marketProviders {
		if marketProviders[i].Provider == "mx" {
			mx = &marketProviders[i]
			break
		}
	}
	if mx == nil {
		t.Fatal("marketProviders 里没有 mx —— 用户无法在凭证页配置它")
	}
	if mx.Purpose != "data" {
		t.Errorf("purpose = %q, want data", mx.Purpose)
	}
	if !mx.Verifiable {
		t.Error("mx 应可验证（probeMX 已实现真实探测）")
	}
	// auth_token 必须必填且是 secret：妙想的 key 是 Bearer 级凭据。
	var token *credentialField
	for i := range mx.Fields {
		if mx.Fields[i].Key == "auth_token" {
			token = &mx.Fields[i]
			break
		}
	}
	if token == nil {
		t.Fatal("mx 缺 auth_token 字段")
	}
	if !token.Required {
		t.Error("mx 的 auth_token 应必填 —— 没有 key 时接口一定 401")
	}
	if token.Storage != "secret" {
		t.Errorf("auth_token 的 storage = %q, want secret（它是凭据，不能进 extra 明文）", token.Storage)
	}
	// endpoint 必须有默认值，否则用户要手抄一个长 URL
	var ep *credentialField
	for i := range mx.Fields {
		if mx.Fields[i].Key == "endpoint" {
			ep = &mx.Fields[i]
			break
		}
	}
	if ep == nil {
		t.Fatal("mx 缺 endpoint 字段")
	}
	if !strings.Contains(ep.Default, "mkapi2.dfcfs.com") {
		t.Errorf("endpoint 默认值 = %q，应含妙想真实域名", ep.Default)
	}
}

// TestSyncMarketCredentialToMCPSkipsMX 妙想同步必须「跳过且不报错」。
//
// # 为什么强调「不报错」
//
// 调用方对 error 的处理是「记进 verify_error 并提示用户」。而
// 「这个 provider 本来就不用同步」不是用户需要知道的事。
// 如果这里返回 error，用户保存妙想凭证时会看到一个与事实无关的失败提示。
func TestSyncMarketCredentialToMCPSkipsMX(t *testing.T) {
	s, _, _ := newAuthServer(t)

	// 建一条妙想凭证（不填 endpoint，让 fallback 生效）
	cred, err := s.m.pg.UpsertMarketCredential(db.MarketCredentialInput{
		Provider: "mx", Kind: "data", AccountLabel: "mx-sync-test", APIKey: "fake-mx-key",
	})
	if err != nil {
		t.Fatalf("建妙想凭证: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.m.pg.Exec(`DELETE FROM market_credentials WHERE account_label='mx-sync-test'`)
	})

	id, res, err := s.SyncMarketCredentialToMCP(t.Context(), cred)
	if err != nil {
		t.Fatalf("mx 的同步应返回 nil error（跳过不是失败），实际: %v", err)
	}
	if id != 0 {
		t.Errorf("应返回 id=0（未建 MCP 行），实际 %d", id)
	}
	if res.Ok {
		t.Error("res.Ok 应为 false —— 零值代表「没做」，不是「做成功」")
	}

	// 核心断言：库里不能出现 mx 的 MCP 行
	var n int
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM mcp_servers WHERE name LIKE '%mx%'`).Scan(&n); err != nil {
		t.Fatalf("查 mcp_servers: %v", err)
	}
	if n != 0 {
		t.Errorf("不该建出 mx 相关的 MCP 行，实际 %d 条", n)
	}
}

// TestProbeMXClassifiesErrors 验证的失败分类必须能区分「key 错」与「网不通」。
//
// # 为什么这条最要紧
//
// 两类失败如果混成一句话，用户会按错误方向修：
//   - key 无效却说「网络问题」→ 用户去查网络/防火墙，查不出问题
//   - 网不通却说「key 无效」→ 用户反复换 key，越换越错
//
// 判据是**我们能在本地决定的**：HTTP 401/403 → key 问题；
// 传输层 error（DNS/连接）→ 网络问题；status!=0 → 业务错误。
func TestProbeMXClassifiesErrors(t *testing.T) {
	cases := []struct {
		name     string
		handler  http.HandlerFunc
		wantOK   bool
		wantHint string // 错误文案里必须出现的词
	}{
		{
			name: "正常返回",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":0,"message":"ok"}`))
			},
			wantOK: true,
		},
		{
			name: "401 key 无效",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"invalid apikey"}`))
			},
			wantOK:   false,
			wantHint: "API Key 无效",
		},
		{
			name: "403 也应归为 key 问题",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
			},
			wantOK:   false,
			wantHint: "API Key 无效",
		},
		{
			name: "业务错误 status!=0",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"status":10005,"message":"查询配额已用完"}`))
			},
			wantOK:   false,
			wantHint: "业务错误",
		},
		{
			name: "500 上游故障",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantOK:   false,
			wantHint: "HTTP 500",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(c.handler)
			defer srv.Close()

			ok, msg := probeMX(t.Context(), srv.URL, "test-key", "贵州茅台最新价")
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v（msg=%s）", ok, c.wantOK, msg)
			}
			if !c.wantOK && !strings.Contains(msg, c.wantHint) {
				t.Errorf("错误文案 %q 应含 %q", msg, c.wantHint)
			}
			// 任何情况下都不能把 key 回显出去
			if strings.Contains(msg, "test-key") {
				t.Errorf("错误文案泄漏了 apiKey: %s", msg)
			}
		})
	}
}

// TestProbeMXCannotConnect 连不上时必须说「不是 key 问题」。
//
// 用 127.0.0.1:1（几乎必然拒绝连接）模拟网络故障。
func TestProbeMXCannotConnect(t *testing.T) {
	ok, msg := probeMX(t.Context(), "http://127.0.0.1:1", "test-key", "x")
	if ok {
		t.Fatal("连不上时不该返回 ok")
	}
	if !strings.Contains(msg, "不是 key 问题") {
		t.Errorf("网络故障的文案应明确排除 key 问题，实际: %s", msg)
	}
}

// TestProbeMXSendsApikeyHeader Header 名必须是 apikey（小写，不是 Authorization）。
//
// # 为什么单独测 header 名
//
// 妙想的认证方式与前面六家 MCP 完全不同：MCP 用 `Authorization: Bearer`，
// 妙想用 `apikey: <key>`。写错成 Bearer 会 401，而错误信息看起来像「key 无效」——
// 用户会去换 key，实际是拼错了 header。
func TestProbeMXSendsApikeyHeader(t *testing.T) {
	var gotHeader, gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("apikey")
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"status":0}`))
	}))
	defer srv.Close()

	if ok, msg := probeMX(t.Context(), srv.URL, "my-secret-key", "茅台最新价"); !ok {
		t.Fatalf("应成功: %s", msg)
	}
	if gotHeader != "my-secret-key" {
		t.Errorf("apikey header = %q, want my-secret-key", gotHeader)
	}
	if !strings.HasSuffix(gotPath, "/query") {
		t.Errorf("请求路径 = %q，应以 /query 结尾", gotPath)
	}
	if !strings.Contains(string(gotBody), "toolQuery") {
		t.Errorf("请求体应含 toolQuery 字段，实际: %s", gotBody)
	}
}
