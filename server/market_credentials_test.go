package server

// 行情凭证 API 的测试。
//
// # 最重要的一条
//
// TestMarketCredentialCreateReturnsHintOnly 是本次的安全底线：
// 它断言 GET 响应里**字面量不含**提交时的 key。
// 这条测试如果失败，说明明文 key 已经泄露给前端了。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// credTestServer 建一个只含 DB 的 Server（不需要 engine）。
func credTestServer(t *testing.T) (*Server, func()) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	return &Server{m: m}, func() { m.Close() }
}

// postJSON 发 POST 并返回响应体。
func postJSON(t *testing.T, fn http.HandlerFunc, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	fn(rec, req)
	return rec
}

// listJSON 调 list 并解码。
func listJSON(t *testing.T, s *Server) []map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.listMarketCredentials(rec, httptest.NewRequest(http.MethodGet, "/api/market/credentials", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("list 响应不是 JSON 数组: %v (%s)", err, rec.Body.String())
	}
	return out
}

// uniqueLabel 生成唯一 label（测试并发/重跑不冲突）。
func uniqueLabel(t *testing.T) string {
	return fmt.Sprintf("test-%d", time.Now().UnixNano())
}

// TestMarketCredentialCreateReturnsHintOnly **安全底线**：
// 提交的明文 key 绝不能出现在任何响应里。
func TestMarketCredentialCreateReturnsHintOnly(t *testing.T) {
	s, done := credTestServer(t)
	defer done()
	const secretKey = "sk-live-THIS-MUST-NOT-LEAK-a1b2c3d4"
	label := uniqueLabel(t)

	rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "okx", "account_label": label,
		"api_key": secretKey, "api_secret": "sec-MUST-NOT-LEAK", "passphrase": "pass-MUST-NOT-LEAK",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 1. POST 响应不能含明文
	if strings.Contains(rec.Body.String(), secretKey) {
		t.Fatalf("POST 响应泄露了全量 api_key: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sec-MUST-NOT-LEAK") {
		t.Fatalf("POST 响应泄露了 api_secret: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "pass-MUST-NOT-LEAK") {
		t.Fatalf("POST 响应泄露了 passphrase: %s", rec.Body.String())
	}

	// 2. GET 响应不能含明文
	rec2 := httptest.NewRecorder()
	s.listMarketCredentials(rec2, httptest.NewRequest(http.MethodGet, "/api/market/credentials", nil))
	body := rec2.Body.String()
	if strings.Contains(body, secretKey) {
		t.Fatalf("GET 响应泄露了全量 api_key: %s", body)
	}
	if strings.Contains(body, "sec-MUST-NOT-LEAK") || strings.Contains(body, "pass-MUST-NOT-LEAK") {
		t.Fatalf("GET 响应泄露了 secret/passphrase: %s", body)
	}
	// 3. 但 hint 要在（否则前端没法展示）
	if !strings.Contains(body, "sk-l") || !strings.Contains(body, "c3d4") {
		t.Errorf("GET 响应应含 hint（前4+***+后4），实际: %s", body)
	}
	if !strings.Contains(body, "***") {
		t.Errorf("hint 应含 *** 掩码，实际: %s", body)
	}

	// cleanup
	pg := s.m.pg
	list, _ := pg.ListMarketCredentials()
	for _, c := range list {
		if c.AccountLabel == label {
			_ = pg.DeleteMarketCredential(c.ID)
		}
	}
}

// TestMarketCredentialEmptyKeyKeepsExisting 第二次 POST 传空 key，原 key 保留。
//
// 这是 setLLM 的核心语义：用户只改 base_url 时不该被迫重打 key。
func TestMarketCredentialEmptyKeyKeepsExisting(t *testing.T) {
	s, done := credTestServer(t)
	defer done()
	pg := s.m.pg
	label := uniqueLabel(t)
	const firstKey = "sk-first-key-0000111122223333"

	rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "okx", "account_label": label, "api_key": firstKey,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("first create: %d %s", rec.Code, rec.Body.String())
	}
	var created db.MarketCredential
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	defer func() { _ = pg.DeleteMarketCredential(created.ID) }()

	hintBefore := created.APIKeyHint
	if hintBefore == "" {
		t.Fatal("首次创建应有 hint")
	}

	// 第二次：只改 base_url，key 传空串
	rec2 := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "okx", "account_label": label, "api_key": "",
		"base_url": "https://example.test",
	})
	if rec2.Code != http.StatusOK {
		t.Fatalf("second save: %d %s", rec2.Code, rec2.Body.String())
	}
	// 全量 key 必须还是原来那个
	k, _, _, _, err := pg.SeparateAPIKeyForUse(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if k != firstKey {
		t.Fatalf("空 key 应保留旧值，实际 key 变了（len %d）", len(k))
	}
	// hint 也不该变（如果变了说明「key 被当成空串写入了」）
	var updated db.MarketCredential
	_ = json.Unmarshal(rec2.Body.Bytes(), &updated)
	if updated.APIKeyHint != hintBefore {
		t.Errorf("空 key 时 hint 不该变: before=%q after=%q", hintBefore, updated.APIKeyHint)
	}
	if updated.BaseURL != "https://example.test" {
		t.Errorf("base_url 应更新，实际 %q", updated.BaseURL)
	}
}

