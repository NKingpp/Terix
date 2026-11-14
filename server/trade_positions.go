package server

// 持仓快照：交易工具判断「加仓 / 减仓 / 反手 / 开新仓」的唯一数据源。
//
// # 为什么必须集中在一处（2026-11-14）
//
// 「net 模式下 posSide 恒为 net，方向看 pos 符号」这段判断原来在三个
// 地方各写了一遍（开仓护栏、平仓定方向、modify_bracket 找保护单）。
// 三份实现里只要有一份与另外两份不一致，症状就是
// 「close_position 能平掉，modify_bracket 却说没有该持仓」——
// 而且只在 long_short 模式出现，纸面测试永远看不见（纸面按 net 处理）。
//
// 现在所有调用方都从这里拿 BySide，方向判定只有一份实现
// （exec.SideQtyOf）。

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/exec"
	"github.com/Autumn-27/artex/exec/okxrest"
)

// heldPosition 是某方向上的持仓摘要（数量恒为绝对值的十进制字符串）。
type heldPosition struct {
	Side   string
	Qty    string
	Avail  string
	AvgPx  string
	MarkPx string
}

// positionSnapshot 是某标的当前的持仓状态。
type positionSnapshot struct {
	// BySide 按 long / short 索引。没有的方向不在 map 里（不是零值条目）。
	BySide map[string]heldPosition
	// Raw 是交易所原始行（供 get_positions 渲染）。
	Raw []okxrest.Position
	// Plans 是纸面未平仓计划（纸面模式专用）。
	Plans []db.PaperTradePlan
	// Paper 为真表示这条快照来自纸面账本，不是交易所。
	Paper bool
}

// side 返回某方向的持仓；不存在时返回零值 + false。
func (s *positionSnapshot) side(side string) (heldPosition, bool) {
	if s == nil || s.BySide == nil {
		return heldPosition{}, false
	}
	p, ok := s.BySide[strings.ToLower(strings.TrimSpace(side))]
	return p, ok
}

// qtyOf 返回某方向的绝对持仓量（不存在时 "0"）。
func (s *positionSnapshot) qtyOf(side string) string {
	if p, ok := s.side(side); ok {
		return p.Qty
	}
	return "0"
}

// empty 判断是否完全无仓。
func (s *positionSnapshot) empty() bool {
	return s == nil || len(s.BySide) == 0
}

// snapshotFromExchange 把交易所返回的持仓行聚合成快照。
//
// 纯函数（不碰网络），便于测试 net / long_short 两种形态。
// 同一方向出现多行时数量累加 —— 正常不会发生，但聚合比覆盖安全：
// 少算数量会让平仓量校验放行超量单。
func snapshotFromExchange(rows []okxrest.Position, instID, posMode string) *positionSnapshot {
	out := &positionSnapshot{BySide: map[string]heldPosition{}, Raw: rows}
	want := strings.ToUpper(strings.TrimSpace(instID))
	for _, p := range rows {
		if want != "" && strings.ToUpper(strings.TrimSpace(p.InstID)) != want {
			continue
		}
		side, qty, ok := exec.SideQtyOf(posMode, p.PosSide, p.Pos)
		if !ok {
			continue
		}
		avail := exec.AvailOf(p.Pos, p.AvailPos)
		if cur, exists := out.BySide[side]; exists {
			cur.Qty = addDecimal(cur.Qty, qty)
			cur.Avail = addDecimal(cur.Avail, avail)
			// 均价在多行合并时按数量加权；这里行数理论上不会 >1，
			// 简单取第一行可读值即可（加权需要更多上下文，不值得）。
			out.BySide[side] = cur
			continue
		}
		out.BySide[side] = heldPosition{
			Side: side, Qty: qty, Avail: avail,
			AvgPx: strings.TrimSpace(p.AvgPx), MarkPx: strings.TrimSpace(p.MarkPx),
		}
	}
	return out
}

