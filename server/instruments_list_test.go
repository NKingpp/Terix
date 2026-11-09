package server

// GET /api/instruments 的测试。
//
// # 为什么这条测试必要
//
// 「前端能选标的」的前提是「后端能列出标的」。而在加这条路由之前，
// 存储层 ListInstruments 已存在但 HTTP 层没有对应端点 ——
// 那种「层间断裂」不会被任何编译或既有测试发现（db 层的测试通过，
// 前端只是调一个 404 的 URL）。

import (
	"encoding/json"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// TestListInstrumentsReturnsList 验证路由存在、返回结构正确、且新标的能出现在结果里。
func TestListInstrumentsReturnsList(t *testing.T) {
	s, _, request := newAuthServer(t)

	instID := upsertTestInstrument(t, s.m.pg, db.InstrumentInput{
		Market: "us", Symbol: "TINSTR-LIST", AssetClass: "stock",
	})

	w := request("GET", "/api/instruments", "")
	if w.Code != 200 {
		t.Fatalf("GET /api/instruments: %d %s", w.Code, w.Body.String())
	}

	var body struct {
		Instruments []db.Instrument `json:"instruments"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应: %v（body=%s）", err, w.Body.String())
	}
	if len(body.Instruments) == 0 {
		t.Fatal("标的列表为空 —— 至少应含基准标的与刚建的测试标的")
	}

	// 刚建的标的必须在列表里（证明不是返回空/写死的列表）
	var found *db.Instrument
	for i := range body.Instruments {
		if body.Instruments[i].ID == instID {
			found = &body.Instruments[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("列表里没有 id=%d 的标的（共 %d 条）", instID, len(body.Instruments))
	}
	if found.Symbol != "TINSTR-LIST" || found.AssetClass != "stock" || found.Market != "us" {
		t.Fatalf("字段不符: %+v", *found)
	}
}

// TestListInstrumentsMarketFilter 验证 market 过滤真的生效。
//
// 这条断言的是「过滤比总数少」而不是「过滤结果恰好 1 条」——
// 因为基准标的里有 index/etf，而测试库状态会随其他测试变化。
func TestListInstrumentsMarketFilter(t *testing.T) {
	s, _, request := newAuthServer(t)

	upsertTestInstrument(t, s.m.pg, db.InstrumentInput{Market: "us", Symbol: "TINSTR-F1", AssetClass: "stock"})
	upsertTestInstrument(t, s.m.pg, db.InstrumentInput{Market: "crypto", Symbol: "TINSTR-F2", AssetClass: "crypto"})

	all := request("GET", "/api/instruments", "")
	usOnly := request("GET", "/api/instruments?market=us", "")
	if all.Code != 200 || usOnly.Code != 200 {
		t.Fatalf("状态码: all=%d usOnly=%d", all.Code, usOnly.Code)
	}

	var allBody, usBody struct {
		Instruments []db.Instrument `json:"instruments"`
	}
	if err := json.Unmarshal(all.Body.Bytes(), &allBody); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(usOnly.Body.Bytes(), &usBody); err != nil {
		t.Fatal(err)
	}
	if len(usBody.Instruments) >= len(allBody.Instruments) {
		t.Fatalf("market=us 过滤未生效：全部 %d 条，us %d 条",
			len(allBody.Instruments), len(usBody.Instruments))
	}
	for _, in := range usBody.Instruments {
		if in.Market != "us" {
			t.Fatalf("market=us 的结果里出现了 market=%q 的标的 %s", in.Market, in.Symbol)
		}
	}
}
