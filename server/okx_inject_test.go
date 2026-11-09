package server

// OKX 凭证注入 CLI 的验收测试。
//
// # 这组测试守的是什么
//
// 2026-11-02 线上事故：用户在凭证页填了**正确的模拟盘 key** 并「验证通过」，
// 但模型交易时报 `API key doesn't exist`（OKX code 50119）。
//
// 根因不是 key 错了，是**两套配置互不相干**：
//
//	凭证页 → market_credentials 表  → server 的 REST 验证用它
//	交易   → ~/.okx/config.toml    → OKX CLI 实际用它
//
// 用户机器上实测：
//
//	config.toml: fd8dd762-... demo=false → 401 50119（key 不存在）
//	凭证表     : 450d***6d7d  demo=true  → HTTP 200，余额 1 BTC
//
// 「验证通过」给了虚假的安全感 —— 它验的是 A，交易用的是 B。
//
// 所以这里钉住：注入后 CLI 拿到的必须是表里那把 key，且 demo 标记跟着走。

import (
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// TestInjectOkxCredentialFromDB 注入的必须是凭证表里的那把 key。
func TestInjectOkxCredentialFromDB(t *testing.T) {
	s, _, _ := newAuthServer(t)

	// 清理历史 okx/trade 凭证（其他测试可能留了）
	t.Cleanup(func() {
		_, _ = s.m.pg.Exec(`DELETE FROM market_credentials WHERE provider='okx' AND (account_label='inject' OR account_label LIKE 'demo-%')`)
	})
	_, _ = s.m.pg.Exec(`DELETE FROM market_credentials WHERE provider='okx' AND (account_label='inject' OR account_label LIKE 'demo-%')`)

	const (
		key    = "inject-test-key-36-chars-padding-x"
		secret = "inject-test-secret-32-bytes-pad_x"
		pass   = "inject-pass"
	)
	if _, err := s.m.pg.UpsertMarketCredential(db.MarketCredentialInput{
		Provider: "okx", Kind: "trade", AccountLabel: "inject",
		APIKey: key, APISecret: secret, Passphrase: pass,
		Extra: map[string]string{"demo": "true"},
	}); err != nil {
		t.Fatalf("建凭证: %v", err)
	}

	cl := agent.NewOkxClient("")
	s.injectOkxCredential(cl)

	cred := cl.CredentialForTest()
	if cred == nil {
		t.Fatal("注入后仍无凭证 —— CLI 会去读 config.toml，那正是事故成因")
	}
	if cred.APIKey != key {
		t.Errorf("APIKey = %q, want %q", cred.APIKey, key)
	}
	if cred.SecretKey != secret {
		t.Errorf("SecretKey 不匹配")
	}
	if cred.Passphrase != pass {
		t.Errorf("Passphrase 不匹配")
	}
	if !cred.Demo {
		t.Error("demo 应为 true —— 模拟盘 key 不带这个标记会被 OKX 判 50101 环境不匹配")
	}
}

// TestInjectOkxCredentialSkipsDisabled 禁用/非 trade 的凭证不该被注入。
//
// 场景：用户在表里留了一条旧的 disabled 记录，而启用的是另一条。
// 取错记录等于又回到「验 A 用 B」。
func TestInjectOkxCredentialSkipsDisabled(t *testing.T) {
	s, _, _ := newAuthServer(t)
	t.Cleanup(func() {
		_, _ = s.m.pg.Exec(`DELETE FROM market_credentials WHERE provider='okx' AND (account_label='inject' OR account_label LIKE 'demo-%')`)
	})
	_, _ = s.m.pg.Exec(`DELETE FROM market_credentials WHERE provider='okx' AND (account_label='inject' OR account_label LIKE 'demo-%')`)

	disabled := false
	if _, err := s.m.pg.UpsertMarketCredential(db.MarketCredentialInput{
		Provider: "okx", Kind: "trade", AccountLabel: "off",
		APIKey: "disabled-key-value", Enabled: &disabled,
	}); err != nil {
		t.Fatalf("建禁用凭证: %v", err)
	}

	cl := agent.NewOkxClient("")
	s.injectOkxCredential(cl)
	if cred := cl.CredentialForTest(); cred != nil {
		t.Errorf("禁用的凭证不该被注入，实际拿到 %q", cred.APIKey)
	}
}

// TestInjectOkxCredentialNoRowsKeepsNil 表里没凭证时不注入（回退 config.toml）。
//
// 这是刻意的降级：开发机上可能就是用 config.toml 配的，
// 表里空着不该让 CLI 直接不可用。
func TestInjectOkxCredentialNoRowsKeepsNil(t *testing.T) {
	s, _, _ := newAuthServer(t)
	t.Cleanup(func() {
		_, _ = s.m.pg.Exec(`DELETE FROM market_credentials WHERE provider='okx' AND (account_label='inject' OR account_label LIKE 'demo-%')`)
	})
	_, _ = s.m.pg.Exec(`DELETE FROM market_credentials WHERE provider='okx' AND (account_label='inject' OR account_label LIKE 'demo-%')`)

	cl := agent.NewOkxClient("")
	s.injectOkxCredential(cl)
	if cred := cl.CredentialForTest(); cred != nil {
		t.Errorf("无凭证时不该注入，实际 %+v", cred)
	}
}

// TestOkxCredentialDemoFollowsExtra demo 标记必须来自 extra.demo。
//
// 错配的后果是 50101「APIKey does not match current environment」——
// 文案指向「key 与环境不符」，但用户会误以为 key 本身错了。
func TestOkxCredentialDemoFollowsExtra(t *testing.T) {
	s, _, _ := newAuthServer(t)
	t.Cleanup(func() {
		_, _ = s.m.pg.Exec(`DELETE FROM market_credentials WHERE provider='okx' AND (account_label='inject' OR account_label LIKE 'demo-%')`)
	})

	// 用 ASCII id 而非中文 name 做 account_label：
	// 部分 PG 客户端/连接在非 UTF8 会话下写中文会报
	// `invalid byte sequence for encoding "UTF8"`。测试数据不该依赖编码环境。
	for _, tc := range []struct {
		id    string
		name  string
		extra map[string]string
		want  bool
	}{
		{"t", "demo=true", map[string]string{"demo": "true"}, true},
		{"u", "demo=TRUE", map[string]string{"demo": "TRUE"}, true}, // 大小写不敏感
		{"f", "demo=false", map[string]string{"demo": "false"}, false},
		{"n", "no demo key", nil, false}, // 缺省按实盘
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 每个子用例开始和结束都清一次。
			//
			// # 为什么必须清两次
			//
			// injectOkxCredential 取的是「第一条启用的 okx/trade 凭证」。
			// 而 UpsertMarketCredential 按 (provider, kind, account_label) 匹配 ——
			// 四个子用例的 label 各不相同，于是会**累积四条**，注入永远命中
			// 第一条（demo=true）。看起来像「Demo 取值逻辑坏了」，实际是
			// 测试间的数据泄漏。
			//
			// 这跟 execution_limits 那类共享全局表的坑是同一个模式：
			// 每条测试必须自己声明并清理依赖，不能靠执行顺序。
			_, _ = s.m.pg.Exec(`DELETE FROM market_credentials WHERE provider='okx' AND (account_label='inject' OR account_label LIKE 'demo-%')`)
			t.Cleanup(func() {
				_, _ = s.m.pg.Exec(`DELETE FROM market_credentials WHERE provider='okx' AND (account_label='inject' OR account_label LIKE 'demo-%')`)
			})

			// ⚠️ Extra 传 nil 的语义是「**保留旧值**」（见 MarketCredentialInput.Extra
			// 的注释），不是「置空」。所以「无 demo 键」这个用例若复用同一个
			// account_label，会继承上一个子用例写进的 demo=true —— 看起来像
			// 「缺省值逻辑错了」，实际是 upsert 的保留语义在起作用。
			//
			// 用不同的 label + 前置 DELETE 保证每个用例都是全新一行。
			if _, err := s.m.pg.UpsertMarketCredential(db.MarketCredentialInput{
				Provider: "okx", Kind: "trade",
				AccountLabel: "demo-" + tc.id,
				APIKey:       "k-" + tc.id,
				Extra:        tc.extra,
			}); err != nil {
				t.Fatalf("建凭证: %v", err)
			}
			cl := agent.NewOkxClient("")
			s.injectOkxCredential(cl)
			cred := cl.CredentialForTest()
			if cred == nil {
				t.Fatal("应注入")
			}
			if cred.Demo != tc.want {
				t.Errorf("Demo = %v, want %v", cred.Demo, tc.want)
			}
		})
	}
}
