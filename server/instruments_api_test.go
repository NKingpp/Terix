package server

// POST /api/instruments/sync 的测试。
//
// 核心断言是「asset_class 不可变」——它是阶段图模板的选择依据，
// 静默更新会让已建任务的图与标的分类不一致。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
)

// newInstrumentsTestServer 建一个可用 sync 端点的测试服务。
func newInstrumentsTestServer(t *testing.T) (*Server, func(method, path string, body any) (int, map[string]any)) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	td := t.TempDir()
	s := New(context.Background(), m, td, td, td)
	h := s.Handler()
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	do := func(method, path string, body any) (int, map[string]any) {
		var r *http.Request
		if body != nil {
			b, _ := json.Marshal(body)
			r = httptest.NewRequest(method, path, bytes.NewReader(b))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		r.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	return s, do
}

// uniqueSymbol 生成测试专用 symbol，避免与真实数据/其它测试冲突。
func uniqueSymbol(prefix string) string {
	return fmt.Sprintf("%s-T%d", prefix, time.Now().UnixNano()%1e12)
}

// cleanupInstrument 删除测试建的标的。
func cleanupInstrument(t *testing.T, s *Server, market, symbol string) {
	t.Helper()
	if _, err := s.m.pg.Exec(`DELETE FROM instruments WHERE market=$1 AND symbol=$2`, market, symbol); err != nil {
		t.Logf("清理 instrument %s/%s 失败: %v", market, symbol, err)
	}
}

// TestSyncInstrumentCreatesNew 新标的进 created。
func TestSyncInstrumentCreatesNew(t *testing.T) {
	s, do := newInstrumentsTestServer(t)
	sym := uniqueSymbol("TESTSYNC")
	t.Cleanup(func() { cleanupInstrument(t, s, "us", sym) })

	code, out := do("POST", "/api/instruments/sync", map[string]any{
		"market":      "us",
		"skip_verify": true, // 测试不走网络
		"items": []map[string]any{
			{"symbol": sym, "asset_class": "stock", "name": "测试标的"},
		},
	})
	if code != 200 {
		t.Fatalf("status=%d body=%v", code, out)
	}
	created, _ := out["created"].([]any)
	if len(created) != 1 {
		t.Fatalf("created 应 1 条，实际 %v (out=%v)", created, out)
	}
	item := created[0].(map[string]any)
	if item["symbol"] != sym {
		t.Errorf("symbol = %v, want %s", item["symbol"], sym)
	}
	if item["asset_class"] != "stock" {
		t.Errorf("asset_class = %v", item["asset_class"])
	}
	if updated, _ := out["updated"].([]any); len(updated) != 0 {
		t.Errorf("updated 应为空，实际 %v", updated)
	}
	if out["verified"] != false {
		t.Errorf("skip_verify=true 时 verified 应为 false，实际 %v", out["verified"])
	}
}

// TestSyncInstrumentUpdatesExisting 已存在标的改 name → 进 updated。
func TestSyncInstrumentUpdatesExisting(t *testing.T) {
	s, do := newInstrumentsTestServer(t)
	sym := uniqueSymbol("TESTUPD")
	t.Cleanup(func() { cleanupInstrument(t, s, "us", sym) })

	// 第一次：建
	code, _ := do("POST", "/api/instruments/sync", map[string]any{
		"market": "us", "skip_verify": true,
		"items": []map[string]any{{"symbol": sym, "asset_class": "stock", "name": "旧名字"}},
	})
	if code != 200 {
		t.Fatalf("首次 sync status=%d", code)
	}

	// 第二次：改 name，asset_class 不变
	code, out := do("POST", "/api/instruments/sync", map[string]any{
		"market": "us", "skip_verify": true,
		"items": []map[string]any{{"symbol": sym, "asset_class": "stock", "name": "新名字"}},
	})
	if code != 200 {
		t.Fatalf("二次 sync status=%d body=%v", code, out)
	}
	if created, _ := out["created"].([]any); len(created) != 0 {
		t.Errorf("二次不应有 created，实际 %v", created)
	}
	updated, _ := out["updated"].([]any)
	if len(updated) != 1 {
		t.Fatalf("updated 应 1 条，实际 %v (out=%v)", updated, out)
	}

	// 验证 name 真的更新了
	inst, err := s.m.pg.Instruments().GetInstrumentByMarketSymbol("us", sym)
	if err != nil {
		t.Fatalf("查标的: %v", err)
	}
	if inst.Name == nil || *inst.Name != "新名字" {
		t.Errorf("name 未更新为「新名字」: %v", inst.Name)
	}
}

// TestSyncInstrumentRejectsAssetClassChange **最重要的一条**：
// 已存在标的改 asset_class 必须报冲突，不能静默更新。
//
// 理由：asset_class 决定阶段图模板（stock 17 节点含 fundamental / crypto 13 节点）。
// 静默改会让已建任务的图与分类不一致，而那种不一致没有任何代码能检测出来。
func TestSyncInstrumentRejectsAssetClassChange(t *testing.T) {
	s, do := newInstrumentsTestServer(t)
	sym := uniqueSymbol("TESTCLS")
	t.Cleanup(func() { cleanupInstrument(t, s, "us", sym) })

	// 先以 stock 入库
	code, _ := do("POST", "/api/instruments/sync", map[string]any{
		"market": "us", "skip_verify": true,
		"items": []map[string]any{{"symbol": sym, "asset_class": "stock"}},
	})
	if code != 200 {
		t.Fatalf("首次 status=%d", code)
	}

	// 再以 etf 提交 → 应进 failed，error_code=ASSET_CLASS_CONFLICT
	code, out := do("POST", "/api/instruments/sync", map[string]any{
		"market": "us", "skip_verify": true,
		"items": []map[string]any{{"symbol": sym, "asset_class": "etf"}},
	})
	if code != 200 {
		t.Fatalf("冲突也是逐项结果，HTTP 应 200，实际 %d", code)
	}
	if created, _ := out["created"].([]any); len(created) != 0 {
		t.Errorf("冲突不应产生 created，实际 %v", created)
	}
	if updated, _ := out["updated"].([]any); len(updated) != 0 {
		t.Errorf("冲突不应产生 updated，实际 %v", updated)
	}
	failed, _ := out["failed"].([]any)
	if len(failed) != 1 {
		t.Fatalf("failed 应 1 条，实际 %v (out=%v)", failed, out)
	}
	f := failed[0].(map[string]any)
	if f["error_code"] != "ASSET_CLASS_CONFLICT" {
		t.Errorf("error_code = %v, want ASSET_CLASS_CONFLICT", f["error_code"])
	}

	// 原记录仍是 stock
	inst, err := s.m.pg.Instruments().GetInstrumentByMarketSymbol("us", sym)
	if err != nil {
		t.Fatalf("查标的: %v", err)
	}
	if inst.AssetClass != "stock" {
		t.Errorf("asset_class 被静默改成 %q，必须保持 stock", inst.AssetClass)
	}
}

// TestSyncInstrumentMissingAssetClass asset_class 缺失 → INVALID_INPUT。
func TestSyncInstrumentMissingAssetClass(t *testing.T) {
	_, do := newInstrumentsTestServer(t)
	code, out := do("POST", "/api/instruments/sync", map[string]any{
		"market": "us", "skip_verify": true,
		"items": []map[string]any{{"symbol": "AAPL"}}, // 缺 asset_class
	})
	if code != 200 {
		t.Fatalf("逐项失败应 HTTP 200，实际 %d", code)
	}
	failed, _ := out["failed"].([]any)
	if len(failed) != 1 {
		t.Fatalf("failed 应 1 条: %v", out)
	}
	f := failed[0].(map[string]any)
	if f["error_code"] != "INVALID_INPUT" {
		t.Errorf("error_code = %v, want INVALID_INPUT", f["error_code"])
	}
	// 错误信息要说明「为什么必填」，否则用户会以为是可选字段
	msg, _ := f["error"].(string)
	if msg == "" {
		t.Error("错误信息不能为空")
	}
}

// TestSyncInstrumentInvalidMarket market 非法 → 400（请求本身不合法）。
func TestSyncInstrumentInvalidMarket(t *testing.T) {
	_, do := newInstrumentsTestServer(t)
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"缺 market", map[string]any{"skip_verify": true,
			"items": []map[string]any{{"symbol": "X", "asset_class": "stock"}}}},
		{"market 空串", map[string]any{"market": "  ", "skip_verify": true,
			"items": []map[string]any{{"symbol": "X", "asset_class": "stock"}}}},
		{"items 空", map[string]any{"market": "us", "items": []map[string]any{}}},
		{"items 缺失", map[string]any{"market": "us"}},
	} {
		code, _ := do("POST", "/api/instruments/sync", tc.body)
		if code != 400 {
			t.Errorf("%s: status = %d, want 400", tc.name, code)
		}
	}
}

