package exec

// 闸门测试的共享辅助。
//
// # 为什么测试辅助单独一个文件
//
// gate_test.go / gate_bypass_test.go / gate_e2e_test.go 三个文件共用同一套
// 建数据的方法。放在哪个测试文件里都会让其他两个「依赖于某个测试文件的内部细节」。
// 单独成文件后，它是三个测试文件的共同基础设施。

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// gateTestEnv 打开测试库并构造一个闸门。返回的 done 用于清理。
func gateTestEnv(t *testing.T) (*Gate, func()) {
	t.Helper()
	d := execTestDB(t) // 复用 db 包的包级共享实例（见 db/exec_store_test.go）
	g := NewGate(d, nil)
	// 时钟固定到 UTC 正午，避免测试在日界线附近跑时结果漂移
	// 时钟固定到 UTC 03:00 = 北京 11:00（A 股上午时段内），
	// 也是美股前一日 23:00（非交易时段）。
	//
	// # 为什么不选 UTC 12:00
	//
	// UTC 12:00 = 北京 20:00（A 股已收盘）→ 所有 A 股测试都会命中
	// 「[内置] 草案-交易时段」被 deny，测不到我要验的分支。
	//
	// UTC 03:00 同时满足：A 股在时段内、加密 24h 在时段内。
	// 美股测试单独处理（见 disableSessionRule）。
	g.WithClock(func() time.Time {
		return time.Date(2026, 10, 27, 3, 0, 0, 0, time.UTC)
	})
	return g, func() {}
}

// modeOf 把字符串转成 db.ExecMode。
func modeOf(s string) db.ExecMode { return db.ExecMode(s) }

func paperMode() db.ExecMode         { return db.ExecModePaper }
func frozenMode() db.ExecMode        { return db.ExecModeFrozen }
func liveManualMode() db.ExecMode    { return db.ExecModeLiveManual }
func liveWhitelistMode() db.ExecMode { return db.ExecModeLiveWhitelist }

// newGateTestAccount 建一个可用的执行账户。
func newGateTestAccount(t *testing.T, g *Gate, tag string) *db.ExecAccount {
	t.Helper()
	uniq := tag + "-" + t.Name() + "-" + time.Now().Format("150405.000000")
	a, err := g.db.ExecAccounts().UpsertAccount(db.ExecAccountInput{
		Channel:       "okx",
		ChannelAcctID: uniq,
		Label:         "闸门测试账户",
		Market:        "crypto",
		MarketSupport: []string{"crypto"},
		Active:        true,
	})
	if err != nil {
		t.Fatalf("建账户失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = g.db.Exec(`DELETE FROM exec_reconciliations WHERE account_id=$1`, a.ID)
		_, _ = g.db.Exec(`DELETE FROM orders_execution WHERE account_id=$1`, a.ID)
		_, _ = g.db.Exec(`DELETE FROM exec_accounts WHERE id=$1`, a.ID)
	})
	return a
}

// newGateTestInstrument 建一个标的（若同 market+symbol 已存在则复用）。
func newGateTestInstrument(t *testing.T, g *Gate, market, symbol, assetClass string) *db.Instrument {
	t.Helper()
	if inst, err := g.db.Instruments().GetInstrumentByMarketSymbol(market, symbol); err == nil {
		return inst
	}
	id, _, err := g.db.Instruments().UpsertInstrument(db.InstrumentInput{
		Market:     market,
		Symbol:     symbol,
		AssetClass: assetClass,
	})
	if err != nil {
		t.Fatalf("建标的失败: %v", err)
	}
	inst, err := g.db.Instruments().GetInstrumentByID(id)
	if err != nil {
		t.Fatalf("读回标的失败: %v", err)
	}
	return inst
}

