package server

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/exec"
	"github.com/Autumn-27/artex/exec/okxrest"
)

// 任务级交易记录接口。
//
// # 为什么实盘与模拟盘分两段返回
//
// 两边的钱不是一回事，混在一起算总额会得出「模拟盘赚 50 + 实盘亏 30 = 赚 20」
// 这种没意义的数字。所以响应里 live / paper 各自一组 fills+pnl+totals，
// 前端分块展示，绝不合并。
//
// 数据来源：
//   - live：trade_fills（OKX /trade/fills 逐笔同步，含手续费）
//   - paper：paper_orders（纸面账本，手续费按设计不记）
type tradeRecordResp struct {
	TaskID string      `json:"task_id"`
	Live   tradeBucket `json:"live"`
	// Actions 是工具级交易台账（开仓/平仓/调杠杆的原始动作）。
	//
	// # 为什么与 live/paper 分开
	//
	// live/paper 是「成交」视角（钱怎么动的，用于算盈亏）；
	// actions 是「决策」视角（谁在什么时候为什么这么做的）。
	// 一笔市价单可能拆成多笔成交，也可能被拒根本没成交 —— 两者的条数不对应，
	// 合并展示会让「下了 3 单只成了 2 笔」这种事实看不出来。
	Actions []db.TradeRecord `json:"actions"`
	Note    string           `json:"note,omitempty"`
}

// tradeBucket 的数组字段必须永远序列化成 []，不能是 null。
//
// Go 的 nil slice → JSON `null`，而前端写的是 `live.fills.length` /
// `.map(...)` —— `null.length` 直接抛 TypeError，整页变
// "This page couldn't load"（实测任务 44：从未成交）。
//
// 修复方式：handler 构造响应时就把两个数组初始化成空切片（见 taskTrades 开头）。
// 那里是唯一的构造点 —— 谁要新增分支，也只会从同一个零值出发。
type tradeBucket struct {
	Fills  []db.TradeFill `json:"fills"`
	PnL    []exec.PnL     `json:"pnl"`
	Totals tradeTotals    `json:"totals"`
	// Synced 为 false 表示「还没拉到任何成交」（≠ 有单但没成交）。
	Synced bool `json:"synced"`
}

type paperTradeRow struct {
	ID     int64  `json:"id"`
	InstID string `json:"inst_id"`
	Side   string `json:"side"`
	Qty    string `json:"qty"`
	Price  string `json:"price"`
	Status string `json:"status"`
	Filled string `json:"filled_at,omitempty"`
}

type tradeTotals struct {
	Realized    string `json:"realized"`
	FeeTotal    string `json:"fee_total"`
	NetRealized string `json:"net_realized"`
}

func (s *Server) taskTrades(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := canonicalTaskID(r.PathValue("id"))
	if !ok {
		writeErr(w, 400, "任务 id 无效")
		return
	}
	numID, _ := strconv.ParseInt(id, 10, 64)
	// 数组字段从这里起就不是 nil（见 tradeBucket 的注释）：本任务没成交时
	// 前端拿到的是 []，不是 null。
	resp := tradeRecordResp{
		TaskID:  id,
		Live:    tradeBucket{Fills: []db.TradeFill{}, PnL: []exec.PnL{}},
		Actions: []db.TradeRecord{},
	}

	// ---- 实盘 ----
	fills, err := pg.ListFills(db.FillsFilter{TaskID: numID, Limit: 1000})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	resp.Live.Fills = append(resp.Live.Fills, fills...)
	resp.Live.PnL = append(resp.Live.PnL, exec.ComputePnLByInst(fills)...)
	resp.Live.Totals = sumPnL(resp.Live.PnL)
	resp.Live.Synced = len(fills) > 0

	// ---- 工具台账 ----
	if recs, rErr := pg.ListTradeRecords(db.TradeRecordFilter{TaskID: numID, Limit: 300}); rErr == nil {
		resp.Actions = append(resp.Actions, recs...)
	}

	if !resp.Live.Synced {
		resp.Note = "本任务还没有任何成交记录。实盘成交每 60 秒从交易所同步一次。"
	}
	writeJSON(w, 200, resp)
}