// TestSyncInstrumentTooMany items 超限 → 400。
func TestSyncInstrumentTooMany(t *testing.T) {
	_, do := newInstrumentsTestServer(t)
	items := make([]map[string]any, maxSyncInstruments+1)
	for i := range items {
		items[i] = map[string]any{"symbol": fmt.Sprintf("S%d", i), "asset_class": "stock"}
	}
	code, out := do("POST", "/api/instruments/sync", map[string]any{
		"market": "us", "skip_verify": true, "items": items,
	})
	if code != 400 {
		t.Errorf("超限应 400，实际 %d (out=%v)", code, out)
	}
}

// TestSyncInstrumentPartialSuccess 批量部分成功：HTTP 200，成功进 created，失败进 failed。
func TestSyncInstrumentPartialSuccess(t *testing.T) {
	s, do := newInstrumentsTestServer(t)
	good := uniqueSymbol("TESTPART")
	t.Cleanup(func() { cleanupInstrument(t, s, "us", good) })

	code, out := do("POST", "/api/instruments/sync", map[string]any{
		"market": "us", "skip_verify": true,
		"items": []map[string]any{
			{"symbol": good, "asset_class": "stock"},
			{"symbol": "", "asset_class": "stock"},                     // 空 symbol
			{"symbol": uniqueSymbol("TESTBAD"), "asset_class": "nope"}, // 非法 asset_class
		},
	})
	if code != 200 {
		t.Fatalf("部分失败应 HTTP 200，实际 %d", code)
	}
	created, _ := out["created"].([]any)
	failed, _ := out["failed"].([]any)
	if len(created) != 1 {
		t.Errorf("created 应 1 条，实际 %v", created)
	}
	if len(failed) != 2 {
		t.Errorf("failed 应 2 条，实际 %v (out=%v)", failed, out)
	}
}

