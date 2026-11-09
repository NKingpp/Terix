package server

// GET /api/dashboard/summary 的契约测试。
//
// # 为什么这些断言值得写
//
// 这个接口的风险不是「算错」（那些 SQL 很简单），而是：
//  ① 枚举键漏一个（by_rating 少 overweight）→ 静默丢弃，total != sum
//  ② 空值不区分（「没成交」和「没算盈亏」混为一谈）→ 前端无法给出正确引导
//  ③ 返回 0 而不说原因 → 用户以为功能坏了
//
// 三条都不会报错，只会让 dashboard 显示误导性的数字。

import (
	"encoding/json"
	"testing"
)

type dashboardResp struct {
	Exec struct {
		Mode     string `json:"mode"`
		Frozen   bool   `json:"frozen"`
		Accounts struct {
			Total   int64 `json:"total"`
			Enabled int64 `json:"enabled"`
			Blocked int64 `json:"blocked"`
		} `json:"accounts"`
	} `json:"exec"`
	Drafts struct {
		Total     int64            `json:"total"`
		ByVerdict map[string]int64 `json:"by_verdict"`
	} `json:"drafts"`
	Ratings struct {
		Total    int64            `json:"total"`
		ByRating map[string]int64 `json:"by_rating"`
		Note     string           `json:"note"`
	} `json:"ratings"`
	Orders struct {
		Total    int64 `json:"total"`
		InFlight int64 `json:"in_flight"`
	} `json:"orders"`
	Pnl struct {
		Positions       int64   `json:"positions"`
		RealizedPnl     string  `json:"realized_pnl"`
		UnrealizedPnl   *string `json:"unrealized_pnl"`
		PricedPositions int64   `json:"priced_positions"`
		TotalPositions  int64   `json:"total_positions"`
		PaperOrders     int64   `json:"paper_orders"`
		Note            string  `json:"note"`
	} `json:"pnl"`
}

// TestDashboardSummaryShape 校验返回结构与字段完整性。
func TestDashboardSummaryShape(t *testing.T) {
	e := newExecAPIEnv(t)
	w := e.do(e.s.dashboardSummary, "GET", "/api/dashboard/summary", "", nil)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got dashboardResp
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败: %v\nbody=%s", err, w.Body.String())
	}

	// ① by_rating 必须有六个键。
	//
	// 这条断言的价值：如果有人按「5 个值」写死了前端或后端，
	// overweight 会被静默丢弃 —— 而这个测试会立刻红。
	for _, k := range []string{"buy", "overweight", "hold", "underweight", "sell", "review"} {
		if _, ok := got.Ratings.ByRating[k]; !ok {
			t.Errorf("by_rating 缺键 %q（值域来自 ratings_rating_check 的 6 个值）", k)
		}
	}

	// ② by_verdict 必须有四个键（pass/deny/ask/none）。
	//
	// none 表示「草案还没被风控判定」（risk_verdict IS NULL）——
	// 它与「判定为 pass」是两件不同的事。
	for _, k := range []string{"pass", "deny", "ask", "none"} {
		if _, ok := got.Drafts.ByVerdict[k]; !ok {
			t.Errorf("by_verdict 缺键 %q", k)
		}
	}

	// ③ mode 必须非空（DB 有默认值 paper，不该是空串）。
	if got.Exec.Mode == "" {
		t.Error("exec.mode 为空")
	}

	// ④ pnl.note 必须非空 —— 它是「为什么没有盈亏数字」的唯一说明。
	if got.Pnl.Note == "" {
		t.Error("pnl.note 为空 —— 前端将无法区分「没成交」与「没算」")
	}

}

