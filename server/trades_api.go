package server

import (
	"context"
	"net/http"
	"strconv"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/exec"
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
	Paper  paperBucket `json:"paper"`
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

type tradeBucket struct {
	Fills  []db.TradeFill `json:"fills"`
	PnL    []exec.PnL     `json:"pnl"`
	Totals tradeTotals    `json:"totals"`
	// Synced 为 false 表示「还没拉到任何成交」（≠ 有单但没成交）。
	Synced bool `json:"synced"`
}

type paperBucket struct {
	Orders []paperTradeRow `json:"orders"`
	PnL    []exec.PnL      `json:"pnl"`
	Totals tradeTotals     `json:"totals"`
	// FeeIncluded 恒 false —— 纸面账本不记手续费，前端必须标注。
	FeeIncluded bool `json:"fee_included"`
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
	ctx := r.Context()

	resp := tradeRecordResp{TaskID: id}

	// ---- 实盘 ----
	fills, err := pg.ListFills(db.FillsFilter{TaskID: numID, Limit: 1000})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	resp.Live.Fills = fills
	resp.Live.PnL = exec.ComputePnLByInst(fills)
	resp.Live.Totals = sumPnL(resp.Live.PnL)
	resp.Live.Synced = len(fills) > 0

	// ---- 模拟盘 ----
	paperRows, pErr := listPaperTrades(ctx, pg, numID)
	if pErr != nil {
		writeErr(w, 500, pErr.Error())
		return
	}
	resp.Paper.Orders = paperRows
	resp.Paper.PnL = paperPnL(paperRows)
	resp.Paper.Totals = sumPnL(resp.Paper.PnL)
	resp.Paper.FeeIncluded = false

	// ---- 工具台账 ----
	if recs, rErr := pg.ListTradeRecords(db.TradeRecordFilter{TaskID: numID, Limit: 300}); rErr == nil {
		resp.Actions = recs
	}

	if !resp.Live.Synced && len(paperRows) == 0 {
		resp.Note = "本任务还没有任何成交记录。实盘成交每 60 秒从交易所同步一次；" +
			"模拟盘成交在下单后立刻入账。"
	} else if !resp.Live.Synced {
		resp.Note = "只有模拟盘成交。实盘尚无成交记录（未下单，或同步还没跑到）。"
	}
	writeJSON(w, 200, resp)
}

// listPaperTrades 读任务的纸面成交。
//
// 直接查表而不是复用 db.ListPaperPositions：那个聚合的是「当前持仓」，
// 而这里要的是「成交流水」（含已完全平掉的单）。
func listPaperTrades(ctx context.Context, d *db.DB, taskID int64) ([]paperTradeRow, error) {
	rows, err := d.Query(`SELECT po.id, COALESCE(i.symbol, ''), po.side, po.filled_qty,
COALESCE(po.avg_fill_price, po.price, ''), po.status,
COALESCE(po.filled_at, po.created_at, '')
FROM paper_orders po
LEFT JOIN instruments i ON i.id = po.instrument_id
WHERE po.task_id = $1
  AND po.status IN ('filled', 'partial')
  AND CAST(po.filled_qty AS REAL) > 0
ORDER BY COALESCE(po.filled_at, po.created_at) ASC, po.id ASC
LIMIT 2000`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]paperTradeRow, 0, 16)
	for rows.Next() {
		var p paperTradeRow
		if err := rows.Scan(&p.ID, &p.InstID, &p.Side, &p.Qty, &p.Price, &p.Status, &p.Filled); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// paperPnL 把纸面成交转成 exec.PnL 的形状，复用同一套盈亏算法。
//
// # 为什么复用而不是另写一套
//
// 两处算法不同会让「模拟盘赚了、实盘亏了」的对比失去意义 ——
// 而那正是模拟盘存在的唯一理由。纸面侧没有手续费，fee 置 0 即可。
func paperPnL(rows []paperTradeRow) []exec.PnL {
	fills := make([]db.TradeFill, 0, len(rows))
	for _, r := range rows {
		fills = append(fills, db.TradeFill{
			ID: r.ID, FillID: "paper:" + strconv.FormatInt(r.ID, 10),
			InstID: r.InstID, Side: r.Side, FillPx: r.Price, FillSz: r.Qty,
			Fee: "0", FilledAt: r.Filled,
		})
	}
	return exec.ComputePnLByInst(fills)
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
	writeJSON(w, 200, out)
}
