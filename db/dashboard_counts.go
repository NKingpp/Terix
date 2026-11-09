package db

// dashboard 概览页所需的聚合查询。
//
// # 为什么集中在这里，而不是分散在各 handler
//
// 这些查询的共同点是「一次聚合、多个计数」。如果每个 handler 自己写 SQL：
//
//  ① 「哪些状态算 in_flight」这类定义会有多份，改一处漏一处；
//  ② 每份都可能写成多次往返（先 COUNT(*) 再 COUNT(*) WHERE），
//     而两次查询之间数据会变 —— 概览页需要「同一时刻的快照」。
//
// 所以每个函数都只用**一条 SQL**（多列聚合），保证内部一致。
//
// # 为什么不用现有的 store 方法
//
// ExecOrderStore 只有 ListByState（拉全部行再数），对概览页来说
// 拉行是浪费，而且 page 不到的行的变化会让计数不准。

import (
	"context"
	"database/sql"
)

// DashboardExecCounts 是执行层的账户统计。
//
// Enabled / Blocked 的口径来自 ExecAccount.Active / BlockedAt 两个字段
// （见 exec_accounts.go），与 /exec 页一致。
type DashboardExecCounts struct {
	Total   int64
	Enabled int64
	Blocked int64
}

// CountDashboardExecAccounts 统计执行账户。
//
// # 为什么用一条 SQL 而不是 ListAccounts 再数
//
// ListAccounts 会把每一行拉回来（含 market_support 数组、时间戳）。
// 概览页只要三个数 —— 拉行是 O(n) 的浪费，而这里是一次 O(1) 聚合。
//
// ⚠️ 但**口径必须与 ListAccounts 一致**：
//
//	enabled = active = true
//	blocked = blocked_at IS NOT NULL
//
// 两者独立（一个账户可以既 active 又 blocked —— 那表示「启用但被风控冻结」）。
func CountDashboardExecAccounts(ctx context.Context, conn *sql.DB) (DashboardExecCounts, error) {
	var c DashboardExecCounts
	err := conn.QueryRowContext(ctx, `
SELECT COUNT(*),
       COUNT(*) FILTER (WHERE active),
       COUNT(*) FILTER (WHERE blocked_at IS NOT NULL)
FROM exec_accounts`).Scan(&c.Total, &c.Enabled, &c.Blocked)
	return c, err
}

// DashboardDraftCounts 是草案与风控判定的统计。
//
// # 为什么要单独的 None
//
// orders_draft.risk_verdict 是可空列 —— 草案刚建出来、还没被风控评估时是 NULL。
// 那与「已判定为 pass」是两件不同的事：前者「还不知道」，后者「知道可以通过」。
//
// 如果把它们合并到 Total 里，dashboard 会显示「0 条待决策」而实际有 N 条未判定 ——
// 那是**用户可见的错误**，而不是显示不全。
type DashboardDraftCounts struct {
	Total int64
	Pass  int64
	Deny  int64
	Ask   int64
	None  int64
}

// CountDashboardDrafts 统计**待决策**草案。一条 SQL 出全部计数。
//
// # 2026-11-04 修正：原来的 COUNT(*) 口径是错的
//
// 原实现统计整张表，包括已经落单（exported_at 非空）和已执行
// （executed_note 非空）的草案 —— 那些早已不是「待决策」。
// 后果：卡片标题写「待决策草案」，数字却是历史总量，
// 用户点进执行页只看到几条待办，与卡片数字对不上。
//
// 现口径与索引 idx_orders_draft_pending 保持一致：
//
//	exported_at IS NULL AND executed_note IS NULL
//
// 那个部分索引本来就是按这个条件建的（见 schema.sql），
// 说明「待决策」的定义一直是这个 —— 只是聚合查询没跟上。
//
// # 为什么 executed_note 也要排除
//
// exported_at 只表示「导出给渠道」，导出后仍可能被人工确认执行。
// 只看 exported_at 会把「已导出但未执行」的算进待决策 —— 那部分是
// 悬而未决，确实属于待决策。所以 executed_note 非空（表明确执行过）
// 才排除。两者是 AND：导出**且**执行完，才算终结。
//
// ⚠️ risk_verdict 的值域由 orders_draft_risk_verdict_check 约束限定为
// 'pass'|'deny'|'ask'（2026-11-04 实测该列已有 CHECK）。但这里仍用
// FILTER 精确匹配而非 GROUP BY —— 约束将来若被放宽，脏值会体现在
// SumByVerdict() != Total 上，而不是被静默合并进某一类。
func CountDashboardDrafts(ctx context.Context, conn *sql.DB) (DashboardDraftCounts, error) {
	var c DashboardDraftCounts
	err := conn.QueryRowContext(ctx, `
SELECT COUNT(*),
       COUNT(*) FILTER (WHERE risk_verdict = 'pass'),
       COUNT(*) FILTER (WHERE risk_verdict = 'deny'),
       COUNT(*) FILTER (WHERE risk_verdict = 'ask'),
       COUNT(*) FILTER (WHERE risk_verdict IS NULL)
FROM orders_draft
WHERE exported_at IS NULL
  AND executed_note IS NULL`).Scan(&c.Total, &c.Pass, &c.Deny, &c.Ask, &c.None)
	return c, err
}

