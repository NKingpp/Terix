package db

// credentialCols 与 scanCredential 的列映射测试（对应 KNOWN-LIMITATIONS L23）。
//
// # 为什么这条测试存在
//
// credentialCols（SQL 列清单）与 scanCredential 的 Scan 参数是跨两处的隐式契约：
// 必须顺序严格一致，而编译器不管、运行时也不报（多个 *string 时会静默互换值）。
//
// # 为什么每个字段要填「互不相同」的值
//
// 这是本测试能抓到互换的唯一前提。
// 如果 kind 和 account_label 恰好填了相同的值，它们互换后测试照样通过。
// 所以：**每个字段的值必须彼此可区分**。

import (
	"testing"
)

// TestCredentialScanMapsAllColumns 建一条凭据，每列填互不相同的值，读回来逐字段断言。
//
// 任何两列的互换都会让至少一个断言失败。
func TestCredentialScanMapsAllColumns(t *testing.T) {
	d := testOpen(t)
	// ⚠️ 清理**必须按 label 限定**，绝不能 `DELETE FROM market_credentials`。
	//
	// 这张表存的是用户的真实交易密钥（OKX / 复星）。全表 DELETE 会把它们删光，
	// 而用户看到的是「开发完后凭据没了」—— 且没有任何日志提示是测试干的。
	//
	// 实测事故（2026-11-05）：跑完整测试后 market_credentials 变成 0 行，
	// 用户需要重新在凭证页录入所有 key。
	//
	// 用本测试自己的 label 前缀隔离：label-alpha-7731。
	const testLabel = "label-alpha-7731"
	if _, err := d.Exec(`DELETE FROM market_credentials WHERE account_label=$1`, testLabel); err != nil {
		t.Fatalf("清理: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM market_credentials WHERE account_label=$1`, testLabel)
	})

	// 每个值的形态刻意不同，便于一眼看出「哪个字段读串了」。
	const (
		wantKind    = "data"                          // vs "trade" 可区分
		wantLabel   = "label-alpha-7731"              // 与 kind 完全不同
		wantBaseURL = "https://base-url-9953.example" // 与上面都不同
	)
	enabled := false // 默认是 true，所以 false 能验证「真的读了这个字段」

	saved, err := d.UpsertMarketCredential(MarketCredentialInput{
		Provider:     "okx",
		Kind:         wantKind,
		AccountLabel: wantLabel,
		BaseURL:      wantBaseURL,
		Enabled:      &enabled,
		APIKey:       "api-key-4412",
	})
	if err != nil {
		t.Fatalf("建凭据: %v", err)
	}

	// 走读路径（ListMarketCredentials 用的是 credentialCols + scanCredential）
	list, err := d.ListMarketCredentials()
	if err != nil {
		t.Fatalf("ListMarketCredentials: %v", err)
	}
	var got *MarketCredential
	for _, c := range list {
		if c.ID == saved.ID {
			got = c
			break
		}
	}
	if got == nil {
		t.Fatalf("列表里没有 id=%d 的凭据", saved.ID)
	}

	// 逐字段断言 —— 任一列错位都会让这里至少一条失败。
	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"Kind", got.Kind, wantKind},
		{"AccountLabel", got.AccountLabel, wantLabel},
		{"BaseURL", got.BaseURL, wantBaseURL},
		{"Enabled", got.Enabled, false},
		{"Provider", got.Provider, "okx"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v —— credentialCols 与 scanCredential 的列顺序可能不一致",
				c.field, c.got, c.want)
		}
	}

	// APIKey 永远不该出现在读路径（credentialCols 不含敏感列）。
	// 这条不是「检查顺序」，是「检查那个设计约束还在」。
	if got.APIKeyHint == "" {
		t.Error("APIKeyHint 为空 —— 读路径应返回 hint（但不是明文 key）")
	}
}
