package agent

// 六值评级解析（W3-14）。
//
// # 六值的唯一事实来源是 DB CHECK
//
// `ARCH-QuantEx.md:451` 的 ratings 表：
//
//	CHECK (rating IN ('Buy','Overweight','Hold','Underweight','Sell','REVIEW'))
//
// **没有 StrongBuy** —— 强度的表达靠 Overweight（增持）。
//
// # 六值分两层
//
// 文档 §3.4:196：
//
//	校验评级 ∈ {Buy, Overweight, Underweight, Sell}
//	Hold / REVIEW 直接拒绝 EXEC_RATING_NOT_ACTIONABLE
//
// 所以「可执行」只有 4 个。见 IsActionableRating。
//
// # 为什么解析要严格（不 trim、不忽略大小写）
//
// "buy" 和 "Buy" 在人类看来是同一个，但模型输出的大小写是**信号**：
// 它说明模型没有严格遵循格式。而「格式不严格」往往伴随「内容不可靠」。
//
// 严格匹配的代价是「模型偶尔输出小写就进 REVIEW」—— 那是可接受的，
// 因为 REVIEW 的代价（人工看一眼）远小于「格式混乱但被当成有效评级」。

import (
	"fmt"
	"log/slog"
	"strings"
)

// 六值常量 —— 与 ratings 表的 CHECK 约束**逐字一致**（DB 是唯一强制的层）。
//
// ⚠️ 曾经这里写的是 "Buy" / "REVIEW"（大小写混合），而 DB CHECK 是小写。
// 那个不一致从未暴露，因为 ParseRating 一直是零调用（见 L9）。
//
// 实际后果（实测于 2026-10-28）：
//
//	INSERT INTO ratings(rating,...) VALUES('Buy',...)
//	→ ERROR: violates check constraint "ratings_rating_check"
//
// 为什么以 DB 为准而不是改 DB：
//
//	DB CHECK 已上线且是唯一强制层；改 DB 要迁移 + 影响已有数据。
//	而 Go 常量和文档都没有强制力，改它们的成本是零。
//
// 为什么 Go 和文档"一致"反而更危险：
//
//	两者都是大写 → 互相印证 → 制造「这是对的」的假象。
//	而唯一的真相在 DB，DB 的 CHECK 定义没人会主动去读。
//
// 一致性由 TestRatingConstantsMatchDBCheck 守护（它直查 pg_get_constraintdef）。
const (
	RatingBuy         = "buy"
	RatingOverweight  = "overweight"
	RatingHold        = "hold"
	RatingUnderweight = "underweight"
	RatingSell        = "sell"
	RatingReview      = "review"
)

// allRatings 是六值的完整集合。
//
// 用 map 而不是 slice：查表 O(1)，而这里每次解析都要查一次。
var allRatings = map[string]bool{
	RatingBuy:         true,
	RatingOverweight:  true,
	RatingHold:        true,
	RatingUnderweight: true,
	RatingSell:        true,
	RatingReview:      true,
}

// actionableRatings 是「能生成草案」的评级集合。
//
// 文档 §3.4:196 明确列了这 4 个；Hold / REVIEW 被拒（EXEC_RATING_NOT_ACTIONABLE）。
var actionableRatings = map[string]bool{
	RatingBuy:         true,
	RatingOverweight:  true,
	RatingUnderweight: true,
	RatingSell:        true,
}

// RatingSource 记录归一后的值从哪来。
//
// # 为什么需要它（这是本类型存在的唯一理由）
//
// 只返回 Rating 的话，"REVIEW" 输入和 "垃圾输入" 都返回 "REVIEW" ——
// **测试无法区分「走对了分支」和「走了 fallback」。**
//
// 而这两种情况在业务上不同：
//
//	SourceLiteral  —— 模型明确说了「我不确定」
//	SourceFallback —— 模型说了胡话，我们替他归到「需人工看」
//
// 前者说明模型**理解**了任务但不愿给结论；后者说明模型**没理解**输出格式。
// 统计这两种的比例，能回答「prompt 是否有效」。
type RatingSource int

const (
	// SourceLiteral 表示输入本身就是合法六值之一。
	SourceLiteral RatingSource = iota
	// SourceFallback 表示输入非法，被归一为 REVIEW。
	SourceFallback
)

// String 让日志和审计可读。
func (s RatingSource) String() string {
	switch s {
	case SourceLiteral:
		return "literal"
	case SourceFallback:
		return "fallback"
	default:
		return fmt.Sprintf("RatingSource(%d)", int(s))
	}
}

// ParseResult 是一次解析的结果。
type ParseResult struct {
	// Rating 是归一后的六值之一。非法输入归一为 RatingReview。
	Rating string
	// Source 记录这个值从哪来（见 RatingSource 的注释）。
	Source RatingSource
	// Raw 是原始输入（已保留，供审计与排查）。
	//
	// # 为什么要留原文
	//
	// 当 Source=SourceFallback 时，运维需要知道「模型到底输出了什么」——
	// 那是改进 prompt 的唯一依据。只记「归一到 REVIEW」等于把线索丢了。
	Raw string
}