// TestSyncInstrumentEmptySymbol 空 symbol 单独一条 failed。
func TestSyncInstrumentEmptySymbol(t *testing.T) {
	_, do := newInstrumentsTestServer(t)
	code, out := do("POST", "/api/instruments/sync", map[string]any{
		"market": "us", "skip_verify": true,
		"items": []map[string]any{{"symbol": "   ", "asset_class": "stock"}},
	})
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	failed, _ := out["failed"].([]any)
	if len(failed) != 1 {
		t.Fatalf("failed 应 1 条: %v", out)
	}
	if failed[0].(map[string]any)["error_code"] != "INVALID_INPUT" {
		t.Errorf("error_code = %v", failed[0].(map[string]any)["error_code"])
	}
}

// TestSyncInstrumentVerifyFailsGracefully 验证失败时错误码必须区分「查不到」与「源故障」。
//
// 这条保证用户不会被误导去改一个本来正确的 symbol。
func TestSyncInstrumentVerifyFailsGracefully(t *testing.T) {
	s, do := newInstrumentsTestServer(t)
	// 用一个几乎不可能存在的 symbol，走真实验证路径。
	sym := "ZZZZ-NOT-EXIST-99999"
	code, out := do("POST", "/api/instruments/sync", map[string]any{
		"market": "us",
		// skip_verify 不设 → 走验证
		"items": []map[string]any{{"symbol": sym, "asset_class": "stock"}},
	})
	if code != 200 {
		t.Fatalf("逐项失败仍应 HTTP 200，实际 %d (out=%v)", code, out)
	}
	failed, _ := out["failed"].([]any)
	if len(failed) != 1 {
		t.Fatalf("不存在的 symbol 应进 failed: %v", out)
	}
	f := failed[0].(map[string]any)
	ec, _ := f["error_code"].(string)
	// 允许三种：符号确实不存在、源不可用、源故障。
	// **不允许**空或 INTERNAL —— 那说明分类逻辑没生效。
	switch ec {
	case "INSTRUMENT_NOT_FOUND", "UPSTREAM_BLOCKED", "UPSTREAM_UNAVAILABLE",
		"UPSTREAM_TIMEOUT", "UPSTREAM_ERROR":
		// ok
	case "":
		t.Error("error_code 不能为空（必须区分查不到与源故障）")
	default:
		t.Errorf("error_code = %q 不在预期集合内", ec)
	}
	// 源类错误的提示里应给出 skip_verify 出路
	msg, _ := f["error"].(string)
	if ec != "INSTRUMENT_NOT_FOUND" && !containsStr(msg, "skip_verify") {
		t.Errorf("源类错误应提示 skip_verify 出路，实际: %s", msg)
	}
	// 清理（如果意外入库了）
	t.Cleanup(func() { cleanupInstrument(t, s, "us", sym) })
}

