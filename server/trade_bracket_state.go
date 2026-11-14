package server

// 下单 + 止盈止损的状态机。
//
// # 唯一要守住的不变量
//
// **该标的该方向上，同时只有一组 OCO。**
//
// 破坏它的后果：两组 OCO 并存时，第一组触发就按 closeFraction=100%
// 平掉**全部**仓（含刚加的那部分），第二组变孤儿。用户看到的收益与
// 「加仓」这个动作毫无关系，而所有回报都是成功。
//
// # 为什么加仓是「先下单后撤旧单」而不是「先撤后下单」
//
//	先撤：撤完成 → 下单被拒 → 整仓裸奔。这个窗口长度取决于下单耗时，
//	      弱网下能到秒级，而裸仓在秒级里就够爆掉。
//	先下单：下单被拒 → 旧 OCO 仍在，仓仍有保护。代价只是「新加的那部分
//	      在重挂前短暂无保护」，而旧单此刻仍覆盖整仓。
//
// 唯一例外：旧 OCO 的触发价已被现价穿过（多单 tp ≤ 现价，或 sl ≥ 现价）。
// 那种单只要一成交就立刻平掉全部仓，所以必须先撤 —— 留着它比裸奔更危险。
//
// # 反手为什么必须撤旧单
//
// net 模式旧 OCO 是 posSide=net, closeFraction=100%。反手后仓的方向
// 已经反过来，旧 OCO 的触发条件在新方向下被翻转（原本的止损成了止盈，
// 原本的止盈成了止损）—— 它会按完全错误的价位把新仓平掉。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/artex/exec"
	"github.com/Autumn-27/artex/exec/okxrest"
)

// protection 状态：模型靠它判断自己是不是留了一个裸仓。
const (
	// ProtectionProtected 该仓已挂 OCO。
	ProtectionProtected = "protected"
	// ProtectionNaked 该仓没有保护 —— 必须在文案里明确说出来。
	ProtectionNaked = "naked"
	// ProtectionDouble 两组 OCO 并存（撤旧失败）。可能按旧价平仓。
	ProtectionDouble = "double"
	// ProtectionUnknown 无法确认（读算法单失败等）。
	ProtectionUnknown = "unknown"
)

// settleParam 是一次下单需要的全部参数。
type settleParam struct {
	InstID  string
	TdMode  string
	PosMode string
	PosSide string // 被保护的方向 long | short
	Side    string // buy | sell
	Qty     string
	Tp      string
	Sl      string
	MarkPx  string
	Effect  string
	ClOrdID string
}

// settleOutcome 是下单 + 挂保护的结果。
type settleOutcome struct {
	OrdID      string
	FillState  string // filled | partial | unknown
	FillQty    string
	FillAvgPx  string
	Protection string
	AlgoID     string
	OldAlgoID  string
	// Note 是给模型看的一句话说明（裸仓/双单时必须非空）。
	Note string
	// Post 是下单后重读的持仓快照（拿不到时为 nil）。
	Post *positionSnapshot
}

func (o settleOutcome) naked() bool { return o.Protection == ProtectionNaked }