// snapshotFromPaperPlans 把纸面未平仓计划聚合成快照。
//
// # 纸面一律按 net 语义聚合
//
// 纸面账本没有双向隔离：paper_orders 按 instrument 汇总成本与盈亏，
// 同标的多空相抵。所以这里也按「净方向」理解 —— 但实际记账仍按
// pos_side 分开存（closePaper 按 posSide 找 plan）。
//
// 同一方向有多条 plan 时累加数量。历史数据里确实存在过重复行
// （2026-11-14 任务 30 的「同标的可重复开仓」bug 造成），
// 累加比只取第一条更接近事实。
func snapshotFromPaperPlans(plans []db.PaperTradePlan, instrumentID int64, symbol string) *positionSnapshot {
	out := &positionSnapshot{BySide: map[string]heldPosition{}, Plans: plans, Paper: true}
	for _, p := range plans {
		if instrumentID > 0 && p.InstrumentID != instrumentID {
			continue
		}
		if symbol != "" && p.Symbol != "" && p.Symbol != symbol {
			continue
		}
		side := strings.ToLower(strings.TrimSpace(p.PosSide))
		if side != "long" && side != "short" {
			continue
		}
		qty := strings.TrimSpace(p.Qty)
		if qty == "" {
			continue
		}
		if cur, exists := out.BySide[side]; exists {
			cur.Qty = addDecimal(cur.Qty, qty)
			out.BySide[side] = cur
			continue
		}
		out.BySide[side] = heldPosition{Side: side, Qty: qty, Avail: qty}
	}
	return out
}

// snapshotPositions 读当前标的的持仓快照。
//
// 纸面模式读纸面账本（交易所里没有纸面仓，读了必然是空 ——
// 那会让模型误判「无需平仓」，2026-11-14 任务 30 实测过）。
// 实盘模式读交易所；读失败返回 error，由调用方决定是否 fail-closed。
func (t *ExecToolSet) snapshotPositions(ctx context.Context, ec *execCtx) (*positionSnapshot, error) {
	if ec == nil || ec.inst == nil {
		return nil, fmt.Errorf("持仓快照缺少标的上下文")
	}
	if ec.isPaper {
		plans, err := t.pg.ListOpenPaperTradePlans(0)
		if err != nil {
			return nil, fmt.Errorf("读纸面持仓失败: %v", err)
		}
		return snapshotFromPaperPlans(plans, ec.inst.ID, ec.inst.Symbol), nil
	}
	if ec.client == nil {
		return nil, fmt.Errorf("交易客户端未装配，无法读持仓")
	}
	rows, err := ec.client.GetPositions(ctx, "SWAP")
	if err != nil {
		return nil, fmt.Errorf("读交易所持仓失败: %v", err)
	}
	return snapshotFromExchange(rows, ec.inst.Symbol, ec.params.PosMode), nil
}

// addDecimal 两个十进制字符串相加，保持精确（不走 float64）。
//
// 解析失败按 0 处理：持仓量算不出来比报错更可接受 ——
// 报错会让整个下单流程中断，而 0 只是让数量校验偏保守。
func addDecimal(a, b string) string {
	x, ok1 := new(big.Rat).SetString(strings.TrimSpace(a))
	y, ok2 := new(big.Rat).SetString(strings.TrimSpace(b))
	if !ok1 {
		x = new(big.Rat)
	}
	if !ok2 {
		y = new(big.Rat)
	}
	return decimalString(new(big.Rat).Add(x, y))
}

// decimalString 把 big.Rat 渲染成不带多余尾零的十进制串。
func decimalString(r *big.Rat) string {
	if r == nil {
		return "0"
	}
	if r.IsInt() {
		return r.Num().String()
	}
	s := strings.TrimRight(r.FloatString(16), "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

// signedQtyStr 把方向 + 原始 pos 渲染成带符号的净量字符串。
//
// net 模式下交易所返回的 pos 本身就带符号，直接用它；
// long_short 模式下 pos 恒正，符号要由方向补上 —— 否则模型看到
// 「short 1」会以为持仓是 +1，与 net 模式的读法打架。
func signedQtyStr(side, rawPos string) string {
	p := strings.TrimSpace(rawPos)
	if p == "" {
		return "0"
	}
	if strings.HasPrefix(p, "-") {
		return p
	}
	if strings.EqualFold(strings.TrimSpace(side), "short") {
		return "-" + p
	}
	return p
}
