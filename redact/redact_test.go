package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---- 7 类模式各自的掩码测试 ----

func TestPEM(t *testing.T) {
	in := `before
-----BEGIN RSA PRIVATE KEY-----
MIIEowIBAAKCAQEA1234567890abcdefghij
-----END RSA PRIVATE KEY-----
after`
	got := Mask(in)
	if strings.Contains(got, "MIIEowIBAAKCAQEA") {
		t.Fatalf("PEM 内容未遮: %q", got)
	}
	if !strings.Contains(got, maskBlock) {
		t.Fatalf("PEM 应替换为 %s, got %q", maskBlock, got)
	}
	// 周围的文字应保留
	if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Fatalf("PEM 前后文字被误伤: %q", got)
	}
}

func TestBearer(t *testing.T) {
	in := `Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxIn0.abcdefghij`
	got := Mask(in)
	if strings.Contains(got, "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9") {
		t.Fatalf("Bearer token 未遮: %q", got)
	}
	if !strings.Contains(got, "Bearer ***") {
		t.Fatalf("应保留 scheme 并遮 token, got %q", got)
	}
}

func TestCookie(t *testing.T) {
	in := `Set-Cookie: session=abc123def456ghi789; Path=/; HttpOnly`
	got := Mask(in)
	if strings.Contains(got, "abc123def456ghi789") {
		t.Fatalf("Cookie 值未遮: %q", got)
	}
	if !strings.Contains(got, "Set-Cookie") {
		t.Fatalf("Cookie 头名被误伤: %q", got)
	}
}

func TestURLCred(t *testing.T) {
	in := `fetch https://alice:s3cr3tP@ssw0rd@api.example.com/v1/quote`
	got := Mask(in)
	if strings.Contains(got, "s3cr3tP@ssw0rd") || strings.Contains(got, "alice:") {
		t.Fatalf("URL 内嵌凭证未遮: %q", got)
	}
	if !strings.Contains(got, "api.example.com") {
		t.Fatalf("host 被误伤: %q", got)
	}
	// 无凭证的 URL 不应被改
	clean := `https://api.example.com/v1/quote?symbol=SPY`
	if Mask(clean) != clean {
		t.Fatalf("正常 URL 被误伤: %q -> %q", clean, Mask(clean))
	}
}

func TestKeyValue(t *testing.T) {
	cases := []string{
		`api_key=sk_live_1234567890abcdef`,
		`"api_key": "sk_live_1234567890abcdef"`,
		`password=hunter2hunter2`,
		`secretKey: abcd1234efgh5678`,
		`OK-ACCESS-PASSPHRASE=MyPass2024!`,
		`sub_account_id=368241963`,
		`clientPrivateKey":"MIIEvQIBADANBg`,
	}
	for _, c := range cases {
		got := Mask(c)
		// 原始值不应完整出现
		for _, token := range []string{"sk_live_1234567890abcdef", "hunter2hunter2", "abcd1234efgh5678", "MyPass2024!", "368241963", "MIIEvQIBADANBg"} {
			if strings.Contains(got, token) {
				t.Fatalf("key=value 值未遮: in=%q got=%q token=%q", c, got, token)
			}
		}
		// key 名应保留（这是刻意的：排查时需要知道哪个字段被遮）
		if !strings.Contains(got, "***") {
			t.Fatalf("应出现掩码: in=%q got=%q", c, got)
		}
	}
}

func TestFosunEnv(t *testing.T) {
	in := "FSOPENAPI_CLIENT_PRIVATE_KEY=-----BEGIN PRIVATE KEY-----abc\nnext=line"
	got := Mask(in)
	if strings.Contains(got, "abc") {
		t.Fatalf("环境变量值未遮: %q", got)
	}
	if !strings.Contains(got, "FSOPENAPI_CLIENT_PRIVATE_KEY=") {
		t.Fatalf("环境变量名被误伤: %q", got)
	}
}