// TestDashboardSummaryEmptyStateNotes 校验「空」时给出原因。
func TestDashboardSummaryEmptyStateNotes(t *testing.T) {
	e := newExecAPIEnv(t)
	// 清空评级与纸面成交（其他测试可能留下）。
	if _, err := e.m.pg.Exec(`DELETE FROM ratings`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.pg.Exec(`DELETE FROM paper_orders`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.m.pg.Exec(`DELETE FROM ratings`)
		_, _ = e.m.pg.Exec(`DELETE FROM paper_orders`)
	})

	w := e.do(e.s.dashboardSummary, "GET", "/api/dashboard/summary", "", nil)
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	var got dashboardResp
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	if got.Ratings.Total != 0 {
		t.Fatalf("期望 ratings.total=0，实际 %d", got.Ratings.Total)
	}
	// 空评级必须带 note（引导用户去创建任务），而不是静默返回 0。
	if got.Ratings.Note == "" {
		t.Error("ratings.total=0 时 ratings.note 必须说明原因")
	}

	if got.Pnl.PaperOrders != 0 {
		t.Fatalf("期望 paper_orders=0，实际 %d", got.Pnl.PaperOrders)
	}
	if got.Pnl.Note != "尚未产生纸面成交" {
		t.Errorf("paper_orders=0 时 note 应为「尚未产生纸面成交」，实际 %q", got.Pnl.Note)
	}
}

// TestDashboardSummaryRatingDistribution 校验分布与总数一致。
func TestDashboardSummaryRatingDistribution(t *testing.T) {
	e := newExecAPIEnv(t)
	_, _ = e.m.pg.Exec(`DELETE FROM ratings`)
	t.Cleanup(func() { _, _ = e.m.pg.Exec(`DELETE FROM ratings`) })

	// 插 3 条：buy / overweight / review
	// review 需要 review_reasons（CHECK 约束要求），所以给全四个键。
	for _, q := range []string{
		`INSERT INTO ratings(rating, confidence) VALUES('buy', 0.8)`,
		`INSERT INTO ratings(rating, confidence) VALUES('overweight', 0.65)`,
		`INSERT INTO ratings(rating, confidence, review_reasons)
		 VALUES('review', 0.4, '{"data_gaps":[],"conflicting_facts":[],"skipped_analysts":[],"next_step":"人工复核"}')`,
	} {
		if _, err := e.m.pg.Exec(q); err != nil {
			t.Fatalf("插入评级失败: %v\nSQL=%s", err, q)
		}
	}

	w := e.do(e.s.dashboardSummary, "GET", "/api/dashboard/summary", "", nil)
	var got dashboardResp
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	if got.Ratings.Total != 3 {
		t.Fatalf("ratings.total=%d, want 3", got.Ratings.Total)
	}
	if got.Ratings.ByRating["buy"] != 1 {
		t.Errorf("buy=%d, want 1", got.Ratings.ByRating["buy"])
	}
	// ⚠️ 这条是核心：如果后端漏了 overweight 分支，它会留在 0 里而不报错。
	if got.Ratings.ByRating["overweight"] != 1 {
		t.Errorf("overweight=%d, want 1（漏这个分支时它会静默丢弃）",
			got.Ratings.ByRating["overweight"])
	}
	if got.Ratings.ByRating["review"] != 1 {
		t.Errorf("review=%d, want 1", got.Ratings.ByRating["review"])
	}

	// 分布之和必须等于 total —— 不等说明有值域外的值被丢了。
	var sum int64
	for k, v := range got.Ratings.ByRating {
		if k == "other" {
			continue
		}
		sum += v
	}
	if sum+got.Ratings.ByRating["other"] != got.Ratings.Total {
		t.Errorf("sum(by_rating)=%d, total=%d —— 有值被静默丢弃", sum, got.Ratings.Total)
	}
}

