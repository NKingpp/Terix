package exec

import (
	"testing"

	"github.com/Autumn-27/artex/db"
)

func fill(id int64, inst, side, px, sz, fee, ts string) db.TradeFill {
	return db.TradeFill{ID: id, FillID: ts + side, InstID: inst, Side: side, FillPx: px, FillSz: sz, Fee: fee, FilledAt: ts}
}

// 单次开平：买 1@100，卖 1@110，手续费 -0.05/-0.05 → 净 +9.90
func TestPnLSingleRoundTrip(t *testing.T) {
	ps := ComputePnLByInst([]db.TradeFill{
		fill(1, "A-USDT-SWAP", "buy", "100", "1", "-0.05", "2026-11-14T01:00:00Z"),
		fill(2, "A-USDT-SWAP", "sell", "110", "1", "-0.05", "2026-11-14T02:00:00Z"),
	})
	if len(ps) != 1 {
		t.Fatalf("应 1 个标的，得到 %d", len(ps))
	}
	p := ps[0]
	if p.Realized != "10" {
		t.Errorf("已实现应 10，得到 %s", p.Realized)
	}
	if p.FeeTotal != "-0.1" {
		t.Errorf("手续费应 -0.1，得到 %s", p.FeeTotal)
	}
	if p.NetRealized != "9.9" {
		t.Errorf("净盈亏应 9.9，得到 %s", p.NetRealized)
	}
	if p.OpenQty != "0" {
		t.Errorf("应已平仓，持仓 0，得到 %s", p.OpenQty)
	}
}

// 做空：卖 2@100，买 2@90 → +20
func TestPnLShortRoundTrip(t *testing.T) {
	p := ComputePnLByInst([]db.TradeFill{
		fill(1, "B-USDT-SWAP", "sell", "100", "2", "0", "2026-11-14T01:00:00Z"),
		fill(2, "B-USDT-SWAP", "buy", "90", "2", "0", "2026-11-14T02:00:00Z"),
	})[0]
	if p.Realized != "20" {
		t.Fatalf("空头已实现应 20，得到 %s", p.Realized)
	}
	if p.OpenQty != "0" {
		t.Fatalf("应已平，得到 %s", p.OpenQty)
	}
}

// 部分平仓：买 2@100，卖 1@110 → 已实现 +10，剩 1@100
func TestPnLPartialClose(t *testing.T) {
	p := ComputePnLByInst([]db.TradeFill{
		fill(1, "C-USDT-SWAP", "buy", "100", "2", "0", "2026-11-14T01:00:00Z"),
		fill(2, "C-USDT-SWAP", "sell", "110", "1", "0", "2026-11-14T02:00:00Z"),
	})[0]
	if p.Realized != "10" {
		t.Fatalf("部分平仓已实现应 10，得到 %s", p.Realized)
	}
	if p.OpenQty != "1" {
		t.Fatalf("剩余持仓应 1，得到 %s", p.OpenQty)
	}
	if p.OpenAvgCost != "100" {
		t.Fatalf("剩余成本应 100，得到 %s", p.OpenAvgCost)
	}
}

// 反手：买 1@100（多），卖 3@110（平多 + 开空 2）→ 已实现 +10，持仓 -2@110
func TestPnLReversePosition(t *testing.T) {
	p := ComputePnLByInst([]db.TradeFill{
		fill(1, "D-USDT-SWAP", "buy", "100", "1", "0", "2026-11-14T01:00:00Z"),
		fill(2, "D-USDT-SWAP", "sell", "110", "3", "0", "2026-11-14T02:00:00Z"),
	})[0]
	if p.Realized != "10" {
		t.Fatalf("反手已实现应 10，得到 %s", p.Realized)
	}
	if p.OpenQty != "-2" {
		t.Fatalf("反手后应持空 2，得到 %s", p.OpenQty)
	}
	if p.OpenAvgCost != "110" {
		t.Fatalf("反手后成本应 110，得到 %s", p.OpenAvgCost)
	}
}

// 顺序无关：乱序输入必须得到同一结果（交易所返回顺序不保证）。
func TestPnLOrderIndependent(t *testing.T) {
	a := ComputePnLByInst([]db.TradeFill{
		fill(1, "E-USDT-SWAP", "buy", "100", "1", "0", "2026-11-14T01:00:00Z"),
		fill(2, "E-USDT-SWAP", "sell", "110", "1", "0", "2026-11-14T02:00:00Z"),
	})[0]
	b := ComputePnLByInst([]db.TradeFill{
		fill(2, "E-USDT-SWAP", "sell", "110", "1", "0", "2026-11-14T02:00:00Z"),
		fill(1, "E-USDT-SWAP", "buy", "100", "1", "0", "2026-11-14T01:00:00Z"),
	})[0]
	if a.Realized != b.Realized || a.OpenQty != b.OpenQty {
		t.Fatalf("顺序应无关：%+v vs %+v", a, b)
	}
}