func TestBlobFallback(t *testing.T) {
	// 64+ 字符的 base64，不含任何 key 名 —— 只有兜底规则能抓
	blob := strings.Repeat("Ab3xY9", 11) // 66 字符
	in := "prefix " + blob + " suffix"
	got := Mask(in)
	if strings.Contains(got, blob) {
		t.Fatalf("长 blob 未遮: %q", got)
	}
	if !strings.Contains(got, "prefix") || !strings.Contains(got, "suffix") {
		t.Fatalf("blob 周围文字被误伤: %q", got)
	}
}

// ---- 幂等 ----

func TestMaskIdempotent(t *testing.T) {
	inputs := []string{
		`api_key=sk_live_1234567890abcdef`,
		`Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxIn0.abcdefghij`,
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA1234567890abcdefghij\n-----END RSA PRIVATE KEY-----",
		`Set-Cookie: session=abc123def456ghi789; Path=/`,
		`fetch https://alice:s3cr3tP@ssw0rd@api.example.com/v1/quote`,
		"FSOPENAPI_CLIENT_PRIVATE_KEY=-----BEGIN PRIVATE KEY-----abc",
		"prefix " + strings.Repeat("Ab3xY9", 11) + " suffix",
		// 已经掩码过的串
		`api_key=***`,
		`Authorization: Bearer ***`,
		maskBlock,
		``,
		` normale text without secrets `,
	}
	for i, in := range inputs {
		once := Mask(in)
		twice := Mask(once)
		if once != twice {
			t.Fatalf("case %d 非幂等:\n in    =%q\n once  =%q\n twice =%q", i, in, once, twice)
		}
	}
}

// ---- 边界 ----

func TestMaskBoundaries(t *testing.T) {
	// key 在字符串开头
	if got := Mask(`api_key=sk_live_1234567890abcdef`); strings.Contains(got, "sk_live_1234567890abcdef") {
		t.Fatalf("开头 key 未遮: %q", got)
	}
	// key 在结尾
	if got := Mask(`config api_key=sk_live_1234567890abcdef`); strings.Contains(got, "sk_live_1234567890abcdef") {
		t.Fatalf("结尾 key 未遮: %q", got)
	}
	// key 在中间
	if got := Mask(`pre api_key=sk_live_1234567890abcdef post`); strings.Contains(got, "sk_live_1234567890abcdef") {
		t.Fatalf("中间 key 未遮: %q", got)
	}
	// 同一行多个 key
	multi := `api_key=sk_live_1111111111111111 secret=sk_live_2222222222222222`
	got := Mask(multi)
	if strings.Contains(got, "sk_live_1111111111111111") || strings.Contains(got, "sk_live_2222222222222222") {
		t.Fatalf("同行多 key 未全遮: %q", got)
	}
	// 已掩码的串再掩码不变形
	already := `api_key=*** password=***`
	if Mask(already) != already {
		t.Fatalf("已掩码串变形: %q -> %q", already, Mask(already))
	}
	// 空串
	if Mask("") != "" {
		t.Fatal("空串应返回空串")
	}
	// 短中文（快速排除路径）不应被改
	zh := "任务已完成"
	if Mask(zh) != zh {
		t.Fatalf("短中文被误伤: %q -> %q", zh, Mask(zh))
	}
}

// ---- MaskJSON 保持 JSON 有效 ----

func TestMaskJSONStaysValid(t *testing.T) {
	in := []byte(`{"api_key":"sk_live_1234567890abcdef","symbol":"SPY","note":"含 \"引号\" 与 \\ 反斜杠"}`)
	got := MaskJSON(in)
	if !json.Valid(got) {
		t.Fatalf("掩码后 JSON 失效: %q", string(got))
	}
	if strings.Contains(string(got), "sk_live_1234567890abcdef") {
		t.Fatalf("JSON 内 key 未遮: %s", string(got))
	}
	if !strings.Contains(string(got), "SPY") {
		t.Fatalf("正常字段被误伤: %s", string(got))
	}
}

// ---- traffic 端点判定 ----

