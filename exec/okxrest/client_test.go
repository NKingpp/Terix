package okxrest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(Credentials{
		APIKey: "k", SecretKey: "s", Passphrase: "p", BaseURL: srv.URL,
	})
}

func TestSignHeadersAndBody(t *testing.T) {
	var gotSign, gotTS, gotURI, gotBody string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotSign = r.Header.Get("OK-ACCESS-SIGN")
		gotTS = r.Header.Get("OK-ACCESS-TIMESTAMP")
		gotURI = r.URL.RequestURI()
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[{"ordId":"1","clOrdId":"artexabc","sCode":"0"}]}`))
	})
	req := OrderRequest{
		InstID: "AAPL-USDT-SWAP", TdMode: "cross", Side: "buy",
		PosSide: "net", OrdType: "market", Sz: "1", ClOrdID: "artexabc",
		AttachAlgos: []AlgoAttach{{
			TpTriggerPx: "210.5", TpOrdPx: "-1", TpTriggerPxType: "last",
			SlTriggerPx: "190", SlOrdPx: "-1", SlTriggerPxType: "last",
		}},
	}
	if _, err := c.PlaceOrder(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gotURI, "/api/v5/trade/order") {
		t.Fatalf("requestURI 不对: %q", gotURI)
	}
	want := sign("s", gotTS, "POST", gotURI, gotBody)
	if gotSign != want {
		t.Fatalf("签名不匹配:\n got=%q\nwant=%q", gotSign, want)
	}
	if len(gotTS) != len("2006-01-02T15:04:05.000Z") || gotTS[10] != 'T' {
		t.Fatalf("时间戳格式不对: %q", gotTS)
	}
	var sent OrderRequest
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.TdMode != "cross" || sent.PosSide != "net" {
		t.Fatalf("swap 字段没带上: %+v", sent)
	}
	if len(sent.AttachAlgos) != 1 || sent.AttachAlgos[0].TpTriggerPx != "210.5" {
		t.Fatalf("attachAlgoOrds 没带上: %+v", sent.AttachAlgos)
	}
}

func TestDemoHeaderNotSentByDefault(t *testing.T) {
	var demo string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		demo = r.Header.Get("x-simulated-trading")
		_, _ = w.Write([]byte(`{"code":"0","data":[{"uid":"1","posMode":"net_mode"}]}`))
	})
	if _, err := c.GetAccountConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if demo != "" {
		t.Fatalf("非 demo 不应带 x-simulated-trading，得到 %q", demo)
	}
}

func TestPlaceOrderSCodeIsPermanent(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[{"sCode":"51008","sMsg":"Insufficient balance"}]}`))
	})
	_, err := c.PlaceOrder(context.Background(), OrderRequest{InstID: "X", ClOrdID: "a"})
	if err == nil || !errors.Is(err, ErrPermanent) {
		t.Fatalf("51008 应归为 ErrPermanent，得到 %v", err)
	}
}

func TestDuplicateCodeClassified(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"51603","msg":"Duplicated clOrdId","data":[]}`))
	})
	_, err := c.PlaceOrder(context.Background(), OrderRequest{InstID: "X", ClOrdID: "a"})
	if err == nil || !errors.Is(err, ErrDuplicate) {
		t.Fatalf("51603 应归为 ErrDuplicate，得到 %v", err)
	}
}

func TestPositionsFilterZero(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "instType=SWAP") {
			t.Errorf("query 缺 instType: %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"code":"0","data":[
			{"instId":"AAPL-USDT-SWAP","posSide":"net","pos":"1.5","avgPx":"200","upl":"3.2","lever":"3","mgnMode":"cross"},
			{"instId":"TSLA-USDT-SWAP","posSide":"net","pos":"0","avgPx":"0","upl":"0"}]}`))
	})
	ps, err := c.GetPositions(context.Background(), "SWAP")
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].InstID != "AAPL-USDT-SWAP" || ps[0].Pos != "1.5" {
		t.Fatalf("pos=0 应被过滤，得到 %+v", ps)
	}
}

func TestListAlgoOrdersKeepsOnlyLive(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"0","data":[
			{"algoId":"1","instId":"A","state":"live","tpTriggerPx":"210"},
			{"algoId":"2","instId":"A","state":"canceled"},
			{"algoId":"3","instId":"A","state":"effective"}]}`))
	})
	as, err := c.ListAlgoOrders(context.Background(), "SWAP", "oco")
	if err != nil {
		t.Fatal(err)
	}
	if len(as) != 2 {
		t.Fatalf("应只留 live/effective，得到 %+v", as)
	}
}

func TestQueryOrderNotFoundIsNil(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"51600","msg":"Order does not exist","data":[]}`))
	})
	o, err := c.QueryOrderByClOrdID(context.Background(), "A", "artex1")
	if err != nil || o != nil {
		t.Fatalf("查无此单应返回 (nil,nil)，得到 %v %v", o, err)
	}
}

// 订单级 sCode 必须区分永久失败与可重试。
//
// 顶层 code=1 + sCode=51050（止盈价不合法）若判成可重试，TradeLoop 会
// 每 15 秒重试同一张注定失败的草案，永不停止（2026-11-14 实测）。
func TestClassifyOrderSCode(t *testing.T) {
	if err := classifyOrderSCode("51050"); !errors.Is(err, ErrPermanent) {
		t.Errorf("51050 应判永久失败，得到 %v", err)
	}
	if err := classifyOrderSCode("51603"); !errors.Is(err, ErrDuplicate) {
		t.Errorf("51603 应判重复，得到 %v", err)
	}
	if err := classifyOrderSCode("50011"); !errors.Is(err, ErrRetryable) {
		t.Errorf("50011 限流应判可重试，得到 %v", err)
	}
	if err := classifyOrderSCode("0"); err != nil {
		t.Errorf("0 表示成功，应返回 nil，得到 %v", err)
	}
}