// newGateTestDraft 建一条草案（美股 / BTC，视 market 而定）。
func newGateTestDraft(t *testing.T, g *Gate, market string) int64 {
	t.Helper()
	// # 标的必须在内置白名单规则的允许名单里
	//
	// db 的 seed 会建 5 条内置草案规则（见 intercept_rules），其中
	// 「[内置] 草案-标的允许名单」的 values 是 ["SPY","QQQ","AAPL","BTC-USDT"]。
	// 用其他符号会命中它并被 deny —— 那测的就不是我要测的东西。
	//
	// A 股没有内置白名单标的，所以 cn 用一条临时加入白名单的符号
	// （见 ensureAshareWhitelisted）。
	symbol := "BTC-USDT"
	assetClass := "crypto"
	if market == "cn" {
		symbol = "600000"
		assetClass = "stock"
		ensureAshareWhitelisted(t, g)
	} else if market == "us" {
		symbol = "AAPL"
		assetClass = "stock"
	}
	inst := newGateTestInstrument(t, g, market, symbol, assetClass)

	price := "100"
	id, err := g.db.OrderDrafts().InsertDraft(db.OrderDraftInput{
		InstrumentID: &inst.ID,
		Side:         "buy",
		Qty:          "1",
		RefPrice:     price,
	})
	if err != nil {
		t.Fatalf("建草案失败: %v", err)
	}
	// cleanup 委托给 trackGateTestDraft —— 避免两处维护同一套删除顺序。
	//
	// # 为什么不让这里自己写三行 DELETE
	//
	// 原先这里和 trackGateTestDraft 各写一套，而**两套都不完整**（都漏了
	// exec_reconciliations）。如果将来外键链又变了、只改一处，另一处就继续静默失效。
	//
	// 单一来源：删除顺序只在 trackGateTestDraft 里定义一次。
	return trackGateTestDraft(t, g, id)
}

// newGateTestDraftWithPrice 建一条指定价格的草案。
func newGateTestDraftWithPrice(t *testing.T, g *Gate, market, qty, refPrice string) int64 {
	t.Helper()
	id := newGateTestDraft(t, g, market)
	// 直接 UPDATE 改 qty/ref_price（InsertDraft 已建好行）
	if refPrice == "" {
		if _, err := g.db.Exec(`UPDATE orders_draft SET qty=$2, ref_price=NULL WHERE id=$1`, id, qty); err != nil {
			t.Fatalf("改草案失败: %v", err)
		}
	} else {
		if _, err := g.db.Exec(`UPDATE orders_draft SET qty=$2, ref_price=$3 WHERE id=$1`, id, qty, refPrice); err != nil {
			t.Fatalf("改草案失败: %v", err)
		}
	}
	return id
}

// ensureAshareWhitelisted 把测试用 A 股符号加进内置白名单规则的 values。
//
// # 为什么必须这么做
//
// 内置规则「[内置] 草案-标的允许名单」的 values 只有美股/加密货币，
// 没有 A 股。不加入的话，所有 A 股测试都会在规则层被 deny，
// 测不到「A股 + live_whitelist」这条我要验的分支。
//
// 用完还原（cleanup），避免影响其他测试。
func ensureAshareWhitelisted(t *testing.T, g *Gate) {
	t.Helper()
	var params []byte
	err := g.db.QueryRow(`SELECT pattern_params FROM intercept_rules
WHERE name='[内置] 草案-标的允许名单'`).Scan(&params)
	if err != nil {
		t.Fatalf("读内置白名单规则失败: %v", err)
	}
	// 已经包含则跳过
	if strings.Contains(string(params), `"600000"`) {
		return
	}
	orig := string(params)
	// # 为什么改成 JSON 解析而不是字符串替换
	//
	// 原来的 `strings.Replace(orig, `"values": [`, …)` 依赖「冒号后有一个空格」
	// 这个具体排版 —— 而 SQLite 迁移后 seed 写入的是紧凑 JSON
	// （`{"mode":"include",…,"values":[…]}`），替换会静默不生效然后 t.Fatal。
	//
	// JSON 解析对排版完全不敏感，也顺带保证写回去的仍是合法 JSON。
	var spec struct {
		Mode   string   `json:"mode"`
		Field  string   `json:"field"`
		Values []string `json:"values"`
	}
	if err := json.Unmarshal(params, &spec); err != nil {
		t.Fatalf("解析 [内置] 草案-标的允许名单 的 pattern_params 失败: %v（原文 %s）", err, orig)
	}
	spec.Values = append(spec.Values, "600000")
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("序列化白名单规则失败: %v", err)
	}
	updated := string(raw)
	if _, err := g.db.Exec(`UPDATE intercept_rules SET pattern_params=$1
WHERE name='[内置] 草案-标的允许名单'`, updated); err != nil {
		t.Fatalf("更新白名单失败: %v", err)
	}
	g.rules.Invalidate()
	t.Cleanup(func() {
		_, _ = g.db.Exec(`UPDATE intercept_rules SET pattern_params=$1
WHERE name='[内置] 草案-标的允许名单'`, orig)
		g.rules.Invalidate()
	})
}

