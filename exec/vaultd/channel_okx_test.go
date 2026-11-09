package vaultd

// OKX 通道的单元测试。
//
// # 测什么
//
//   - 签名 header 四个都在（且 sign 是 base64 的 32 字节 HMAC）
//   - body 里 clOrdId / instId / tdMode / side / ordType / sz 正确
//   - 同一 clientOrderID 提交两次 → 只发一次 HTTP（内存幂等）
//   - Health 打到 /api/v5/public/time
//   - demo 模式带 x-simulated-trading: 1
//   - 请求里绝不出现 secret 明文的任何可打印形式以外的泄漏（header 值必须有，但只在 header 里）
//
// # 为什么用 httptest 而不是打真 OKX
//
// 打真 OKX 需要真 key，且结果不确定（下单会花钱）。httptest 能精确断言
// 「我方发了什么」，这才是通道实现的责任边界。

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// okxRecorder 记录假 OKX 收到的请求。
type okxRecorder struct {
	mu       sync.Mutex
	requests []recordedReq
}

type recordedReq struct {
	method string
	path   string
	query  string
	header http.Header
	body   []byte
}

func (r *okxRecorder) add(req *http.Request) []byte {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, recordedReq{
		method: req.Method,
		path:   req.URL.Path,
		query:  req.URL.RawQuery,
		header: req.Header.Clone(),
		body:   body,
	})
	return body
}

func (r *okxRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func (r *okxRecorder) last(t *testing.T) recordedReq {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) == 0 {
		t.Fatal("假 OKX 没收到任何请求")
	}
	return r.requests[len(r.requests)-1]
}

