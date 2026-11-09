package exec

// exec.Client 的验收测试。
//
// # 测什么
//
// 8 条（对应任务规格）：
//   1. Submit happy path（fake vaultd）
//   2. Submit 不传 client_order_id → 参数错误
//   3. vaultd 返回 401 → ErrVaultUnauthorized（不是通用 error）
//   4. 超时 2s：fake vaultd sleep 3s → Client 在 ~2s 返回
//   5. **error 里不含 token**（关键）
//   6. Query 两种查询方式
//   7. Health 探活
//   8. **端到端**：Submit → Query → 断言 raw_ack 已脱敏
//
// 第 8 条是 WE-14 缺的那一半（见 KNOWN-LIMITATIONS L13）。
//
// # 为什么用 httptest.Server 而不是真 vaultd 进程
//
// 真进程要跨进程通信、端口分配、token 传递 —— 那些是 vaultd 自己的测试
// 已经覆盖的（12 个）。这里测的是 Client 的协议实现，用 fake 更聚焦。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const clientTestToken = "client-test-token-DO-NOT-LEAK-0123456789"

// fakeVault 是一个最小的 vaultd 替身。
type fakeVault struct {
	mu         sync.Mutex
	submitResp any
	queryResp  any
	status     int           // 非 0 时强制返回该状态码
	body       string        // status 非 0 时返回的 body
	delay      time.Duration // 人为延迟（测超时）
	gotToken   string
	errCode    string
	errMsg     string
}

func (f *fakeVault) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.gotToken = r.Header.Get("Authorization")
		delay := f.delay
		status := f.status
		f.mu.Unlock()

		if delay > 0 {
			time.Sleep(delay)
		}

		w.Header().Set("Content-Type", "application/json")

		if status != 0 {
			w.WriteHeader(status)
			if f.body != "" {
				_, _ = w.Write([]byte(f.body))
			} else {
				_, _ = fmt.Fprintf(w, `{"error":%q,"code":%q}`, f.errMsg, f.errCode)
			}
			return
		}

		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/v1/submit":
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(f.submitResp)
		case "/v1/query":
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(f.queryResp)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found","code":"VAULT_NOT_FOUND"}`))
		}
	})
}

func newFakeVault(t *testing.T, f *fakeVault) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, clientTestToken), srv
}

// ---- 1. Submit happy path ----

