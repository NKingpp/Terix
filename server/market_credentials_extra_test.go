package server

// extra JSONB 存储 + meta 的 storage 字段的测试。
//
// # extra 的设计约束（这些测试守的）
//
//  1. extra 只放**非敏感**字段 —— 它是整体返回给前端的，
//     没有 hint 机制可以遮蔽。放敏感值等于绕过读写分离。
//  2. extra 的「保留/清空」语义与敏感字段不同：
//     nil（未提供）= 保留；{}（空 map）= 显式清空。
//  3. meta 的 storage 字段告诉前端「这个值该放哪」，
//     前端据此决定提交形态。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// TestMarketCredentialExtraStoredAndReturned extra 能存能读。
func TestMarketCredentialExtraStoredAndReturned(t *testing.T) {
	s, done := credTestServer(t)
	defer done()
	label := uniqueLabel(t)
	defer cleanupLabelCreds(t, s, label)

	rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "fosun", "account_label": label,
		"api_key":    "-----BEGIN-PUB-KEY-----",
		"api_secret": "-----BEGIN-PRIV-KEY-----",
		"extra":      map[string]string{"apply_account_id": "ACC-12345"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created db.MarketCredential
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	defer func() { _ = s.m.pg.DeleteMarketCredential(created.ID) }()

	if created.Extra["apply_account_id"] != "ACC-12345" {
		t.Errorf("extra 未存住，实际 %v", created.Extra)
	}

	// GET 也要返回（extra 是给前端看的，非敏感）
	rec2 := httptest.NewRecorder()
	s.listMarketCredentials(rec2, httptest.NewRequest(http.MethodGet, "/api/market/credentials", nil))
	if !strings.Contains(rec2.Body.String(), "ACC-12345") {
		t.Errorf("GET 应返回 extra 内容（它是非敏感的），实际: %s", rec2.Body.String())
	}
}

// TestMarketCredentialExtraNilKeepsExisting nil（字段省略）= 保留旧值。
//
// # 为什么语义与敏感字段不同
//
// 敏感字段的空串是合法输入（用户清空 key），所以要用哨兵区分「保留」。
// map 的零值 nil 本身就能表达「字段未提供」，不必借道哨兵。
func TestMarketCredentialExtraNilKeepsExisting(t *testing.T) {
	s, done := credTestServer(t)
	defer done()
	pg := s.m.pg
	label := uniqueLabel(t)
	defer cleanupLabelCreds(t, s, label)

	rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "fosun", "account_label": label, "api_key": "-----PEM-----",
		"extra": map[string]string{"apply_account_id": "ACC-KEEP"},
	})
	var created db.MarketCredential
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	defer func() { _ = pg.DeleteMarketCredential(created.ID) }()

	// 第二次不传 extra（字段省略）
	rec2 := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "fosun", "account_label": label, "base_url": "https://x.test",
	})
	if rec2.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec2.Code, rec2.Body.String())
	}
	var after db.MarketCredential
	_ = json.Unmarshal(rec2.Body.Bytes(), &after)
	if after.Extra["apply_account_id"] != "ACC-KEEP" {
		t.Errorf("不传 extra 时应保留旧值，实际 %v", after.Extra)
	}
	if after.BaseURL != "https://x.test" {
		t.Errorf("base_url 应更新，实际 %q", after.BaseURL)
	}
}

// TestMarketCredentialExtraEmptyMapClears {} = 显式清空。
func TestMarketCredentialExtraEmptyMapClears(t *testing.T) {
	s, done := credTestServer(t)
	defer done()
	pg := s.m.pg
	label := uniqueLabel(t)
	defer cleanupLabelCreds(t, s, label)

	rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "fosun", "account_label": label, "api_key": "-----PEM-----",
		"extra": map[string]string{"apply_account_id": "ACC-CLEAR-ME"},
	})
	var created db.MarketCredential
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	defer func() { _ = pg.DeleteMarketCredential(created.ID) }()

	rec2 := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "fosun", "account_label": label,
		"extra": map[string]string{}, // 显式空 map
	})
	if rec2.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec2.Code, rec2.Body.String())
	}
	var after db.MarketCredential
	_ = json.Unmarshal(rec2.Body.Bytes(), &after)
	if len(after.Extra) != 0 {
		t.Errorf("传空 map 应清空 extra，实际 %v", after.Extra)
	}
}

