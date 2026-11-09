package agent

// 六值评级解析的验收测试（W3-14）。
//
// # 为什么要这么多畸形用例
//
// 文档要求「≥8 个畸形用例全落 REVIEW」。而畸形用例的价值在于
// **覆盖模型真实会输出的形态**，不是构造无意义的垃圾。
//
// 下面的用例来自 LLM 的常见偏差：
//   - 大小写不统一（"buy" / "BUY"）
//   - 下划线/连字符变体（"strong_buy"）
//   - 前导/尾随空白（" Buy" / "Buy "）
//   - 尾随换行（"Buy\n"）
//   - 评级 + 解释（"Buy - because..."）
//   - 中文（"买入"）
//   - 多值（"Buy/Hold"）
//
// # 关键的三个测试
//
//  1. TestParseRatingLiteralREVIEW      —— 合法 REVIEW 必须走 literal 分支
//  2. TestParseRatingFallbackIsDistinct —— 垃圾输入必须是 fallback
//  3. TestParseRatingStrictNoNormalize  —— 不做任何归一
//
// 第 1、2 条合起来才能证明「两个分支真的分开了」——
// 只测「垃圾 → REVIEW」无法发现「REVIEW 分支缺失」。

import (
	"strings"
	"testing"
)

// ---- 合法值（6 个，全部 literal）----

func TestParseRatingAllSixLiteral(t *testing.T) {
	for _, want := range []string{
		RatingBuy, RatingOverweight, RatingHold,
		RatingUnderweight, RatingSell, RatingReview,
	} {
		r := ParseRating(want)
		if r.Rating != want {
			t.Errorf("ParseRating(%q) = %q, want %q", want, r.Rating, want)
		}
		if r.Source != SourceLiteral {
			t.Errorf("ParseRating(%q).Source = %v, want SourceLiteral（它本身就是合法值）",
				want, r.Source)
		}
		if r.Raw != want {
			t.Errorf("ParseRating(%q).Raw = %q, want 原样保留", want, r.Raw)
		}
	}
}

// TestParseRatingLiteralREVIEW 是「REVIEW 是正式值」的关键测试。
//
// # 为什么这条单独存在
//
// 如果实现写成「任何未知值 → REVIEW」，那 ParseRating("REVIEW") 也返回 REVIEW ——
// **测试看起来通过，但通过的原因是错的**（走的是 fallback 分支）。
//
// 而 Source 让这两个分支可区分：合法 REVIEW 必须是 SourceLiteral。
func TestParseRatingLiteralREVIEW(t *testing.T) {
	r := ParseRating(RatingReview)
	if r.Rating != RatingReview {
		t.Fatalf("ParseRating(%q) = %q", RatingReview, r.Rating)
	}
	if r.Source != SourceLiteral {
		t.Fatal("输入 REVIEW 本身是合法六值之一，不该走 fallback 分支 —— " +
			"这会让「模型说不确定」和「模型说胡话」无法区分")
	}
}

// ---- 畸形用例（全部 fallback）----

