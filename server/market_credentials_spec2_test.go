package server

// 第二批契约细节的测试：
//   - PROVIDER_IMMUTABLE（id 路径禁改 provider）
//   - <redacted> 占位符的字面量边界
//   - meta 的 required 字段与 RequiredFields 一致

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// TestMarketCredentialProviderImmutableByID 按 id 更新时不能改 provider。
//
// # 为什么必须拒
//
// 换 provider 等于换一条记录：字段集合不同（okx 三元组 vs fosun 两个 PEM）、
// 验证方式不同、甚至 api_key 列的语义都不同（API Key vs PEM 公钥）。
//
// 允许原地改会让已存的密码学材料与新的字段语义错配 —— 那是数据损坏。
func TestMarketCredentialProviderImmutableByID(t *testing.T) {
	s, done := credTestServer(t)
	defer done()
	label := uniqueLabel(t)
	defer cleanupLabelCreds(t, s, label)

	rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "okx", "account_label": label, "api_key": "sk-immutable-test-1234",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created db.MarketCredential
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	defer func() { _ = s.m.pg.DeleteMarketCredential(created.ID) }()

	// 试图把 provider 改成 fosun
	rec2 := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"id": created.ID, "provider": "fosun", "account_label": label,
	})
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("改 provider 应 400，实际 %d: %s", rec2.Code, rec2.Body.String())
	}
	var payload map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &payload)
	if payload["code"] != "PROVIDER_IMMUTABLE" {
		t.Errorf("code = %v, want PROVIDER_IMMUTABLE", payload["code"])
	}
	// provider 必须没被改掉
	after, err := s.m.pg.GetMarketCredential(created.ID)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if after.Provider != "okx" {
		t.Errorf("provider 不该被改动，实际 %q", after.Provider)
	}
}

// TestTruncateAndMaskRedactedLiteralBoundary 输入里本来就有的 <redacted> 要转义。
//
// # 为什么需要这个边界
//
// 如果 CLI stderr 里恰好含 "<redacted>"（可能来自上游自己的脱敏，
// 也可能是恶意构造），它会被下游误认为「我们已经脱敏过了」。
// 转义成 <redacted-literal> 让「我们的脱敏」和「原本就有」可区分。
func TestTruncateAndMaskRedactedLiteralBoundary(t *testing.T) {
	// 输入里预先塞一个 <redacted>，模拟上游已脱敏或恶意构造
	in := `error: upstream said <redacted> for key sk-real-alpha-1234`
	out := truncateAndMask(in, 300)

	// 原本的 <redacted> 必须变成 <redacted-literal>（可区分）
	if !strings.Contains(out, "<redacted-literal>") {
		t.Errorf("输入里原有的 <redacted> 应转义成 <redacted-literal>，实际: %s", out)
	}
	// 真实 key 必须被遮
	if strings.Contains(out, "sk-real-alpha-1234") {
		t.Errorf("真实 key 未遮: %s", out)
	}
}

// TestTruncateAndMaskStarsNotUsedAsMarker *** 不能作为脱敏占位符。
//
// # 为什么这条测试存在
//
// "***" 可能出现在正常 base_url 里（如 https://api.x.com/***/v1）。
// 如果用 *** 做占位符，那么：
//  1. 断言「脱敏生效了吗」会命中误报（原文里本来就有 ***）
//  2. 同时无法区分「这里是凭证被遮了」还是「这里本来就有三个星」
func TestTruncateAndMaskStarsNotUsedAsMarker(t *testing.T) {
	// 输入含 *** 但不含任何凭证 —— 输出应保持 ***（不能变成 <redacted>）
	plain := `connecting to https://api.example.com/***/v1 endpoint`
	out := truncateAndMask(plain, 300)
	if strings.Contains(out, "<redacted>") {
		t.Errorf("无凭证的输入不该出现 <redacted>（*** 是正常内容）: %s", out)
	}
	if !strings.Contains(out, "***") {
		t.Errorf("正常内容里的 *** 应保留: %s", out)
	}

	// 输入含 *** 且含凭证 —— 凭证被遮，*** 保留
	mixed := `url=https://api.example.com/***/v1 key=sk-mixed-test-9999`
	out2 := truncateAndMask(mixed, 300)
	if strings.Contains(out2, "sk-mixed-test-9999") {
		t.Errorf("凭证未遮: %s", out2)
	}
	if !strings.Contains(out2, "<redacted>") {
		t.Errorf("应出现 <redacted> 占位符: %s", out2)
	}
	if !strings.Contains(out2, "***") {
		t.Errorf("原本的 *** 应保留: %s", out2)
	}
}

// TestMetaRequiredConsistent required 字段与 required_fields 清单一致。
//
// # 为什么两份都要有
//
//   - required_fields 是 key 清单：程序可快速判断「哪些必填」
//   - fields[].required 是字段自身属性：前端渲染时直接读，不用再查表
//
// 两份如果漂移，前端会出现「标了星号但后端说非必填」这种矛盾。
// 所以必须测它们一致。
func TestMetaRequiredConsistent(t *testing.T) {
	s, done := credTestServer(t)
	defer done()

	rec := httptest.NewRecorder()
	s.listMarketCredentialMeta(rec,
		httptest.NewRequest(http.MethodGet, "/api/market/credentials/meta", nil))
	var body struct {
		Providers []struct {
			Provider       string   `json:"provider"`
			RequiredFields []string `json:"required_fields"`
			Fields         []struct {
				Key      string `json:"key"`
				Required bool   `json:"required"`
			} `json:"fields"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("meta 解析失败: %v", err)
	}
	for _, p := range body.Providers {
		declared := map[string]bool{}
		for _, k := range p.RequiredFields {
			declared[k] = true
		}
		fromFields := map[string]bool{}
		for _, f := range p.Fields {
			if f.Required {
				fromFields[f.Key] = true
			}
			// 交叉验证：fields 里声明 required 的，必须在 required_fields 里
			if f.Required && !declared[f.Key] {
				t.Errorf("%s: 字段 %q 标了 required=true，但不在 required_fields 里",
					p.Provider, f.Key)
			}
		}
		// 反向：required_fields 里的每一项，fields 里必须标 required
		for _, k := range p.RequiredFields {
			if !fromFields[k] {
				t.Errorf("%s: required_fields 含 %q，但该字段没标 required=true",
					p.Provider, k)
			}
		}
	}
}

// TestMetaFosunApplyAccountIDNotSecret apply_account_id 是非敏感字段。
//
// 它不该在 secret_fields 里 —— 否则前端会把账户 ID 当密码渲染（显示成 ***）。
func TestMetaFosunApplyAccountIDNotSecret(t *testing.T) {
	for _, p := range marketProviders {
		// apply_account_id 只用于「下单」（purpose=trade）——
		// 行情数据源不需要账户 ID。所以只检查 trade 那一项。
		if p.Provider != "fosun" || p.Purpose != "trade" {
			continue
		}
		for _, s := range p.SecretFields {
			if s == "apply_account_id" {
				t.Error("apply_account_id 不该在 secret_fields 里（它是非敏感字段）")
			}
		}
		// 反过来确认它在 fields 里且 kind=text
		found := false
		for _, f := range p.Fields {
			if f.Key == "apply_account_id" {
				found = true
				if f.Kind != "text" {
					t.Errorf("apply_account_id 应为 kind=text，实际 %q", f.Kind)
				}
				if f.Required {
					t.Error("apply_account_id 不该必填（fosun 暂未接入）")
				}
			}
		}
		if !found {
			t.Error("fosun 的 fields 里缺 apply_account_id")
		}
	}
}
