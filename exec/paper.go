package exec

// 纸面执行（WE-9）：把一张已通过闸门的草案落成 paper 订单并立即成交。
//
// # 设计原则：差异化只在「执行」，不在「放行」
//
// paper 模式也走 `Gate.Authorize`，与 live 完全同一套判定。
//
// 理由：paper 的唯一价值是「用真实数据验证策略」。
// 如果 paper 放行一个实盘必被拒的单，paper 的胜率统计就是假的 ——
// 你会以为策略可行，上实盘才发现根本下不进去。
//
// 所以 ExecutePaper **不再求值**：求值只做一次，在 Gate.Authorize 里。
// 它只做「确认闸门结论仍然有效」的前置检查，然后落单。
//
// # 为什么不能把 Gate.Authorize 包进 ExecutePaper 的事务里
//
// Authorize 内部要读 5+ 张表、跑 intercept matcher、并写 draft_risk_verdicts
// （它自己开事务并提交）。嵌套事务做不了；而且长事务会锁住 orders_draft 行，
// 将来接行情后风险更大。
//
// 所以是「两个事务 + 前置条件检查」，不是「一个大事务」。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// ErrPaperNotAuthorized 表示草案没有有效的闸门通过记录。
//
// 调用方应把它当作「流程错误」而不是「风控拒绝」：
// 风控拒绝有 ErrCode（如 EXEC_RISK_DENIED），而这个错误意味着
// **调用顺序错了** —— 没有先过闸门就想执行。
var ErrPaperNotAuthorized = errors.New("draft not authorized for execution")

// ErrPaperAlreadyExecuted 表示这张草案已经有执行记录了。
//
// 不是「重复下单」的保护 —— 而是「一次草案只执行一次」的语义。
// 要再下就新建一张草案（新的风控评估）。这与 live 的语义一致。
var ErrPaperAlreadyExecuted = errors.New("draft already executed")

// ExecutePaper 把一张已通过闸门的草案落成 paper 订单，并立即全额成交。
//
// # 前置条件（三个，缺一不可）
//
//  1. `draft_risk_verdicts` 的最新一条 verdict == "allow"
//  2. 那条记录的 rules_revision 与当前限额版本一致
//  3. 草案尚未被执行过（`orders_draft.executed_note` 为空）
//
// # 为什么检查的是 verdicts 表而不是 orders_draft.risk_verdict
//
// `orders_draft.risk_verdict` 是派生快照，可能滞后（比如写入失败时）。
// 权威是 `draft_risk_verdicts` 表 —— 那是每次求值的原始记录。
//
// # 为什么还要比 rules_revision
//
// 闸门通过后到执行之间，限额可能被改过。
// 「刚才过了」不等于「现在还能过」—— 如果限额变严了，这张单可能不再合法。
//
// 这一层不重新求值规则（那是 Gate 的事），只确认「依据没变」。
//
// # 虚拟账本的口径（刻意简化）
//
//	fill_price = draft.ref_price    不模拟滑点
//	filled_qty = draft.qty          假设全部成交，不做部分成交
//	status     = filled             立即成交
//	fee        = 不记               见下方说明
//
// ponytail: 不做滑点 / 部分成交 / 手续费模型。
//
//	升级路径：
//	  - 滑点：加 slippage_model 表，按 market+asset_class 查，在 fill_price 上叠加
//	  - 部分成交：需要「成交量分布」参数，那属于回测范畴，不在执行层
//	  - 手续费：加 exec_accounts.fee_rate + paper_orders.fee，
//	    费率按 channel + market 查（不同交易所/市场不同）
//
// # 为什么 fee 不记
//
// 当前 paper_orders 没有 fee 列，exec_accounts 没有费率字段。
// paper 的盈亏因此是「毛盈亏」—— 胜率偏高，高频策略差异更大
// （每笔 0.1%，来回 0.2%，100 笔就是 20% 的成本差）。
//
// 为什么不先用一个常量费率算：那会让回测结果「看起来精确」——
// 而比「明显少了什么」更危险。用户会信它。
//
// 调用方展示 paper 收益时必须标明「不含手续费」（见 EquityCurve.FeeIncluded）。
func (g *Gate) ExecutePaper(ctx context.Context, draftID, accountID int64) (paperOrderID int64, err error) {
	if draftID <= 0 {
		return 0, fmt.Errorf("%w: draft_id 必须为正，实际 %d", ErrPaperNotAuthorized, draftID)
	}
	if accountID <= 0 {
		return 0, fmt.Errorf("account_id 必须为正，实际 %d", accountID)
	}

	// ---- 前置条件 1+2：闸门结论仍然有效 ----
	verdict, err := g.db.LatestDraftRiskVerdict(draftID)
	if err != nil {
		if errors.Is(err, db.ErrDraftVerdictNotFound) {
			return 0, fmt.Errorf(
				"%w: 草案 %d 没有任何风控记录 —— 必须先调 Gate.Authorize",
				ErrPaperNotAuthorized, draftID)
		}
		return 0, fmt.Errorf("读风控记录失败: %w", err)
	}
	if verdict.Verdict != verdictAllow {
		return 0, fmt.Errorf(
			"%w: 草案 %d 最近判定是 %q（不是 allow）—— 不能执行",
			ErrPaperNotAuthorized, draftID, verdict.Verdict)
	}

	// 限额版本核对。
	//
	// 读当前版本而不是缓存：限额可能刚被改过。
	//
	// GetLimits 在「金额上限为 NULL」时返回 (记录, ErrLimitNotConfigured) ——
	// 那是合法状态（paper 允许未配置限额，live 不允许），所以这里要区分：
	// 有记录就比版本，无记录（ErrExecutionLimitsNotFound）表示从没配过，
	// 此时 verdict 的 revision 一定来自「未配置」的求值，不做比对。
	curLimits, lErr := g.db.ExecutionLimits().GetLimits(limitsKeyGlobal)
	if lErr == nil || errors.Is(lErr, db.ErrLimitNotConfigured) {
		if curLimits != nil && verdict.RulesRevision != curLimits.Revision {
			return 0, fmt.Errorf(
				"%w: 草案 %d 的判定基于限额版本 %d，当前是 %d —— 限额已变更，必须重新过闸门",
				ErrPaperNotAuthorized, draftID, verdict.RulesRevision, curLimits.Revision)
		}
	} else if !errors.Is(lErr, db.ErrExecutionLimitsNotFound) {
		return 0, fmt.Errorf("读限额失败: %w", lErr)
	}

	// ---- 读草案 ----
	draft, err := g.db.OrderDrafts().GetDraft(draftID)
	if err != nil {
		return 0, fmt.Errorf("读草案失败: %w", err)
	}

	// ---- 前置条件 3：未被执行过 ----
	if draft.ExecutedNote != nil && strings.TrimSpace(*draft.ExecutedNote) != "" {
		return 0, fmt.Errorf("%w: 草案 %d 已执行过（%s）",
			ErrPaperAlreadyExecuted, draftID, *draft.ExecutedNote)
	}

	// ---- 账户可用 ----
	// RequireUsable 会检查 blocked_at 与 active。
	// 这里再查一次是因为「闸门通过」到「执行」之间账户可能被冻结。
	if _, err := g.db.ExecAccounts().RequireUsable(accountID); err != nil {
		return 0, fmt.Errorf("账户不可用: %w", err)
	}

	// ---- 成交价：必须有 ref_price ----
	//
	// 没有 ref_price 就无法确定成交价，而「用一个猜的价格成交」会污染 paper 收益。
	// 所以拒绝，而不是回退到 0 或某个默认值。
	if draft.RefPrice == nil || strings.TrimSpace(*draft.RefPrice) == "" {
		return 0, fmt.Errorf(
			"%w: 草案 %d 没有 ref_price —— paper 成交必须有参考价，否则收益无意义",
			ErrPaperNotAuthorized, draftID)
	}

	// ---- 落单 + 标记草案已执行（一个事务两件事）----
	//
	// 这两件事必须同事务：只落单不标记 → 下次能重复执行；
	// 只标记不落单 → 草案永远执行不了。
	note := fmt.Sprintf("paper executed: draft=%d account=%d", draftID, accountID)
	paperOrderID, err = g.db.PaperOrders().InsertFilledOrder(db.PaperOrderInput{
		TaskID:       draft.TaskID,
		SignalID:     draft.SignalID,
		InstrumentID: draft.InstrumentID,
		AccountID:    &accountID,
		Mode:         db.OrderModePaper,
		Side:         draft.Side,
		Qty:          draft.Qty,
		Price:        *draft.RefPrice,
		Status:       db.OrderStatusFilled,
	}, draftID, note)
	if err != nil {
		return 0, err
	}
	return paperOrderID, nil
}

