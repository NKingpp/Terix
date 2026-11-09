package db

// exec_accounts.market_support 的 CHECK 降级复核（ARCH-SQLite-Migration.md §2.8 / 任务书坑⑥）。
//
// # PG 原约束
//
//	CHECK (market_support <@ ARRAY['us','hk','cn','crypto'])
//	—— 「是这四个的子集」+ 「非空」两件事同时成立
//
// # SQLite 降级后
//
//	CHECK (market_support LIKE '%"us"%' OR … OR LIKE '%"crypto"%')
//	—— 只能表达「文本里出现过某个已知市场的引号形态」
//
// # 实测这个降级两边都错（本文件把它钉死成可回归的事实）
//
//	["crypto"]       PG 允许 → SQLite 允许          ✅ 一致
//	[]               PG 允许 → SQLite **拒绝**      ❌ 误拒（不包含任何子串）
//	["us","bogus"]   PG 拒绝 → SQLite **允许**      ❌ 漏放（只要含一个已知市场）
//
// 结论：CHECK 降级不可接受，语义搬到 UpsertAccount 的入参校验里
// （含未知市场 → ErrOrderInvalid；空数组 → ErrOrderInvalid）。

import (
	"errors"
	"testing"
)

// TestExecAccountMarketSupportRejectsUnknownMarket 未知市场必须在应用层被拒。
//
// 这条覆盖降级 CHECK 「漏放」的那一半：`["us","bogus"]` 能通过 DB 的 LIKE 检查，
// 所以拦它的只能是 Go 侧校验。
func TestExecAccountMarketSupportRejectsUnknownMarket(t *testing.T) {
	d := testOpen(t)
	store := d.ExecAccounts()
	tag := uniqueName(t, "MSUP")
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM exec_accounts WHERE channel_account_id=$1`, tag)
	})

	if _, err := store.UpsertAccount(ExecAccountInput{
		Channel: "okx", ChannelAcctID: tag, Market: "crypto",
		MarketSupport: []string{"us", "bogus"}, Active: true,
	}); !errors.Is(err, ErrOrderInvalid) {
		t.Fatalf("含未知市场的 market_support 应被拒（ErrOrderInvalid），实际 %v", err)
	}

	// 确认那一行**没有**落库：校验必须在写之前。
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM exec_accounts WHERE channel_account_id=$1`, tag).Scan(&n); err != nil {
		t.Fatalf("查账户: %v", err)
	}
	if n != 0 {
		t.Fatalf("校验失败的账户不该落库，实际 %d 行", n)
	}
}

// TestExecAccountMarketSupportRejectsEmpty 空数组在应用层被拒。
//
// 这条覆盖降级 CHECK 「误拒」的那一半：DB 会拒绝 `[]`（不包含任何子串），
// 但那**不是**正确语义（PG 允许空数组）。应用层给出明确错误，
// 让调用方知道是「API 要求至少一个市场」而不是「数据库抽风」。
func TestExecAccountMarketSupportRejectsEmpty(t *testing.T) {
	d := testOpen(t)
	store := d.ExecAccounts()
	tag := uniqueName(t, "MSUP0")

	if _, err := store.UpsertAccount(ExecAccountInput{
		Channel: "okx", ChannelAcctID: tag, Market: "crypto",
		MarketSupport: nil, Active: true,
	}); !errors.Is(err, ErrOrderInvalid) {
		t.Fatalf("空的 market_support 应被拒（ErrOrderInvalid），实际 %v", err)
	}
}

// TestExecAccountMarketSupportRoundTrip 合法值能存能读，且顺序/去重稳定。
func TestExecAccountMarketSupportRoundTrip(t *testing.T) {
	d := testOpen(t)
	store := d.ExecAccounts()
	tag := uniqueName(t, "MSUPOK")
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM exec_accounts WHERE channel_account_id=$1`, tag)
	})

	acc, err := store.UpsertAccount(ExecAccountInput{
		Channel: "okx", ChannelAcctID: tag, Market: "crypto",
		// 带空白与重复，验证 normalizeMarketSupport 的行为。
		MarketSupport: []string{" crypto ", "us", "crypto"}, Active: true,
	})
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	want := []string{"crypto", "us"}
	if len(acc.MarketSupport) != len(want) {
		t.Fatalf("market_support = %v, want %v（去重 + 去空白）", acc.MarketSupport, want)
	}
	for i := range want {
		if acc.MarketSupport[i] != want[i] {
			t.Fatalf("market_support = %v, want %v（保持输入顺序）", acc.MarketSupport, want)
		}
	}
}
