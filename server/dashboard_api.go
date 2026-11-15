package server

// GET /api/dashboard/summary —— 概览页的唯一后端接口。
//
// # 为什么是「一个聚合接口」而不是 5 个独立接口
//
// dashboard 是概览页，它天然需要「同一时刻的快照」。
// 5 个独立请求会被前端用 Promise.all 并发发出，而它们各自在**不同时刻**执行 ——
// 于是「草案 3 条」和「评级 5 条」可能来自相隔几十毫秒的两个瞬间。
//
// 那种不一致在概览页上不可解释：用户看到「待决策 3」而点进去只有 2 条，
// 会以为有 bug。而真正的原因只是「两次查询之间状态变了」。
//
// # 为什么「空」也要有 note
//
// 返回 0 而不说原因，前端只能猜：
//   - pnl.paper_orders=0 → 「没成交」还是「功能没做」？
//   - ratings.total=0    → 「没分析过」还是「表读失败」？
//
// 两种情况的用户动作完全不同（前者去创建任务，后者报告 bug）。
// 所以每个「空」都带一句 note 说明**为什么**是空。

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/exec"
)

// dashboardSummaryDTO 是 GET /api/dashboard/summary 的响应。
//
// ⚠️ 字段名与前端 types.ts 的 DashboardSummary 一一对应 —— 改一处必须改两处。
type dashboardSummaryDTO struct {
	Exec    dashboardExecDTO    `json:"exec"`
	Drafts  dashboardDraftsDTO  `json:"drafts"`
	Ratings dashboardRatingsDTO `json:"ratings"`
	Orders  dashboardOrdersDTO  `json:"orders"`
	Pnl     dashboardPnlDTO     `json:"pnl"`
}

type dashboardExecDTO struct {
	Mode     string                   `json:"mode"`
	Frozen   bool                     `json:"frozen"`
	Accounts dashboardAccountCountDTO `json:"accounts"`
}

type dashboardAccountCountDTO struct {
	Total int64 `json:"total"`
	// Enabled 是 active=true 的账户数。
	Enabled int64 `json:"enabled"`
	// Blocked 是 blocked_at IS NOT NULL 的账户数。
	//
	// ⚠️ 与 Enabled 独立：一个账户可以既 active 又 blocked
	// （那表示「启用中，但被风控冻结」）。所以 Enabled + Blocked 可能 > Total。
	Blocked int64 `json:"blocked"`
}

type dashboardDraftsDTO struct {
	Total     int64            `json:"total"`
	ByVerdict map[string]int64 `json:"by_verdict"`
}

type dashboardRatingsDTO struct {
	Total    int64            `json:"total"`
	ByRating map[string]int64 `json:"by_rating"`
	// Note 只在 total==0 时非空 —— 说明「为什么没有评级」。
	Note string `json:"note,omitempty"`
}

type dashboardOrdersDTO struct {
	Total    int64 `json:"total"`
	InFlight int64 `json:"in_flight"`
}