// settleBracket 下单并按 effect 管理止盈止损。
//
// effect 决定顺序：
//
//	open      一次 PlaceOrder（带 attachAlgoOrds），不单独调 PlaceAlgoOCO
//	scale_in  下单 → 等成交 → 重读持仓 → 撤旧 → 挂新（穿价时先撤）
//	reduce    PlaceOrder(reduceOnly)，不动 OCO（closeFraction=100% 仍覆盖剩余量）
//	reverse   撤旧 → 下单 → 不挂新（方向刚反过来，让模型决定新保护）
func (t *ExecToolSet) settleBracket(ctx context.Context, c okxTrader, p settleParam) (settleOutcome, error) {
	var out settleOutcome
	if c == nil {
		return out, fmt.Errorf("交易客户端未装配")
	}

	// ---- 先读现有 OCO（除 open 外都要知道有没有旧单）----
	var oldAlgo *okxrest.AlgoOrder
	if p.Effect != exec.EffectOpen {
		algos, aErr := c.ListAlgoOrders(ctx, "SWAP", "oco")
		if aErr != nil {
			// 读不到算法单时不能盲目下单：加仓会留下两组 OCO；
			// 反手会按旧方向平仓。这是必须 fail-closed 的点。
			return out, fmt.Errorf("读现有止盈止损失败（无法安全下单）: %v", aErr)
		}
		oldAlgo = pickAlgoFor(algos, p.InstID, p.PosMode, p.PosSide)
	}
	if oldAlgo != nil {
		out.OldAlgoID = oldAlgo.AlgoID
	}

	// ---- 穿价判断：旧 OCO 必须在被新单触发前撤掉 ----
	cancelFirst := false
	if oldAlgo != nil && p.Effect != exec.EffectReduce {
		cancelFirst = oldBracketCrossed(p.PosSide, p.MarkPx, oldAlgo.TpTriggerPx, oldAlgo.SlTriggerPx)
	}

	// ---- 需要先撤的分支 ----
	if oldAlgo != nil && (cancelFirst || p.Effect == exec.EffectReverse) {
		if cErr := c.CancelAlgo(ctx, p.InstID, oldAlgo.AlgoID); cErr != nil {
			if p.Effect == exec.EffectReverse {
				// 反手不能带着旧 OCO 下单：它会按旧方向平掉新仓。
				return out, fmt.Errorf(
					"撤旧止盈止损失败（%v）。反手必须撤掉旧 OCO —— 否则它会在成交后按旧方向平仓，"+
						"所以本次未下单。稍后重试", cErr)
			}
			// 加仓：继续下单，但要如实回报「两组 OCO 并存」。
			out.Protection = ProtectionDouble
			out.Note = "旧止盈止损撤不掉，正在与旧单并存 —— 它一触发就会平掉全部仓（含本次加的）。" +
				"立刻用 get_positions 核对，并调 modify_bracket 修掉"
		} else {
			oldAlgo = nil // 已撤，后续不再撤
		}
	}

	// ---- 组装下单请求 ----
	req := okxrest.OrderRequest{
		InstID:     p.InstID,
		TdMode:     p.TdMode,
		Side:       p.Side,
		PosSide:    exec.DerivePosSide(p.PosMode, p.Side, p.Effect == exec.EffectReduce, p.PosSide),
		OrdType:    "market",
		Sz:         p.Qty,
		ClOrdID:    p.ClOrdID,
		ReduceOnly: p.Effect == exec.EffectReduce,
	}
	// 只有纯开仓才随主单附带 OCO —— 加仓/反手要走「成交后重挂」，
	// 因为附带的那组保护是按「本次下单量」算的，仓量已经变了。
	if p.Effect == exec.EffectOpen && (p.Tp != "" || p.Sl != "") {
		req.AttachAlgos = []okxrest.AlgoAttach{{
			TpTriggerPx: p.Tp, TpOrdPx: "-1", TpTriggerPxType: "last",
			SlTriggerPx: p.Sl, SlOrdPx: "-1", SlTriggerPxType: "last",
		}}
	}

	ack, oErr := c.PlaceOrder(ctx, req)
	if oErr != nil && exec.IsDuplicateErr(oErr) {
		// 重复单不是失败：上一轮（超时重试）其实已经下成了。
		if st, qErr := c.QueryOrderByClOrdID(ctx, p.InstID, p.ClOrdID); qErr == nil && st != nil {
			ack = &okxrest.OrderAck{OrdID: st.OrdID, ClOrdID: st.ClOrdID}
			oErr = nil
		}
	}
	if oErr != nil {
		return out, fmt.Errorf("下单失败: %v", oErr)
	}
	out.OrdID = ack.OrdID
	out.FillState = "unknown"

	// 纯开仓：保护已随主单附带，到此为止。
	if p.Effect == exec.EffectOpen {
		if p.Tp != "" || p.Sl != "" {
			out.Protection = ProtectionProtected
		} else {
			out.Protection = ProtectionNaked
			out.Note = "本次未给止盈止损，该仓没有保护 —— 调 modify_bracket 补挂"
		}
		return out, nil
	}

	// ---- 等成交，再决定保护单怎么挂 ----
	fillState, _, _ := awaitFill(ctx, c, p.InstID, p.ClOrdID)
	out.FillState = fillState

	// 重读持仓：OCO 用 closeFraction=100%，不依赖具体数量，
	// 但回报里给模型一个真实仓位比给「预计值」有用得多。
	if snap, sErr := t.reloadSnapshot(ctx, c, p); sErr == nil {
		out.Post = snap
	}

	// ---- reduce：不动 OCO ----
	if p.Effect == exec.EffectReduce {
		// 旧 OCO 的 closeFraction 是相对**当前持仓**的，减仓后仍覆盖剩余量。
		// 撤掉反而会让剩余仓裸奔。
		if oldAlgo != nil {
			out.Protection = ProtectionProtected
			out.AlgoID = oldAlgo.AlgoID
			out.Note = "减仓成功，原止盈止损继续保护剩余仓（closeFraction 按当前持仓计算）"
		} else {
			out.Protection = ProtectionNaked
			out.Note = "减仓后该仓没有止盈止损（原本就没有）—— 需要则调 modify_bracket"
		}
		return out, nil
	}

	// ---- reverse：不挂新保护（方向刚反过来，让模型决定）----
	if p.Effect == exec.EffectReverse {
		out.Protection = ProtectionNaked
		out.Note = "反手成功。旧止盈止损已撤，**当前无保护** —— 立刻调 modify_bracket 按新方向挂"
		return out, nil
	}

	// ---- scale_in：挂新 OCO ----
	if p.Tp == "" && p.Sl == "" {
		out.Protection = ProtectionNaked
		out.Note = "加仓成功，但本次未给止盈止损，**当前无保护** —— 调 modify_bracket"
		return out, nil
	}
	// 触发价若已在新仓方向的错误一侧，挂了也会立刻触发 —— 拒绝挂单并说清。
	markPx := p.MarkPx
	if markPx == "" && out.Post != nil {
		if hp, ok := out.Post.side(p.PosSide); ok {
			markPx = hp.MarkPx
		}
	}
	if why := bracketSideInvalid(p.PosSide, markPx, p.Tp, p.Sl); why != "" {
		out.Protection = ProtectionNaked
		out.Note = fmt.Sprintf("加仓成功但止盈止损未挂（%s）—— **当前无保护**，用 modify_bracket 按新持仓重设价格", why)
		return out, nil
	}

	// 旧单若还没撤（未穿价分支会走到这里），先撤再挂。
	if oldAlgo != nil {
		if cErr := c.CancelAlgo(ctx, p.InstID, oldAlgo.AlgoID); cErr != nil {
			out.Protection = ProtectionDouble
			out.Note = "加仓成功，但旧止盈止损撤不掉 —— 两组并存，旧单触发会平掉全部仓。" +
				"立刻 get_positions 核对并用 modify_bracket 修掉"
			return out, nil
		}
		oldAlgo = nil
	}

	algoID, pErr := c.PlaceAlgoOCO(ctx, okxrest.AlgoRequest{
		InstID: p.InstID, TdMode: p.TdMode, Side: closeSideOf(p.PosSide),
		PosSide: exec.DerivePosSideForAlgo(p.PosMode, p.PosSide),
		OrdType: "oco", CloseFraction: "1",
		TpTriggerPx: p.Tp, TpOrdPx: "-1",
		SlTriggerPx: p.Sl, SlOrdPx: "-1",
	})
	if pErr != nil {
		out.Protection = ProtectionNaked
		out.Note = fmt.Sprintf("加仓成功但重挂止盈止损失败（%v）—— **该仓现在没有保护单**，"+
			"立刻调 modify_bracket 重挂", pErr)
		return out, nil
	}
	out.Protection = ProtectionProtected
	out.AlgoID = algoID
	if fillState != "filled" {
		out.Note = "未能确认成交（" + fillState + "），已按当前持仓重挂止盈止损；" +
			"稍后可用 get_positions 核对实际数量"
	}
	return out, nil
}

