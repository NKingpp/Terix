package server

// 测试用标的的创建 helper。
//
// # 为什么必须用这个 helper 而不是直接调 UpsertInstrument
//
// instruments 是父表 —— 它不会被任何子表的清理带走（级联方向是「删父表时删子表」）。
// 而一个完整的 cleanup 有 5-6 行，其中只有 1 行是删父表，那行最容易被漏。
//
// ⚠️ 实际漏过：
//   - server/rating_draft_test.go     cleanup 删了 orders_draft/ratings/bars，漏 instruments
//   - server/engine_rating_draft_test.go 同上
//   - agent/tools_emit_test.go        完全没有 cleanup
//
// 后果：每跑一次测试在表里留 1-2 行，已积累 18 条 RD-*/DR-*/GATE-TEST-* 垃圾，
// 污染了真实标的表，并让「标的选择器」不可用。
//
// # 为什么 cleanup 顺序是先子后父
//
// tasks / signals / ratings / orders_draft / paper_orders 五个外键**没有 ON DELETE CASCADE**，
// 直接删父表会违反外键约束。
// （bars / instrument_relations / watchlist_items 有 CASCADE，不用显式删。）
//
// # symbol 前缀为什么用 TINSTR-
//
// 不用 TEST-，因为将来若想给 instruments 加 CHECK 拦 TEST-% 前缀，那些测试就建不了了。
// TINSTR-（test instrument）语义明确，且不与任何真实代码形态冲突。

import (
	"testing"

	"github.com/Autumn-27/artex/db"
)

// upsertTestInstrument 建一个测试用标的，并注册 cleanup 删掉它。
func upsertTestInstrument(t *testing.T, d *db.DB, in db.InstrumentInput) int64 {
	t.Helper()
	instID, _, err := d.Instruments().UpsertInstrument(in)
	if err != nil {
		t.Fatalf("建测试标的 %s: %v", in.Symbol, err)
	}
	t.Cleanup(func() {
		// 无 CASCADE 的子表 → 父表。（bars/relations/watchlist 有 CASCADE，省略）
		//
		// 子表用 Logf：删 0 行是正常的（可能本来没数据）。
		for _, q := range []string{
			`DELETE FROM ratings WHERE instrument_id=$1`,
			`DELETE FROM signals WHERE instrument_id=$1`,
			`DELETE FROM orders_draft WHERE instrument_id=$1`,
			`DELETE FROM paper_orders WHERE instrument_id=$1`,
			`DELETE FROM tasks WHERE instrument_id=$1`,
		} {
			if _, e := d.Exec(q, instID); e != nil {
				t.Logf("清理测试标的 %d 的子表失败: %v（%s）", instID, e, q)
			}
		}
		// 父表用 Errorf：留下垃圾是真实的失败 —— 这正是这次问题的形态。
		if _, e := d.Exec(`DELETE FROM instruments WHERE id=$1`, instID); e != nil {
			t.Errorf("清理测试标的 %d 失败: %v —— 这会在 instruments 表留下垃圾", instID, e)
		}
	})
	return instID
}
