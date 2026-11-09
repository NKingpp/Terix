package server

// 「评级 → 订单草案」的转换（P0-2）。
//
// # 为什么单独一个文件
//
// 它是一条独立的业务规则（不涉及 HTTP、不涉及 LLM），而它要被两个地方调用：
//   ① emit_rating 成功后自动触发
//   ② 人工补建（P1 的 API）
//
// 放进 exec_api.go 会让它和 HTTP 绑定；放进 agent 包会让 agent 依赖 db 的草案表。
//
// # 分层位置
//
//	agent  → 产生评级（写入 ratings 表）
//	本文件  → 判断「这个评级能否转成草案」，能则建
//	exec   → 闸门 + 执行（消费草案）
//
// 本文件是 agent 与 exec 之间的**唯一接缝** —— 而它只做一件事：
// 校验评级可执行 + 建一条草案。

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	"log/slog"
)

// ErrRatingNoInstrument 表示评级的 instrument_id 为 NULL。
//
// # 为什么这是错误而不是「跳过」
//
// 草案必须有标的 —— 否则闸门无法判定白名单和交易时段。
// 而「评级没有标的」本身是数据缺陷（评级应该总是针对某个标的）。
//
// 如果静默跳过，那个缺陷会被掩盖成「这条评级不可执行」——
// 而实际原因是「数据不全」，两者的修复方向完全不同。
var ErrRatingNoInstrument = errors.New("评级没有关联标的（instrument_id 为 NULL）—— 无法生成草案")

// ErrRatingQtyUnavailable 表示无法确定草案数量。
//
// 当前 P0 没有仓位计算模块，所以走 ref_price 的固定名义额。
// 见 draftQtyFromRating 的注释。
var ErrRatingQtyUnavailable = errors.New("无法确定草案数量（评级缺少可用价格）")

// ratingSide 把评级映射成草案方向。
//
// # 为什么 underweight 是 sell 而不是 buy
//
// `underweight` = 「低配 / 减持」，方向是**卖**。
// 而「underweight 是不是该小仓位买入」是很容易搞错的地方 ——
// 字面上「under」听起来像「低于目标」，可能被读成「还没到目标所以买」。
//
// 正确的读法：评级的四个可执行值是**相对当前持仓的动作意图**：
//
//	buy          → 建仓/加仓，方向 buy
//	overweight   → 超配/加仓，方向 buy
//	underweight  → 低配/减仓，方向 sell
//	sell         → 清仓/卖出，方向 sell
//
// 而 hold / review 不在本函数返回范围内（它们不可执行，见 IsActionableRating）。
func ratingSide(rating string) (string, error) {
	switch rating {
	case "buy", "overweight":
		return "buy", nil
	case "underweight", "sell":
		return "sell", nil
	default:
		return "", fmt.Errorf("评级 %q 不可执行（只有 buy/overweight/underweight/sell 能转草案）", rating)
	}
}

// draftQtyFromRating 由评级推算草案数量。
//
// # P0 的简化（必须登记）
//
// 当前没有仓位计算模块（PRD 的 position sizing 在 P1），所以：
//   - 用 rating 的 ref_price（如果调用方提供）算出一个「名义额 = 10000」的数量
//   - 拿不到价格 → 返回 ErrRatingQtyUnavailable
//
// ⚠️ 这意味着 P0 的草案数量不是「按风险预算算的」，而是一个固定名义额。
// 它是一个**占位实现**，不是最终方案 —— 所以草案的 executed_note 里会写明
// 这个数量是怎么来的，让人在执行前能看到「这不是按仓位算的」。
//
// 修复路径：接入 position sizing 后，本函数改成「按账户权益 × 风险比例 ÷ 价格」。
func draftQtyFromRating(refPrice string) (string, error) {
	price := strings.TrimSpace(refPrice)
	if price == "" || price == "0" {
		return "", ErrRatingQtyUnavailable
	}
	// 占位：固定名义额 10000 / 价格。
	//
	// # 为什么在 Go 里算而不是让 SQL 算（SQLite 迁移）
	//
	// PG 版是 `round(10000::numeric / NULLIF('x'::numeric,0), 10)`，靠 DB 的
	// numeric 保精度。SQLite 下金额列是 TEXT，SQL 除法会把两侧转成 float64
	// （`10000.0 / '250.5'` → double），尾部就漂了；而 qty 会和 ref_price
	// 一起参与后续成本基数计算，漂一点就会让「下单量」不是整数手。
	//
	// 所以用 db.ParseDecimal + big.Rat 精确算，再输出成 10 位小数的**字面量**
	// （仍是 SQL 片段，调用方照原样拼进 INSERT，不引入参数占位）。
	//
	// 精度与 PG 的 round(...,10) 一致：四舍五入、远离零。
	denom, err := db.ParseDecimal(price)
	if err != nil {
		return "", ErrRatingQtyUnavailable
	}
	nominal, err := db.ParseDecimal("10000")
	if err != nil {
		return "", ErrRatingQtyUnavailable
	}
	q, ok := db.DecimalQuo(nominal, denom)
	if !ok {
		return "", ErrRatingQtyUnavailable
	}
	return quoteNumeric(db.DecimalString(q, 10)), nil
}

