package server

// 补充规格（create_only / id / meta / PEM 归一化）的测试。
//
// 独立成文件而不是追加到 market_credentials_test.go：
// 那个文件测的是「基础 CRUD + 安全底线」，这个测的是「契约细节」。
// 分开的好处是失败时一眼知道是哪一层坏了。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// cleanupLabelCreds 删除指定 label 的所有凭证（测试收尾用）。
func cleanupLabelCreds(t *testing.T, s *Server, label string) {
	t.Helper()
	if list, err := s.m.pg.ListMarketCredentials(); err == nil {
		for _, c := range list {
			if c.AccountLabel == label {
				_ = s.m.pg.DeleteMarketCredential(c.ID)
			}
		}
	}
}

// TestMarketCredentialCreateOnlyConflict create_only=true 撞名返回 409。
//
// 前端「新增」流程传 create_only=true：用户确实想新建，撞名要提示而不是静默覆盖。
func TestMarketCredentialCreateOnlyConflict(t *testing.T) {
	s, done := credTestServer(t)
	defer done()
	label := uniqueLabel(t)
	defer cleanupLabelCreds(t, s, label)

	rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "okx", "account_label": label,
		"api_key": "sk-first-1111222233334444", "create_only": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("首次 create_only 应成功: %d %s", rec.Code, rec.Body.String())
	}

	rec2 := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "okx", "account_label": label,
		"api_key": "sk-second-5555666677778888", "create_only": true,
	})
	if rec2.Code != http.StatusConflict {
		t.Fatalf("撞名应 409，实际 %d: %s", rec2.Code, rec2.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &payload); err != nil {
		t.Fatalf("错误响应不是 JSON: %v", err)
	}
	// 扁平错误形态：error 必须是字符串（前端 api.ts 只读这个）
	if _, ok := payload["error"].(string); !ok {
		t.Errorf("error 必须是字符串（扁平），实际类型 %T: %s",
			payload["error"], rec2.Body.String())
	}
	if payload["code"] != "CREDENTIAL_EXISTS" {
		t.Errorf("code = %v, want CREDENTIAL_EXISTS", payload["code"])
	}
	// 原记录不该被覆盖
	list, _ := s.m.pg.ListMarketCredentials()
	for _, c := range list {
		if c.AccountLabel == label {
			k, _, _, _, _ := s.m.pg.SeparateAPIKeyForUse(c.ID)
			if k == "sk-second-5555666677778888" {
				t.Error("create_only 失败时不该覆盖原记录")
			}
		}
	}
}

// TestMarketCredentialUpdateLabelByID P0：传 id + 新 label，原记录被重命名（不新建）。
//
// # 这个测试守的是什么
//
// 如果不支持按 id 更新，用户「重命名」时后端按 provider+新label 查不到 →
// 新建一条，旧的还在。用户以为在改名，结果多出一条重复的。
func TestMarketCredentialUpdateLabelByID(t *testing.T) {
	s, done := credTestServer(t)
	defer done()
	pg := s.m.pg
	oldLabel := uniqueLabel(t)
	newLabel := oldLabel + "-renamed"
	defer cleanupLabelCreds(t, s, newLabel)

	rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "okx", "account_label": oldLabel, "api_key": "sk-keep-me-12345678",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created db.MarketCredential
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	defer func() { _ = pg.DeleteMarketCredential(created.ID) }()

	before, _ := pg.ListMarketCredentials()
	countBefore := len(before)

	// 按 id 改名，key 传空（保留旧值）
	rec2 := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"id": created.ID, "provider": "okx", "account_label": newLabel, "api_key": "",
	})
	if rec2.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", rec2.Code, rec2.Body.String())
	}
	var renamed db.MarketCredential
	_ = json.Unmarshal(rec2.Body.Bytes(), &renamed)

	if renamed.ID != created.ID {
		t.Errorf("id 应不变：before=%d after=%d", created.ID, renamed.ID)
	}
	if renamed.AccountLabel != newLabel {
		t.Errorf("label 应变成 %q，实际 %q", newLabel, renamed.AccountLabel)
	}
	// 关键断言：记录数不变（没有新建）
	after, _ := pg.ListMarketCredentials()
	if len(after) != countBefore {
		t.Errorf("改名不该新建记录：before=%d after=%d", countBefore, len(after))
	}
	// key 应保留
	k, _, _, _, err := pg.SeparateAPIKeyForUse(created.ID)
	if err != nil || k != "sk-keep-me-12345678" {
		t.Errorf("改名时空 key 应保留旧值，实际 len=%d err=%v", len(k), err)
	}
}

