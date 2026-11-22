package server

import (
	"encoding/json"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/exec"
)

// 无成交时响应里的数组字段必须是 []，不能是 null。
//
// # 为什么这条值得单独一个测试（2026-11-20 任务 44）
//
// 前端交易记录页写的是 `live.fills.length` / `live.fills.map(...)`。
// Go 的 nil slice 序列化成 JSON `null`，而 `null.length` 直接抛 TypeError ——
// 表现为整页 "This page couldn't load"，且错误在浏览器控制台之外看不到，
// 只能靠打开该 Tab 才发现。
//
// 任务 44 从未成交，于是 `pg.ListFills` 返回 nil → 页面崩。
// 修复是两端都归一（后端发 []，前端 api.ts 也兜一层），这里锁住后端那一半。
func TestTradeRecordRespMarshalsEmptyArraysNotNull(t *testing.T) {
	// 与 handler 相同的构造方式（server/trades_api.go 的 taskTrades 开头）。
	// 用真正的构造点而不是零值 —— 零值天然是 nil，测它等于测「nil 会变 null」，
	// 那是 Go 的既定行为，不是我们要锁的契约。
	resp := tradeRecordResp{
		TaskID:  "44",
		Live:    tradeBucket{Fills: []db.TradeFill{}, PnL: []exec.PnL{}},
		Actions: []db.TradeRecord{},
		Note:    "本任务还没有任何成交记录。",
	}

	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}

	live, ok := got["live"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺 live 段: %s", b)
	}
	for _, path := range []struct {
		name string
		v    any
	}{
		{"live.fills", live["fills"]},
		{"live.pnl", live["pnl"]},
		{"actions", got["actions"]},
	} {
		if path.v == nil {
			t.Errorf("%s 是 null —— 前端 .length/.map 会抛，整页崩。零值必须序列化成 []", path.name)
			continue
		}
		if _, isArr := path.v.([]any); !isArr {
			t.Errorf("%s 不是数组（%T）", path.name, path.v)
		}
	}
}

// 有成交时同样必须是数组，且条数对得上。
func TestTradeRecordRespMarshalsPopulatedArrays(t *testing.T) {
	resp := tradeRecordResp{
		TaskID: "39",
		Live: tradeBucket{
			Fills:  make([]db.TradeFill, 3),
			Synced: true,
		},
		Actions: make([]db.TradeRecord, 2),
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Live struct {
			Fills []any `json:"fills"`
		} `json:"live"`
		Actions []any `json:"actions"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Live.Fills) != 3 || len(got.Actions) != 2 {
		t.Fatalf("条数不对: fills=%d actions=%d", len(got.Live.Fills), len(got.Actions))
	}
}
