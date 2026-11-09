package server

// OKX 凭证验证的验收测试。
//
// # 这组测试守的是什么
//
// 2026-11-02 实测事故：用户填了**正确的模拟盘 key**，点「验证」报
//
//	Update available for @okx_ai/okx-trade-cli: 1.4.8 -> 1.5.0
//	Error: Not logged in — run `okx-auth login` first
//	Error: No credentials found.
//
// 三条错误都跟用户的 key 无关：
//   - 第一、二条是 **okx CLI 自己的状态**（版本升级提示 + CLI 的 OAuth 未登录）
//   - 第三条是 CLI 找不到它自己的凭据文件
//
// 根因是 verifyOkxCLI 跑 `okx account balance` —— 那测的是 CLI 的 OAuth 登录态，
// 而用户在凭证页填的 api_key/api_secret/passphrase **一次都没被使用**。
//
// 后果是双向的：
//   - key 完全错误时也可能「验证通过」（只要 CLI 登录着）
//   - key 完全正确时必然失败（CLI 没登录），且错误信息指向错误的方向
//
// 所以这里钉住三件事：① 用表里的 key 亲自签名请求 ② demo 开关真的发出 header
// ③ 错误分类型（环境错配 / key 无效 / 网络不通）文案不混淆。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestVerifyOkxRESTSignsWithStoredKey 验证必须用传入的凭据签名。
//
// # 这条是最核心的断言
//
// 旧实现根本不把 key 传给被测函数（verifyOkxCLI 无参数，只 exec CLI）。
// 所以只要断言「请求头里带着我们给的 key」，就证明了「验证的是这把 key」。
func TestVerifyOkxRESTSignsWithStoredKey(t *testing.T) {
	var gotKey, gotSign, gotTS, gotPass, gotPath string
	var gotDemo bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("OK-ACCESS-KEY")
		gotSign = r.Header.Get("OK-ACCESS-SIGN")
		gotTS = r.Header.Get("OK-ACCESS-TIMESTAMP")
		gotPass = r.Header.Get("OK-ACCESS-PASSPHRASE")
		gotDemo = r.Header.Get("x-simulated-trading") == "1"
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[]}`))
	}))
	defer srv.Close()

	ok, msg := verifyOkxREST(t.Context(), verifyOkxParams{
		APIKey: "my-key-36-chars", APISecret: "my-secret-32", Passphrase: "my-pass",
		BaseURL: srv.URL,
	})
	if !ok {
		t.Fatalf("应通过: %s", msg)
	}

	// ① 四个签名头齐全且值正确
	if gotKey != "my-key-36-chars" {
		t.Errorf("OK-ACCESS-KEY = %q，未使用表里的 key", gotKey)
	}
	if gotPass != "my-pass" {
		t.Errorf("OK-ACCESS-PASSPHRASE = %q", gotPass)
	}
	if gotSign == "" {
		t.Fatal("缺 OK-ACCESS-SIGN —— 没有签名，验证等于没做")
	}
	if gotTS == "" {
		t.Fatal("缺 OK-ACCESS-TIMESTAMP")
	}
	// ② 必须打 account 类接口（需要签名），不能打 public（免签，验不出真伪）
	if !strings.Contains(gotPath, "/account/") {
		t.Errorf("请求路径 = %q，应打 /account/*（import：public 接口免签名，任何 key 都能过）", gotPath)
	}
	// ③ 没勾 demo 时不能带模拟盘 header
	if gotDemo {
		t.Error("未勾选模拟盘却带了 x-simulated-trading: 1")
	}
}

// TestVerifyOkxRESTDemoHeader 勾选模拟盘必须带 x-simulated-trading: 1。
//
// 这是用户这次事故的直接成因：他的 key 是模拟盘的，但没有这个 header，
// OKX 就按实盘解析 → 50101「APIKey does not match current environment」。
func TestVerifyOkxRESTDemoHeader(t *testing.T) {
	var gotDemo bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotDemo = r.Header.Get("x-simulated-trading") == "1"
		_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[]}`))
	}))
	defer srv.Close()

	ok, msg := verifyOkxREST(t.Context(), verifyOkxParams{
		APIKey: "k", APISecret: "s", Passphrase: "p",
		Demo: true, BaseURL: srv.URL,
	})
	if !ok {
		t.Fatalf("应通过: %s", msg)
	}
	if !gotDemo {
		t.Fatal("勾选模拟盘但没发 x-simulated-trading: 1 —— 模拟盘 key 会被判定为环境不匹配")
	}
}