func TestShouldRedactURL(t *testing.T) {
	yes := []string{
		"https://api.fosunwealth.com/api/v1/trade/create_order",
		"https://api.fosunwealth.com/api/v1/account/list",
		"https://api.fosunwealth.com/api/v1/portfolio/holdings",
		"https://www.okx.com/api/v5/trade/order",
		"https://www.okx.com/api/v5/account/balance",
		"https://www.okx.com/api/v5/asset/transfer",
	}
	for _, u := range yes {
		if !ShouldRedactURL(u) {
			t.Fatalf("交易类端点应遮: %s", u)
		}
	}
	no := []string{
		"https://api.fosunwealth.com/api/v1/market/secu/quote",
		"https://api.fosunwealth.com/api/v1/market/kline",
		"https://www.okx.com/api/v5/market/ticker",
		"https://www.okx.com/api/v5/public/instruments",
		"https://query1.finance.yahoo.com/v8/finance/chart/QQQ",
		"http://127.0.0.1/health",
	}
	for _, u := range no {
		if ShouldRedactURL(u) {
			t.Fatalf("行情类端点不应遮（保可复现）: %s", u)
		}
	}
	// query 不参与判定
	if ShouldRedactURL("https://api.x.com/api/v1/market/quote?api_key=abc123456789") {
		t.Fatal("query 里的 key 不应让行情端点被判为交易类")
	}
}

// ---- 端到端：模拟 6 个落库点全跑一遍，扫 4 张"表" ----

// fakeTable 模拟一张落库表：把写入的每条记录收集起来，最后统一扫描。
type fakeTable struct{ rows []string }

func (f *fakeTable) insert(s string) { f.rows = append(f.rows, s) }
func (f *fakeTable) all() string     { return strings.Join(f.rows, "\n") }

// stubChannel 是 vaultd 通道的测试替身。
//
// 接口刻意最小化：只有下单与查询两个方法。真实实现（fosunChannel / okxChannel）
// 返回的 rawAck 会回显 sub_account_id 与 passphrase 校验详情，
// 这正是本测试要验证「被遮住」的东西。
type stubChannel struct {
	redactOn bool
}

type stubOrder struct {
	ClientOrderID string
	Symbol        string
	Qty           string
	RawAck        string
}

func (c *stubChannel) Submit(o stubOrder) stubOrder {
	// 真实通道的 ack 会长这样（含账户标识）
	ack := `{"order_id":"ord-1","sub_account_id":"368241963","status":"submitted","api_key":"sk_live_abcdef1234567890"}`
	if c.redactOn {
		ack = Mask(ack)
	}
	o.RawAck = ack
	return o
}

func (c *stubChannel) Query(clientOrderID string) string {
	raw := `{"order_id":"ord-1","sub_account_id":"368241963","filled_qty":"1","avg_price":"100.00","passphrase_check":"MyPass2024!"}`
	if c.redactOn {
		return Mask(raw)
	}
	return raw
}