// newFakeOKX 起一个假 OKX 服务。
func newFakeOKX(t *testing.T, rec *okxRecorder) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v5/public/time":
			_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[{"ts":"1700000000000"}]}`))
		case r.URL.Path == "/api/v5/trade/order":
			_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[{"ordId":"ORD-123","clOrdId":"","sCode":"0","sMsg":""}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"404","msg":"unknown"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestOKX(t *testing.T, baseURL string, demo bool) *OKXChannel {
	t.Helper()
	ch, err := NewOKXChannel(OKXConfig{
		APIKey:     "test-key",
		SecretKey:  "test-secret",
		Passphrase: "test-pass",
		BaseURL:    baseURL,
		Demo:       demo,
	})
	if err != nil {
		t.Fatalf("NewOKXChannel: %v", err)
	}
	return ch
}

func testSubmitReq(clOrdID string) SubmitRequest {
	return SubmitRequest{
		Channel:       ChannelOKX,
		AccountRef:    "acct-1",
		ClientOrderID: clOrdID,
		Symbol:        "BTC/USDT",
		Side:          "buy",
		Qty:           "0.01",
		OrderType:     "limit",
		Price:         "30000",
	}
}

// TestOKXSubmitSignsAndBuildsBody 断言签名 header 与 body 字段。
func TestOKXSubmitSignsAndBuildsBody(t *testing.T) {
	rec := &okxRecorder{}
	srv := newFakeOKX(t, rec)
	ch := newTestOKX(t, srv.URL, false)

	resp, err := ch.Submit(testSubmitReq("coid-001"))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if resp.ChannelOrderID != "ORD-123" {
		t.Errorf("ChannelOrderID 应取 data[0].ordId = ORD-123，实际 %q", resp.ChannelOrderID)
	}

	got := rec.last(t)
	if got.method != http.MethodPost {
		t.Errorf("下单应为 POST，实际 %s", got.method)
	}
	if got.path != "/api/v5/trade/order" {
		t.Errorf("下单路径应为 /api/v5/trade/order，实际 %s", got.path)
	}

	// 四个签名 header 一个都不能少。
	for _, h := range []string{
		"OK-ACCESS-KEY", "OK-ACCESS-SIGN", "OK-ACCESS-TIMESTAMP", "OK-ACCESS-PASSPHRASE",
	} {
		if got.header.Get(h) == "" {
			t.Errorf("缺少签名 header %s", h)
		}
	}
	if got.header.Get("OK-ACCESS-KEY") != "test-key" {
		t.Errorf("OK-ACCESS-KEY 应为 test-key，实际 %q", got.header.Get("OK-ACCESS-KEY"))
	}

	// 独立重算一遍签名，证明拼接规则是 timestamp+METHOD+path+body。
	ts := got.header.Get("OK-ACCESS-TIMESTAMP")
	prehash := ts + got.method + got.path + string(got.body)
	mac := hmac.New(sha256.New, []byte("test-secret"))
	mac.Write([]byte(prehash))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if got.header.Get("OK-ACCESS-SIGN") != want {
		t.Errorf("签名不匹配：\n want %q\n got  %q\n prehash=%q", want, got.header.Get("OK-ACCESS-SIGN"), prehash)
	}

	// body 字段。
	var body map[string]string
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatalf("下单 body 不是合法 JSON: %v body=%s", err, got.body)
	}
	checks := map[string]string{
		"clOrdId": "coid-001",
		"instId":  "BTC-USDT", // BTC/USDT 必须转成 BTC-USDT
		"tdMode":  "cash",
		"side":    "buy",
		"ordType": "limit",
		"sz":      "0.01",
		"px":      "30000",
	}
	for k, want := range checks {
		if body[k] != want {
			t.Errorf("body[%q] 应为 %q，实际 %q", k, want, body[k])
		}
	}
}

// TestOKXSubmitIsIdempotent 同一 clientOrderID 两次 → 只发一次 HTTP。
func TestOKXSubmitIsIdempotent(t *testing.T) {
	rec := &okxRecorder{}
	srv := newFakeOKX(t, rec)
	ch := newTestOKX(t, srv.URL, false)

	req := testSubmitReq("coid-idem")
	r1, err := ch.Submit(req)
	if err != nil {
		t.Fatalf("第一次 Submit: %v", err)
	}
	r2, err := ch.Submit(req)
	if err != nil {
		t.Fatalf("第二次 Submit: %v", err)
	}

	if n := rec.count(); n != 1 {
		t.Fatalf("同一 clientOrderID 提交两次应只发 1 次 HTTP，实际 %d 次", n)
	}
	if r1.ChannelOrderID != r2.ChannelOrderID {
		t.Errorf("两次返回的 ChannelOrderID 应一致：%q vs %q", r1.ChannelOrderID, r2.ChannelOrderID)
	}

	// 不同 clientOrderID 必须真的再发一次（不能把去重做成「只发一次」）。
	if _, err := ch.Submit(testSubmitReq("coid-other")); err != nil {
		t.Fatalf("不同 clientOrderID 的 Submit: %v", err)
	}
	if n := rec.count(); n != 2 {
		t.Errorf("不同 clientOrderID 应各发一次，累计应 2 次，实际 %d", n)
	}
}

// TestOKXConcurrentSubmitOnlyOnce 并发同一 clientOrderID → 仍然只发一次。
//
// # 为什么单独测
//
// 「先查 map 再写 map」的朴素实现会在并发下放行多次。
// 这条用 10 个 goroutine 同时提交同一个 ID 来钉住这一点。
func TestOKXConcurrentSubmitOnlyOnce(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[{"ordId":"ORD-C","sCode":"0"}]}`))
	}))
	t.Cleanup(srv.Close)

	ch := newTestOKX(t, srv.URL, false)
	req := testSubmitReq("coid-race")

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ch.Submit(req); err != nil {
				t.Errorf("并发 Submit: %v", err)
			}
		}()
	}
	wg.Wait()

	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("并发提交同一 clientOrderID 应只发 1 次，实际 %d 次", n)
	}
}

// TestOKXHealthHitsPublicTime Health 打到 /api/v5/public/time。
func TestOKXHealthHitsPublicTime(t *testing.T) {
	rec := &okxRecorder{}
	srv := newFakeOKX(t, rec)
	ch := newTestOKX(t, srv.URL, false)

	if err := ch.Health(); err != nil {
		t.Fatalf("Health: %v", err)
	}
	got := rec.last(t)
	if got.path != "/api/v5/public/time" {
		t.Errorf("Health 应打 /api/v5/public/time，实际 %s", got.path)
	}
	// 健康检查是公开接口：不该带签名 header（带了说明实现串了）。
	if got.header.Get("OK-ACCESS-SIGN") != "" {
		t.Error("/api/v5/public/time 是公开接口，不应带 OK-ACCESS-SIGN")
	}
}