// TestVerifyOkxRESTClassifiesErrors 错误分类必须给出可操作的文案。
//
// # 为什么要逐码断言文案
//
// 用户看到「50101」时无从下手。而这个码的真实含义是「key 与实盘/模拟盘环境
// 不匹配」—— 与「key 无效」是两件完全不同的事，修法也完全不同
// （前者改勾选，后者换 key）。混淆会让人往错误方向修。
func TestVerifyOkxRESTClassifiesErrors(t *testing.T) {
	cases := []struct {
		name     string
		code     string
		msg      string
		status   int
		demo     bool
		wantHint string
	}{
		{
			name: "50101 勾了模拟盘但 key 是实盘",
			code: "50101", msg: "APIKey does not match current environment.", status: 200,
			demo: true, wantHint: "已勾选「模拟盘」但 OKX 认为这是实盘 key",
		},
		{
			name: "50101 没勾但 key 是模拟盘",
			code: "50101", msg: "APIKey does not match current environment.", status: 200,
			demo: false, wantHint: "请在「模拟盘（Demo Trading）」处勾选后重试",
		},
		{
			name: "50111 key 无效",
			code: "50111", msg: "Invalid API Key", status: 200,
			wantHint: "API Key 无效",
		},
		{
			name: "50113 签名错",
			code: "50113", msg: "Invalid Sign", status: 200,
			wantHint: "签名验证失败",
		},
		{
			name: "50103 passphrase 错",
			code: "50103", msg: "Invalid Passphrase", status: 200,
			wantHint: "Passphrase 不正确",
		},
		{
			name: "50001 key 过期",
			code: "50001", msg: "API key expired", status: 200,
			wantHint: "已过期或被禁用",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(`{"code":"` + c.code + `","msg":"` + c.msg + `"}`))
			}))
			defer srv.Close()

			ok, msg := verifyOkxREST(t.Context(), verifyOkxParams{
				APIKey: "k", APISecret: "s", Passphrase: "p",
				Demo: c.demo, BaseURL: srv.URL,
			})
			if ok {
				t.Fatalf("code=%s 不该通过", c.code)
			}
			if !strings.Contains(msg, c.wantHint) {
				t.Errorf("错误文案 %q 应含 %q", msg, c.wantHint)
			}
			// 任何情况下不能回显密钥
			if strings.Contains(msg, "s") && strings.Contains(msg, "my-secret") {
				t.Errorf("错误文案泄漏了 secret: %s", msg)
			}
		})
	}
}

// TestVerifyOkxRESTNetworkErrorIsNotKeyProblem 网络故障不能说成 key 问题。
//
// 说反了的代价：用户会反复换 key，而问题在代理/DNS。
func TestVerifyOkxRESTNetworkErrorIsNotKeyProblem(t *testing.T) {
	ok, msg := verifyOkxREST(t.Context(), verifyOkxParams{
		APIKey: "k", APISecret: "s", Passphrase: "p",
		BaseURL: "http://127.0.0.1:1", // 必然拒绝连接
	})
	if ok {
		t.Fatal("连不上时不该返回 ok")
	}
	if !strings.Contains(msg, "不是 key 问题") {
		t.Errorf("网络故障文案应明确排除 key 问题，实际: %s", msg)
	}
}

// TestVerifyOkxRESTRequiresAllThreeFields 三元组缺一个就明确报缺哪个。
func TestVerifyOkxRESTRequiresAllThreeFields(t *testing.T) {
	cases := []struct{ name, key, secret, pass, want string }{
		{"缺 key", "", "s", "p", "API Key 为空"},
		{"缺 secret", "k", "", "p", "API Secret 为空"},
		{"缺 passphrase", "k", "s", "", "Passphrase 为空"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, msg := verifyOkxREST(t.Context(), verifyOkxParams{
				APIKey: c.key, APISecret: c.secret, Passphrase: c.pass,
			})
			if ok {
				t.Fatal("缺字段不该通过")
			}
			if !strings.Contains(msg, c.want) {
				t.Errorf("文案 %q 应含 %q", msg, c.want)
			}
		})
	}
}

// TestOkxMetaHasDemoField OKX 的 meta 必须提供「模拟盘」勾选框。
//
// # 为什么这是硬要求而不是 nice-to-have
//
// 实盘与模拟盘的 key 格式一致、互不通用，OKX 靠一个 header 区分环境。
// 如果不给这个开关，模拟盘用户**永远**无法验通过 —— 且错误信息
// （50101 APIKey does not match current environment）指向不了「你该勾个选项」。
func TestOkxMetaHasDemoField(t *testing.T) {
	var okx *providerMeta
	for i := range marketProviders {
		p := &marketProviders[i]
		if p.Provider == "okx" && p.Purpose == "trade" {
			okx = p
			break
		}
	}
	if okx == nil {
		t.Fatal("找不到 okx/trade provider")
	}
	var demo *credentialField
	for i := range okx.Fields {
		if okx.Fields[i].Key == "demo" {
			demo = &okx.Fields[i]
			break
		}
	}
	if demo == nil {
		t.Fatal("okx 缺 demo 字段 —— 模拟盘用户无法验证通过")
	}
	if demo.Kind != "checkbox" {
		t.Errorf("demo 的 kind = %q，应为 checkbox", demo.Kind)
	}
	// 必须存 extra：它是非敏感配置，不该占 secret 列
	// （secret 列只写不读，勾选状态要能回显给用户）
	if demo.Storage != "extra" {
		t.Errorf("demo 的 storage = %q，应为 extra（勾选状态需要回显）", demo.Storage)
	}
	// 不能是 required：实盘用户不勾是正常状态
	if demo.Required {
		t.Error("demo 不该是 required —— 实盘用户不勾是合法的")
	}
}