// TestNoSecretInPersistedPaths 是 redact 包唯一能证明「三重保证真的生效」的测试。
// 它不测功能，只测「有没有漏」。
//
// 流程：用假 key 走一遍 submit → query → 对账 → 写 4 张表，
// 然后对 4 张表的全部内容拼接，断言假 key 一次都不出现。
func TestNoSecretInPersistedPaths(t *testing.T) {
	const fakeKey = "sk_live_abcdef1234567890"
	const fakePass = "MyPass2024!"
	const fakeAcct = "368241963"

	// 4 张表（对应真实落点：llm_records / activity / traffic_evidence_snapshots / exec_reconciliations）
	llmRecords := &fakeTable{}
	activity := &fakeTable{}
	snapshots := &fakeTable{}
	reconciliations := &fakeTable{}

	ch := &stubChannel{redactOn: true}

	// 1) submit 的 ack 落 exec_reconciliations（并回显到 activity）
	ord := ch.Submit(stubOrder{ClientOrderID: "c-1", Symbol: "SPY", Qty: "1"})
	reconciliations.insert(Mask(ord.RawAck))
	activity.insert(Mask(`submit ack: ` + ord.RawAck))

	// 2) query 的结果同样落两处
	q := ch.Query("c-1")
	reconciliations.insert(Mask(q))

	// 3) 交易类端点的 traffic 快照
	body := []byte(`{"sub_account_id":"368241963","api_key":"sk_live_abcdef1234567890"}`)
	masked, redacted := MaskForURL("https://api.fosunwealth.com/api/v1/trade/create_order", body)
	if !redacted {
		t.Fatal("交易类端点应被标记 redacted")
	}
	snapshots.insert(string(masked))

	// 4) 行情类端点：不遮（保可复现），但也不能含凭证
	quoteBody := []byte(`{"symbol":"SPY","last":"580.12"}`)
	quoteMasked, quoteRedacted := MaskForURL("https://api.fosunwealth.com/api/v1/market/secu/quote", quoteBody)
	if quoteRedacted {
		t.Fatal("行情类端点不应被标记 redacted")
	}
	snapshots.insert(string(quoteMasked))

	// 5) LLM 记录（prompt 里意外带了 key 的极端情况）
	llmRecords.insert(Mask(`user said: my api_key=sk_live_abcdef1234567890 please help`))

	// ---- 扫描 ----
	for name, tbl := range map[string]*fakeTable{
		"llm_records": llmRecords, "activity": activity,
		"traffic_evidence_snapshots": snapshots, "exec_reconciliations": reconciliations,
	} {
		got := tbl.all()
		for _, secret := range []string{fakeKey, fakePass, fakeAcct} {
			if strings.Contains(got, secret) {
				t.Fatalf("表 %s 泄漏 %q\n内容:\n%s", name, secret, got)
			}
		}
	}

	// 反例对照：若通道不遮，上面的断言必须能抓到（证明测试本身有效）
	raw := &stubChannel{redactOn: false}
	bad := raw.Submit(stubOrder{ClientOrderID: "c-2", Symbol: "SPY", Qty: "1"})
	if !strings.Contains(bad.RawAck, fakeKey) {
		t.Fatalf("对照组应有泄漏（否则测试无意义）: %q", bad.RawAck)
	}
	if Mask(bad.RawAck) == bad.RawAck {
		t.Fatal("掩码未生效")
	}
}

// ---- 性能 ----

func BenchmarkMaskShortCN(b *testing.B) {
	s := "分析师已完成技术面分析，结论为中性偏多"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Mask(s)
	}
}