func containsStr(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestSyncInstrumentRealStock 集成测试：真实 sync 一只 A 股，验证端到端可用。
//
// 这是唯一确认「用户能真的选一只股票」的测试 —— 前面的测试都用 skip_verify，
// 只验证了入库逻辑，没验证行情通道能识别真实 symbol。
func TestSyncInstrumentRealStock(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过真实行情验证")
	}
	s, do := newInstrumentsTestServer(t)
	const sym = "600519"
	t.Cleanup(func() { cleanupInstrument(t, s, "cn", sym) })

	// 先清掉可能存在的同名记录，让本次走「新建」路径
	cleanupInstrument(t, s, "cn", sym)

	code, out := do("POST", "/api/instruments/sync", map[string]any{
		"market": "cn",
		// 不设 skip_verify → 走 akshare 真实验证
		"items": []map[string]any{{"symbol": sym, "asset_class": "stock", "name": "贵州茅台"}},
	})
	if code != 200 {
		t.Fatalf("status=%d out=%v", code, out)
	}
	created, _ := out["created"].([]any)
	failed, _ := out["failed"].([]any)
	if len(created) == 1 {
		t.Logf("真实 sync 成功: %v", created[0])
		inst, err := s.m.pg.Instruments().GetInstrumentByMarketSymbol("cn", sym)
		if err != nil {
			t.Fatalf("查标的: %v", err)
		}
		if inst.AssetClass != "stock" {
			t.Errorf("asset_class = %q, want stock", inst.AssetClass)
		}
		return
	}
	// 通道不可用时不算失败（CI 无 sidecar），但必须说明原因分类正确。
	if len(failed) == 1 {
		f := failed[0].(map[string]any)
		t.Logf("行情通道不可用，同步失败（预期内的降级）: code=%v msg=%v",
			f["error_code"], f["error"])
		ec, _ := f["error_code"].(string)
		if ec == "INSTRUMENT_NOT_FOUND" {
			t.Errorf("600519 是真实存在的股票，不该报 INSTRUMENT_NOT_FOUND")
		}
		return
	}
	t.Fatalf("既无 created 也无 failed: %v", out)
}