// sumPnL 合计各标的的盈亏（精确十进制，不用 float）。
func sumPnL(pnl []exec.PnL) tradeTotals {
	if len(pnl) == 0 {
		return tradeTotals{Realized: "0", FeeTotal: "0", NetRealized: "0"}
	}
	var realized, fee, net string
	for _, p := range pnl {
		realized = exec.DecimalAddStr(realized, p.Realized)
		fee = exec.DecimalAddStr(fee, p.FeeTotal)
		net = exec.DecimalAddStr(net, p.NetRealized)
	}
	return tradeTotals{Realized: realized, FeeTotal: fee, NetRealized: net}
}

// taskTradesSync 手工触发一次实盘成交同步（只同步本任务涉及的账户）。
//
// # 为什么需要手工入口
//
// 自动同步是 60 秒一轮。用户刚平完仓就想看盈亏时，等一轮的体验很差，
// 而这里拉的是同一个接口、同一条幂等路径，多一个触发口没有额外风险。
func (s *Server) taskTradesSync(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := canonicalTaskID(r.PathValue("id"))
	if !ok {
		writeErr(w, 400, "任务 id 无效")
		return
	}
	numID, _ := strconv.ParseInt(id, 10, 64)

	acctIDs, err := pg.AccountIDsForTask(numID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if len(acctIDs) == 0 {
		writeErr(w, 400, "本任务还没有与任何执行账户关联的下单记录，无从同步")
		return
	}
	syncer := exec.NewFillsSyncer(pg, s.okxTradeCred)
	inserted := 0
	var lastErr error
	for _, aid := range acctIDs {
		n, sErr := syncer.SyncAccount(r.Context(), aid)
		inserted += n
		if sErr != nil {
			lastErr = sErr
		}
	}
	out := map[string]any{"inserted": inserted, "accounts": len(acctIDs)}
	if lastErr != nil {
		out["warning"] = lastErr.Error()
	}

	// ---- 同步完立刻对账：本地成交推算的净持仓 vs 交易所真实持仓 ----
	//
	// # 为什么放在同步响应里（2026-11-15 事故）
	//
	// 那次的不一致（本地 -68.95 / 交易所 0）存活了一整天没人发现：
	// 两边都不报错，报表只是「偏小」。等有人手动比对 SQL 才暴露，
	// 期间已经基于错误的净持仓做了决策。
	//
	// 把它做成每次同步都返回的结论，不一致就在响应里说出来，
	// 不需要任何人记得去查。
	cred, cErr := s.okxTradeCred()
	if cErr == nil && cred != nil {
		client := okxrest.NewClient(okxrest.Credentials{
			APIKey: cred.APIKey, SecretKey: cred.SecretKey, Passphrase: cred.Passphrase,
			Demo: cred.Demo, BaseURL: cred.BaseURL,
		})
		var drifts []exec.PositionDrift
		for _, aid := range acctIDs {
			local, nErr := pg.LocalNetPositions(aid)
			if nErr != nil {
				continue
			}
			d, dErr := exec.DetectPositionDrift(r.Context(), client, local, "")
			if dErr != nil {
				if out["warning"] == nil {
					out["warning"] = dErr.Error()
				}
				continue
			}
			drifts = append(drifts, d...)
		}
		if len(drifts) > 0 {
			out["position_drift"] = drifts
			out["drift_note"] = "本地成交推算的净持仓与交易所不一致 —— " +
				"常见成因是成交明细漏同步（大单拆成上百笔成交时分页不足）、" +
				"或有人用交易所 APP/CLI 手工操作过。请以交易所为准"
			slog.Warn("持仓漂移", "task", numID, "drifts", len(drifts))
		} else {
			out["position_drift"] = []exec.PositionDrift{}
		}
	}
	writeJSON(w, 200, out)
}
