package db

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// executionLimitsSetup opens a DB and returns its ExecutionLimitsStore.
// Skips if no PG.
func executionLimitsSetup(t *testing.T) (*DB, *ExecutionLimitsStore) {
	t.Helper()
	d := testOpen(t)
	return d, d.ExecutionLimits()
}

// TestExecutionLimitsRoundtrip 验证 upsert 新建 → 读回 → 金额精度 → 白名单数组。
func TestExecutionLimitsRoundtrip(t *testing.T) {
	d, store := executionLimitsSetup(t)

	key := uniqueName(t, "lim")
	defer d.Exec(`DELETE FROM execution_limits WHERE key = $1`, key) //nolint:errcheck

	maxOrders := int64(20)
	got, err := store.UpsertLimits(ExecutionLimitsInput{
		Key:              key,
		Mode:             ExecutionModeLiveWhitelist,
		MaxSingleAmount:  "10000.12345678",
		MaxDailyAmount:   "50000.00000000",
		MaxDailyOrders:   &maxOrders,
		WhitelistSymbols: []string{"AAPL", "MSFT"},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got.Key != key || got.Mode != ExecutionModeLiveWhitelist {
		t.Errorf("unexpected limits: %+v", got)
	}
	// 金额按字符串原样回来,不能因 float64 丢精度。
	if got.MaxSingleAmount == nil || *got.MaxSingleAmount != "10000.12345678" {
		t.Errorf("max_single_amount = %v", got.MaxSingleAmount)
	}
	if got.MaxDailyAmount == nil || *got.MaxDailyAmount != "50000.00000000" {
		t.Errorf("max_daily_amount = %v", got.MaxDailyAmount)
	}
	if got.MaxDailyOrders == nil || *got.MaxDailyOrders != maxOrders {
		t.Errorf("max_daily_orders = %v", got.MaxDailyOrders)
	}
	// TEXT[] 必须完整有序地回来 — 这是 array 走 to_json 的关键验证点。
	if len(got.WhitelistSymbols) != 2 ||
		got.WhitelistSymbols[0] != "AAPL" || got.WhitelistSymbols[1] != "MSFT" {
		t.Errorf("whitelist_symbols = %v", got.WhitelistSymbols)
	}

	// 再读一次确认落库。
	loaded, err := store.GetLimits(key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if loaded.ID != got.ID {
		t.Errorf("id mismatch: %d != %d", loaded.ID, got.ID)
	}

	// 第二次 upsert 走 ON CONFLICT 更新,id 不变,金额被替换。
	loaded2, err := store.UpsertLimits(ExecutionLimitsInput{
		Key:  key,
		Mode: ExecutionModePaper,
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if loaded2.ID != got.ID {
		t.Errorf("upsert should update in place, id changed %d → %d", got.ID, loaded2.ID)
	}
	if loaded2.Mode != ExecutionModePaper {
		t.Errorf("mode = %q, want paper", loaded2.Mode)
	}
	// 清空后金额应为 NULL、白名单应为空切片(不是 nil)。
	if loaded2.MaxSingleAmount != nil || loaded2.MaxDailyAmount != nil || loaded2.MaxDailyOrders != nil {
		t.Errorf("expected cleared limits, got %+v", loaded2)
	}
	if loaded2.WhitelistSymbols == nil {
		t.Error("expected non-nil empty whitelist")
	}
	if len(loaded2.WhitelistSymbols) != 0 {
		t.Errorf("expected empty whitelist, got %v", loaded2.WhitelistSymbols)
	}
}

// TestExecutionLimitsErrors 覆盖必填、模式、负数与查不到。
func TestExecutionLimitsErrors(t *testing.T) {
	d, store := executionLimitsSetup(t)

	key := uniqueName(t, "limerr")
	defer d.Exec(`DELETE FROM execution_limits WHERE key = $1`, key) //nolint:errcheck

	cases := []struct {
		name string
		in   ExecutionLimitsInput
	}{
		{"empty key", ExecutionLimitsInput{Mode: ExecutionModePaper}},
		{"bad mode", ExecutionLimitsInput{Key: key, Mode: "yolo"}},
		{"bad amount", ExecutionLimitsInput{Key: key, Mode: ExecutionModePaper, MaxSingleAmount: "lots"}},
		// 负限额必须拒 —— 「负额度」会让所有上限判断失效。
		{"negative amount", ExecutionLimitsInput{Key: key, Mode: ExecutionModePaper, MaxSingleAmount: "-1"}},
		{"negative daily", ExecutionLimitsInput{Key: key, Mode: ExecutionModePaper, MaxDailyAmount: "-100"}},
	}
	for _, c := range cases {
		if _, err := store.UpsertLimits(c.in); !errors.Is(err, ErrExecutionLimitsInvalid) {
			t.Errorf("%s: expected ErrExecutionLimitsInvalid, got %v", c.name, err)
		}
	}

	neg := int64(-5)
	if _, err := store.UpsertLimits(ExecutionLimitsInput{
		Key: key, Mode: ExecutionModePaper, MaxDailyOrders: &neg,
	}); !errors.Is(err, ErrExecutionLimitsInvalid) {
		t.Errorf("expected ErrExecutionLimitsInvalid for negative orders, got %v", err)
	}

	// 合法单子证明上面的拒绝没有误伤。
	if _, err := store.UpsertLimits(ExecutionLimitsInput{
		Key: key, Mode: ExecutionModePaper,
	}); err != nil {
		t.Fatalf("valid upsert rejected: %v", err)
	}

	if _, err := store.GetLimits(uniqueName(t, "no-such-limits")); !errors.Is(err, ErrExecutionLimitsNotFound) {
		t.Errorf("expected ErrExecutionLimitsNotFound, got %v", err)
	}
	if _, err := store.GetLimits("  "); !errors.Is(err, ErrExecutionLimitsInvalid) {
		t.Errorf("expected ErrExecutionLimitsInvalid for blank key, got %v", err)
	}
}

// TestExecutionLimitsFailClosed 是 P0-A 的核心用例:金额上限 NULL 必须 fail-closed。
//
// 语义:NULL = 未配置 = 禁止进入 live 模式。禁止把 NULL 解释为"不限制"。
func TestExecutionLimitsFailClosed(t *testing.T) {
	d, store := executionLimitsSetup(t)

	key := uniqueName(t, "limfc")
	defer d.Exec(`DELETE FROM execution_limits WHERE key = $1`, key) //nolint:errcheck

	// 只填 mode 不填金额 —— 就是那个危险的「无上限实盘」配置。
	if _, err := store.UpsertLimits(ExecutionLimitsInput{
		Key: key, Mode: ExecutionModeLiveWhitelist,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// GetLimits 必须报 ErrLimitNotConfigured,但同时返回可读记录(paper 要用)。
	got, err := store.GetLimits(key)
	if !errors.Is(err, ErrLimitNotConfigured) {
		t.Fatalf("expected ErrLimitNotConfigured, got %v", err)
	}
	if got == nil {
		t.Fatal("expected readable record alongside ErrLimitNotConfigured")
	}
	if got.MaxSingleAmount != nil || got.MaxDailyAmount != nil {
		t.Errorf("expected NULL amounts, got %+v / %+v", got.MaxSingleAmount, got.MaxDailyAmount)
	}

	// RequireLiveReady 是 live 门禁,必须拒绝。
	if _, err := store.RequireLiveReady(key); !errors.Is(err, ErrLimitNotConfigured) {
		t.Errorf("RequireLiveReady must reject unconfigured limits, got %v", err)
	}

	// 只配一半也不行 —— 单笔有上限、日累计无上限同样是 fail-open。
	if _, err := store.UpsertLimits(ExecutionLimitsInput{
		Key: key, Mode: ExecutionModeLiveWhitelist, MaxSingleAmount: "1000",
	}); err != nil {
		t.Fatalf("upsert half: %v", err)
	}
	if _, err := store.RequireLiveReady(key); !errors.Is(err, ErrLimitNotConfigured) {
		t.Errorf("expected rejection when max_daily_amount missing, got %v", err)
	}

	// 金额 0 或负等于没有上限,也必须拒。
	for _, bad := range []string{"0", "0.00000000"} {
		if _, err := store.UpsertLimits(ExecutionLimitsInput{
			Key: key, Mode: ExecutionModeLiveWhitelist,
			MaxSingleAmount: bad, MaxDailyAmount: "5000",
		}); err != nil {
			t.Fatalf("upsert zero %s: %v", bad, err)
		}
		// 负数在校验层就被拒,max_daily_orders 也得配。
		var orders int64 = 10
		if _, err := store.UpsertLimits(ExecutionLimitsInput{
			Key: key, Mode: ExecutionModeLiveWhitelist,
			MaxSingleAmount: bad, MaxDailyAmount: "5000", MaxDailyOrders: &orders,
		}); err != nil {
			t.Fatalf("upsert zero with orders %s: %v", bad, err)
		}
		if _, err := store.RequireLiveReady(key); !errors.Is(err, ErrLimitNotConfigured) {
			t.Errorf("expected rejection for zero amount %q, got %v", bad, err)
		}
	}

	// 缺 max_daily_orders 同样拒。
	if _, err := store.UpsertLimits(ExecutionLimitsInput{
		Key: key, Mode: ExecutionModeLiveWhitelist,
		MaxSingleAmount: "1000", MaxDailyAmount: "5000",
	}); err != nil {
		t.Fatalf("upsert no orders: %v", err)
	}
	if _, err := store.RequireLiveReady(key); !errors.Is(err, ErrLimitNotConfigured) {
		t.Errorf("expected rejection when max_daily_orders missing, got %v", err)
	}

	// 全部配齐才放行 —— 这是唯一的放行路径。
	orders := int64(20)
	if _, err := store.UpsertLimits(ExecutionLimitsInput{
		Key: key, Mode: ExecutionModeLiveWhitelist,
		MaxSingleAmount: "10000", MaxDailyAmount: "50000", MaxDailyOrders: &orders,
	}); err != nil {
		t.Fatalf("upsert fully configured: %v", err)
	}
	ready, err := store.RequireLiveReady(key)
	if err != nil {
		t.Fatalf("fully configured limits must pass, got %v", err)
	}
	if ready.MaxSingleAmount == nil || *ready.MaxSingleAmount != "10000.00000000" {
		t.Errorf("max_single_amount = %v", ready.MaxSingleAmount)
	}

	// 不存在的 key 也必须被 RequireLiveReady 拒(不能因查不到而放行)。
	if _, err := store.RequireLiveReady(uniqueName(t, "no-such")); !errors.Is(err, ErrExecutionLimitsNotFound) {
		t.Errorf("expected ErrExecutionLimitsNotFound, got %v", err)
	}
}

// TestExecutionLimitsRevision 验证 revision 每次 Upsert 递增(审计链)。
func TestExecutionLimitsRevision(t *testing.T) {
	d, store := executionLimitsSetup(t)

	key := uniqueName(t, "limrev")
	defer d.Exec(`DELETE FROM execution_limits WHERE key = $1`, key) //nolint:errcheck

	first, err := store.UpsertLimits(ExecutionLimitsInput{
		Key: key, Mode: ExecutionModePaper, MaxSingleAmount: "100",
		MaxDailyAmount: "500",
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// 新建行的 revision 走 schema 默认值 1。
	if first.Revision != 1 {
		t.Errorf("initial revision = %d, want 1", first.Revision)
	}

	second, err := store.UpsertLimits(ExecutionLimitsInput{
		Key: key, Mode: ExecutionModePaper, MaxSingleAmount: "200",
		MaxDailyAmount: "800",
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if second.Revision != 2 {
		t.Errorf("revision after update = %d, want 2", second.Revision)
	}
	if second.ID != first.ID {
		t.Errorf("upsert should update in place, id %d → %d", first.ID, second.ID)
	}

	third, err := store.UpsertLimits(ExecutionLimitsInput{
		Key: key, Mode: ExecutionModePaper,
	})
	if err != nil {
		t.Fatalf("third upsert: %v", err)
	}
	if third.Revision != 3 {
		t.Errorf("revision after third = %d, want 3", third.Revision)
	}
}

// TestExecutionLimitsJSONExcludesDeprecatedFields 废弃字段不出现在 JSON 里。
//
// # 为什么需要这条测试
//
// `Mode` 和 `WhitelistSymbols` 在 struct 里还留着（SQL 需要），
// 但加了 `json:"-"`。如果将来有人删掉那个 tag（「这字段还有用啊」），
// 它们会重新出现在 API 响应里，误导前端。
//
// # 为什么是双向断言
//
// 只断言「废弃的不在」不够 —— 把整个 struct 的 tag 全删了也能过。
// 必须同时断言「有效字段在」。
func TestExecutionLimitsJSONExcludesDeprecatedFields(t *testing.T) {
	maxSingle := "1000"
	l := ExecutionLimits{
		ID:              1,
		Key:             "global",
		MaxSingleAmount: &maxSingle,
		Revision:        3,

		// 这两个有值，但不该被序列化
		Mode:             "paper",
		WhitelistSymbols: []string{"SPY", "QQQ"},
	}

	b, err := json.Marshal(l)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(b)

	// ---- 反向断言：废弃字段不该出现 ----
	for _, gone := range []string{`"mode"`, `"whitelist_symbols"`} {
		if strings.Contains(s, gone) {
			t.Errorf("废弃字段 %s 不该出现在 JSON 里（会误导前端以为限额有模式概念）\n实际: %s", gone, s)
		}
	}

	// ---- 正向断言：有效字段必须出现 ----
	//
	// 这一半同样重要：如果有人为了「干净」把所有 tag 都改成 json:"-"，
	// 只有反向断言的话测试仍会通过。
	for _, want := range []string{`"id"`, `"key"`, `"max_single_amount"`, `"revision"`} {
		if !strings.Contains(s, want) {
			t.Errorf("有效字段 %s 必须出现在 JSON 里\n实际: %s", want, s)
		}
	}
}

// TestExecutionLimitsFieldsStillUsableForSQL 字段本身仍可用（只是不序列化）。
//
// 守的是「`json:"-"` 不等于字段被删」这个契约：
// UpsertLimits 需要 Mode / WhitelistSymbols 参与 INSERT。
func TestExecutionLimitsFieldsStillUsableForSQL(t *testing.T) {
	l := ExecutionLimits{
		Key:              "global",
		Mode:             "paper",
		WhitelistSymbols: []string{"SPY"},
	}
	// 编译期即证明：字段仍存在且可读写。
	if l.Mode != "paper" {
		t.Errorf("Mode 字段应保留（SQL 需要），实际 %q", l.Mode)
	}
	if len(l.WhitelistSymbols) != 1 || l.WhitelistSymbols[0] != "SPY" {
		t.Errorf("WhitelistSymbols 字段应保留，实际 %v", l.WhitelistSymbols)
	}
}