func BenchmarkMaskTypicalJSON(b *testing.B) {
	s := `{"symbol":"SPY","last":"580.12","change_pct":"0.34","volume":"61234500","ts":"2026-10-26T16:00:00-04:00","source":"ifind"}`
	b.SetBytes(int64(len(s)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Mask(s)
	}
}

func BenchmarkMaskWithSecret(b *testing.B) {
	s := `{"api_key":"sk_live_1234567890abcdef","symbol":"SPY","Authorization":"Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9abcdefghij"}`
	b.SetBytes(int64(len(s)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Mask(s)
	}
}

func BenchmarkMaskLargeBody(b *testing.B) {
	// 模拟 256KB 的行情响应体（traffic 里的典型大包）
	s := strings.Repeat(`{"symbol":"SPY","last":"580.12"},`, 9000)
	b.SetBytes(int64(len(s)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Mask(s)
	}
}

// ---- slog.ReplaceAttr 的递归助手（供 vaultd 日志用） ----

func TestMaskAttrsRecursive(t *testing.T) {
	attrs := []any{
		"channel", "fosunwealth",
		"api_key", "sk_live_1234567890abcdef",
		"nested", map[string]any{
			"passphrase": "MyPass2024!",
			"symbol":     "SPY",
		},
	}
	got := MaskAnySlice(attrs)
	flat := ""
	for _, v := range got {
		flat += toString(v) + "|"
	}
	for _, secret := range []string{"sk_live_1234567890abcdef", "MyPass2024!"} {
		if strings.Contains(flat, secret) {
			t.Fatalf("嵌套属性泄漏 %q: %v", secret, got)
		}
	}
	if !strings.Contains(flat, "SPY") {
		t.Fatalf("正常值被误伤: %v", got)
	}
	if !strings.Contains(flat, "fosunwealth") {
		t.Fatalf("正常值被误伤: %v", got)
	}
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case map[string]any:
		var sb strings.Builder
		for k, vv := range x {
			sb.WriteString(k)
			sb.WriteString("=")
			sb.WriteString(toString(vv))
			sb.WriteString(",")
		}
		return sb.String()
	case []any:
		var sb strings.Builder
		for _, vv := range x {
			sb.WriteString(toString(vv))
			sb.WriteString("|")
		}
		return sb.String()
	default:
		return ""
	}
}

// TestMaskAttrsNestedSensitiveKeys 覆盖「嵌套 map 里的敏感 key」这一类泄漏。
//
// 回归 bug(mask_attrs.go 的 map 分支):原实现是
//
//	out[Mask(k)] = MaskAny(vv)
//
// MaskAny 只看**值形态**,不看 key 名。于是 "passphrase": "MyPass2024!"
// 这种「key 敏感但值不匹配任何凭证正则」的条目会原样放行 —— 值是个普通
// 字符串,Mask 认不出它是秘密;而 key 名本来已经说明了它是。
//
// 正确做法是把 key 带进递归走 maskAnyWithKey,它先判 IsSensitiveKey(key)。
func TestMaskAttrsNestedSensitiveKeys(t *testing.T) {
	secrets := map[string]string{
		"passphrase": "MyPass2024!",
		"api_key":    "plainlookingkey123", // 不匹配 sk_ 前缀,只能靠 key 名判
		"password":   "hunter2hunter2",     // 同上
		"secret":     "totally-not-a-pattern",
		"token":      "abcdef123456",
	}
	for key, val := range secrets {
		t.Run(key, func(t *testing.T) {
			attrs := []any{"nested", map[string]any{key: val, "symbol": "SPY"}}
			got := MaskAnySlice(attrs)
			flat := ""
			for _, v := range got {
				flat += toString(v) + "|"
			}
			if strings.Contains(flat, val) {
				t.Fatalf("嵌套敏感 key %q 的值泄漏: %v", key, got)
			}
			// 非敏感同层字段不能被误伤。
			if !strings.Contains(flat, "SPY") {
				t.Fatalf("正常值被误伤: %v", got)
			}
		})
	}
}

// TestMaskAttrsDeepNested 覆盖三层嵌套:每一层都必须把 key 带下去。
//
// 修复只改了 map 分支一层调用的话,两层以上仍会漏 —— 因为
// maskAnyWithKey(k, map) 会走 MaskAny(map),而 MaskAny 的 map 分支若
// 依旧是 MaskAny(vv),第三层的 key 又丢了。这个测试专门锁住「递归每一层都带 key」。
func TestMaskAttrsDeepNested(t *testing.T) {
	const secret = "DeepNestedSecret2024!"

	t.Run("三层 map", func(t *testing.T) {
		attrs := []any{
			"ctx", map[string]any{
				"level2": map[string]any{
					"level3": map[string]any{
						"passphrase": secret,
						"symbol":     "QQQ",
					},
				},
			},
		}
		got := MaskAnySlice(attrs)
		flat := toString(got)
		if strings.Contains(flat, secret) {
			t.Fatalf("三层嵌套泄漏: %v", flat)
		}
		if !strings.Contains(flat, "QQQ") {
			t.Fatalf("正常值被误伤: %v", flat)
		}
	})

	t.Run("map 套 slice 套 map", func(t *testing.T) {
		attrs := []any{
			"ctx", map[string]any{
				"items": []any{
					map[string]any{"api_key": secret, "symbol": "IWM"},
				},
			},
		}
		got := MaskAnySlice(attrs)
		flat := toString(got)
		if strings.Contains(flat, secret) {
			t.Fatalf("map→slice→map 泄漏: %v", flat)
		}
		if !strings.Contains(flat, "IWM") {
			t.Fatalf("正常值被误伤: %v", flat)
		}
	})

	t.Run("slice 套 slice 套 map", func(t *testing.T) {
		attrs := []any{
			"ctx", []any{
				[]any{
					map[string]any{"password": secret, "symbol": "DIA"},
				},
			},
		}
		got := MaskAnySlice(attrs)
		flat := toString(got)
		if strings.Contains(flat, secret) {
			t.Fatalf("slice→slice→map 泄漏: %v", flat)
		}
		if !strings.Contains(flat, "DIA") {
			t.Fatalf("正常值被误伤: %v", flat)
		}
	})
}
