package server

// 「评级 → 草案」的验收测试（P0-2）。
//
// # 最核心的一条：hold / review 不生成草案
//
// 这条判据的失败方式是「静默」：如果不检查，hold 也会生成草案，
// 而草案会进闸门、进执行 —— 那等于「系统决定买入」而模型说的是「持有」。
// 所以测试必须断言「草案表中确实没有新行」，而不只是「返回 created=false」。

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// testOpenDB 打开测试库（与其它 server 测试一致）。
func testOpenDB(t *testing.T) *db.DB {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no test database configured (%v)", err)
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// ratingDraftEnv 建一套测试数据：标的 + 行情 + 评级。
type ratingDraftEnv struct {
	t      *testing.T
	d      *db.DB
	instID int64
}

func newRatingDraftEnv(t *testing.T, tag string) *ratingDraftEnv {
	t.Helper()
	d := testOpenDB(t)

	instID := upsertTestInstrument(t, d, db.InstrumentInput{
		Market:     "crypto",
		Symbol:     "TINSTR-RD-" + tag,
		AssetClass: "crypto",
	})
	// 一条行情（给 ref_price 用）
	if _, err := d.Exec(`
INSERT INTO bars(instrument_id, interval, ts, trading_date, open, high, low, close, volume)
VALUES ($1, '1d', now(), CURRENT_DATE, 100, 101, 99, 100, 1000)`, instID); err != nil {
		t.Fatalf("建行情: %v", err)
	}

	return &ratingDraftEnv{t: t, d: d, instID: instID}
}

// insertRating 建一条评级并返回它。
func (e *ratingDraftEnv) insertRating(rating string) *db.Rating {
	e.t.Helper()
	instID := e.instID
	in := db.RatingInput{
		InstrumentID:     &instID,
		Rating:           rating,
		Confidence:       "0.7000",
		ConfidenceReason: "测试用评级，理由足够长以通过校验",
	}
	// review 必须带四键（DB CHECK 要求），其它评级必须不带
	if rating == "review" {
		b, mErr := json.Marshal(map[string]any{
			"data_gaps": []string{"测试"}, "conflicting_facts": []string{},
			"skipped_analysts": []string{}, "next_step": "测试下一步",
		})
		if mErr != nil {
			e.t.Fatalf("序列化 review_reasons: %v", mErr)
		}
		in.ReviewReasons = b
	}
	id, err := e.d.Ratings().InsertRating(in)
	if err != nil {
		e.t.Fatalf("InsertRating(%s): %v", rating, err)
	}
	got, err := e.d.Ratings().GetRating(id)
	if err != nil {
		e.t.Fatalf("GetRating(%d): %v", id, err)
	}
	return got
}

// draftCount 数该标的的草案数。
func (e *ratingDraftEnv) draftCount() int {
	e.t.Helper()
	var n int
	if err := e.d.QueryRow(`SELECT count(*) FROM orders_draft WHERE instrument_id=$1`, e.instID).Scan(&n); err != nil {
		e.t.Fatalf("数草案: %v", err)
	}
	return n
}

// ---- ① buy → side=buy ----

func TestCreateDraftFromRatingBuy(t *testing.T) {
	e := newRatingDraftEnv(t, "buy")
	r := e.insertRating("buy")

	draftID, created, err := CreateDraftFromRating(e.d, r)
	if err != nil {
		t.Fatalf("CreateDraftFromRating: %v", err)
	}
	if !created || draftID <= 0 {
		t.Fatalf("buy 应生成草案，实际 draftID=%d created=%v", draftID, created)
	}

	var side, qty string
	if err := e.d.QueryRow(`SELECT side, qty FROM orders_draft WHERE id=$1`, draftID).
		Scan(&side, &qty); err != nil {
		t.Fatalf("读草案: %v", err)
	}
	if side != "buy" {
		t.Errorf("side = %q, want \"buy\"", side)
	}
	// qty = 10000 / 100 = 100
	if qty != "100.0000000000" {
		t.Errorf("qty = %q, want \"100.0000000000\"（10000/100）", qty)
	}
}

// ---- ② underweight → side=sell（不是 buy）----

func TestCreateDraftFromRatingUnderweightIsSell(t *testing.T) {
	e := newRatingDraftEnv(t, "underweight")
	r := e.insertRating("underweight")

	draftID, created, err := CreateDraftFromRating(e.d, r)
	if err != nil {
		t.Fatalf("CreateDraftFromRating: %v", err)
	}
	if !created {
		t.Fatal("underweight 应生成草案")
	}

	var side string
	if err := e.d.QueryRow(`SELECT side FROM orders_draft WHERE id=$1`, draftID).Scan(&side); err != nil {
		t.Fatalf("读草案: %v", err)
	}
	// 这是最容易搞错的地方：underweight 是「低配/减持」，方向是卖
	if side != "sell" {
		t.Errorf("underweight 的 side = %q, want \"sell\"（减持方向是卖，不是买）", side)
	}
}

// ---- ③④ hold / review → created=false 且无草案 ----

func TestCreateDraftFromRatingNonActionable(t *testing.T) {
	for _, rating := range []string{"hold", "review"} {
		t.Run(rating, func(t *testing.T) {
			e := newRatingDraftEnv(t, rating)
			r := e.insertRating(rating)

			before := e.draftCount()
			draftID, created, err := CreateDraftFromRating(e.d, r)
			if err != nil {
				t.Fatalf("%s 不该报错（不生成草案是正常结论）：%v", rating, err)
			}
			if created || draftID != 0 {
				t.Errorf("%s 不该生成草案，实际 draftID=%d created=%v", rating, draftID, created)
			}
			// 关键：草案表里确实没有新行（不只是返回值说没有）
			if after := e.draftCount(); after != before {
				t.Errorf("%s 之后草案数变了：%d → %d（静默生成了一条草案）", rating, before, after)
			}
		})
	}
}

// ---- ⑤ instrument_id = NULL → 报错 ----

func TestCreateDraftFromRatingMissingInstrument(t *testing.T) {
	d := testOpenDB(t)

	// instrument_id 为 NULL 的评级（DB 允许）
	id, err := d.Ratings().InsertRating(db.RatingInput{
		Rating:           "buy",
		Confidence:       "0.6000",
		ConfidenceReason: "测试没有标的的评级，理由足够长",
	})
	if err != nil {
		t.Fatalf("InsertRating: %v", err)
	}
	t.Cleanup(func() {
		if _, e := d.Exec(`DELETE FROM ratings WHERE id=$1`, id); e != nil {
			t.Errorf("清理 rating: %v", e)
		}
	})
	r, err := d.Ratings().GetRating(id)
	if err != nil {
		t.Fatalf("GetRating: %v", err)
	}

	_, created, err := CreateDraftFromRating(d, r)
	if err == nil {
		t.Fatal("instrument_id 为 NULL 应报错（数据缺陷，不是「不可执行」）")
	}
	if !errors.Is(err, ErrRatingNoInstrument) {
		t.Errorf("错误应为 ErrRatingNoInstrument，实际 %v", err)
	}
	if created {
		t.Error("不该生成草案")
	}
}

// ---- ⑥ 幂等：同一评级调两次只生成一个草案 ----

func TestCreateDraftFromRatingIdempotent(t *testing.T) {
	e := newRatingDraftEnv(t, "idem")
	r := e.insertRating("buy")

	first, created1, err := CreateDraftFromRating(e.d, r)
	if err != nil {
		t.Fatalf("第一次: %v", err)
	}
	if !created1 {
		t.Fatal("第一次应 created=true")
	}
	if first <= 0 {
		t.Fatalf("第一次应返回有效 draftID，实际 %d", first)
	}

	second, created2, err := CreateDraftFromRating(e.d, r)
	if err != nil {
		t.Fatalf("第二次不该报错（幂等命中是正常结论）：%v", err)
	}
	if created2 || second != 0 {
		t.Errorf("第二次应 created=false draftID=0，实际 draftID=%d created=%v", second, created2)
	}

	// 关键：草案表里只有一行
	if n := e.draftCount(); n != 1 {
		t.Errorf("草案数 = %d, want 1（幂等失效，重复建了草案）", n)
	}
}

// ---- 附加：review 不生成草案，但 hold 也不生成 —— 两者原因不同 ----
//
// 这条是给未来的人看的：hold 是「模型判断持有」，review 是「需人工看」，
// 两者的业务含义不同，但在「是否生成草案」上一致。
// 如果将来有人想「hold 也建个 0 数量的草案表示不动」—— 那会破坏这个不变量。
func TestCreateDraftFromRatingHoldAndReviewSameOutcomeDifferentMeaning(t *testing.T) {
	e := newRatingDraftEnv(t, "holdreview")

	holdR := e.insertRating("hold")
	reviewR := e.insertRating("review")

	for _, r := range []*db.Rating{holdR, reviewR} {
		_, created, err := CreateDraftFromRating(e.d, r)
		if err != nil {
			t.Fatalf("rating=%s: %v", r.Rating, err)
		}
		if created {
			t.Errorf("rating=%s 不该生成草案", r.Rating)
		}
	}
	if n := e.draftCount(); n != 0 {
		t.Errorf("草案数 = %d, want 0", n)
	}
}
