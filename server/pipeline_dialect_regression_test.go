package server

// 方言回归测试：锁住三处「看起来能简化、实则静默出错」的 SQL。
//
// # 为什么需要这个文件
//
// 两个具体的事故形态：
//
//  1. PG 的 `jsonb || jsonb` 是**深合并**，SQLite 的 `||` 是**字符串拼接**。
//     把 `|| $2::jsonb` 的 `::jsonb` 删掉看起来是「无害的机械改动」，
//     实际得到 `'{"a":1}{"b":2}'` —— 非法 JSON，且**不报错**。
//  2. `payload->>'k'` 是 PG 专有操作符。删 `::` 的人很容易漏掉 `->>`。
//
// 三条测试各锁一处。它们断言的是**语义**（能 json.Unmarshal、深合并、精度不丢），
// 不是「SQL 文本长什么样」—— 后者会随重构失效，前者不会。

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// TestStageGraphVersionPatchIsDeepMerge 证明版本标记是深合并而不是字符串拼接。
//
// # 断言链（缺一不可）
//
//	① 写完之后 payload 必须能 json.Unmarshal —— 字符串拼接通不过这条
//	② 原有顶层字段必须还在 —— 覆盖（json_set 到顶层）会丢，这条能抓到
//	③ 嵌套对象的**两侧**字段都在 —— 证明是深合并而不是整键替换
func TestStageGraphVersionPatchIsDeepMerge(t *testing.T) {
	d, err := db.Open(mustDSN(t))
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	expID, err := d.CreateExploration("patch-merge", "json_patch 深合并")
	if err != nil {
		t.Fatalf("CreateExploration: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, expID) })

	// 根 fact：带一个嵌套对象，用来验证「深」合并。
	rootID, err := d.Exploration(expID).AddNode(db.KindFact, map[string]any{
		"a":      1,
		"nested": map[string]any{"x": 1},
	}, 0, db.StateOrigin, "system", nil)
	if err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	rootIDCopy := rootID
	if err := inTxForTest(d, func(tx *sql.Tx) error {
		if err := writeStageGraphVersion(tx, rootIDCopy, "v0", "v1"); err != nil {
			return err
		}
		// 第二次写入，patch 与已有 nested 再合并一次。
		return writeStageGraphVersion(tx, rootIDCopy, "v1", "v2")
	}); err != nil {
		t.Fatalf("writeStageGraphVersion: %v", err)
	}

	var raw string
	if err := d.QueryRow(`SELECT payload FROM exploration_nodes WHERE id=$1`, rootID).Scan(&raw); err != nil {
		t.Fatalf("读 payload: %v", err)
	}
	// ① 必须是合法 JSON —— 这是「字符串拼接 vs 深合并」最直接的判别式。
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("payload 不是合法 JSON（说明用了 || 而不是 json_patch）: %v\n原文：%s", err, raw)
	}
	// ② 原有顶层字段必须保留。
	if got["a"] != float64(1) {
		t.Errorf("payload.a 丢了（覆盖了而不是合并）: %v", got)
	}
	// ③ 嵌套对象的**原有**字段必须还在（json_set 整键替换会丢掉它）。
	nested, ok := got["nested"].(map[string]any)
	if !ok {
		t.Fatalf("payload.nested 丢了: %v", got)
	}
	if nested["x"] != float64(1) {
		t.Errorf("nested.x 丢了（不是深合并）: %v", nested)
	}
	// 版本字段本身写对（第二次写入生效）。
	if got["stage_graph_applied"] != "v2" {
		t.Errorf("stage_graph_applied = %v, want v2", got["stage_graph_applied"])
	}
	if got["stage_graph_applied_prev"] != "v1" {
		t.Errorf("stage_graph_applied_prev = %v, want v1", got["stage_graph_applied_prev"])
	}
}

// TestReadStageGraphVersionUsesJSONExtract 证明读版本用的是 json_extract。
//
// # 为什么单独一条
//
// `payload->>'stage_graph_applied'` 在 SQLite 下是**语法错误**
// （`unrecognized token: ">"`），会被 readStageGraphVersion 的 err 分支抓到并
// 包成 "读阶段图版本: …"。所以这条测试的价值是：如果有人改回 `->>`，
// 它会以「读阶段图版本」这个明确信息失败，而不是在别处莫名其妙地空转。
func TestReadStageGraphVersionUsesJSONExtract(t *testing.T) {
	d, err := db.Open(mustDSN(t))
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	expID, err := d.CreateExploration("read-version", "json_extract 读版本")
	if err != nil {
		t.Fatalf("CreateExploration: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, expID) })

	rootID, err := d.Exploration(expID).AddNode(db.KindFact, map[string]any{
		"stage_graph_applied": "v9",
	}, 0, db.StateOrigin, "system", nil)
	if err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	var version string
	var gotRoot int64
	if err := inTxForTest(d, func(tx *sql.Tx) error {
		var e error
		version, gotRoot, e = readStageGraphVersion(tx, expID)
		return e
	}); err != nil {
		t.Fatalf("readStageGraphVersion: %v", err)
	}
	if gotRoot != rootID {
		t.Errorf("rootID = %d, want %d", gotRoot, rootID)
	}
	if version != "v9" {
		t.Errorf("version = %q, want v9", version)
	}
}

// TestRatingRefPriceKeepsDecimalPrecision 证明 bars.close（TEXT 存的十进制）能原样读出。
//
// # 为什么这条必要
//
// `COALESCE(close::text, ”)` 改成 `COALESCE(CAST(close AS TEXT), ”)` 之后
// 语义没变，但**如果谁把它改成 `CAST(close AS REAL)` 或直接扫 float64**，
// 78000.5 会在某处变成 78000.49999999999 或丢尾数 —— 而 ref_price 会参与
// 「名义额 ÷ 价格 = 下单量」的计算。这条测试断言**逐字符相等**。
func TestRatingRefPriceKeepsDecimalPrecision(t *testing.T) {
	d, err := db.Open(mustDSN(t))
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	instID, err := d.Instruments().InsertInstrument(db.InstrumentInput{
		Market: db.MarketCrypto, Symbol: uniqueSymbol("PREC"), AssetClass: db.AssetClassCrypto,
	})
	if err != nil {
		t.Fatalf("InsertInstrument: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM instruments WHERE id=$1`, instID) })

	// 用带小数的价格：float64 无法精确表示 78000.5 之后再加任意小数的情形，
	// 而 "78000.1234567890" 这种 10 位小数会被 float64 明显截断。
	const want = "78000.1234567890"
	if _, err := d.Exec(`
INSERT INTO bars(instrument_id, "interval", ts, trading_date, close, source)
VALUES ($1, '1d', $2, $3, $4, 'test')`,
		instID, "2026-01-02T00:00:00.000Z", "2026-01-02", want); err != nil {
		t.Fatalf("写 bars: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM bars WHERE instrument_id=$1`, instID) })

	got, err := ratingRefPrice(d, instID)
	if err != nil {
		t.Fatalf("ratingRefPrice: %v", err)
	}
	if got != want {
		t.Fatalf("ref price = %q, want %q（精度丢了 —— 可能被 CAST 成 REAL 了）", got, want)
	}
}

// inTxForTest 在本包测试里开事务执行 fn（与 stageGraphEnv.inTx 同语义，
// 但不需要先建 stageGraphEnv —— 本文件的三条测试都不需要完整阶段图环境）。
func inTxForTest(d *db.DB, fn func(tx *sql.Tx) error) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