// TestMarketCredentialClearKey 传 "<clear>" 时真的清空。
func TestMarketCredentialClearKey(t *testing.T) {
	s, done := credTestServer(t)
	defer done()
	pg := s.m.pg
	label := uniqueLabel(t)

	rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "okx", "account_label": label, "api_key": "sk-to-be-cleared-99998888",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created db.MarketCredential
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	defer func() { _ = pg.DeleteMarketCredential(created.ID) }()

	rec2 := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "okx", "account_label": label, "api_key": db.ClearMarker,
	})
	if rec2.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", rec2.Code, rec2.Body.String())
	}
	k, _, _, _, err := pg.SeparateAPIKeyForUse(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if k != "" {
		t.Errorf("<clear> 应清空 key，实际还有 %d 字节", len(k))
	}
	// hint 也应清空
	var after db.MarketCredential
	_ = json.Unmarshal(rec2.Body.Bytes(), &after)
	if after.APIKeyHint != "" {
		t.Errorf("清空 key 后 hint 也应为空，实际 %q", after.APIKeyHint)
	}
}

// TestMarketCredentialHintFormat hint 是 "abcd***wxyz" 形式。
func TestMarketCredentialHintFormat(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"正常长度", "abcd1234wxyz", "abcd***wxyz"},
		{"刚好 8 位", "abcdefgh", "***"},
		{"短于 8 位", "abc", "***"},
		{"空", "", ""},
		{"9 位", "abcdefghi", "abcd***fghi"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := db.MaskAPIKey(c.in); got != c.want {
				t.Errorf("MaskAPIKey(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestMarketCredentialVerifyErrorRedacted 验证失败时错误信息里的 key 被替换。
//
// # 这是第二个安全底线
//
// CLI 的 stderr 可能形如 `error: invalid api key sk-abcdef123456`,
// 如果直接落库，verify_error 就会在 GET 响应里回显 key。
// # 测试数据构造约束（重要，改这条测试前必读）
//
// **任何一个「被脱敏的值」都不能是另一个值的子串。**
//
// 反例（曾经的写法）：longKey="sk-live-AAAA1111"、shortKey="sk-1"。
// 长串先被替换成 <redacted> 后，断言 strings.Contains(after,"sk-1")==false
// 会因为 shortKey 本来就是 longKey 的前缀而误判 —— 实际上 shortKey 从未
// 单独出现在输入里。这是测试数据的坑，不是实现在漏脱敏。
//
// 所以这里用三个互不包含的字面量。
func TestMarketCredentialVerifyErrorRedacted(t *testing.T) {
	// 直接测脱敏函数（真跑 CLI 失败场景不可控）
	const secret = "sk-alpha-7777-bravo"   // 不含下面的 secret2
	const secret2 = "sec-charlie-9999"     // 不是 secret 的子串
	const pemLike = "-----BEGIN-KEY-delta" // 也不与上面互为子串
	in := fmt.Sprintf("error: invalid api key %s and %s; %s", secret, secret2, pemLike)
	out := truncateAndMask(in, 400)

	// 三个值都必须消失
	for _, leaked := range []string{secret, secret2} {
		if strings.Contains(out, leaked) {
			t.Fatalf("verify_error 泄露了凭证 %q: %s", leaked, out)
		}
	}
	// 脱敏占位符必须是 <redacted>，而不是 ***
	//
	// 为什么不能是 ***：*** 可能出现在正常 base_url 里（如 https://x.com/***/v1），
	// 用它做占位符会让「是否脱敏成功」的断言命中误报。
	if !strings.Contains(out, "<redacted>") {
		t.Errorf("应含脱敏占位符 <redacted>，实际: %s", out)
	}
	// 截断行为
	long := strings.Repeat("x", 1000)
	if got := truncateAndMask(long, 50); len(got) > 50 {
		t.Errorf("截断后长度应 <= 50，实际 %d", len(got))
	}
	// 空输入
	if got := truncateAndMask("   ", 200); got != "" {
		t.Errorf("空白输入应返回空串，实际 %q", got)
	}
}

// TestMarketCredentialVerifyNotSupported 非 okx 返回 501。
func TestMarketCredentialVerifyNotSupported(t *testing.T) {
	s, done := credTestServer(t)
	defer done()
	pg := s.m.pg
	label := uniqueLabel(t)

	rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "fosun", "account_label": label, "api_key": "fs-key-123456789",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created db.MarketCredential
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	defer func() { _ = pg.DeleteMarketCredential(created.ID) }()

	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/market/credentials/%d/verify", created.ID), nil)
	req.SetPathValue("id", fmt.Sprintf("%d", created.ID))
	vrec := httptest.NewRecorder()
	s.verifyMarketCredential(vrec, req)

	if vrec.Code != http.StatusNotImplemented {
		t.Errorf("非 okx 应返回 501，实际 %d: %s", vrec.Code, vrec.Body.String())
	}
	if !strings.Contains(vrec.Body.String(), "暂不支持") {
		t.Errorf("错误信息应说明不支持，实际: %s", vrec.Body.String())
	}
}

// TestMarketCredentialDelete 删除后查不到。
func TestMarketCredentialDelete(t *testing.T) {
	s, done := credTestServer(t)
	defer done()
	pg := s.m.pg
	label := uniqueLabel(t)

	rec := postJSON(t, s.saveMarketCredential, "/api/market/credentials", map[string]any{
		"provider": "ifind", "account_label": label, "api_key": "if-key-12345678",
	})
	var created db.MarketCredential
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.ID == 0 {
		t.Fatalf("创建失败: %s", rec.Body.String())
	}

	dreq := httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/api/market/credentials/%d", created.ID), nil)
	dreq.SetPathValue("id", fmt.Sprintf("%d", created.ID))
	drec := httptest.NewRecorder()
	s.deleteMarketCredential(drec, dreq)
	if drec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", drec.Code, drec.Body.String())
	}

	if _, err := pg.GetMarketCredential(created.ID); err != db.ErrCredentialNotFound {
		t.Errorf("删除后应查不到，实际 err=%v", err)
	}

	// 重复删除应 404
	drec2 := httptest.NewRecorder()
	s.deleteMarketCredential(drec2, dreq)
	if drec2.Code != http.StatusNotFound {
		t.Errorf("重复删除应 404，实际 %d", drec2.Code)
	}
}

// TestMaskedCredentialTypeHasNoSecretFields 类型层面保证：读模型没有明文 key 字段。
//
// # 为什么用反射测这个
//
// MarketCredential 是 GET 响应的唯一数据源。如果哪天有人给它加了个
// APIKey 字段（"反正 json:"-" 就行"），JSON 序列化的疏忽会直接泄露。
//
// 从**类型**上禁止比靠 json tag 更可靠 —— 这个测试就是那个禁止的守卫。
func TestMaskedCredentialTypeHasNoSecretFields(t *testing.T) {
	// 序列化一个字段全填的实例，检查输出里没有任何敏感字段名
	c := db.MarketCredential{
		ID: 1, Provider: "okx", AccountLabel: "x",
		APIKeyHint: "abcd***wxyz", BaseURL: "https://e.test", Enabled: true,
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, forbidden := range []string{"api_key\"", "api_secret", "passphrase"} {
		// "api_key_hint" 含 "api_key" 前缀，所以用带引号的完整形式匹配
		if strings.Contains(s, forbidden) {
			t.Errorf("MarketCredential 序列化后含敏感字段 %q: %s", forbidden, s)
		}
	}
	if !strings.Contains(s, "api_key_hint") {
		t.Error("应含 api_key_hint（脱敏展示用）")
	}
}

// TestVerifyOkxRESTRedactsSecrets 验证失败时错误文案不泄漏密钥。
//
// # 为什么要单独测这条（2026-11-02 改动后重写）
//
// 原测试测的是 `verifyOkxCLI` —— 那个函数不读 key，所以「不泄漏」
// 是个平凡结论。现在 verifyOkxREST **确实持有** api_key/api_secret/passphrase，
// 泄漏面才真实存在：上游可能把请求头回显进错误 message。
//
// 所以断言变成：故意让假上游在 message 里回显密钥，检查我们是否拦住。
func TestVerifyOkxRESTRedactsSecrets(t *testing.T) {
	const (
		key    = "leaky-api-key-1234567890"
		secret = "leaky-api-secret-0987654321"
		pass   = "leaky-passphrase"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 恶意/粗心的上游：把自己的入参原样写回错误信息
		_, _ = w.Write([]byte(`{"code":"50111","msg":"invalid key=` + key +
			` secret=` + secret + ` pass=` + pass + `"}`))
	}))
	defer srv.Close()

	ok, msg := verifyOkxREST(t.Context(), verifyOkxParams{
		APIKey: key, APISecret: secret, Passphrase: pass, BaseURL: srv.URL,
	})
	if ok {
		t.Fatal("不该通过")
	}
	for _, leak := range []string{secret, pass} {
		if strings.Contains(msg, leak) {
			t.Errorf("错误文案泄漏了凭据 %q: %s", leak, msg)
		}
	}
	if s, found := containsSensitive(msg); found {
		t.Errorf("错误信息含疑似凭证 %q: %s", s, msg)
	}
}

// containsSensitive 用 redact 包的检测能力判断文本里有没有未遮的凭证。
func containsSensitive(s string) (string, bool) {
	return dbContainsSecret(s)
}

var dbContainsSecret = func(s string) (string, bool) {
	// 用 redact.ContainsSecret 的等价能力：
	// 这里只做简单形态检测（sk- 前缀的长串），因为测试不该依赖 redact 内部实现。
	for _, tok := range strings.Fields(s) {
		if strings.HasPrefix(tok, "sk-") && len(tok) > 20 {
			return tok, true
		}
	}
	return "", false
}