// TestMarketCredentialIDNotFound 传不存在的 id 返回 404。
func TestMarketCredentialIDNotFound(t *testing.T) {
	s, done := credTestServer(t)
	defer done()
	rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"id": 999999999, "provider": "okx", "account_label": uniqueLabel(t),
	})
	if rec.Code != http.StatusNotFound {
		t.Errorf("不存在的 id 应 404，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	if payload["code"] != "CREDENTIAL_NOT_FOUND" {
		t.Errorf("code = %v, want CREDENTIAL_NOT_FOUND", payload["code"])
	}
}

// TestMarketCredentialLabelConflictOnUpdate 改 label 撞上另一条返回 409。
//
// 与 create_only 的 409 区分：那个是「你要新建的撞名了」，
// 这个是「你要改成的新名字被别人占了」—— 错误码不同，前端可给不同提示。
func TestMarketCredentialLabelConflictOnUpdate(t *testing.T) {
	s, done := credTestServer(t)
	defer done()
	pg := s.m.pg
	labelA := uniqueLabel(t)
	labelB := labelA + "-b"
	defer cleanupLabelCreds(t, s, labelA)
	defer cleanupLabelCreds(t, s, labelB)

	var ids []int64
	for _, l := range []string{labelA, labelB} {
		rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
			"provider": "okx", "account_label": l, "api_key": "sk-conflict-test-1234",
		})
		var c db.MarketCredential
		_ = json.Unmarshal(rec.Body.Bytes(), &c)
		ids = append(ids, c.ID)
	}
	defer func() {
		for _, id := range ids {
			_ = pg.DeleteMarketCredential(id)
		}
	}()

	rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"id": ids[0], "provider": "okx", "account_label": labelB,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("改 label 撞另一条应 409，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	if payload["code"] != "LABEL_CONFLICT" {
		t.Errorf("code = %v, want LABEL_CONFLICT", payload["code"])
	}
}