// quoteNumeric 把已校验的十进制字面量包成 SQL 字面量。
//
// # 为什么不用参数占位
//
// 因为上面的 qty 表达式是**拼进 INSERT 的 SQL 片段**，不是值。
// 而调用方已通过 validDecimalLiteral 校验过（只含数字、点、负号），
// 所以拼接是安全的。这里再包一层引号只是让它在 SQL 里成为字符串字面量。
func quoteNumeric(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// CreateDraftFromRating 从一条评级生成订单草案。
//
// 返回值：
//
//	(draftID, true, nil)  —— 本次建了草案
//	(0, false, nil)       —— 该评级不可执行（hold/review）或已有草案（幂等）
//	(0, false, err)       —— 数据缺陷或 DB 错误
//
// # 为什么 created=false 是正常返回而不是错误
//
// hold / review 不生成草案是**设计意图**（文档 §3.4:196），不是失败。
// 返回 error 会让调用方以为「出错了」并重试 —— 而重试永远不会成功。
func CreateDraftFromRating(d *db.DB, rating *db.Rating) (draftID int64, created bool, err error) {
	if rating == nil {
		return 0, false, errors.New("rating 为 nil")
	}

	// ---- ① 可执行性判定（核心安全边界）----
	//
	// ⚠️ 这条判据的失败方式是「静默」：
	// 如果不检查，hold 也会生成草案，而草案会进闸门、进执行 ——
	// 那等于「系统决定买入」而模型说的是「持有」。
	if !isActionableRatingString(rating.Rating) {
		return 0, false, nil
	}

	// ---- ② 标的必须存在 ----
	if rating.InstrumentID == nil {
		return 0, false, fmt.Errorf("%w（rating_id=%d）", ErrRatingNoInstrument, rating.ID)
	}

	// ---- ③ 方向由评级决定 ----
	side, sErr := ratingSide(rating.Rating)
	if sErr != nil {
		// 走到这里说明 isActionableRatingString 和 ratingSide 的判定不一致 ——
		// 那是代码缺陷，不是数据问题。
		return 0, false, fmt.Errorf("内部不一致：%w", sErr)
	}

	// ---- ④ 数量 ----
	//
	// P0 没有仓位模块，用「固定名义额 ÷ 参考价」占位（见 draftQtyFromRating）。
	// 拿不到价格时返回错误而不是跳过 —— 因为「评级可执行但没法算数量」
	// 是数据缺陷，需要人补价格。
	refPrice, qErr := ratingRefPrice(d, *rating.InstrumentID)
	if qErr != nil {
		return 0, false, qErr
	}
	qtyExpr, qErr := draftQtyFromRating(refPrice)
	if qErr != nil {
		return 0, false, fmt.Errorf("%w（rating_id=%d instrument_id=%d）",
			qErr, rating.ID, *rating.InstrumentID)
	}

	// ---- ⑤ 幂等插入 ----
	//
	// 幂等键是 rating_id（唯一索引 uq_orders_draft_rating）。
	// 见 db.InsertDraftForRating 的注释：用 DB 约束而不是应用层查重。
	ratingID := rating.ID
	draftID, created, err = d.OrderDrafts().InsertDraftQtyExpr(db.OrderDraftInput{
		RatingID:     &ratingID,
		InstrumentID: rating.InstrumentID,
		Side:         side,
		// Qty 由 QtyExpr 覆盖（见 InsertDraftQtyExpr）。
		RefPrice: refPrice,
		// risk_verdict 留空 —— 它由闸门（intercept.Evaluate）填，不是这里。
		ExecutedNote: fmt.Sprintf(
			"由评级 #%d（%s）自动生成；数量＝固定名义额 10000 ÷ 参考价 %s（P0 占位，非仓位计算）",
			rating.ID, rating.Rating, refPrice),
	}, qtyExpr)
	return draftID, created, err
}

// isActionableRatingString 判断评级字符串可否生成草案。
//
// # 为什么这里不调 agent.IsActionableRating
//
// `server` 包已经 import `agent`（用 Expand / MaterializeGraph），所以能调。
// 但这里刻意重复那四个值 —— 因为：
//
//   - `agent.IsActionableRating` 的存在意义是「agent 层知道哪些评级能执行」
//   - 本函数的意义是「server 层在建草案前再确认一次」
//
// 两者一致由 TestRatingActionableMatchesAgent 守护。
// 如果只调 agent 的，那这层校验就没有独立价值（等于没有）。
func isActionableRatingString(r string) bool {
	switch r {
	case "buy", "overweight", "underweight", "sell":
		return true
	default:
		return false
	}
}

// ratingRefPrice 取标的的参考价。
//
// # 为什么用标的的最新收盘价
//
// 评级没有价格字段，而草案需要 ref_price 算数量。
// 用标的行情的最新收盘价是当前唯一的可得价格源。
//
// ⚠️ 这是 P0 的简化：真实实现应该用「决策时刻的报价」，
// 而那样的价格需要从行情表按 as_of 取 —— 见 L18。
func ratingRefPrice(d *db.DB, instrumentID int64) (string, error) {
	var price string
	err := d.QueryRow(`
SELECT COALESCE(CAST(close AS TEXT), '') FROM bars
 WHERE instrument_id=$1 AND close IS NOT NULL
 ORDER BY ts DESC LIMIT 1`, instrumentID).Scan(&price)
	if err != nil {
		// 没有行情 —— 返回空串，让 draftQtyFromRating 报 ErrRatingQtyUnavailable。
		// 不在这里报错，因为「没有价格」和「没有行情表」是两件事，
		// 前者由数量计算统一处理。
		return "", nil
	}
	return price, nil
}

// createDraftFromIntentRating 是 D 方案的入口：intent 结束时按「它产出的评级」建草案。
//
// # 为什么在 server 包（而不是 agent 或 db）
//
// 它同时需要：
//   - `db.Node`（intent）
//   - `db.Rating`（评级）
//   - `CreateDraftFromRating`（本文件的函数）
//
// 而 agent 不能 import server，db 不该知道「草案」这个业务概念。
// 所以这层编排只能在 server。
//
// # 为什么返回 error 但调用方只记日志
//
// 见 engine.go 的注释：「草案生成失败不该让 intent 已结束这个事实回滚」。
// 返回 error 是为了让调用方能记录它；而不上抛是因为那不是致命错误。
func createDraftFromIntentRating(d *db.DB, intent *db.Node) error {
	if d == nil || intent == nil {
		return nil
	}
	rating, err := d.Ratings().GetLatestRatingByIntent(intent.ID)
	if err != nil {
		return fmt.Errorf("查 intent #%d 的评级: %w", intent.ID, err)
	}
	if rating == nil {
		// PM 跑完了但没调 emit_rating —— 那是模型行为问题，不是系统错误。
		//
		// 记一条日志便于统计「多少 PM intent 没产出评级」，
		// 因为那个比例是 prompt 有效性的指标之一（与 review fallback 率同类）。
		slog.Warn("PM intent 结束但没有产出评级（模型未调用 emit_rating）",
			"intent", intent.ID)
		return nil
	}

	draftID, created, err := CreateDraftFromRating(d, rating)
	if err != nil {
		return fmt.Errorf("rating #%d → 草案: %w", rating.ID, err)
	}
	if !created {
		// hold/review → 不生成（正常），或幂等命中（重复触发）。
		slog.Info("评级未生成草案（不可执行或已生成过）",
			"intent", intent.ID, "rating_id", rating.ID, "rating", rating.Rating)
		return nil
	}
	slog.Info("评级 → 草案已生成",
		"intent", intent.ID, "rating_id", rating.ID,
		"rating", rating.Rating, "draft_id", draftID)
	return nil
}