func TestParseRatingMalformed(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		why  string
	}{
		// ---- 大小写 ----
		{"大写 Buy", "Buy", "评级必须小写（DB CHECK 是小写）"},
		{"全大写 BUY", "BUY", "评级必须小写"},
		{"混写 bUy", "bUy", "评级必须小写"},
		{"大写 REVIEW", "REVIEW", "评级必须小写（DB CHECK 是 review）"},

		// ---- 变体分隔 ----
		{"下划线 strong_buy", "strong_buy", "StrongBuy 不存在（强度靠 Overweight）"},
		{"连字符 strong-buy", "strong-buy", "非六值"},
		{"无分隔 strongbuy", "strongbuy", "非六值"},

		// ---- 空白 ----
		{"前导空格", " Buy", "评级不含空白"},
		{"尾随空格", "Buy ", "评级不含空白"},
		{"前后空格", "  Buy  ", "评级不含空白"},
		{"尾随换行", "Buy\n", "评级不含换行"},
		{"中间空格", "Over weight", "非六值"},

		// ---- 空 ----
		{"空串", "", "模型没给出判断，不是 Hold"},
		{"只有空格", "   ", "同上"},
		{"只有换行", "\n", "同上"},

		// ---- 完全无关 ----
		{"Excellent", "Excellent", "非六值"},
		{"推荐", "推荐", "非六值"},

		// ---- 中文（语义对但格式错）----
		{"中文 买入", "买入", "语义等于 Buy，但格式非法 —— 归一成 Buy 会掩盖格式偏差"},
		{"中文 持有", "持有", "语义等于 Hold，但格式非法 —— 归一成 Hold 会掩盖格式偏差"},
		{"中文 增持", "增持", "同上"},

		// ---- 评级 + 解释（LLM 最常见格式）----
		{"带理由", "Buy - because fundamentals are strong", "纯评级字段不该含理由"},
		{"带括号", "Buy (强烈推荐)", "同上"},
		{"带冒号", "Rating: Buy", "含前缀"},

		// ---- 多值 ----
		{"两个值斜杠", "Buy/Hold", "对不确定的标的不该给两个答案"},
		{"两个值逗号", "Buy, Hold", "同上"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := ParseRating(c.raw)
			if r.Rating != RatingReview {
				t.Errorf("ParseRating(%q) = %q, want REVIEW（%s）", c.raw, r.Rating, c.why)
			}
			if r.Source != SourceFallback {
				t.Errorf("ParseRating(%q).Source = %v, want SourceFallback", c.raw, r.Source)
			}
			// Raw 必须保留原文 —— 那是改进 prompt 的唯一依据
			if r.Raw != c.raw {
				t.Errorf("ParseRating(%q).Raw = %q, want 原样保留", c.raw, r.Raw)
			}
		})
	}
}

// TestParseRatingFallbackIsDistinct 证明「垃圾输入」和「合法 REVIEW」可区分。
//
// # 这条是 TestParseRatingLiteralREVIEW 的对照
//
// 两条合起来证明：Source 字段真的能区分两个分支。
func TestParseRatingFallbackIsDistinct(t *testing.T) {
	literal := ParseRating(RatingReview)
	fallback := ParseRating("Excellent")

	// 归一后的 Rating 相同（都是 REVIEW）——
	// 这正是「结果相同」让错误难暴露的原因。
	if literal.Rating != fallback.Rating {
		t.Fatalf("前提不成立：两者归一后应相同，实际 %q vs %q",
			literal.Rating, fallback.Rating)
	}

	// 但 Source 必须不同 —— 这是唯一的区分手段。
	if literal.Source == fallback.Source {
		t.Fatal("合法 REVIEW 与垃圾输入的 Source 相同 —— 两个分支无法区分，\n" +
			"这意味着「模型说不确定」和「模型说胡话」会被统计成同一件事")
	}
	if literal.Source != SourceLiteral {
		t.Errorf("合法 REVIEW 的 Source = %v, want SourceLiteral", literal.Source)
	}
	if fallback.Source != SourceFallback {
		t.Errorf("垃圾输入的 Source = %v, want SourceFallback", fallback.Source)
	}
}

// TestParseRatingStrictNoNormalize 明确断言「不做任何归一」。
//
// # 为什么单独一条
//
// 上面那些畸形用例断言「结果是 REVIEW」，但没断言「**为什么**是 REVIEW」——
// 如果实现里加了 trim，那 " Buy" 会被 trim 成 "Buy" 然后走 literal 分支，
// 结果仍是 "Buy" 而不是 "REVIEW" —— 那会红。
//
// 但如果实现里加了「trim 后再判断，若仍是非法则 fallback」，
// 那 "  garbage  " 会被 trim 后 fallback，结果仍是 REVIEW、Source 仍是 fallback ——
// **看不出区别**。
//
// 所以这条要直接测「带空白的合法值」：它必须是 fallback，不能是 literal。
func TestParseRatingStrictNoNormalize(t *testing.T) {
	// 带空白的合法值必须 fallback（如果实现 trim 了，这里会变成 literal）
	for _, raw := range []string{" Buy", "Buy ", "  Buy  ", "Buy\t", "Buy\n"} {
		r := ParseRating(raw)
		if r.Rating != RatingReview {
			t.Errorf("ParseRating(%q) = %q, want REVIEW —— 实现不能 trim 空白",
				raw, r.Rating)
		}
		if r.Source != SourceFallback {
			t.Errorf("ParseRating(%q).Source = %v, want SourceFallback", raw, r.Source)
		}
	}
}