// TestMarketCredentialMeta meta 端点：okx + fosun 两种 purpose，以及 6 家数据源。
func TestMarketCredentialMeta(t *testing.T) {
	s, done := credTestServer(t)
	defer done()

	rec := httptest.NewRecorder()
	s.listMarketCredentialMeta(rec,
		httptest.NewRequest(http.MethodGet, "/api/market/credentials/meta", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("meta status=%d", rec.Code)
	}
	var body struct {
		Providers []map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("meta 不是合法 JSON: %v", err)
	}
	// 10 项 = okx/fosun × trade/data（4）+ 6 家 MCP 数据源。
	//
	// 同一个 provider 出现两次是**有意的**：交易凭据与行情凭据是不同的密钥、
	// 不同的权限（OKX 公开行情免凭证，填 key 只为提高限频配额）。
	//
	// 因此下面按 "provider:purpose" 做 lookup —— 裸 provider 会取到第一个。
	//
	// ⚠️ 断言历史：这条断言原为 4（2026-10-29 时只有 okx/fosun 两种 purpose）。
	// 2026-10-31 新增 6 个数据源 provider（ifind/fosun_data/tdx/neodata/
	// kuaicha/westock），meta 数量随之变成 10 —— 这里同步更新断言。
	// 同时原「ifind/tdx 不该出现在 meta」的断言已过期：那六家从「走 Connector /
	// 本地免凭证」改成「凭证页统一配」（见 mcp_credential_sync.go 的说明）。
	if len(body.Providers) != 11 {
		t.Fatalf("应恰好 11 项（4 既有 + 6 MCP 数据源 + 1 妙想 REST），实际 %d", len(body.Providers))
	}
	byKey := map[string]map[string]any{}
	for _, p := range body.Providers {
		key := p["provider"].(string) + ":" + p["purpose"].(string)
		byKey[key] = p
	}
	// akshare 仍是本地 Python sidecar 免凭证 —— 它不该出现在 meta 里。
	for k := range byKey {
		if strings.HasPrefix(k, "akshare:") {
			t.Errorf("%q 不该出现在 meta 里（本地 sidecar 免凭证）", k)
		}
	}
	// 6 家数据源必须在，且都 Verifiable + 有 endpoint 默认值。
	for _, want := range []string{"ifind", "fosun_data", "tdx", "neodata", "kuaicha", "westock"} {
		p, ok := byKey[want+":data"]
		if !ok {
			t.Errorf("meta 缺 %s:data（2026-10-31 新增的 6 家数据源之一）", want)
			continue
		}
		if p["verifiable"] != true {
			t.Errorf("%s:data 应 verifiable=true（走 MCP 同步 + discover）", want)
		}
		if p["supports_base_url"] != true {
			t.Errorf("%s:data 应 supports_base_url=true", want)
		}
	}

	okx, ok := byKey["okx:trade"]
	if !ok {
		t.Fatal("meta 缺 okx:trade")
	}
	if okx["verifiable"] != true {
		t.Error("okx 应 verifiable=true")
	}
	// 2026-11-02：okx 从 CLI 改走 REST，endpoint 可配（base_url 留空则用
	// https://www.okx.com）。旧断言「走 CLI 无可配 endpoint」已不成立。
	if okx["supports_base_url"] != true {
		t.Error("okx 应 supports_base_url=true（走 REST，endpoint 可配）")
	}
	sf, _ := json.Marshal(okx["secret_fields"])
	for _, k := range []string{"api_key", "api_secret", "passphrase"} {
		if !strings.Contains(string(sf), k) {
			t.Errorf("okx secret_fields 应含 %q，实际 %s", k, sf)
		}
	}

	fosun, ok := byKey["fosun:trade"]
	if !ok {
		t.Fatal("meta 缺 fosun:trade")
	}
	if fosun["verifiable"] != false {
		t.Error("fosun 应 verifiable=false（前端应隐藏验证按钮，而不是点了才 501）")
	}
	fields, _ := fosun["fields"].([]any)
	foundPEM := false
	for _, f := range fields {
		fm := f.(map[string]any)
		if fm["key"] == "server_public_key" {
			foundPEM = true
			if fm["multiline"] != true {
				t.Error("PEM 字段应为 multiline=true")
			}
			if fm["preserve_whitespace"] != true {
				t.Error("PEM 字段应为 preserve_whitespace=true")
			}
			if fm["kind"] != "password" {
				t.Error("PEM 字段应为 kind=password（脱敏显示）")
			}
		}
	}
	if !foundPEM {
		t.Error("fosun 缺 server_public_key 字段")
	}
}

// TestNormalizePEM PEM 归一化三步：trim 首尾 / CRLF→LF / 保留中间换行。
//
// # 为什么「保留中间换行」必须单独断言
//
// 最常见的错误实现是 `strings.Split(s,"\n")` + 逐行 trim + `Join(lines,"")`
// —— 把换行删光拼成一行。那会让 PEM 解析器读不到 base64 的结束位置。
// 只断言「首尾 trim 了」抓不到这个错。
func TestNormalizePEM(t *testing.T) {
	in := "\r\n\r\n-----BEGIN PUBLIC KEY-----\r\nMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8A\r\nMIIBCgKCAQEA1234\r\n-----END PUBLIC KEY-----\r\n\r\n"
	got := normalizePEM(in)

	if strings.HasPrefix(got, "\n") {
		t.Error("首部空行应被去掉")
	}
	if strings.HasSuffix(got, "\n") {
		t.Error("尾部空行应被去掉")
	}
	if strings.Contains(got, "\r") {
		t.Error("应统一成 LF，不该有 CR")
	}
	// 中间换行必须保留
	if !strings.Contains(got, "-----\nMIIBIjAN") {
		t.Errorf("中间换行被删除 —— PEM 结构破坏了: %q", got)
	}
	// 4 行 → 3 个换行
	if n := strings.Count(got, "\n"); n != 3 {
		t.Errorf("应有 3 个换行（4 行），实际 %d: %q", n, got)
	}
}

// TestNormalizeSecretFieldKind preserve_whitespace 与普通字段的处理差异。
func TestNormalizeSecretFieldKind(t *testing.T) {
	pem := "  -----BEGIN-----\nAAA\nBBB\n-----END-----  \n"
	if got := normalizeSecretField(pem, true); strings.HasSuffix(got, "\n") {
		t.Errorf("PEM 首尾空白应去掉，实际 %q", got)
	}
	plain := "  sk-abc123  "
	if got := normalizeSecretField(plain, false); got != "sk-abc123" {
		t.Errorf("普通字段应 TrimSpace，实际 %q", got)
	}
}