// TestDashboardDraftsCountOnlyPending 「待决策草案」只数未终结的。
//
// # 2026-11-04 修正的回归保护
//
// 原实现 COUNT(*) 整表 —— 已导出（exported_at 非空）和已执行
// （executed_note 非空）的草案也被算进「待决策」。卡片标题承诺的是
// 「还没拍板的」，数字却是历史总量。
//
// 这条测试造 4 条草案：2 条真待决策、1 条已导出、1 条已执行；
// 断言 total == 2。若有人改回 COUNT(*)，这里立刻红。
func TestDashboardDraftsCountOnlyPending(t *testing.T) {
	e := newExecAPIEnv(t)

	// 清掉历史残留，保证可重复运行。
	if _, err := e.s.m.pg.Exec(`DELETE FROM orders_draft`); err != nil {
		t.Fatalf("清理: %v", err)
	}
	defer func() { _, _ = e.s.m.pg.Exec(`DELETE FROM orders_draft`) }()

	if _, err := e.s.m.pg.Exec(`
INSERT INTO orders_draft (side, qty, ref_price, risk_verdict, exported_at, executed_note) VALUES
  ('buy','1','100','pass', NULL,   NULL),
  ('buy','1','100',NULL,   NULL,   NULL),
  ('sell','1','100','ask',  now(), NULL),
  ('buy','1','100','pass', now(),  'executed')`); err != nil {
		t.Fatalf("造数据: %v", err)
	}

	w := e.do(e.s.dashboardSummary, "GET", "/api/dashboard/summary", "", nil)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got dashboardResp
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析: %v", err)
	}

	if got.Drafts.Total != 2 {
		t.Errorf("待决策草案 = %d, want 2（已导出/已执行的不该算进来）", got.Drafts.Total)
	}
	// 分类之和必须等于 total —— 否则分布图与总数会互相矛盾。
	var sum int64
	for _, v := range got.Drafts.ByVerdict {
		sum += v
	}
	if sum != got.Drafts.Total {
		t.Errorf("by_verdict 之和 = %d, total = %d —— 分布与总数不一致", sum, got.Drafts.Total)
	}
}

// TestDashboardPaperOrdersCountOnlyFilled 纸面成交数只数真正成交的。
//
// # 2026-11-04 修正的回归保护
//
// 原实现 COUNT(*) FROM paper_orders —— 把 status='draft'（还没成交）
// 的下单意图也算成「有成交」，导致 pnl 走进「有成交」分支。
//
// paper_orders 同时承载「下单意图」与「成交结果」，所以必须按
// filled_qty 筛。这条测试造 3 条：2 条未成交 + 1 条成交。
func TestDashboardPaperOrdersCountOnlyFilled(t *testing.T) {
	e := newExecAPIEnv(t)

	if _, err := e.s.m.pg.Exec(`DELETE FROM paper_orders`); err != nil {
		t.Fatalf("清理: %v", err)
	}
	defer func() { _, _ = e.s.m.pg.Exec(`DELETE FROM paper_orders`) }()

	// ⚠️ 必须给 instrument_id：持仓聚合按标的关联，
	// 而 instrument_id 为 NULL 的行会被排除（取不到现价，算不出盈亏）。
	var instID int64
	if err := e.s.m.pg.QueryRow(
		`SELECT id FROM instruments WHERE symbol='600000' LIMIT 1`).Scan(&instID); err != nil {
		t.Skipf("库里没有 600000，跳过: %v", err)
	}

	if _, err := e.s.m.pg.Exec(`
INSERT INTO paper_orders (instrument_id, mode, side, qty, price, filled_qty, avg_fill_price, status) VALUES
  ($1,'paper','buy','1','100','0', NULL,   'draft'),
  ($1,'paper','buy','1','100','0', NULL,   'submitted'),
  ($1,'paper','buy','1','100','1', '100.5','filled')`, instID); err != nil {
		t.Fatalf("造数据: %v", err)
	}

	w := e.do(e.s.dashboardSummary, "GET", "/api/dashboard/summary", "", nil)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got dashboardResp
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析: %v", err)
	}

	if got.Pnl.PaperOrders != 1 {
		t.Errorf("纸面成交数 = %d, want 1（未成交的下单意图不该计入）；note=%q",
			got.Pnl.PaperOrders, got.Pnl.Note)
	}
	// 有成交时 note 必须说明「为什么还是没有盈亏数字」——
	// 只说「暂不支持」会让用户以为功能坏了，而真相是缺字段。
	if got.Pnl.Note == "" {
		t.Error("pnl.note 为空")
	}
}