// disableSessionRule 临时停用「交易时段」内置规则。
//
// # 为什么需要它
//
// 测试时钟固定在 UTC 03:00（为了让 A 股落在时段内）。而美股在那一刻
// 是前一日 23:00（收盘后），会命中时段规则被 deny。
//
// 美股相关的测试如果不关心时段，就把它停掉 —— 否则测到的是时段规则，
// 不是我要验的那条分支。
//
// 顺带：这条规则被停用后**不进指纹**（指纹只算 enabled 的），
// 所以不会影响规则变更测试。
func disableSessionRule(t *testing.T, g *Gate) {
	t.Helper()
	var id int64
	err := g.db.QueryRow(`SELECT id FROM intercept_rules
WHERE name='[内置] 草案-交易时段'`).Scan(&id)
	if err != nil {
		t.Fatalf("找不到交易时段规则: %v", err)
	}
	if _, err := g.db.Exec(`UPDATE intercept_rules SET enabled=false WHERE id=$1`, id); err != nil {
		t.Fatalf("停用时段规则失败: %v", err)
	}
	g.rules.Invalidate()
	t.Cleanup(func() {
		_, _ = g.db.Exec(`UPDATE intercept_rules SET enabled=true WHERE id=$1`, id)
		g.rules.Invalidate()
	})
}

// setLimits 写一组限额。
func setLimits(t *testing.T, g *Gate, single, daily string, dailyCount int64) {
	t.Helper()
	_, err := g.db.ExecutionLimits().UpsertLimits(db.ExecutionLimitsInput{
		Key: "global",
		// Mode 是 NOT NULL 且有 CHECK 约束（paper/live_whitelist/live_manual）。
		// 这一列已标记 DEPRECATED，但约束还在，所以必须给值。
		Mode:            "live_manual",
		MaxSingleAmount: single,
		MaxDailyAmount:  daily,
		MaxDailyOrders:  &dailyCount,
	})
	if err != nil {
		t.Fatalf("写限额失败: %v", err)
	}
	t.Cleanup(func() { _, _ = g.db.Exec(`DELETE FROM execution_limits WHERE key='global'`) })
}

// ensureAllowPassRules 确保有能放行的草案规则。
//
// # 为什么必须建规则
//
// intercept.EvaluateDraft 在「draft_field 规则 0 条」时返回 deny
// （fail-closed，见 intercept.go:822）。所以测试放行路径必须先有规则。
//
// 这里建一条「永不命中」的规则（numeric_lte 且上限极大）——
// 它存在（满足「规则集非空」），但不会拦任何单。
func ensureAllowPassRules(t *testing.T, g *Gate) {
	t.Helper()
	rules, err := g.db.ListInterceptRules()
	if err != nil {
		t.Fatalf("读规则失败: %v", err)
	}
	for _, r := range rules {
		if r.MatchTarget == "draft_field" && r.Enabled {
			return // 已有草案规则，不必新建
		}
	}
	// 建一条不会命中的规则：单笔金额上限极大
	_, err = g.db.CreateInterceptRule(
		"闸门测试-永不命中", "draft_field", "numeric_lte",
		"999999999999", "deny", "测试规则（不应命中）", 0, true, false, 60, "deny",
	)
	if err != nil {
		t.Fatalf("建测试规则失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = g.db.Exec(`DELETE FROM intercept_rules WHERE name='闸门测试-永不命中'`)
	})
}

// ensureDenyRule 建一条必然命中的 deny 规则。
func ensureDenyRule(t *testing.T, g *Gate) {
	t.Helper()
	_, err := g.db.CreateInterceptRule(
		"闸门测试-必然拒绝", "draft_field", "numeric_lte",
		"0", "deny", "测试：金额上限为 0，必然拒绝", 100, true, false, 60, "deny",
	)
	if err != nil {
		t.Fatalf("建 deny 规则失败: %v", err)
	}
	// 规则有缓存，必须失效
	g.rules.Invalidate()
	t.Cleanup(func() {
		_, _ = g.db.Exec(`DELETE FROM intercept_rules WHERE name='闸门测试-必然拒绝'`)
		g.rules.Invalidate()
	})
}

// newGateTestOrder 建一条处于指定状态的执行订单（用于当日额度统计）。
func newGateTestOrder(t *testing.T, g *Gate, accountID, draftID int64, state, qty string, filledQty, avgPrice string) int64 {
	t.Helper()
	id, err := g.db.ExecOrders().InsertOrder(db.ExecOrderInput{
		DraftID:       draftID,
		AccountID:     accountID,
		Mode:          string(db.ExecModeLiveManual),
		State:         state,
		ClientOrderID: "gate-" + t.Name() + "-" + time.Now().Format("150405.000000000"),
		Qty:           qty,
	})
	if err != nil {
		t.Fatalf("建执行订单失败: %v", err)
	}
	// 直接改 filled_qty / avg_price（InsertOrder 不接这两个）
	if filledQty != "" || avgPrice != "" {
		_, err = g.db.Exec(`UPDATE orders_execution
SET filled_qty = COALESCE($2, filled_qty),
    avg_price  = COALESCE($3, avg_price)
WHERE id = $1`, id, nullable(filledQty), nullable(avgPrice))
		if err != nil {
			t.Fatalf("改执行订单失败: %v", err)
		}
	}
	// 把 state_at 固定到测试时钟的当天，确保被 DailyUsage 统计到
	_, _ = g.db.Exec(`UPDATE orders_execution SET state_at = $2 WHERE id = $1`, id,
		time.Date(2026, 10, 27, 3, 0, 0, 0, time.UTC))
	return id
}

