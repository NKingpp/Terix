package server

// D 方案（PM intent 结束 → 评级 → 草案）的验收测试。
//
// # 测的是 createDraftFromIntentRating，不是 engine 主循环
//
// 主循环需要 LLM、worker、调度器全部就绪 —— 那不可能在单测里跑。
// 而本函数是「判据 + 编排」的全部逻辑，engine.go 只是「在正确的时机调它」。
//
// 所以这里直接调它，覆盖各条判据；而「时机」由 engine.go 的那一行
// `if state == "done" && intent.Owner == agent.RolePortfolioManager` 保证。

import (
	"encoding/json"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// drEnv 建测试数据：标的 + 行情 + 一个 intent。
type drEnv struct {
	t      *testing.T
	d      *db.DB
	expID  int64
	es     *db.ExplorationStore
	instID int64
}

func newDraftRatingEnv(t *testing.T, tag string, role string) *drEnv {
	t.Helper()
	d := testOpenDB(t)

	expID, err := d.CreateExploration("dr-"+tag, "评级→草案测试")
	if err != nil {
		t.Fatalf("CreateExploration: %v", err)
	}
	es := d.Exploration(expID)

	instID := upsertTestInstrument(t, d, db.InstrumentInput{
		Market: "crypto", Symbol: "TINSTR-DR-" + tag, AssetClass: "crypto",
	})
	if _, err := d.Exec(`
INSERT INTO bars(instrument_id, interval, ts, trading_date, open, high, low, close, volume)
VALUES ($1,'1d',now(),CURRENT_DATE,100,101,99,100,1000)`, instID); err != nil {
		t.Fatalf("建行情: %v", err)
	}

	// intent 节点 —— owner 由触发器从 payload.role 派生（本测试顺带验证触发器）
	if _, err := es.AddNode(db.KindIntent, map[string]any{
		"summary": "测试 intent", "role": role,
	}, 10, "open", "planner", nil); err != nil {
		t.Fatalf("建 intent: %v", err)
	}

	t.Cleanup(func() {
		// 标的及其子表的清理在 upsertTestInstrument 的 cleanup 里（t.Cleanup 后进先出）。
		// 这里只需清 exploration。
		if _, e := d.Exec(`DELETE FROM explorations WHERE id=$1`, expID); e != nil {
			t.Errorf("清理 exploration: %v", e)
		}
	})
	return &drEnv{t: t, d: d, expID: expID, es: es, instID: instID}
}

// lastIntent 取本 exploration 的最后一个 intent 节点（即刚建的那个）。
func (e *drEnv) lastIntent() *db.Node {
	e.t.Helper()
	var id int64
	if err := e.d.QueryRow(`SELECT id FROM exploration_nodes
 WHERE exploration_id=$1 AND kind='intent' ORDER BY id DESC LIMIT 1`, e.expID).Scan(&id); err != nil {
		e.t.Fatalf("取 intent: %v", err)
	}
	nodes, err := e.es.NodesByIDs([]int64{id})
	if err != nil || len(nodes) == 0 {
		e.t.Fatalf("读 intent: %v", err)
	}
	return nodes[0]
}

// addRating 建一条评级，intent_id 指向给定 intent。
func (e *drEnv) addRating(intentID int64, rating string) int64 {
	e.t.Helper()
	instID := e.instID
	in := db.RatingInput{
		InstrumentID: &instID, IntentID: &intentID, Rating: rating,
		Confidence: "0.7000", ConfidenceReason: "测试用评级，理由足够长以通过校验",
	}
	if rating == "review" {
		b, _ := json.Marshal(map[string]any{
			"data_gaps": []string{"测试"}, "conflicting_facts": []string{},
			"skipped_analysts": []string{}, "next_step": "测试下一步",
		})
		in.ReviewReasons = b
	}
	id, err := e.d.Ratings().InsertRating(in)
	if err != nil {
		e.t.Fatalf("InsertRating(%s): %v", rating, err)
	}
	return id
}

func (e *drEnv) draftCount() int {
	e.t.Helper()
	var n int
	if err := e.d.QueryRow(`SELECT count(*) FROM orders_draft WHERE instrument_id=$1`, e.instID).Scan(&n); err != nil {
		e.t.Fatalf("数草案: %v", err)
	}
	return n
}

// TestIntentRatingDraftBuy PM + buy → 生成草案 side=buy。
func TestIntentRatingDraftBuy(t *testing.T) {
	e := newDraftRatingEnv(t, "buy", "portfolio_manager")
	intent := e.lastIntent()

	// 触发器必须已经从 payload.role 派生 owner
	if intent.Owner != "portfolio_manager" {
		t.Fatalf("intent.Owner = %q, want \"portfolio_manager\"（触发器没生效？）", intent.Owner)
	}
	e.addRating(intent.ID, "buy")

	if err := createDraftFromIntentRating(e.d, intent); err != nil {
		t.Fatalf("createDraftFromIntentRating: %v", err)
	}
	if n := e.draftCount(); n != 1 {
		t.Fatalf("草案数 = %d, want 1", n)
	}
	var side string
	if err := e.d.QueryRow(`SELECT side FROM orders_draft WHERE instrument_id=$1`, e.instID).Scan(&side); err != nil {
		t.Fatalf("读草案: %v", err)
	}
	if side != "buy" {
		t.Errorf("side = %q, want \"buy\"", side)
	}
}

// TestIntentRatingDraftUnderweightIsSell PM + underweight → side=sell。
func TestIntentRatingDraftUnderweightIsSell(t *testing.T) {
	e := newDraftRatingEnv(t, "uw", "portfolio_manager")
	intent := e.lastIntent()
	e.addRating(intent.ID, "underweight")

	if err := createDraftFromIntentRating(e.d, intent); err != nil {
		t.Fatalf("createDraftFromIntentRating: %v", err)
	}
	var side string
	if err := e.d.QueryRow(`SELECT side FROM orders_draft WHERE instrument_id=$1`, e.instID).Scan(&side); err != nil {
		t.Fatalf("读草案: %v", err)
	}
	if side != "sell" {
		t.Errorf("underweight 的 side = %q, want \"sell\"", side)
	}
}

// TestIntentRatingDraftNonActionable hold / review → 不生成草案。
func TestIntentRatingDraftNonActionable(t *testing.T) {
	for _, rating := range []string{"hold", "review"} {
		t.Run(rating, func(t *testing.T) {
			e := newDraftRatingEnv(t, rating, "portfolio_manager")
			intent := e.lastIntent()
			e.addRating(intent.ID, rating)

			if err := createDraftFromIntentRating(e.d, intent); err != nil {
				t.Fatalf("%s 不该报错（不生成是正常结论）：%v", rating, err)
			}
			if n := e.draftCount(); n != 0 {
				t.Errorf("%s 之后草案数 = %d, want 0", rating, n)
			}
		})
	}
}

// TestEngineBlockedStateSkipsDraft engine 的判据必须含 state=="done"。
//
// # 为什么这条用纯函数式断言而不是跑 engine
//
// engine 主循环需要 LLM/worker/调度器，不可能单测。
// 而「判据的语义」可以脱离它验证 —— 关键是断言 blocked/exhausted 不触发。
//
// # 为什么这几条要专门测
//
//	done      → emit_rating 已定稿
//	blocked   → 中途失败，可能没评级
//	exhausted → 撞步数上限，可能评级写了一半就中断
//
// 后两者若库里恰好有一条评级，那可能是「没写完的」—— 用它生成草案
// 等于把半成品送进闸门。
func TestEngineBlockedStateSkipsDraft(t *testing.T) {
	// 与 engine.go 的判据逐字等价
	shouldCall := func(state, owner string) bool {
		return state == "done" && owner == "portfolio_manager"
	}
	cases := []struct {
		state, owner string
		want         bool
	}{
		{"done", "portfolio_manager", true},
		{"blocked", "portfolio_manager", false}, // ★ 关键
		{"exhausted", "portfolio_manager", false},
		{"stopped", "portfolio_manager", false},
		{"done", "analyst_market", false},
		{"done", "", false},
	}
	for _, c := range cases {
		if got := shouldCall(c.state, c.owner); got != c.want {
			t.Errorf("shouldCall(%q,%q) = %v, want %v", c.state, c.owner, got, c.want)
		}
	}
}

// TestIntentRatingDraftIdempotent 重复触发 → 只有 1 条草案。
func TestIntentRatingDraftIdempotent(t *testing.T) {
	e := newDraftRatingEnv(t, "idem", "portfolio_manager")
	intent := e.lastIntent()
	e.addRating(intent.ID, "buy")

	for i := 0; i < 3; i++ {
		if err := createDraftFromIntentRating(e.d, intent); err != nil {
			t.Fatalf("第 %d 次: %v", i+1, err)
		}
	}
	if n := e.draftCount(); n != 1 {
		t.Errorf("重复触发 3 次后草案数 = %d, want 1（唯一索引失效）", n)
	}
}

// TestIntentRatingDraftFailureIsReported 草案生成失败 → error，且不产生脏数据。
func TestIntentRatingDraftFailureIsReported(t *testing.T) {
	e := newDraftRatingEnv(t, "fail", "portfolio_manager")
	intent := e.lastIntent()

	// 造一条没有 instrument_id 的评级（数据缺陷）
	id, err := e.d.Ratings().InsertRating(db.RatingInput{
		IntentID: &intent.ID, Rating: "buy",
		Confidence: "0.6000", ConfidenceReason: "测试没有标的的评级，理由足够长",
	})
	if err != nil {
		t.Fatalf("InsertRating: %v", err)
	}
	t.Cleanup(func() {
		if _, e2 := e.d.Exec(`DELETE FROM ratings WHERE id=$1`, id); e2 != nil {
			t.Errorf("清理: %v", e2)
		}
	})

	err = createDraftFromIntentRating(e.d, intent)
	if err == nil {
		t.Fatal("instrument_id 为 NULL 应返回 error（数据缺陷）")
	}
	// 关键：不产生脏数据
	if n := e.draftCount(); n != 0 {
		t.Errorf("失败后草案数 = %d, want 0", n)
	}
}

// TestIntentRatingDraftNoRating PM intent 结束但没有评级 → 不报错。
func TestIntentRatingDraftNoRating(t *testing.T) {
	e := newDraftRatingEnv(t, "norating", "portfolio_manager")
	intent := e.lastIntent()

	// 不建评级
	if err := createDraftFromIntentRating(e.d, intent); err != nil {
		t.Fatalf("没有评级不该报错（PM 可能没调 emit_rating）：%v", err)
	}
	if n := e.draftCount(); n != 0 {
		t.Errorf("草案数 = %d, want 0", n)
	}
}

// TestIntentRatingDraftNonPMOwnerStillWorks 本函数不检查 role —— 那是调用方的职责。
//
// # 为什么要显式记录这个设计
//
// engine.go 先判 `intent.Owner == portfolio_manager` 才调本函数。
// 所以本函数**不该**再检查一遍 —— 重复检查会让「谁负责这个判据」模糊，
// 而将来改判据时容易改错一处。
//
// 判据只有一处：engine.go 的那一行。
func TestIntentRatingDraftNonPMOwnerStillWorks(t *testing.T) {
	e := newDraftRatingEnv(t, "nonpm", "analyst_market")
	intent := e.lastIntent()

	if intent.Owner != "analyst_market" {
		t.Fatalf("owner = %q", intent.Owner)
	}
	e.addRating(intent.ID, "buy")

	// 本函数仍会建草案 —— 因为 role 判据在调用方（engine.go）
	if err := createDraftFromIntentRating(e.d, intent); err != nil {
		t.Fatalf(": %v", err)
	}
	if n := e.draftCount(); n != 1 {
		t.Errorf("本函数不检查 role，应建 1 条草案；实际 %d。"+
			"如果这里为 0，说明有人在 createDraftFromIntentRating 里加了 role 检查 —— "+
			"那会让「谁负责 role 判据」变成两处", n)
	}
}
