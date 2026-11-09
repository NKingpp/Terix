package server

// 纸面盈亏计算的验收测试（2026-11-04）。
//
// # 为什么这些断言值得写
//
// 盈亏算错是**静默的**：数字看起来合理，但方向或量级错了，
// 用户会拿着它做真实决策。而这类错误不会报错、不抛异常。
//
// 所以每条测试都用「手算已知答案」的方式锁死：
//
//	买 100 @100 → 卖 50 @120
//	已实现 = 50 × (120 - 100) = 1000
//	净持仓 = 50，成本价 = 100
//
// 这三个数字必须精确等于手算值，任何偏差都是 bug。
//
// # 为什么测 db 层而不是只测 HTTP
//
// 金算在 SQL 里（窗口函数）。HTTP 层只做格式转换，
// 在那里断言会把「计算错」和「序列化错」混在一起，难定位。

import (
	"context"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// TestPaperPositionRealizedPnl 买卖各一笔，验证已实现盈亏与成本价。
func TestPaperPositionRealizedPnl(t *testing.T) {
	e := newExecAPIEnv(t)
	ctx := context.Background()

	if _, err := e.s.m.pg.Exec(`DELETE FROM paper_orders`); err != nil {
		t.Fatalf("清理: %v", err)
	}
	defer func() { _, _ = e.s.m.pg.Exec(`DELETE FROM paper_orders`) }()

	// 用真实标的（instruments 有外键约束）。600000 是浦发银行（cn）。
	var instID int64
	if err := e.s.m.pg.QueryRow(
		`SELECT id FROM instruments WHERE symbol='600000' LIMIT 1`).Scan(&instID); err != nil {
		t.Skipf("库里没有 600000，跳过: %v", err)
	}

	if _, err := e.s.m.pg.Exec(`
INSERT INTO paper_orders (instrument_id, mode, side, qty, price, filled_qty, avg_fill_price, status, filled_at) VALUES
  ($1,'paper','buy','100','100','100','100','filled', strftime('%Y-%m-%dT%H:%M:%fZ','now','-2 days')),
  ($1,'paper','sell','50','120','50','120','filled', strftime('%Y-%m-%dT%H:%M:%fZ','now','-1 days'))`, instID); err != nil {
		t.Fatalf("造数据: %v", err)
	}

	// ① 已实现盈亏 = 50 × (120-100) = 1000
	realized, err := dbSumRealized(ctx, e)
	if err != nil {
		t.Fatalf("算已实现: %v", err)
	}
	if realized != "1000.0000000000" && realized != "1000.00000000" {
		t.Errorf("已实现盈亏 = %q, want 1000（手算 50×(120-100)）", realized)
	}

	// ② 持仓与成本价
	positions, err := dbListPositions(ctx, e)
	if err != nil {
		t.Fatalf("算持仓: %v", err)
	}
	if len(positions) != 1 {
		t.Fatalf("持仓数 = %d, want 1", len(positions))
	}
	p := positions[0]
	if p.NetQty != "50.0000000000" && p.NetQty != "50.00000000" {
		t.Errorf("净持仓 = %q, want 50（买 100 - 卖 50）", p.NetQty)
	}
	if p.CostBasis == nil {
		t.Fatal("成本价为空 —— 有净持仓时必须有成本")
	}
	if *p.CostBasis != "100.0000000000" && *p.CostBasis != "100.00000000" {
		t.Errorf("成本价 = %q, want 100（卖出不改变剩余持仓的成本）", *p.CostBasis)
	}
}

// TestPaperPositionFullClose 全部平仓：净持仓 0，已实现盈亏保留。
//
// # 这是最容易被写错的一条
//
// 直觉实现常在「净数量为 0」时把整条记录过滤掉 ——
// 那样已实现盈亏就丢了，而它恰恰是真实的、已落袋的收益。
func TestPaperPositionFullClose(t *testing.T) {
	e := newExecAPIEnv(t)
	ctx := context.Background()

	if _, err := e.s.m.pg.Exec(`DELETE FROM paper_orders`); err != nil {
		t.Fatalf("清理: %v", err)
	}
	defer func() { _, _ = e.s.m.pg.Exec(`DELETE FROM paper_orders`) }()

	var instID int64
	if err := e.s.m.pg.QueryRow(
		`SELECT id FROM instruments WHERE symbol='600000' LIMIT 1`).Scan(&instID); err != nil {
		t.Skipf("库里没有 600000，跳过: %v", err)
	}

	// 买 100@100，卖 100@110 → 已实现 1000，净持仓 0
	if _, err := e.s.m.pg.Exec(`
INSERT INTO paper_orders (instrument_id, mode, side, qty, price, filled_qty, avg_fill_price, status, filled_at) VALUES
  ($1,'paper','buy','100','100','100','100','filled', strftime('%Y-%m-%dT%H:%M:%fZ','now','-2 days')),
  ($1,'paper','sell','100','110','100','110','filled', strftime('%Y-%m-%dT%H:%M:%fZ','now','-1 days'))`, instID); err != nil {
		t.Fatalf("造数据: %v", err)
	}

	realized, err := dbSumRealized(ctx, e)
	if err != nil {
		t.Fatalf("算已实现: %v", err)
	}
	if realized != "1000.0000000000" && realized != "1000.00000000" {
		t.Errorf("清仓后已实现盈亏 = %q, want 1000（100×(110-100)）", realized)
	}
}

// TestPaperPnlUnrealizedNilWithoutQuote 取不到现价时 unrealized 必须是 null。
//
// # 为什么这个断言重要
//
// null 与 "0" 在金融上完全不同。若实现里用 0 兜底「取不到现价」，
// 前端会显示「+0.00」—— 用户读成「有持仓但不赚不亏」，
// 而真相是「我们不知道」。那是把未知伪装成已知。
func TestPaperPnlUnrealizedNilWithoutQuote(t *testing.T) {
	e := newExecAPIEnv(t)
	ctx := context.Background()

	if _, err := e.s.m.pg.Exec(`DELETE FROM paper_orders`); err != nil {
		t.Fatalf("清理: %v", err)
	}
	defer func() { _, _ = e.s.m.pg.Exec(`DELETE FROM paper_orders`) }()

	// 用一个不存在的标的代码，保证取不到现价。
	// 但 instruments 有外键，所以先插一个假标的再插订单。
	var instID int64
	err := e.s.m.pg.QueryRow(
		`SELECT id FROM instruments WHERE symbol='ZZZNOTEXIST' LIMIT 1`).Scan(&instID)
	if err != nil {
		if err := e.s.m.pg.QueryRow(`
INSERT INTO instruments (symbol, name, asset_class, market)
VALUES ('ZZZNOTEXIST','测试假标的','stock','cn') RETURNING id`).Scan(&instID); err != nil {
			t.Skipf("无法造假标的（可能 asset_class 约束不同）: %v", err)
		}
		defer func() { _, _ = e.s.m.pg.Exec(`DELETE FROM instruments WHERE id=$1`, instID) }()
	}

	if _, err := e.s.m.pg.Exec(`
INSERT INTO paper_orders (instrument_id, mode, side, qty, price, filled_qty, avg_fill_price, status, filled_at)
VALUES ($1,'paper','buy','10','100','10','100','filled', now())`, instID); err != nil {
		t.Fatalf("造数据: %v", err)
	}

	// 把行情客户端置空 —— 模拟「行情不可用」。
	savedMarket, savedCrypto := e.s.marketCl, e.s.cryptoCl
	e.s.marketCl, e.s.cryptoCl = nil, nil
	defer func() { e.s.marketCl, e.s.cryptoCl = savedMarket, savedCrypto }()

	pnl, err := e.s.computePaperPnl(ctx, "paper")
	if err != nil {
		t.Fatalf("computePaperPnl: %v", err)
	}
	if pnl.UnrealizedTotal != nil {
		t.Errorf("取不到现价时 unrealized 必须是 nil，实际 %q", *pnl.UnrealizedTotal)
	}
	if pnl.Note == "" {
		t.Error("取不到现价时必须有 note 说明原因")
	}
	// 已实现盈亏仍应为 "0"（没卖过）—— 它不依赖现价
	if pnl.RealizedTotal != "0.0000000000" && pnl.RealizedTotal != "0" {
		t.Errorf("已实现 = %q, want 0（没有卖出）", pnl.RealizedTotal)
	}
}

// ---------------------------------------------------------------------
// 测试辅助：直接调 db 层，绕过 HTTP 序列化
// ---------------------------------------------------------------------

func dbSumRealized(ctx context.Context, e *execAPIEnv) (string, error) {
	return db.SumRealizedPnlNumeric(ctx, e.s.m.pg.DB, "paper")
}

func dbListPositions(ctx context.Context, e *execAPIEnv) ([]*db.PaperPosition, error) {
	return db.ListPaperPositions(ctx, e.s.m.pg.DB, "paper")
}