// nullable 把空串转成 nil（用于 SQL NULL）。
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// execTestDB 的副本 —— exec 包不能直接引用 db 包的测试辅助，
// 但这里需要「包级共享实例」这个行为。
//
// 复用 db 包已经验证过的模式（见 db/exec_store_test.go 的注释）。
func execTestDB(t *testing.T) *db.DB {
	t.Helper()
	return sharedTestDB(t)
}

// 保证 os 被使用（DSN 可能从环境读）
var _ = os.Getenv

// ctxOrBackground 便于在测试里传 context。
func ctxOrBackground() context.Context { return context.Background() }

// trackGateTestDraft 给一个**已建好**的 draft 注册 cleanup。
//
// # 为什么需要它（而 newGateTestDraft 不够）
//
// newGateTestDraft 自带 t.Cleanup，所以「用 helper 建 draft」永远会被清理。
// 但有些测试需要自定义 InsertDraft 的入参（比如 Qty 是 0.0000000001 测精度、
// 或者要写 executed_note 塞假 secret），那时用不了 helper ——
// 而自己调 InsertDraft 就**必须**自己注册 cleanup。
//
// 实测（2026-10-29）：3 处测试绕过 helper 直接建 draft 却忘了 cleanup，
// 积累 1556 条残留（全部指向 BTC-USDT），而**没有任何测试会红**。
//
// # 为什么返回 id
//
// 便于链式用法：
//
//	draftID := trackGateTestDraft(t, g, mustInsertDraft(...))
//
// 而不是先赋值再单独一行调用（那样容易漏写）。
//
// # 为什么删三张表
//
// orders_draft 被 draft_risk_verdicts 和 orders_execution 以外键引用，
// 直接删父表会报外键冲突。所以三张都删（先子后父）。
func trackGateTestDraft(t *testing.T, g *Gate, id int64) int64 {
	t.Helper()
	t.Cleanup(func() {
		// 顺序：先删叶子（引用别人的行），再删根（被引用的行）。
		//
		// # 为什么删四张表（而不是三张）
		//
		// 外键链是三层：
		//   exec_reconciliations → orders_execution → orders_draft
		// 且后两层都是 ON DELETE RESTRICT（不是 CASCADE）。
		//
		// 所以删 orders_draft 之前必须先删 orders_execution，
		// 而删 orders_execution 之前必须先删 exec_reconciliations ——
		// 否则 RESTRICT 会让中间那步失败，整条链静默失效。
		//
		// ⚠️ 实测（2026-10-29）：只删三张（漏 exec_reconciliations）时，
		// 只有对账测试会漏 —— 因为只有它会建 reconciliation 行。
		// 每次跑留 12-13 条 draft 残留，而 cleanup 代码**看起来完全合理**
		// （它删了它知道的那两层）。
		for _, q := range []string{
			`DELETE FROM exec_reconciliations WHERE execution_id IN (SELECT id FROM orders_execution WHERE draft_id=$1)`,
			`DELETE FROM orders_execution WHERE draft_id=$1`,
			`DELETE FROM draft_risk_verdicts WHERE draft_id=$1`,
			`DELETE FROM orders_draft WHERE id=$1`,
		} {
			if _, err := g.db.Exec(q, id); err != nil {
				// # 为什么用 Errorf 而不是吞掉错误
				//
				// 「cleanup 静默失败」比「没写 cleanup」更糟：它让人以为已经清了。
				// 而吞错误时，「删了但没删掉」和「删成功」在输出里完全一样。
				//
				// 用 Errorf 而不是 Fatalf：cleanup 阶段 Fatal 会掩盖原本的测试结果。
				t.Errorf("清理 draft %d 失败: %v | SQL: %s | "+
					"这会让 orders_draft 留下残留 —— 检查外键链是否又多了引用方。",
					id, err, q)
			}
		}
	})
	return id
}