// ---- 可执行性 ----

func TestIsActionableRating(t *testing.T) {
	// 4 个可执行
	for _, r := range []string{RatingBuy, RatingOverweight, RatingUnderweight, RatingSell} {
		if !IsActionableRating(r) {
			t.Errorf("IsActionableRating(%q) = false, want true（文档 §3.4:196 列为可执行）", r)
		}
	}
	// 2 个不可执行
	for _, r := range []string{RatingHold, RatingReview} {
		if IsActionableRating(r) {
			t.Errorf("IsActionableRating(%q) = true, want false（EXEC_RATING_NOT_ACTIONABLE）", r)
		}
	}
	// 未知值必须 false（保守方向）
	for _, r := range []string{"", "StrongBuy", "Buy", "Excellent"} {
		if IsActionableRating(r) {
			t.Errorf("IsActionableRating(%q) = true, want false（未知值不给执行权）", r)
		}
	}
}

// ---- 严格校验（写入前用）----

func TestValidateRatingStrictAcceptsSix(t *testing.T) {
	for _, r := range []string{
		RatingBuy, RatingOverweight, RatingHold,
		RatingUnderweight, RatingSell, RatingReview,
	} {
		if err := ValidateRatingStrict(r); err != nil {
			t.Errorf("ValidateRatingStrict(%q) = %v, want nil", r, err)
		}
	}
}

func TestValidateRatingStrictRejectsMalformed(t *testing.T) {
	for _, r := range []string{"Buy", " buy", "", "StrongBuy", "买入"} {
		if err := ValidateRatingStrict(r); err == nil {
			t.Errorf("ValidateRatingStrict(%q) = nil, want error", r)
		}
	}
}

// TestValidateRatingStrictErrorIsActionable 错误信息必须指出「哪里错了」。
//
// # 为什么这条重要
//
// 如果错误只说 "invalid rating"，模型（或人）不知道是
// 「该写 Buy 却写了 buy」还是「根本不该是评级」。
//
// 而这决定了下一步动作：改大小写 vs 改内容。
func TestValidateRatingStrictErrorIsActionable(t *testing.T) {
	// 大小写问题 → 应指出正确写法
	err := ValidateRatingStrict("Buy")
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), RatingBuy) {
		t.Errorf("错误信息应指出正确写法 %q，实际：%v", RatingBuy, err)
	}
	if !strings.Contains(err.Error(), "大小写") {
		t.Errorf("错误信息应说明是大小写问题，实际：%v", err)
	}

	// 空白问题 → 应指出「逐字匹配，不含空白」
	err = ValidateRatingStrict(" buy")
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "空白") && !strings.Contains(err.Error(), "逐字") {
		t.Errorf("错误信息应说明空白问题，实际：%v", err)
	}

	// 完全无关的值 → 应列出合法值
	err = ValidateRatingStrict("Excellent")
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{RatingBuy, RatingReview} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应列出合法值（缺 %q），实际：%v", want, err)
		}
	}
}

// ---- 六值与 DB CHECK 的一致性 ----