// verdictAllow 是 draft_risk_verdicts 表里「放行」的存储值。
//
// ⚠️ 注意措辞差异：GateResult.Decision 用 "allow"（intercept 的词汇），
// 而 draft_risk_verdicts.verdict 存的是 "pass"（db 层的归一，见
// db/draft_risk_verdicts.go 的 validVerdictActions 注释）。
//
// 这里必须是存储值而不是 Decision 值 —— 因为我们读的是表。
const verdictAllow = "pass"

// limitsKeyGlobal 是全局限额配置集在 execution_limits 里的 key。
//
// 与 exec/gate.go 的 RequireLiveReady("global") 保持一致 ——
// 两处用同一个 key，否则「闸门读的限额」和「执行核对的限额」不是同一组。
const limitsKeyGlobal = "global"

// EquityCurve 是 paper 的资金曲线，附带「口径说明」。
//
// # 为什么 FeeIncluded 是字段而不是注释
//
// 注释会被忽略，API 返回值会被消费（前端会读它渲染「不含手续费」的提示）。
// 将来加了 fee，这个字段改成 true，调用方的 UI 会自动更新。
type EquityCurve struct {
	Points []*db.EquityPoint
	// FeeIncluded 当前恒为 false —— paper 不记手续费。
	//
	// 调用方展示收益时必须标明这一点，否则用户会以为那是净收益。
	FeeIncluded bool
}

// GetPaperEquityCurve 读 paper 资金曲线，并明确标注「不含手续费」。
func (g *Gate) GetPaperEquityCurve(_ context.Context) (EquityCurve, error) {
	pts, err := g.db.PaperOrders().GetEquityCurve(db.OrderModePaper)
	if err != nil {
		return EquityCurve{}, err
	}
	return EquityCurve{Points: pts, FeeIncluded: false}, nil
}

// 让 json 包被使用（LimitsSnapshot 等字段的类型需要它）。
var _ = json.Marshal