// TestMetaStorageField meta 的 storage 字段正确标注每个值的去向。
//
// # 这个测试守的是「前端不会把敏感字段当普通字段渲染」
//
// 如果 okx 的 api_key 标成 storage="column"，前端会把它当普通列渲染
// （明文显示）。这类错误在 UI 上就是「密钥明文可见」。
func TestMetaStorageField(t *testing.T) {
	s, done := credTestServer(t)
	defer done()

	rec := httptest.NewRecorder()
	s.listMarketCredentialMeta(rec,
		httptest.NewRequest(http.MethodGet, "/api/market/credentials/meta", nil))

	var body struct {
		Providers []struct {
			Provider string `json:"provider"`
			Purpose  string `json:"purpose"`
			Fields   []struct {
				Key     string `json:"key"`
				Storage string `json:"storage"`
			} `json:"fields"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("meta 解析失败: %v", err)
	}

	// key 用 "provider:purpose" —— 因为同一个 provider 现在有 trade/data 两项，
	// 而它们的字段集合不同（apply_account_id 只在 trade 那项）。
	want := map[string]map[string]string{
		"okx:trade": {
			"api_key":    "secret",
			"api_secret": "secret",
			"passphrase": "secret",
		},
		"okx:data": {
			"api_key":    "secret",
			"api_secret": "secret",
			"passphrase": "secret",
		},
		"fosun:trade": {
			"server_public_key":  "secret",
			"client_private_key": "secret",
			"apply_account_id":   "extra", // ← 关键：非敏感字段走 extra
		},
		"fosun:data": {
			"server_public_key":  "secret",
			"client_private_key": "secret",
		},
	}
	for _, p := range body.Providers {
		key := p.Provider + ":" + p.Purpose
		expect, ok := want[key]
		if !ok {
			continue
		}
		got := map[string]string{}
		for _, f := range p.Fields {
			got[f.Key] = f.Storage
			// 每个字段都必须有 storage（空串说明漏标了）
			if f.Storage == "" {
				t.Errorf("%s.%s: storage 未标注（前端无法判断该放哪）", key, f.Key)
			}
		}
		for k, v := range expect {
			if got[k] != v {
				t.Errorf("%s.%s: storage = %q, want %q", key, k, got[k], v)
			}
		}
	}
}

// TestMetaStorageNeverSaysHintForSecret secret 字段不该标 storage="hint"。
//
// # 为什么单独测
//
// "hint" 的语义是「后端自动生成，客户端不用传」。如果 api_key 被标成 hint，
// 前端会认为「不需要提交 key」—— 用户填了也不发，凭证永远存不进去。
//
// hint 只用于展示，对应的是 api_key_hint 这个**输出**列，不是输入字段。
func TestMetaStorageNeverSaysHintForSecret(t *testing.T) {
	for _, p := range marketProviders {
		for _, f := range p.Fields {
			if f.Storage == "hint" {
				t.Errorf("%s.%s 标了 storage=hint —— "+
					"hint 是输出列（api_key_hint），不该出现在输入字段上",
					p.Provider, f.Key)
			}
			// 反向：secret 字段必须是 kind=password（脱敏渲染）
			if f.Storage == "secret" && f.Kind != "password" {
				t.Errorf("%s.%s 是 secret 但 kind=%q —— 前端会明文显示密钥",
					p.Provider, f.Key, f.Kind)
			}
		}
	}
}

// TestMarketCredentialExtraNotInSecretFields extra 字段不该出现在 secret_fields。
func TestMarketCredentialExtraNotInSecretFields(t *testing.T) {
	for _, p := range marketProviders {
		secretSet := map[string]bool{}
		for _, k := range p.SecretFields {
			secretSet[k] = true
		}
		for _, f := range p.Fields {
			if f.Storage == "extra" && secretSet[f.Key] {
				t.Errorf("%s.%s 存 extra 却在 secret_fields 里 —— "+
					"extra 不做脱敏，放敏感值等于绕过读写分离",
					p.Provider, f.Key)
			}
		}
	}
}

// TestTruncateAndMask16CharSecret 16 位 secret 必须被遮（原阈值 32 会漏）。
//
// # 这个测试对应一个真实漏洞
//
// `sec-charlie-9999` 长 16，原实现的两档（已知前缀 / >=32 高熵）都覆盖不到，
// 被原样存进 verify_error 且 GET 返回给前端。
func TestTruncateAndMask16CharSecret(t *testing.T) {
	cases := []string{
		"sec-charlie-9999", // 16 位，无已知前缀（除了 sec- 现在已加）
		"abcd1234efgh5678", // 16 位纯字母数字混合
		"ZZZZ9999YYYY8888", // 16 位大写混合
	}
	for _, secret := range cases {
		out := truncateAndMask("error: invalid credential "+secret+" rejected", 300)
		if strings.Contains(out, secret) {
			t.Errorf("16 位凭证 %q 未被遮（阈值应该是 16）: %s", secret, out)
		}
		if !strings.Contains(out, "<redacted>") {
			t.Errorf("%q 应被替换成 <redacted>: %s", secret, out)
		}
	}
}

// TestTruncateAndMaskShortIDNotMasked 短 ID / 时间戳不该被遮。
//
// 阈值定在 16 而不是更低的理由：低于 16 的正常标识符会被误遮，
// 错误信息就没用了。
func TestTruncateAndMaskShortIDNotMasked(t *testing.T) {
	// 典型的非凭证内容：短 ID、错误码、时间戳前缀
	safe := []string{
		"ERR401",     // 6 位
		"req-12345",  // 9 位
		"acc_9876",   // 8 位
		"HTTP 500",   // 带空格
		"2026-10-27", // 日期
	}
	for _, s := range safe {
		out := truncateAndMask("failed: "+s+" occurred", 300)
		if !strings.Contains(out, s) {
			t.Errorf("非凭证内容 %q 不该被遮: %s", s, out)
		}
	}
}