// 多标的互不影响。
func TestPnLMultipleInsts(t *testing.T) {
	ps := ComputePnLByInst([]db.TradeFill{
		fill(1, "F-USDT-SWAP", "buy", "100", "1", "0", "2026-11-14T01:00:00Z"),
		fill(2, "F-USDT-SWAP", "sell", "120", "1", "0", "2026-11-14T02:00:00Z"),
		fill(3, "G-USDT-SWAP", "buy", "50", "1", "0", "2026-11-14T01:00:00Z"),
		fill(4, "G-USDT-SWAP", "sell", "40", "1", "0", "2026-11-14T02:00:00Z"),
	})
	if len(ps) != 2 {
		t.Fatalf("应 2 个标的，得到 %d", len(ps))
	}
	if ps[0].InstID != "F-USDT-SWAP" || ps[0].Realized != "20" {
		t.Errorf("F 应 +20，得到 %+v", ps[0])
	}
	if ps[1].InstID != "G-USDT-SWAP" || ps[1].Realized != "-10" {
		t.Errorf("G 应 -10，得到 %+v", ps[1])
	}
}

// 只开仓不平时：已实现 0，持仓与成本正确。
func TestPnLOpenOnly(t *testing.T) {
	p := ComputePnLByInst([]db.TradeFill{
		fill(1, "H-USDT-SWAP", "buy", "100", "1.5", "-0.03", "2026-11-14T01:00:00Z"),
	})[0]
	if p.Realized != "0" || p.OpenQty != "1.5" || p.OpenAvgCost != "100" {
		t.Fatalf("只开仓结果不对：%+v", p)
	}
	if p.NetRealized != "-0.03" {
		t.Fatalf("净盈亏应等于手续费 -0.03，得到 %s", p.NetRealized)
	}
}

// long_short 模式下同标的多空必须各自算账。
//
// # 不分的后果
//
// 一边大赚、一边大亏且两边都已平仓时，合并成一条净持仓会让
// 报表显示「没赚没亏」—— 而钱其实一进一出。这个错误不会报错，
// 只是数字没有意义。
func TestPnLGroupsByPosSide(t *testing.T) {
	fills := []db.TradeFill{
		// 多单：100 开、110 平 → +10
		{FillID: "L1", InstID: "X", Side: "buy", PosSide: "long", FillPx: "100", FillSz: "1", FilledAt: "2026-11-14T01:00:00Z"},
		{FillID: "L2", InstID: "X", Side: "sell", PosSide: "long", FillPx: "110", FillSz: "1", FilledAt: "2026-11-14T02:00:00Z"},
		// 空单：100 开、105 平 → -5
		{FillID: "S1", InstID: "X", Side: "sell", PosSide: "short", FillPx: "100", FillSz: "1", FilledAt: "2026-11-14T01:10:00Z"},
		{FillID: "S2", InstID: "X", Side: "buy", PosSide: "short", FillPx: "105", FillSz: "1", FilledAt: "2026-11-14T02:10:00Z"},
	}
	out := ComputePnLByInst(fills)
	if len(out) != 2 {
		t.Fatalf("同标的多空应各一条，实际 %d 条: %+v", len(out), out)
	}
	bySide := map[string]PnL{}
	for _, p := range out {
		bySide[p.PosSide] = p
	}
	long, ok := bySide["long"]
	if !ok {
		t.Fatal("缺 long 组")
	}
	short, ok2 := bySide["short"]
	if !ok2 {
		t.Fatal("缺 short 组")
	}
	if long.Realized != "10" {
		t.Errorf("多单已实现 = %q, want 10", long.Realized)
	}
	if short.Realized != "-5" {
		t.Errorf("空单已实现 = %q, want -5", short.Realized)
	}
}

// net 模式（pos_side = net / 空）必须合并成一条，且 PosSide 留空。
//
// 这条是向后兼容的护栏：纸面账本与历史数据都没有双向信息，
// 分裂会算成两条都不完整的盈亏。
func TestPnLMergesNetFills(t *testing.T) {
	fills := []db.TradeFill{
		{FillID: "N1", InstID: "Y", Side: "buy", PosSide: "net", FillPx: "100", FillSz: "1", FilledAt: "2026-11-14T01:00:00Z"},
		{FillID: "N2", InstID: "Y", Side: "sell", PosSide: "", FillPx: "110", FillSz: "1", FilledAt: "2026-11-14T02:00:00Z"},
	}
	out := ComputePnLByInst(fills)
	if len(out) != 1 {
		t.Fatalf("net 模式应合并成 1 条，实际 %d 条", len(out))
	}
	if out[0].PosSide != "" {
		t.Errorf("合并时 PosSide 应为空，实际 %q", out[0].PosSide)
	}
	if out[0].Realized != "10" {
		t.Errorf("已实现 = %q, want 10", out[0].Realized)
	}
}