// awaitFill 轮询我方 clOrdId 直到成交或超时。
//
// # 为什么不能固定 sleep 一秒就继续
//
// 市价单通常在 1 秒内成交，但弱网/流动性差时可能更久。用固定 sleep 猜，
// 猜短了会按旧持仓算保护，猜长了白等。轮询拿到的是确定状态。
//
// 上限 6 次 × 500ms = 3s。超时不视为失败：调用方继续按当前持仓挂
// closeFraction=100% 的 OCO —— 那个不依赖数量，所以数量未知也安全。
func awaitFill(ctx context.Context, c okxTrader, instID, clOrdID string) (state, fillQty, avgPx string) {
	const attempts = 6
	const gap = 500 * time.Millisecond
	for i := 0; i < attempts; i++ {
		select {
		case <-ctx.Done():
			return "unknown", "", ""
		case <-time.After(gap):
		}
		st, err := c.QueryOrderByClOrdID(ctx, instID, clOrdID)
		if err != nil || st == nil {
			continue
		}
		switch st.State {
		case "filled":
			return "filled", strings.TrimSpace(st.AccFillSz), strings.TrimSpace(st.AvgPx)
		case "partially_filled":
			// 继续等：部分成交时挂 OCO 会按「当前持仓」算，
			// 而部分成交的持仓还在变，等它稳定更准。
			continue
		case "canceled":
			return "canceled", "", ""
		}
	}
	return "unknown", "", ""
}