// TestOKXDemoHeader demo 模式必须带 x-simulated-trading: 1。
func TestOKXDemoHeader(t *testing.T) {
	rec := &okxRecorder{}
	srv := newFakeOKX(t, rec)
	ch := newTestOKX(t, srv.URL, true)

	if _, err := ch.Submit(testSubmitReq("coid-demo")); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if v := rec.last(t).header.Get("x-simulated-trading"); v != "1" {
		t.Errorf("demo 模式应带 x-simulated-trading: 1，实际 %q", v)
	}

	// 非 demo 不能带 —— 带着它打实盘会静默走模拟，从而「下单成功但没成交」。
	rec2 := &okxRecorder{}
	srv2 := newFakeOKX(t, rec2)
	ch2 := newTestOKX(t, srv2.URL, false)
	if _, err := ch2.Submit(testSubmitReq("coid-live")); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if v := rec2.last(t).header.Get("x-simulated-trading"); v != "" {
		t.Errorf("非 demo 不应带 x-simulated-trading，实际 %q", v)
	}
}

// TestOKXNoCredentialLeakInError 错误信息里不能出现 secret 值。
func TestOKXNoCredentialLeakInError(t *testing.T) {
	const secret = "SUPER-SECRET-DO-NOT-LEAK"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 模拟 OKX 的业务拒绝（HTTP 200 + code!=0）。
		_, _ = w.Write([]byte(`{"code":"51008","msg":"Insufficient balance","data":[]}`))
	}))
	t.Cleanup(srv.Close)

	ch, err := NewOKXChannel(OKXConfig{
		APIKey: "k", SecretKey: secret, Passphrase: "p", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("NewOKXChannel: %v", err)
	}
	_, err = ch.Submit(testSubmitReq("coid-err"))
	if err == nil {
		t.Fatal("业务拒绝应返回 error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("错误信息泄漏了 secret：%v", err)
	}
	if !strings.Contains(err.Error(), "51008") {
		t.Errorf("错误信息应带上 OKX 的业务码，实际：%v", err)
	}
}

// TestOKXRejectsBadSymbol 非法 symbol 直接报错，不发请求。
func TestOKXRejectsBadSymbol(t *testing.T) {
	rec := &okxRecorder{}
	srv := newFakeOKX(t, rec)
	ch := newTestOKX(t, srv.URL, false)

	req := testSubmitReq("coid-badsym")
	req.Symbol = "BTCUSDT"
	if _, err := ch.Submit(req); err == nil {
		t.Fatal("BTCUSDT（无分隔符）应被拒绝")
	}
	if n := rec.count(); n != 0 {
		t.Errorf("参数非法时不应发请求，实际发了 %d 次", n)
	}
}

// TestOKXLimitOrderRequiresPrice 限价单缺 price 直接报错。
func TestOKXLimitOrderRequiresPrice(t *testing.T) {
	rec := &okxRecorder{}
	srv := newFakeOKX(t, rec)
	ch := newTestOKX(t, srv.URL, false)

	req := testSubmitReq("coid-nopx")
	req.Price = ""
	if _, err := ch.Submit(req); err == nil {
		t.Fatal("限价单缺 price 应被拒绝")
	}
	if n := rec.count(); n != 0 {
		t.Errorf("参数非法时不应发请求，实际发了 %d 次", n)
	}
}

// TestNewOKXChannelRequiresCredentials 缺凭证构造即失败（不 panic）。
func TestNewOKXChannelRequiresCredentials(t *testing.T) {
	cases := map[string]OKXConfig{
		"缺 api_key":    {SecretKey: "s", Passphrase: "p"},
		"缺 secret":     {APIKey: "k", Passphrase: "p"},
		"缺 passphrase": {APIKey: "k", SecretKey: "s"},
		"全缺":           {},
	}
	for name, cfg := range cases {
		if _, err := NewOKXChannel(cfg); err == nil {
			t.Errorf("%s 应返回 error", name)
		}
	}
}
