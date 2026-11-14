package okxrest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 真实的 OKX /trade/fills 响应片段（**没有 fillId**，唯一标识是 tradeId）。
//
// 这个测试的存在理由：2026-11-14 部署实测时，代码按文档想当然解 fillId，
// 拿到空串 → 落库被唯一键拒绝 → 每轮同步静默丢全部成交。
// 只有拿真实响应形状喂一遍才能挡住这类错。
func TestListFillsParsesTradeId(t *testing.T) {
	const body = `{"code":"0","msg":"","data":[
	{"instType":"SWAP","instId":"BTC-USDT-SWAP","tradeId":"123456","ordId":"312269865356374016",
	 "clOrdId":"artex1a2b3c4d","billId":"1111","tag":"","fillPx":"62961.4","fillSz":"1",
	 "side":"buy","posSide":"net","execType":"M","feeCcy":"USDT","fee":"-0.05","ts":"1724740225685"},
	{"instType":"SWAP","instId":"BTC-USDT-SWAP","tradeId":"123457","ordId":"312269865356374016",
	 "clOrdId":"artex1a2b3c4d","billId":"1112","tag":"","fillPx":"62970.0","fillSz":"0.5",
	 "side":"sell","posSide":"net","execType":"T","feeCcy":"USDT","fee":"-0.03","ts":"1724740230000"}]}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	c := NewClient(Credentials{APIKey: "k", SecretKey: "s", Passphrase: "p", BaseURL: srv.URL})

	fills, err := c.ListFillsByInst(context.Background(), "BTC-USDT-SWAP", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(fills) != 2 {
		t.Fatalf("应解出 2 笔，得到 %d", len(fills))
	}
	if fills[0].TradeID != "123456" {
		t.Fatalf("tradeId 应解出 123456，得到 %q（若为空说明 json tag 写错了）", fills[0].TradeID)
	}
	if fills[0].BillID != "1111" {
		t.Fatalf("billId 应解出 1111，得到 %q", fills[0].BillID)
	}
	if fills[0].FillPx != "62961.4" || fills[0].FillSz != "1" {
		t.Fatalf("价格/数量解析错误: %+v", fills[0])
	}
	if fills[0].Fee != "-0.05" {
		t.Fatalf("fee 应保留负号，得到 %q", fills[0].Fee)
	}
	if fills[0].ExecType != "M" || fills[1].ExecType != "T" {
		t.Fatalf("execType 解析错误: %+v", fills)
	}
	if fills[1].Side != "sell" {
		t.Fatalf("side 解析错误: %q", fills[1].Side)
	}
}

// 请求必须带 instType/instId/begin/end/limit，且 limit=100（接口上限）。
func TestListFillsQueryShape(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"code":"0","data":[]}`)
	}))
	defer srv.Close()
	c := NewClient(Credentials{APIKey: "k", SecretKey: "s", Passphrase: "p", BaseURL: srv.URL})

	if _, err := c.ListFillsByInst(context.Background(), "AAPL-USDT-SWAP", 3); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"instType=SWAP", "instId=AAPL-USDT-SWAP", "begin=", "end=", "limit=100"} {
		if !strings.Contains(got, want) {
			t.Errorf("query 缺 %q: %s", want, got)
		}
	}
}