// TestRatingConstantsMatchDBCheck 六值常量必须与 ratings 表的 CHECK **逐字**一致。
//
// # 为什么这条测试直查 DB，而不是拿一个硬编码的 map 对照
//
// 硬编码 map 只能证明「常量和我想的一样」，不能证明「常量和 DB 一样」。
// 而这轮真实发生的漂移正是「常量（大写）+ 文档（大写）一致，但和 DB（小写）不一致」——
// 一个硬编码的 map 只会是第三份「同一个人写错三次」。
//
// 所以这里查 DB 里的真实表定义，拿它作判据。PG 时代用 `pg_get_constraintdef`；
// SQLite 没有系统表暴露约束表达式，但 `sqlite_master.sql` 就是建表语句原文
// （含所有 CHECK），等价可得。
//
// # 失败时的报错要能直接指导修复
//
// 它会打出 DB 的原始定义，看的人立刻知道该改成什么。
func TestRatingConstantsMatchDBCheck(t *testing.T) {
	d := testDB(t)

	var def string
	err := d.QueryRow(`SELECT sql FROM sqlite_master
 WHERE type='table' AND name='ratings'`).Scan(&def)
	if err != nil {
		t.Fatalf("读 ratings 的建表语句: %v", err)
	}
	if !strings.Contains(def, "'buy'") {
		t.Fatalf("ratings 的建表语句里没有 rating 的 CHECK 枚举，实际定义：%s", def)
	}

	for _, r := range []string{
		RatingBuy, RatingOverweight, RatingHold,
		RatingUnderweight, RatingSell, RatingReview,
	} {
		if !strings.Contains(def, "'"+r+"'") {
			t.Errorf("Go 常量 %q 不在 DB CHECK 里 —— 写库必然违反约束；DB 定义：%s；修法：改 agent/rating.go 的常量，或改 DB（后者要迁移）", r, def)
		}
	}

	// 反向：DB 允许的每个值都必须在 Go 常量里（防止「DB 加了第七个值但 Go 没跟」）
	for _, v := range []string{"buy", "overweight", "hold", "underweight", "sell", "review"} {
		if !strings.Contains(def, "'"+v+"'") {
			t.Errorf("断言用的值 %q 不在 DB CHECK 里 —— 这条测试本身的前提错了；DB 定义：%s", v, def)
		}
		if !allRatings[v] {
			t.Errorf("DB 允许 %q 但 Go 常量里没有它；DB 定义：%s", v, def)
		}
	}
}

// TestRatingReviewRequiresReviewReasons ratings 表的另一条 CHECK：
// rating='review' 时 review_reasons 必须非 NULL 且含四个键。
//
// # 为什么这条要单独测
//
// 六值一致性只保证「review 是合法值」。而 review 还有第二道约束：
// 必须带 review_reasons（含 data_gaps/conflicting_facts/skipped_analysts/next_step）。
//
// 如果实现只写 rating='review' 而不写 review_reasons，INSERT 会失败 ——
// 而那是一个「看起来对（值合法）但实际错」的状态。
func TestRatingReviewRequiresReviewReasons(t *testing.T) {
	d := testDB(t)

	var def string
	if err := d.QueryRow(`SELECT sql FROM sqlite_master
 WHERE type='table' AND name='ratings'`).Scan(&def); err != nil {
		t.Fatalf("读 ratings 的建表语句: %v", err)
	}
	if !strings.Contains(def, "review_reasons") {
		t.Fatalf("ratings 的建表语句里没有 review_reasons 的 CHECK，实际定义：%s", def)
	}

	for _, k := range []string{"data_gaps", "conflicting_facts", "skipped_analysts", "next_step"} {
		if !strings.Contains(def, k) {
			t.Errorf("review_reasons 必须含 %q（DB CHECK 要求），实际定义：%s", k, def)
		}
	}
}

// TestNoStrongBuy 「StrongBuy 不存在」是一个显式的约定。
//
// # 为什么要专门测「某个值不存在」
//
// 因为「增持 / 买入 / 强烈买入」在自然语言里都有，很容易被误加。
// 而加了之后：模型输出 StrongBuy → 通过解析 → **DB CHECK 拒绝 → INSERT 失败**。
//
// 这条测试让「加 StrongBuy」在解析层就被拦住。
func TestNoStrongBuy(t *testing.T) {
	for _, bad := range []string{"StrongBuy", "Strong_Buy", "strongBuy"} {
		if allRatings[bad] {
			t.Errorf("%q 不该是合法评级 —— 六值里没有 StrongBuy（强度表达靠 Overweight）", bad)
		}
		r := ParseRating(bad)
		if r.Rating != RatingReview {
			t.Errorf("ParseRating(%q) = %q, want REVIEW", bad, r.Rating)
		}
	}
}

// ---- Source 的可读性 ----

func TestRatingSourceString(t *testing.T) {
	if SourceLiteral.String() != "literal" {
		t.Errorf("SourceLiteral.String() = %q", SourceLiteral.String())
	}
	if SourceFallback.String() != "fallback" {
		t.Errorf("SourceFallback.String() = %q", SourceFallback.String())
	}
}
