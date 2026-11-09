package vaultd

// vaultd 的验收测试（WE-5）。
//
// # 测什么
//
// WE-5 的验收标准原文：
//   - 只监听 127.0.0.1
//   - 无 token / 错 token → 401
//   - /v1/credentials → 404
//   - vaultd 不 import agent/server/llmpool（go list -deps 断言）
//
// 前三条在这里测；第四条在 build.sh 的 `go list` 断言里（见
// scripts/check-vaultd-isolation.sh）。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const testToken = "test-token-0123456789abcdef"

func newTestServer(t *testing.T, chans map[string]Channel) *Server {
	t.Helper()
	s, err := NewServer(testToken, chans)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

// do 发一个请求，带可选的 token。
func do(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Buffer
	if body == "" {
		rd = bytes.NewBufferString("{}")
	} else {
		rd = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ---- 认证 ----

func TestNoTokenReturns401(t *testing.T) {
	s := newTestServer(t, nil)
	rec := do(t, s.Handler(), http.MethodPost, "/v1/submit", "", `{}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("无 token 应 401，实际 %d", rec.Code)
	}
	body := decode(t, rec)
	if body["code"] != CodeUnauthorized {
		t.Errorf("错误码应为 %s，实际 %v", CodeUnauthorized, body["code"])
	}
}

func TestWrongTokenReturns401(t *testing.T) {
	s := newTestServer(t, nil)
	rec := do(t, s.Handler(), http.MethodPost, "/v1/submit", "wrong-token", `{}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("错 token 应 401，实际 %d", rec.Code)
	}
}

func TestMissingBearerPrefixReturns401(t *testing.T) {
	s := newTestServer(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/submit", strings.NewReader("{}"))
	req.Header.Set("Authorization", testToken) // 缺 "Bearer " 前缀
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("缺 Bearer 前缀应 401，实际 %d", rec.Code)
	}
}

// ---- 非目标：不提供 GET ----

func TestCredentialsEndpointReturns404(t *testing.T) {
	s := newTestServer(t, nil)
	// 带正确 token 也必须是 404 —— 这个接口根本不该存在。
	rec := do(t, s.Handler(), http.MethodGet, "/v1/credentials", testToken, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("/v1/credentials 应 404（明确的非目标），实际 %d body=%s",
			rec.Code, rec.Body.String())
	}
}

func TestAnyGetReturns404(t *testing.T) {
	s := newTestServer(t, nil)
	for _, p := range []string{"/v1/submit", "/v1/sign", "/", "/v1/anything"} {
		rec := do(t, s.Handler(), http.MethodGet, p, testToken, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s 应 404（只暴露 3 个 POST），实际 %d", p, rec.Code)
		}
	}
}

// ---- /v1/sign 是 501 ----

func TestSignReturns501(t *testing.T) {
	s := newTestServer(t, nil)
	rec := do(t, s.Handler(), http.MethodPost, "/v1/sign", testToken, `{"channel":"okx"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("/v1/sign 在 P0 应 501，实际 %d", rec.Code)
	}
	body := decode(t, rec)
	if body["code"] != CodeNotImplemented {
		t.Errorf("错误码应为 %s，实际 %v", CodeNotImplemented, body["code"])
	}
}

// ---- 通道未配置时 503（不是 500）----

func TestSubmitWithoutChannelReturns503(t *testing.T) {
	s := newTestServer(t, nil) // 无通道
	rec := do(t, s.Handler(), http.MethodPost, "/v1/submit", testToken,
		`{"channel":"okx","account_ref":"a","client_order_id":"c1",
		  "symbol":"BTC-USDT","side":"buy","qty":"1"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("通道未配置应 503（预期部署状态，不是故障），实际 %d", rec.Code)
	}
	body := decode(t, rec)
	if body["code"] != CodeChannelUnavailable {
		t.Errorf("错误码应为 %s，实际 %v", CodeChannelUnavailable, body["code"])
	}
}

// ---- 参数校验 ----

func TestSubmitValidatesClientOrderID(t *testing.T) {
	s := newTestServer(t, nil)
	rec := do(t, s.Handler(), http.MethodPost, "/v1/submit", testToken,
		`{"channel":"okx","account_ref":"a","client_order_id":"",
		  "symbol":"BTC-USDT","side":"buy","qty":"1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 client_order_id 应 400，实际 %d", rec.Code)
	}
	body := decode(t, rec)
	msg, _ := body["error"].(string)
	// 错误信息要说明「为什么必须有」——它是幂等键。
	if !strings.Contains(msg, "幂等") {
		t.Errorf("错误信息应说明 client_order_id 是幂等键，实际：%s", msg)
	}
}

func TestSubmitValidatesSide(t *testing.T) {
	s := newTestServer(t, nil)
	rec := do(t, s.Handler(), http.MethodPost, "/v1/submit", testToken,
		`{"channel":"okx","account_ref":"a","client_order_id":"c1",
		  "symbol":"BTC-USDT","side":"long","qty":"1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法 side 应 400，实际 %d", rec.Code)
	}
}

func TestSubmitValidatesChannel(t *testing.T) {
	s := newTestServer(t, nil)
	rec := do(t, s.Handler(), http.MethodPost, "/v1/submit", testToken,
		`{"channel":"binance","account_ref":"a","client_order_id":"c1",
		  "symbol":"BTC-USDT","side":"buy","qty":"1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 channel 应 400，实际 %d", rec.Code)
	}
	body := decode(t, rec)
	if body["code"] != CodeChannelInvalid {
		t.Errorf("错误码应为 %s，实际 %v", CodeChannelInvalid, body["code"])
	}
}

// ---- query 参数校验 ----

func TestQueryRequiresEitherClientOrderIDOrRange(t *testing.T) {
	s := newTestServer(t, nil)
	rec := do(t, s.Handler(), http.MethodPost, "/v1/query", testToken,
		`{"channel":"okx","account_ref":"a"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("既无 client_order_id 也无时间区间应 400，实际 %d", rec.Code)
	}
	body := decode(t, rec)
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "client_order_id") {
		t.Errorf("错误信息应指出两种查询方式，实际：%s", msg)
	}
}

// ---- Submit 成功路径 + raw_ack 脱敏 ----

func TestSubmitRedactsRawAck(t *testing.T) {
	const fakeKey = "sk-live-abcdefghijklmnopqrstuvwxyz123456"
	ch := &fakeChannel{
		submitResp: SubmitResponse{
			ChannelOrderID: "CH-1",
			AcceptedAt:     "2026-10-28T00:00:00Z",
			// 通道把凭证原样带回 —— vaultd 的出口必须脱敏。
			RawAck: json.RawMessage(`{"api_key":"` + fakeKey + `","ok":true}`),
		},
	}
	s := newTestServer(t, map[string]Channel{ChannelOKX: ch})

	rec := do(t, s.Handler(), http.MethodPost, "/v1/submit", testToken,
		`{"channel":"okx","account_ref":"a","client_order_id":"c1",
		  "symbol":"BTC-USDT","side":"buy","qty":"1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("submit 应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}

	out := rec.Body.String()
	if strings.Contains(out, fakeKey) {
		t.Fatalf("响应里出现了假 key —— 出参脱敏失效\nbody=%s", out)
	}
	if !strings.Contains(out, "CH-1") {
		t.Error("响应应保留 channel_order_id（脱敏不能把正常字段也抹掉）")
	}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\nbody=%s", err, rec.Body.String())
	}
	return m
}

// ---- fakeChannel ----

// fakeChannel 是测试用通道。
//
// # 为什么它存在于生产包里
//
// 因为 WE-14 的泄漏测试需要「一条会返回带凭证 raw_ack 的通道」来验证脱敏。
// 放在 _test.go 里的话，exec 包的测试用不到它。
//
// ⚠️ 它不会被 cmd/vaultd 装配（main.go 传 nil），所以生产路径不受影响。
type fakeChannel struct {
	mu         sync.Mutex
	submitResp SubmitResponse
	submitErr  error
	queryResp  QueryResponse
	queryErr   error
	healthErr  error

	submitCalls int
	seenIDs     map[string]bool
}

func (f *fakeChannel) Submit(req SubmitRequest) (SubmitResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submitCalls++
	if f.seenIDs == nil {
		f.seenIDs = map[string]bool{}
	}
	// 模拟幂等：同一个 clientOrderID 第二次返回同样的响应但不重复下单。
	if f.seenIDs[req.ClientOrderID] {
		return f.submitResp, f.submitErr
	}
	f.seenIDs[req.ClientOrderID] = true
	return f.submitResp, f.submitErr
}

func (f *fakeChannel) Query(_ QueryRequest) (QueryResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queryResp, f.queryErr
}

func (f *fakeChannel) Health() error { return f.healthErr }

// NewFakeChannelForTest 导出 fakeChannel，供其他包的泄漏测试使用。
//
// P0 只需要「能返回带凭证的 raw_ack」，所以参数只有那一个。
func NewFakeChannelForTest(rawAck json.RawMessage) Channel {
	return &fakeChannel{
		submitResp: SubmitResponse{
			ChannelOrderID: "FAKE-1",
			AcceptedAt:     "2026-10-28T00:00:00Z",
			RawAck:         rawAck,
		},
	}
}
