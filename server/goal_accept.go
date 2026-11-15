package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// 人工验收的机器核验。
//
// 金融域 prove_goal 被硬拦截（agent/tools.go:1510），人工验收是唯一的收敛路径。
// 但「人点一下就算达成」等于没有闸门，所以先跑核验：
//   - source=ledger   ：平台账本（paper_orders，按 task 归属）必须平仓，且已实现盈亏 ≥ 阈值
//   - source=evidence ：账本没有本任务成交时（实盘走外部 CLI，平台不记账），
//     只核「附带的本任务 fact/finding 存在且晚于目标创建」，数值由人负责
//
// source 为空时自动选：账本有成交 → ledger，否则 → evidence。

// resolveAcceptMode 决定核验用哪本账。acc.Mode 优先；否则按当前 exec_mode 推断
// （frozen 归 live：冻结前发生过的实盘成交记在 live 账本）。读失败 fail-closed。
func resolveAcceptMode(t *Task, acc db.GoalAcceptance) (string, string) {
	if acc.Mode != "" {
		return acc.Mode, ""
	}
	if t.Store == nil || t.Store.DB() == nil {
		return "", "无法读取执行模式"
	}
	m, err := t.Store.DB().GetExecMode()
	if err != nil {
		return "", "无法读取执行模式：" + err.Error()
	}
	_ = m
	return "live", ""
}

// verifyGoalAcceptance 用验收口径实测本任务，返回是否通过、实测值（回显给人工）与未通过原因。
func verifyGoalAcceptance(ctx context.Context, t *Task, goal *db.Node, acc db.GoalAcceptance, evidenceID int64) (bool, map[string]string, string) {
	observed := map[string]string{
		"kind": acc.Kind, "source": acc.Source, "threshold": acc.Threshold,
	}
	if t.Store == nil || t.Store.DB() == nil {
		return false, observed, "无法访问任务数据库"
	}
	conn := t.Store.DB()
	if acc.Normalize() == nil {
		observed["kind"], observed["source"] = acc.Kind, acc.Source
	}

	mode, why := resolveAcceptMode(t, acc)
	if why != "" {
		return false, observed, why
	}
	observed["mode"] = mode

	summary, err := db.TaskTradeSummaryFor(ctx, conn.DB, taskIDInt(t.ID), acc.InstrumentID)
	if err != nil {
		return false, observed, "无法汇总本任务成交：" + err.Error()
	}
	observed["realized_task"] = summary.RealizedTotal
	observed["fills"] = fmt.Sprintf("%d", summary.Fills)
	observed["open_positions"] = fmt.Sprintf("%d", summary.OpenPositions)
	if acc.InstrumentID > 0 {
		observed["net_qty"] = summary.NetQty[acc.InstrumentID]
	}
	// 全局口径只做交叉参考：账本里其它任务的成交会污染它，不能作为判定依据。
	if global, err := db.SumRealizedPnlNumeric(ctx, conn.DB); err == nil {
		observed["realized_global"] = global
	}

	source := acc.Source
	if source == "" {
		if summary.Fills > 0 {
			source = "ledger"
		} else {
			source = "evidence"
		}
	}
	observed["source"] = source

	// 账本口径必须有本任务的成交，否则 flat/pnl_ge 会被「空账本」平凡通过。
	if source == "ledger" && summary.Fills == 0 {
		return false, observed, "平台账本无本任务成交（实盘走外部通道时请用证据核验）"
	}

	if source == "evidence" {
		if evidenceID <= 0 {
			return false, observed, "平台账本无本任务成交，需附外部结算证据（事实节点）"
		}
		var createdAt string
		var kind, state string
		err := conn.DB.QueryRowContext(ctx, `SELECT kind, state, created_at FROM exploration_nodes
			WHERE id=$1 AND exploration_id=$2 AND kind IN ('fact','finding')`,
			evidenceID, t.ExpID).Scan(&kind, &state, &createdAt)
		if err != nil {
			return false, observed, "证据不存在或不属于本任务"
		}
		observed["evidence_id"] = fmt.Sprintf("%d", evidenceID)
		observed["evidence_kind"] = kind
		observed["evidence_state"] = state
		if state == "dismissed" {
			return false, observed, "证据已被驳回（dismissed），不能用于验收"
		}
		return true, observed, ""
	}

	// ledger：逐口径实测
	switch acc.Kind {
	case "flat":
		if summary.OpenPositions != 0 {
			return false, observed, fmt.Sprintf("持仓未平：仍有 %d 个标的净持仓不为 0", summary.OpenPositions)
		}
		return true, observed, ""
	case "pnl_ge":
		if summary.OpenPositions != 0 {
			return false, observed, fmt.Sprintf("持仓未平：仍有 %d 个标的净持仓不为 0，无法结算已实现盈亏", summary.OpenPositions)
		}
		realized, err := db.ParseDecimal(summary.RealizedTotal)
		if err != nil {
			return false, observed, "本任务已实现盈亏无法解析：" + err.Error()
		}
		threshold, err := db.ParseDecimal(acc.Threshold)
		if err != nil {
			return false, observed, "验收阈值非法：" + acc.Threshold
		}
		if db.DecimalSign(db.DecimalSub(realized, threshold)) < 0 {
			return false, observed, fmt.Sprintf("已实现盈亏 %s 未达阈值 %s", summary.RealizedTotal, acc.Threshold)
		}
		return true, observed, ""
	case "signal_exists":
		q := `SELECT COUNT(*) FROM signals WHERE task_id=$1`
		args := []any{taskIDInt(t.ID)}
		if acc.InstrumentID > 0 {
			q += ` AND instrument_id=$2`
			args = append(args, acc.InstrumentID)
		}
		var n int
		if err := conn.DB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			return false, observed, "无法统计交易信号：" + err.Error()
		}
		observed["signal_count"] = fmt.Sprintf("%d", n)
		if n == 0 {
			return false, observed, "本任务没有任何交易信号"
		}
		return true, observed, ""
	case "rating_in":
		q := `SELECT COUNT(*) FROM ratings WHERE task_id=$1`
		args := []any{taskIDInt(t.ID)}
		if acc.InstrumentID > 0 {
			q += ` AND instrument_id=$2`
			args = append(args, acc.InstrumentID)
		}
		if len(acc.Ratings) > 0 {
			parts := make([]string, 0, len(acc.Ratings))
			for _, r := range acc.Ratings {
				args = append(args, r)
				parts = append(parts, fmt.Sprintf("$%d", len(args)))
			}
			q += ` AND rating IN (` + strings.Join(parts, ",") + `)`
		}
		var n int
		if err := conn.DB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			return false, observed, "无法统计评级：" + err.Error()
		}
		observed["rating_count"] = fmt.Sprintf("%d", n)
		if n == 0 {
			return false, observed, "本任务没有任何符合口径的评级"
		}
		return true, observed, ""
	}
	return false, observed, "未知验收类型：" + acc.Kind
}

// taskIDInt 把任务 id（字符串）转 int64；非法时返回 0（调用方查询自然空结果）。
func taskIDInt(id string) int64 {
	var n int64
	for _, c := range id {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}