// dashboardPnlDTO 是盈亏段。**纸面与实盘字段全部带前缀**。
//
// # 为什么每个字段都带 paper_ / live_ 前缀（2026-11-15 改名）
//
// 原来纸面字段叫 realized_pnl / unrealized_pnl / positions，
// 实盘叫 live_realized_pnl。命名不对称的后果是实打实的误读：
//
//	2026-11-15 有人看到 realized_pnl=2766.73，以为是「系统总盈亏」，
//	而它其实是**纸面**的数字 —— 其中还包含一笔现实中必然被拒的
//	10000 张单（名义 160 万 vs 权益 8.7 万）。同一时刻实盘是 -307.48。
//
// 两个数一个天上一个地下，却只有一个带 live_ 前缀。所以现在
// 纸面侧全部补上 paper_：光看字段名就知道这钱是不是真的。
//
// # 纸面字段
//
//	paper_orders              有成交的纸面单数
//	paper_positions           有净持仓（买卖差额 != 0）的标的数
//	paper_realized_pnl        已实现盈亏（只依赖成交数据，永远可算）
//	paper_unrealized_pnl      浮动盈亏（需现价；取不到时为 null，**不是 0**）
//	paper_priced_positions    浮动盈亏覆盖了几个持仓
//	paper_total_positions     一共几个持仓（与上者的差值 = 取不到现价的）
//
// # 为什么用指针表示 paper_unrealized_pnl
//
// null 与 "0" 在金融上完全不同：
//
//	null → 算不出来（没有现价 / 没有持仓）
//	"0"  → 算出来了，刚好打平
//
// 用 string 而非 number：金额精度必须精确保留（NUMERIC），
// JSON number 会被 JS 的 float64 截断两位以上小数。
type dashboardPnlDTO struct {
	// ---- 纸面 ----
	PaperOrders          int64   `json:"paper_orders"`
	PaperPositions       int64   `json:"paper_positions"`
	PaperRealizedPnl     string  `json:"paper_realized_pnl"`
	PaperUnrealizedPnl   *string `json:"paper_unrealized_pnl"`
	PaperPricedPositions int64   `json:"paper_priced_positions"`
	PaperTotalPositions  int64   `json:"paper_total_positions"`
	// ---- 实盘 ----
	// LiveFills 是实盘逐笔成交数（来自 trade_fills，每 60s 从交易所同步）。
	LiveFills int64 `json:"live_fills"`
	// LiveRealizedPnl 是实盘已实现盈亏（含手续费）。与纸面分开算 ——
	// 两边的钱不是一回事，合并会得出「模拟赚 50 + 实盘亏 30 = 赚 20」。
	LiveRealizedPnl string `json:"live_realized_pnl"`
	// LiveFeeTotal 是实盘手续费合计（负数，交易所口径）。
	LiveFeeTotal string `json:"live_fee_total"`
	// Note 说明当前状态（为什么没有浮动盈亏、覆盖了多少）。
	Note string `json:"note"`
}