// fakeQuoteClient 是可控的假行情客户端，用于覆盖「验证成功」路径。
type fakeQuoteClient struct {
	quote *agent.Quote
	err   error
}

func (f *fakeQuoteClient) Health(context.Context) (bool, string, error) { return true, "fake", nil }
func (f *fakeQuoteClient) Timeout() time.Duration                       { return 5 * time.Second }
func (f *fakeQuoteClient) FetchQuote(_ context.Context, symbol, market string, ch agent.Channel) (*agent.Quote, error) {
	if f.err != nil {
		return nil, f.err
	}
	q := *f.quote
	q.Symbol = symbol
	q.Market = market
	q.Channel = ch
	return &q, nil
}
func (f *fakeQuoteClient) FetchKline(context.Context, string, string, string, int, agent.Channel) ([]agent.Bar, error) {
	return nil, nil
}
func (f *fakeQuoteClient) FetchMacro(context.Context, string) (*agent.Macro, error) { return nil, nil }

// TestSyncInstrumentVerifySuccessWithInjectedClient 注入假 client 后，
// 走完「验证成功 → 入库」的完整路径（不依赖 sidecar）。
//
// 这是装配 SetMarketClients 的直接收益：没有它，这条路径只能靠
// sidecar 在跑来覆盖，CI 里永远测不到。
func TestSyncInstrumentVerifySuccessWithInjectedClient(t *testing.T) {
	s, do := newInstrumentsTestServer(t)
	s.SetMarketClients(
		&fakeQuoteClient{quote: &agent.Quote{Last: "1349.98", TradingDate: "2026-10-26"}},
		&fakeQuoteClient{quote: &agent.Quote{Last: "80412.1", TradingDate: "2026-10-26"}},
		nil,
	)
	sym := uniqueSymbol("TESTVERIFYOK")
	t.Cleanup(func() { cleanupInstrument(t, s, "us", sym) })

	code, out := do("POST", "/api/instruments/sync", map[string]any{
		"market": "us",
		// 不设 skip_verify → 走验证（此时由假 client 响应）
		"items": []map[string]any{{"symbol": sym, "asset_class": "stock"}},
	})
	if code != 200 {
		t.Fatalf("status=%d out=%v", code, out)
	}
	created, _ := out["created"].([]any)
	if len(created) != 1 {
		t.Fatalf("验证成功应入库，实际 out=%v", out)
	}
	if out["verified"] != true {
		t.Errorf("verified 应为 true，实际 %v", out["verified"])
	}
}

// TestSyncInstrumentVerifyNotFoundWithInjectedClient 假 client 返回「查不到」时
// 必须报 INSTRUMENT_NOT_FOUND（而不是源故障）。
func TestSyncInstrumentVerifyNotFoundWithInjectedClient(t *testing.T) {
	s, do := newInstrumentsTestServer(t)
	s.SetMarketClients(
		&fakeQuoteClient{err: &agent.MarketError{Code: "NO_DATA", Message: "无数据"}},
		nil, nil,
	)
	sym := uniqueSymbol("TESTVERIFYMISS")
	t.Cleanup(func() { cleanupInstrument(t, s, "us", sym) })

	code, out := do("POST", "/api/instruments/sync", map[string]any{
		"market": "us",
		"items":  []map[string]any{{"symbol": sym, "asset_class": "stock"}},
	})
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	failed, _ := out["failed"].([]any)
	if len(failed) != 1 {
		t.Fatalf("应进 failed: %v", out)
	}
	f := failed[0].(map[string]any)
	if f["error_code"] != "INSTRUMENT_NOT_FOUND" {
		t.Errorf("error_code = %v, want INSTRUMENT_NOT_FOUND", f["error_code"])
	}
	// 不能入库
	if _, err := s.m.pg.Instruments().GetInstrumentByMarketSymbol("us", sym); err == nil {
		t.Error("验证失败不该入库")
	}
}
