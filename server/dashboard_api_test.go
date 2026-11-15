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
		// 实盘（纸面模式已删除）
		LiveFills       int64  `json:"live_fills"`
		LiveRealizedPnl string `json:"live_realized_pnl"`
		LiveFeeTotal    string `json:"live_fee_total"`
		Note            string `json:"note"`
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


}

// TestDashboardSummaryEmptyStateNotes 校验「空」时给出原因。
func TestDashboardSummaryEmptyStateNotes(t *testing.T) {
	e := newExecAPIEnv(t)
	// 清空评级与纸面成交（其他测试可能留下）。
	if _, err := e.m.pg.Exec(`DELETE FROM ratings`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.pg.Exec(`DELETE FROM trade_fills`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.m.pg.Exec(`DELETE FROM ratings`)
		_, _ = e.m.pg.Exec(`DELETE FROM trade_fills`)
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

	if got.Pnl.LiveFills != 0 {
		t.Fatalf("期望 live_fills=0，实际 %d", got.Pnl.LiveFills)
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


// 盈亏字段必须成对出现 paper_ / live_ 前缀。
//
// # 为什么这条测试值得写（2026-11-15 误读事故）
//
// 原来纸面字段叫 realized_pnl / unrealized_pnl / positions，
// 而实盘叫 live_realized_pnl。命名不对称的后果是实打实的：
//
//	有人看到 realized_pnl=2766.73 以为是「系统总盈亏」—— 那是纸面的
//	数字，还含一笔现实中必然被拒的 10000 张单（名义 160 万 vs 权益
//	8.7 万）。同一时刻实盘是 -307.48。
//
// 两个数一个天上一个地下，却只有一个带 live_ 前缀。
// 这条测试钉住「不许再出现无前缀的业务字段」。
func TestDashboardPnlFieldsAreNamespaced(t *testing.T) {
	e := newExecAPIEnv(t)
	w := e.do(e.s.dashboardSummary, "GET", "/api/dashboard/summary", "", nil)
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	var raw struct {
		Pnl map[string]json.RawMessage `json:"pnl"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	// 必须存在的带前缀字段
	want := []string{"live_fills", "live_realized_pnl", "live_fee_total"}
	for _, k := range want {
		if _, ok := raw.Pnl[k]; !ok {
			t.Errorf("缺少字段 %q", k)
		}
	}

	// 业务字段不允许无前缀（note 是说明文字，不属业务指标）
	allowed := map[string]bool{"note": true}
	for _, k := range want {
		allowed[k] = true
	}
	for k := range raw.Pnl {
		if allowed[k] {
			continue
		}
		t.Errorf("字段 %q 没有 paper_/live_ 前缀 —— "+
			"纸面与实盘的钱不是一回事，光看名字就要能分清", k)
	}

	// 旧的裸字段名必须消失（否则前端可能悄悄读错那个）
	for _, gone := range []string{"positions", "realized_pnl", "unrealized_pnl",
		"priced_positions", "total_positions"} {
		if _, ok := raw.Pnl[gone]; ok {
			t.Errorf("旧字段 %q 应已移除（它的口径是纸面，但名字看起来像总账）", gone)
		}
	}
}