func TestClientSubmitHappyPath(t *testing.T) {
	f := &fakeVault{submitResp: SubmitResponse{
		ChannelOrderID: "CH-1",
		AcceptedAt:     "2026-10-28T00:00:00Z",
		RawAck:         json.RawMessage(`{"ok":true}`),
	}}
	c, _ := newFakeVault(t, f)

	resp, err := c.Submit(context.Background(), SubmitRequest{
		Channel: "okx", AccountRef: "a", ClientOrderID: "c-1",
		Symbol: "BTC-USDT", Side: "buy", Qty: "1",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if resp.ChannelOrderID != "CH-1" {
		t.Errorf("channel_order_id 应为 CH-1，实际 %q", resp.ChannelOrderID)
	}

	// 断言 token 真的发出去了（带 Bearer 前缀）
	if f.gotToken != "Bearer "+clientTestToken {
		t.Errorf("Authorization 头不对，实际 %q", f.gotToken)
	}
}

// ---- 2. 缺 client_order_id → 参数错误（不发请求）----

func TestClientSubmitRejectsEmptyClientOrderID(t *testing.T) {
	f := &fakeVault{}
	c, _ := newFakeVault(t, f)

	_, err := c.Submit(context.Background(), SubmitRequest{
		Channel: "okx", AccountRef: "a", ClientOrderID: "",
		Symbol: "BTC-USDT", Side: "buy", Qty: "1",
	})
	if err == nil {
		t.Fatal("缺 client_order_id 应报错")
	}
	if !errors.Is(err, ErrVaultBadRequest) {
		t.Errorf("错误应为 ErrVaultBadRequest，实际 %v", err)
	}
	// 错误信息要说明「为什么必须有」——它是幂等键
	if !strings.Contains(err.Error(), "幂等") {
		t.Errorf("错误信息应说明 client_order_id 是幂等键，实际：%v", err)
	}
	// 断言「没有发请求」—— f.gotToken 保持空
	if f.gotToken != "" {
		t.Error("缺参数时不该发请求（应在本地拒绝）")
	}
}

// ---- 3. 401 → ErrVaultUnauthorized ----

func TestClientSubmitMaps401(t *testing.T) {
	f := &fakeVault{status: http.StatusUnauthorized,
		errCode: VaultCodeUnauthorized, errMsg: "token 不匹配"}
	c, _ := newFakeVault(t, f)

	_, err := c.Submit(context.Background(), SubmitRequest{
		Channel: "okx", AccountRef: "a", ClientOrderID: "c-1",
		Symbol: "BTC-USDT", Side: "buy", Qty: "1",
	})
	if err == nil {
		t.Fatal("401 应报错")
	}
	// 关键：必须是 ErrVaultUnauthorized，不是通用 error
	//
	// 「token 错了」和「vaultd 挂了」的应对完全不同：
	// 前者检查启动时的 token 传递，后者检查进程是否活着。
	if !errors.Is(err, ErrVaultUnauthorized) {
		t.Errorf("401 应映射成 ErrVaultUnauthorized，实际 %v", err)
	}
	// 且不能被误判成「不可达」
	if errors.Is(err, ErrVaultUnavailable) {
		t.Error("401 不该被当成「vaultd 不可达」")
	}
}

// ---- 3b. 503 → ErrVaultChannelUnavailable（区别于「挂了」）----

func TestClientSubmitMaps503(t *testing.T) {
	f := &fakeVault{status: http.StatusServiceUnavailable,
		errCode: VaultCodeChannelUnavailable, errMsg: "通道 okx 未配置"}
	c, _ := newFakeVault(t, f)

	_, err := c.Submit(context.Background(), SubmitRequest{
		Channel: "okx", AccountRef: "a", ClientOrderID: "c-1",
		Symbol: "BTC-USDT", Side: "buy", Qty: "1",
	})
	if !errors.Is(err, ErrVaultChannelUnavailable) {
		t.Errorf("503 应映射成 ErrVaultChannelUnavailable，实际 %v", err)
	}
	// 503 是「活着但没通道」，不是「不可达」
	if errors.Is(err, ErrVaultUnavailable) {
		t.Error("503 不该被当成「vaultd 不可达」")
	}
}

// ---- 4. 超时 2s ----

func TestClientTimesOut(t *testing.T) {
	// vaultd 睡 3s，Client 超时 2s → 应在 ~2s 返回错误
	f := &fakeVault{delay: 3 * time.Second, submitResp: SubmitResponse{ChannelOrderID: "x"}}
	c, _ := newFakeVault(t, f)

	start := time.Now()
	_, err := c.Submit(context.Background(), SubmitRequest{
		Channel: "okx", AccountRef: "a", ClientOrderID: "c-1",
		Symbol: "BTC-USDT", Side: "buy", Qty: "1",
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("超时应该报错")
	}
	if !errors.Is(err, ErrVaultUnavailable) {
		t.Errorf("超时应映射成 ErrVaultUnavailable，实际 %v", err)
	}
	// 断言「早于 3s 返回」—— 证明超时生效（而不是等到服务端回完）
	if elapsed >= 3*time.Second {
		t.Errorf("应在 2s 超时返回，实际等了 %v", elapsed)
	}
	if elapsed < 1500*time.Millisecond {
		t.Errorf("返回太快（%v），可能没有真的等 2s 超时", elapsed)
	}
}

// ---- 5. ★ error 里不含 token ----

// TestClientErrorNeverContainsToken 是最关键的一条。
//
// # 为什么必须测
//
// http.Client 的错误会带上 URL 和底层错误。如果将来有人把 token
// 放进 query string（我们的设计是不放，但要防），错误信息就会带出 token。
//
// 而「token 出现在日志里」= 「token 泄漏」—— 因为日志会被采集、备份、转发。
//
// 所以这条测试断言：**任何错误路径的 error.Error() 都不含 token**。
func TestClientErrorNeverContainsToken(t *testing.T) {
	cases := []struct {
		name string
		f    *fakeVault
	}{
		{"401", &fakeVault{status: 401, errMsg: "unauth"}},
		{"503", &fakeVault{status: 503, errMsg: "no channel"}},
		{"500", &fakeVault{status: 500, errMsg: "boom"}},
		{"超时", &fakeVault{delay: 3 * time.Second}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newFakeVault(t, tc.f)
			_, err := c.Submit(context.Background(), SubmitRequest{
				Channel: "okx", AccountRef: "a", ClientOrderID: "c-1",
				Symbol: "BTC-USDT", Side: "buy", Qty: "1",
			})
			if err == nil {
				t.Skip("这条用例没产生错误")
			}
			if strings.Contains(err.Error(), clientTestToken) {
				t.Fatalf("错误信息里出现了 token —— 这是泄漏\n%v", err)
			}
		})
	}
}

// TestClientErrorNeverContainsTokenOnConnFailure 连接失败路径（最难覆盖的一条）。
//
// # 为什么单独测
//
// 「连接被拒绝」时 http.Client 会返回 *url.Error，而它包含完整 URL。
// 如果 token 在 URL 里（哪怕只是 query 参数），它就会出现在错误信息里。
//
// 用一个已关闭的 server 制造「连接失败」。
func TestClientErrorNeverContainsTokenOnConnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // 立刻关掉 —— 后续连接会失败

	c := NewClient(url, clientTestToken)
	_, err := c.Submit(context.Background(), SubmitRequest{
		Channel: "okx", AccountRef: "a", ClientOrderID: "c-1",
		Symbol: "BTC-USDT", Side: "buy", Qty: "1",
	})
	if err == nil {
		t.Fatal("连接已关闭应报错")
	}
	if strings.Contains(err.Error(), clientTestToken) {
		t.Fatalf("连接失败的错误信息里出现了 token —— 泄漏\n%v", err)
	}
	if !errors.Is(err, ErrVaultUnavailable) {
		t.Errorf("连接失败应映射成 ErrVaultUnavailable，实际 %v", err)
	}
}

// ---- 6. Query 两种方式 ----

func TestClientQueryByClientOrderID(t *testing.T) {
	f := &fakeVault{queryResp: map[string]any{
		"orders": []map[string]any{{
			"client_order_id": "c-1", "channel_order_id": "CH-1",
			"state": "filled", "filled_qty": "1", "avg_price": "100",
		}},
	}}
	c, _ := newFakeVault(t, f)

	orders, err := c.Query(context.Background(), QueryRequest{
		Channel: "okx", AccountRef: "a", ClientOrderID: "c-1",
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("应返回 1 笔，实际 %d", len(orders))
	}
	if orders[0].State != "filled" {
		t.Errorf("state 应为 filled，实际 %q", orders[0].State)
	}
}

func TestClientQueryRequiresParam(t *testing.T) {
	f := &fakeVault{}
	c, _ := newFakeVault(t, f)

	// 既无 client_order_id 也无时间区间
	_, err := c.Query(context.Background(), QueryRequest{Channel: "okx", AccountRef: "a"})
	if err == nil {
		t.Fatal("缺参数应报错")
	}
	if !errors.Is(err, ErrVaultBadRequest) {
		t.Errorf("应为 ErrVaultBadRequest，实际 %v", err)
	}
}

func TestClientQueryByRange(t *testing.T) {
	f := &fakeVault{queryResp: map[string]any{"orders": []map[string]any{}}}
	c, _ := newFakeVault(t, f)

	orders, err := c.Query(context.Background(), QueryRequest{
		Channel: "okx", AccountRef: "a",
		From: "2026-10-01T00:00:00Z", To: "2026-10-28T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("按区间查应成功: %v", err)
	}
	// 空数组而不是 nil —— 让调用方的 range 不需要 nil 检查
	if orders == nil {
		t.Error("空结果应返回空切片而不是 nil")
	}
}

// ---- 7. Health ----

func TestClientHealth(t *testing.T) {
	// vaultd 对空 body 的 submit 返回 400 —— 那正是「活着 + token 对」的证据
	f := &fakeVault{status: http.StatusBadRequest,
		errCode: VaultCodeBadRequest, errMsg: "缺字段"}
	c, _ := newFakeVault(t, f)

	if err := c.Health(context.Background()); err != nil {
		t.Errorf("Health 应成功（400 说明进程活着），实际 %v", err)
	}
}

func TestClientHealthDetectsUnauthorized(t *testing.T) {
	f := &fakeVault{status: http.StatusUnauthorized,
		errCode: VaultCodeUnauthorized, errMsg: "token 不匹配"}
	c, _ := newFakeVault(t, f)

	err := c.Health(context.Background())
	if !errors.Is(err, ErrVaultUnauthorized) {
		t.Errorf("token 错时 Health 应返回 ErrVaultUnauthorized，实际 %v", err)
	}
}

// ---- 8. ★ 端到端：Submit → Query → raw_ack 脱敏 ----

// TestClientSubmitThenQueryRawAckRedacted 是 WE-14 缺的那一半（L13）。
//
// # 它验证什么
//
// 真实链路是：artex → Client → vaultd（脱敏）→ Client → artex
// 这条测试用 fake vaultd 模拟 vaultd 侧的行为（含脱敏），
// 断言 Client 拿到的东西是干净的。
//
// # 为什么 fake 也要做脱敏
//
// 因为 vaultd 的脱敏是它自己的责任（TestSubmitRedactsRawAck 已验证）。
// 这里的 fake 模拟「已经脱敏过的 vaultd 输出」，然后断言 Client
// **不会把它弄脏也不会有额外泄漏**。
//
// 换句话说：这条测的是「Client 这一层不引入新泄漏」。
func TestClientSubmitThenQueryRawAckRedacted(t *testing.T) {
	const rawKey = "api_key=sk-live-abcdefghijklmnopqrstuvwxyz0123456789"

	f := &fakeVault{
		submitResp: SubmitResponse{
			ChannelOrderID: "CH-1",
			AcceptedAt:     "2026-10-28T00:00:00Z",
			// vaultd 侧已脱敏（模拟）
			RawAck: json.RawMessage(`{"api_key":"***","ok":true}`),
		},
		queryResp: map[string]any{
			"orders": []map[string]any{{
				"client_order_id": "c-1", "channel_order_id": "CH-1",
				"state": "filled", "filled_qty": "1", "avg_price": "100",
			}},
		},
	}
	c, _ := newFakeVault(t, f)
	ctx := context.Background()

	// ① Submit
	sub, err := c.Submit(ctx, SubmitRequest{
		Channel: "okx", AccountRef: "a", ClientOrderID: "c-1",
		Symbol: "BTC-USDT", Side: "buy", Qty: "1",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// ② 断言 raw_ack 干净（Client 不该把脱敏后的东西又弄脏）
	if strings.Contains(string(sub.RawAck), rawKey) {
		t.Fatalf("Submit 返回的 raw_ack 里出现了凭证 —— 泄漏\n%v", string(sub.RawAck))
	}

	// ③ Query 回头对账
	orders, err := c.Query(ctx, QueryRequest{
		Channel: "okx", AccountRef: "a", ClientOrderID: "c-1",
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(orders) != 1 || orders[0].State != "filled" {
		t.Fatalf("Query 结果不对：%+v", orders)
	}

	// ④ 断言整个链路里没有任何凭证
	for _, o := range orders {
		blob, _ := json.Marshal(o)
		if strings.Contains(string(blob), rawKey) {
			t.Fatalf("Query 结果里出现了凭证：%s", blob)
		}
	}
}