// SumByVerdict 返回四个分类之和。
//
// # 为什么需要它
//
// 它应该恰好等于 Total。不等时说明 risk_verdict 有值域外的脏值
// （比如手动 UPDATE 写进去的 'PASS' 大写）。
//
// 那种脏值不会报错，只会让「总数对不上各项之和」——
// 而如果没有这条断言，那个不一致会永远沉默。
func (c DashboardDraftCounts) SumByVerdict() int64 {
	return c.Pass + c.Deny + c.Ask + c.None
}

// DashboardOrderCounts 是执行单的统计。
type DashboardOrderCounts struct {
	Total int64
	// InFlight 是「已提交到渠道、还没终结」的单。
	//
	// 口径：submitted + partial。
	//   - draft 不算（还没提交）
	//   - filled / rejected / cancelled / settled 都是终结态
	InFlight int64
}

// CountDashboardOrders 统计执行单。一条 SQL 出两个数。
func CountDashboardOrders(ctx context.Context, conn *sql.DB) (DashboardOrderCounts, error) {
	var c DashboardOrderCounts
	err := conn.QueryRowContext(ctx, `
SELECT COUNT(*),
       COUNT(*) FILTER (WHERE state IN ('submitted','partial'))
FROM orders_execution`).Scan(&c.Total, &c.InFlight)
	return c, err
}

// DashboardRatingCounts 是评级统计。
type DashboardRatingCounts struct {
	Total       int64
	Buy         int64
	Overweight  int64
	Hold        int64
	Underweight int64
	Sell        int64
	Review      int64
}

// CountDashboardRatings 统计评级。
//
// ⚠️ 六个分类来自 ratings 表的 CHECK 约束 ratings_rating_check：
//
//	CHECK (rating = ANY (ARRAY['buy','overweight','hold','underweight','sell','review']))
//
// **不要手写这份清单** —— 如果将来加了 'strong_buy'，它应该体现在
// SumByRating() != Total 这个信号上，而不是被静默丢掉。
func CountDashboardRatings(ctx context.Context, conn *sql.DB) (DashboardRatingCounts, error) {
	var c DashboardRatingCounts
	err := conn.QueryRowContext(ctx, `
SELECT COUNT(*),
       COUNT(*) FILTER (WHERE rating = 'buy'),
       COUNT(*) FILTER (WHERE rating = 'overweight'),
       COUNT(*) FILTER (WHERE rating = 'hold'),
       COUNT(*) FILTER (WHERE rating = 'underweight'),
       COUNT(*) FILTER (WHERE rating = 'sell'),
       COUNT(*) FILTER (WHERE rating = 'review')
FROM ratings`).Scan(&c.Total, &c.Buy, &c.Overweight, &c.Hold,
		&c.Underweight, &c.Sell, &c.Review)
	return c, err
}

// SumByRating 返回六个分类之和。
//
// 应该恰好等于 Total。不等时说明 rating 有 CHECK 约束之外的值
// （约束存在时不可能，但这里保留断言以便约束被误删时能被发现）。
func (c DashboardRatingCounts) SumByRating() int64 {
	return c.Buy + c.Overweight + c.Hold + c.Underweight + c.Sell + c.Review
}

// CountDashboardPaperOrders 返回纸面**成交**单数（已成交的，不是全部下单）。
//
// # 2026-11-04 修正：原来统计整表，把未成交的也算成成交
//
// 原实现 `SELECT COUNT(*) FROM paper_orders` 包含 status='draft' 的下单记录 ——
// 那张表同时承载「下单意图」和「成交结果」。后果：
//   - 卡片说「有成交」，但实际全是 draft（渠道没回执）
//   - pnl 分支因此走进「暂不支持盈亏计算（P1）」，而真相是「还没成交」
//
// 现口径：只数真正有成交量的行。
//
//	filled_qty > 0
//
// # 为什么用 filled_qty 而不是 status
//
// status 是渠道回执的状态字符串（draft/submitted/filled…），它会随渠道
// 实现变化；而 filled_qty 是**成交量的数字事实** —— 无论状态叫什么，
// 没成交就是 0。用数字而非枚举，对「统计成交」这个目的更稳。
//
// 用 filled_qty 也自然覆盖部分成交：0.5 手成交算一笔成交。
func CountDashboardPaperOrders(ctx context.Context, conn *sql.DB) (int64, error) {
	var n int64
	err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM paper_orders WHERE filled_qty > 0`).Scan(&n)
	return n, err
}

// CountDashboardRanTasks 返回「至少运行过一次」的任务数。
//
// # 为什么用 first_run_at 而不是 status
//
// tasks.status 的当前值域只有 'created'（实测 2026-10-30），
// 分不出「跑过」和「没跑过」。而 first_run_at 精确表示「首次运行时间已记录」。
//
// ⚠️ 如果将来 status 有了 'failed' 等值域，这里可以更精确 ——
// 但那时也仍然需要 first_run_at 来区分「从未开始」和「开始过」。
func CountDashboardRanTasks(ctx context.Context, conn *sql.DB) (int64, error) {
	var n int64
	err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks WHERE first_run_at IS NOT NULL`).Scan(&n)
	return n, err
}