// ParseRating 解析模型输出的评级。
//
// # 严格匹配，不做任何归一
//
// 不 trim、不忽略大小写、不做同义词映射、不提取前缀。原因见包注释。
//
// # 非法输入返回 REVIEW（不是 error）
//
// # 为什么不是 error
//
// error 会让调用方决定「怎么办」，而那里没有上下文。
// REVIEW 是明确的「交给人工」，这正是未知值该走的路。
//
// 而 error 的一个后果：pipeline 会中断，整条链白跑。
// REVIEW 的后果：链继续，但该标的不生成草案。
//
// # 为什么空字符串也是 REVIEW（不是 Hold）
//
// Hold = 「模型判断应该持有」。
// 空 = 「模型没给出判断」。
//
// 两者在业务上完全不同：前者是一个有效结论，后者是缺失。
// 混同会让统计把「模型没答」算成「模型建议持有」。
func ParseRating(raw string) ParseResult {
	// 精确匹配：raw 必须与六值之一逐字相同。
	if allRatings[raw] {
		return ParseResult{Rating: raw, Source: SourceLiteral, Raw: raw}
	}

	// 其余一律 REVIEW。
	//
	// 注意这里**包含**「已 trim 后等于 REVIEW」的情况 ——
	// 例如 " REVIEW"（前导空格）也算 fallback，因为它不是逐字匹配。
	//
	// # 为什么这里打 warn 日志
	//
	// 现阶段要回答的问题只有一个：「prompt 有没有让模型稳定输出六值」。
	// 而这个问题用 `grep 'rating_fallback' data/artex.log | wc -l` 就能回答。
	//
	// # 为什么是日志而不是表列
	//
	// 表列要改 DDL + 所有读路径，而 W3-12/13 还没接线 —— 那时可能发现
	// 「合规率该按 agent 统计」而不是按 rating 统计。过早定 schema 会返工。
	//
	// 升级路径：如果将来要在 UI 上显示合规率，那时加 rating_parse_log 表
	// 并从日志回填 —— 日志里的 raw 是回填的唯一依据。
	//
	// # 日志必须带 raw
	//
	// 那是改进 prompt 的唯一依据。只记「归一到 REVIEW」等于把线索丢了。
	slog.Warn("评级解析落入 fallback（模型输出非六值）",
		"raw", raw,
		"normalized", RatingReview,
		"hint", "检查 prompt 是否明确要求「只输出六个值之一，逐字匹配」")

	return ParseResult{Rating: RatingReview, Source: SourceFallback, Raw: raw}
}

// IsActionableRating 判断一个评级能否生成草案。
//
// # 为什么 Hold 不可执行
//
// Hold = 「持有不动」。而「不动」不需要下单 —— 下任何单都会改变持仓。
//
// # 为什么 REVIEW 不可执行
//
// REVIEW = 「需要人工看」。在人类看完之前，系统不该替他下单。
//
// # 为什么 default 返回 false
//
// 未知值不给执行权 —— 保守方向。
// 如果将来有人加了第七个评级但忘了加进 actionableRatings，
// 这里会静默拒绝（而不是静默放行）。
func IsActionableRating(rating string) bool {
	return actionableRatings[rating]
}

// ValidateRatingStrict 校验一个字符串是否是合法六值（逐字匹配）。
//
// 与 ParseRating 的区别：它返回 error 而不是归一。
// 用于「写入前的校验」——例如 `emit_rating` 工具的参数校验。
func ValidateRatingStrict(raw string) error {
	if allRatings[raw] {
		return nil
	}
	// 给出可操作的错误信息：列出合法值 + 指出输入有什么问题。
	//
	// # 为什么错误信息要能区分「格式问题」和「值问题」
	//
	// 如果只是 "invalid rating"，模型（或人）不知道是「该写 Buy 却写了 buy」
	// 还是「根本不该是评级」。而提示里带上「逐字匹配」这个说明，
	// 能让格式问题自己暴露。
	hint := ""
	if trimmed := strings.TrimSpace(raw); allRatings[trimmed] {
		hint = fmt.Sprintf("（%q 去掉首尾空白后是合法值 %q —— 评级必须逐字匹配，不含空白）",
			raw, trimmed)
	} else if lower := strings.ToLower(trimmed); allRatings[titleCaseRating(lower)] {
		hint = fmt.Sprintf("（%q 大小写不匹配 —— 评级区分大小写，应为 %q）",
			raw, titleCaseRating(lower))
	}
	return fmt.Errorf("评级 %q 不是六值之一（合法值：%s / %s / %s / %s / %s / %s；逐字匹配）%s",
		raw, RatingBuy, RatingOverweight, RatingHold,
		RatingUnderweight, RatingSell, RatingReview, hint)
}

// titleCaseRating 把全小写的评级还原成规范形式（仅用于错误信息的提示）。
func titleCaseRating(lower string) string {
	switch lower {
	case "buy":
		return RatingBuy
	case "overweight":
		return RatingOverweight
	case "hold":
		return RatingHold
	case "underweight":
		return RatingUnderweight
	case "sell":
		return RatingSell
	case "review":
		return RatingReview
	default:
		return lower
	}
}