func (s *Server) dashboardSummary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	conn := s.m.pg.DB

	var out dashboardSummaryDTO

	// ---- exec ----
	mode, err := s.m.pg.GetExecMode()
	if err != nil {
		writeErrCode(w, 500, "读取执行模式失败", "DASHBOARD_EXEC_MODE_FAILED")
		return
	}
	out.Exec.Mode = string(mode)
	out.Exec.Frozen = mode == db.ExecModeFrozen

	acc, err := db.CountDashboardExecAccounts(ctx, conn)
	if err != nil {
		writeErrCode(w, 500, "读取执行账户统计失败", "DASHBOARD_ACCOUNTS_FAILED")
		return
	}
	out.Exec.Accounts = dashboardAccountCountDTO{
		Total: acc.Total, Enabled: acc.Enabled, Blocked: acc.Blocked,
	}

	// ---- drafts ----
	drafts, err := db.CountDashboardDrafts(ctx, conn)
	if err != nil {
		writeErrCode(w, 500, "读取草案统计失败", "DASHBOARD_DRAFTS_FAILED")
		return
	}
	out.Drafts.Total = drafts.Total
	out.Drafts.ByVerdict = map[string]int64{
		"pass": drafts.Pass,
		"deny": drafts.Deny,
		"ask":  drafts.Ask,
		// none = 「还没被风控判定」（risk_verdict IS NULL）。
		//
		// 这个键必须存在且命名如此 —— 前端若把它当拼写错误删掉，
		// 「未判定」的草案就会从分布里消失，而 total 仍包含它们，
		// 于是 sum(by_verdict) != total 且没人知道为什么。
		"none": drafts.None,
	}
	// 值域外的脏值会让 sum < total。它不是错误（数据可能被手工改过），
	// 但要让人知道 —— 否则「总数对不上」会永远沉默。
	if sum := drafts.SumByVerdict(); sum != drafts.Total {
		out.Drafts.ByVerdict["other"] = drafts.Total - sum
	}

	// ---- ratings ----
	ratings, err := db.CountDashboardRatings(ctx, conn)
	if err != nil {
		writeErrCode(w, 500, "读取评级统计失败", "DASHBOARD_RATINGS_FAILED")
		return
	}
	out.Ratings.Total = ratings.Total
	out.Ratings.ByRating = map[string]int64{
		"buy":         ratings.Buy,
		"overweight":  ratings.Overweight,
		"hold":        ratings.Hold,
		"underweight": ratings.Underweight,
		"sell":        ratings.Sell,
		"review":      ratings.Review,
	}
	if sum := ratings.SumByRating(); sum != ratings.Total {
		out.Ratings.ByRating["other"] = ratings.Total - sum
	}

	if ratings.Total == 0 {
		ranTasks, rErr := db.CountDashboardRanTasks(ctx, conn)
		if rErr != nil {
			writeErrCode(w, 500, "读取任务运行统计失败", "DASHBOARD_RAN_TASKS_FAILED")
			return
		}
		out.Ratings.Note = ratingsNote(ratings.Total, ranTasks)
	}

	// ---- orders ----
	orders, err := db.CountDashboardOrders(ctx, conn)
	if err != nil {
		writeErrCode(w, 500, "读取执行单统计失败", "DASHBOARD_ORDERS_FAILED")
		return
	}
	out.Orders = dashboardOrdersDTO{Total: orders.Total, InFlight: orders.InFlight}

	// ---- pnl ----
	//
	// 2026-11-04 起真正计算盈亏（见 paper_pnl.go）。
	// 失败不阻断整个概览页 —— 盈亏是「锦上添花」的信息，
	// 而 drafts/ratings 是核心。所以这里降级为 note 而不是返回 500。
	pnl, pnlErr := s.computePaperPnl(ctx, "paper")
	if pnlErr != nil {
		out.Pnl.Note = "盈亏计算失败：" + pnlErr.Error()
	} else {
		out.Pnl.PaperOrders = pnl.FilledOrderCount
		out.Pnl.PaperPositions = pnl.PositionCount
		out.Pnl.PaperRealizedPnl = pnl.RealizedTotal
		out.Pnl.PaperUnrealizedPnl = pnl.UnrealizedTotal
		out.Pnl.PaperPricedPositions = pnl.PricedPositions
		out.Pnl.PaperTotalPositions = pnl.TotalPositions
		out.Pnl.Note = pnl.Note
	}

	// ---- 实盘盈亏（2026-11-14 补）----
	//
	// 旧版只算纸面。TradeLoop 下线后实盘成交全部来自工具下单，
	// 数据源是 trade_fills（逐笔含手续费），与任务详情页同一套算法。
	// 失败不阻断概览页：盈亏是附加信息，drafts/ratings 才是核心。
	if liveFills, fErr := s.m.pg.ListFills(db.FillsFilter{Limit: 2000}); fErr == nil {
		livePnL := exec.ComputePnLByInst(liveFills)
		var realized, fee string
		for _, p := range livePnL {
			realized = exec.DecimalAddStr(realized, p.Realized)
			fee = exec.DecimalAddStr(fee, p.FeeTotal)
		}
		out.Pnl.LiveFills = int64(len(liveFills))
		if realized == "" {
			realized = "0"
		}
		if fee == "" {
			fee = "0"
		}
		out.Pnl.LiveRealizedPnl = realized
		out.Pnl.LiveFeeTotal = fee
	} else {
		out.Pnl.LiveRealizedPnl = "0"
		out.Pnl.LiveFeeTotal = "0"
		out.Pnl.Note = strings.TrimSpace(out.Pnl.Note + " 实盘盈亏读取失败：" + fErr.Error())
	}

	writeJSON(w, 200, out)
}

// 确保 context 被使用（CountDashboard* 都接受 ctx）。
var _ = context.Background

// ratingsNote 返回「为什么没有评级」—— 只陈述事实，不含任何行动指引。
//
// # 为什么行动指引不在这里
//
// 「创建任务并跑完分析」这句话归前端：只有前端知道给不给按钮、按钮跳哪里。
// 后端只说「事实」，因为那三种事实只有后端查得出来。
//
// 判据：这个信息能从这个 API 的查询结果直接得出吗？
//   - 能（表空）           → 后端返回
//   - 不能（给不给按钮）   → 前端决定
//
// # 三种空状态
//
//	① 从未跑过分析        → 「还没有分析记录」
//	② 有任务但未产出评级  → 「已有 N 个任务运行过，但没有产出评级」
//	③ 评级全部失败        → ⚠️ 无法区分，降级为 ②（见 L26）
//
// # 为什么 ③ 降级而不是编造
//
// tasks.status 的当前值域只有 'created'，没有「失败」态；ratings 也不写失败记录。
// 所以数据库无法回答「失败还是没产出」。而编造「失败」会让用户以为
// 系统知道原因 —— 那是比「不知道」更糟的错。
func ratingsNote(totalRatings int64, ranTasks int64) string {
	if totalRatings > 0 {
		return ""
	}
	if ranTasks > 0 {
		return fmt.Sprintf("已有 %d 个任务运行过，但没有产出评级", ranTasks)
	}
	return "还没有分析记录"
}