// reloadSnapshot 下单后重读持仓。
func (t *ExecToolSet) reloadSnapshot(ctx context.Context, c okxTrader, p settleParam) (*positionSnapshot, error) {
	rows, err := c.GetPositions(ctx, "SWAP")
	if err != nil {
		return nil, err
	}
	return snapshotFromExchange(rows, p.InstID, p.PosMode), nil
}

// pickAlgoFor 在算法单列表里挑出该标的该方向的那一条。
//
// long_short 模式下同标的有 long / short 两条仓、各一组 OCO，
// 必须精确匹配 —— 撤错方向会让另一条仓裸奔，且用户看不出原因。
func pickAlgoFor(algos []okxrest.AlgoOrder, instID, posMode, posSide string) *okxrest.AlgoOrder {
	for i := range algos {
		ag := algos[i]
		if !strings.EqualFold(strings.TrimSpace(ag.InstID), strings.TrimSpace(instID)) {
			continue
		}
		if st := strings.ToLower(strings.TrimSpace(ag.State)); st != "" && st != "live" {
			continue
		}
		if !exec.AlgoSideMatches(posMode, ag.PosSide, posSide) {
			continue
		}
		return &algos[i]
	}
	return nil
}

// oldBracketCrossed 判断旧 OCO 的触发价是否已被现价穿过。
//
// 穿过的意思是「这条保护单一挂上去就该成交」：多单 tp ≤ 现价（已达标）
// 或 sl ≥ 现价（已破位）。这种单必须在下新单前撤掉 ——
// 留着它，加仓成交的瞬间它就把全部仓平了。
func oldBracketCrossed(posSide, markPx, tp, sl string) bool {
	if strings.TrimSpace(markPx) == "" {
		return false
	}
	// bracketSideInvalid 返回非空 = 方向不合法 = 已越界。
	return bracketSideInvalid(posSide, markPx, tp, sl) != ""
}

// closeSideOf 返回平该方向仓的下单方向。
func closeSideOf(posSide string) string {
	if strings.EqualFold(strings.TrimSpace(posSide), "short") {
		return "buy"
	}
	return "sell"
}
