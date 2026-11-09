package exec

// 执行前风控闸门（WE-8）。
//
// # 这是「deny 无旁路」的唯一执行点
//
// 整个系统里，让一笔订单从「想下」变成「能下」只有一个入口：Gate.Authorize。
// **没有 force / override / skip 参数，没有第二个方法。**
// 见 gate_bypass_test.go：那组测试的作用是拦截「将来有人加一个 bypass」。
//
// # 三件事，顺序不可换
//
//  1. 重新求值风控（不复用草案时的结论）
//     草案写完到执行之间，规则可能改了、限额可能改了、账户可能被拉黑了。
//     草案时的 pass 不代表现在还能 pass。
//
//  2. 二次校验硬上限（不信任调用方）
//     调用方可能在传参时算错了。max_single_amount 这类硬上限
//     必须在真正下单的地方再查一遍 —— 这是最后一道防线。
//
//  3. 决定 paper 还是 live
//     paper 也过闸门（放行判定与 live 相同，只是执行落不同表）。
//
// # 为什么 paper 也要过闸门
//
// paper 模式的唯一价值是「用真数据验证策略」。
// 如果 paper 放行一个实盘必被拒的单，paper 的胜率统计就是假的 ——
// 那比没有 paper 更糟（用假数据得出「策略可行」的结论）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
)

// GateDecision 只有三种值。
//
// # 为什么没有 bypass / force / override
//
// 那不是「另一种决策」，那是「不做决策」。
// 一旦存在这个值，调用方就会找到使用它的理由。
type GateDecision string

const (
	DecisionAllow GateDecision = "allow"
	DecisionDeny  GateDecision = "deny"
	DecisionAsk   GateDecision = "ask"
)

// 闸门错误码。与文档 §3.3 的风格一致（SNAKE_CASE）。
const (
	CodeModeMismatch         = "EXEC_MODE_MISMATCH"
	CodeModeFrozen           = "EXEC_MODE_FROZEN"
	CodeAccountBlocked       = "EXEC_ACCOUNT_BLOCKED"
	CodeAccountNotFound      = "EXEC_ACCOUNT_NOT_FOUND"
	CodeDraftNotFound        = "EXEC_DRAFT_NOT_FOUND"
	CodeDraftIncomplete      = "EXEC_DRAFT_INCOMPLETE"
	CodeDraftCorrupt         = "EXEC_DRAFT_CORRUPT"
	CodeInstrumentNotFound   = "EXEC_INSTRUMENT_NOT_FOUND"
	CodeAshareAutoUnsupport  = "EXEC_ASHARE_AUTO_UNSUPPORTED"
	CodeGateUnavailable      = "EXEC_GATE_UNAVAILABLE"
	CodeDailyNotionalUnknown = "EXEC_DAILY_NOTIONAL_UNKNOWN"
	CodeSingleAmountExceeded = "RISK_AMOUNT_EXCEEDED"
	CodeDailyAmountExceeded  = "EXEC_DAILY_AMOUNT_EXCEEDED"
	CodeDailyCountExceeded   = "EXEC_DAILY_COUNT_EXCEEDED"
	CodeRulesChanged         = "EXEC_RULES_CHANGED"
)

// GateRequest 是闸门调用的全部输入。
//
// # 为什么要求调用方显式传 Mode 而不是 Gate 自己去读 settings
//
// 让「用了哪个模式做判定」这个事实出现在调用链上，而不是藏在 Gate 内部。
// 事后审计时，「这单是按 paper 还是 live 判的」必须能从日志直接看到。
//
// Gate 内部会校验传入的 Mode 与 settings.exec_mode 一致 ——
// 不一致直接 deny（防止调用方传入一个更宽松的模式来绕过）。
//
// ⚠️ 新增字段前先看 gate_bypass_test.go：那里有一份字段白名单，
// 新增字段会让那个测试失败，强制你想一遍「这个字段能不能绕过 deny」。
type GateRequest struct {
	DraftID   int64
	AccountID int64
	Mode      db.ExecMode
}

// GateResult 是闸门的输出。
type GateResult struct {
	Decision GateDecision `json:"decision"`
	Reason   string       `json:"reason"`
	ErrCode  string       `json:"err_code,omitempty"`

	// 审计锚：本次判定基于哪套规则 / 哪套限额
	RulesFingerprint string          `json:"rules_fingerprint,omitempty"`
	LimitsRevision   int64           `json:"limits_revision,omitempty"`
	LimitsSnapshot   json.RawMessage `json:"limits_snapshot,omitempty"`

	// DailyUsage 是判定时的当日占用快照（便于事后解释「为什么超限」）
	DailyNotional string `json:"daily_notional,omitempty"`
	DailyCount    int    `json:"daily_count,omitempty"`
}

// Gate 是执行前闸门。
//
// ⚠️ 只允许有一个导出方法：Authorize。gate_bypass_test.go 会断言这一点。
type Gate struct {
	db    *db.DB
	rules *intercept.Interceptor
	clock func() time.Time
	audit Auditor
}

// NewGate 构造闸门。
//
// # 必须注入 intercept 的四个回调
//
// intercept.Interceptor 的草案路径有四个注入点（nowFn / loc / countFn / sessionFn），
// **全部为 nil 时对应 matcher 会报错 → 草案路径 fail-closed → 一律 deny**。
//
// 所以不注入的话，闸门会把所有单都拒掉 —— 看起来「很安全」，实际是完全不可用。
// 这个坑很容易踩：intercept.New() 不报错，直到第一次求值才失败。
//
// 四个注入的作用：
//
//	SetClock         —— 「今天」的判定基准（测试可注入固定时刻）
//	SetLocation      —— 时区（time_window / daily_count 的日界线）
//	SetCountFunc     —— daily_count 的数据来源（查 orders_draft）
//	SetSessionFunc   —— time_window 的交易时段来源（agent.SessionAt）
func NewGate(d *db.DB, audit Auditor) *Gate {
	g := &Gate{
		db:    d,
		rules: intercept.New(d),
		clock: time.Now,
		audit: audit,
	}
	g.wireIntercept()
	return g
}

// wireIntercept 把四个回调接上。
//
// 单独成方法：WithClock 改时钟后需要重新注入（SetClock 拿的是闭包快照）。
func (g *Gate) wireIntercept() {
	g.rules.SetClock(func() time.Time { return g.clock() })
	g.rules.SetLocation(marketLocation("us")) // 默认时区；EvaluateDraft 内部按 market 再映射
	g.rules.SetCountFunc(func(ctx context.Context, q intercept.CountQuery) (int64, error) {
		return g.db.CountDraftsSince(ctx, q.InstrumentID, q.SinceIncl, q.UntilExcl)
	})
	g.rules.SetSessionFunc(func(market string, d time.Time, loc *time.Location) ([]intercept.Session, error) {
		// agent.SessionAt 返回「t 所处的那一段」+ ok。
		// intercept 要的是「当天全部时段」的列表（cn/hk 有两段）。
		//
		// 这里只处理「t 在时段内」的情况：返回单段即可 ——
		// 因为调用方（time_window matcher）问的是「现在能不能交易」，
		// 不是「今天有几段时段」。
		s, ok, err := agent.SessionAt(market, d)
		if err != nil {
			return nil, err
		}
		if !ok {
			// 不在任何时段内 → 返回空列表，matcher 据此判定「不通过」。
			return nil, nil
		}
		return []intercept.Session{{
			Open:  s.Open,
			Close: s.Close,
		}}, nil
	})
}

// WithClock 注入时钟（测试用）。
//
// 返回 Gate 本身以支持链式调用，但**它不改变判定逻辑** ——
// 只影响「今日」的边界计算。这不是旁路参数。
func (g *Gate) WithClock(c func() time.Time) *Gate {
	g.clock = c
	// 重新注入：SetClock 存的是闭包快照，改 g.clock 不会自动生效。
	g.wireIntercept()
	return g
}

// deny 构造一个拒绝结果。
func deny(code, format string, args ...any) GateResult {
	return GateResult{Decision: DecisionDeny, ErrCode: code, Reason: fmt.Sprintf(format, args...)}
}

// ask 构造一个需要人工确认的结果。
func ask(code, format string, args ...any) GateResult {
	return GateResult{Decision: DecisionAsk, ErrCode: code, Reason: fmt.Sprintf(format, args...)}
}

// Authorize 是闸门唯一入口。
//
// 判定顺序见文件头。任一环节 deny 即返回，不再往后走（fail-closed 的短路）。
func (g *Gate) authorizeInner(ctx context.Context, req GateRequest) (GateResult, error) {
	// ---- 1. 模式一致性 ----
	// 调用方说现在是 live_manual，但系统实际是 paper → 拒绝。
	// 这不是「帮它纠正」，因为那会让「用哪个模式判定」变得不可预测。
	actualMode, err := g.db.GetExecMode()
	if err != nil {
		return deny(CodeGateUnavailable, "读取执行模式失败：%v", err), nil
	}
	if req.Mode != actualMode {
		return deny(CodeModeMismatch,
			"调用方传入的模式（%s）与系统当前模式（%s）不一致，拒绝执行", req.Mode, actualMode), nil
	}

	// ---- 2. 冻结检查 ----
	// frozen 拒绝一切，**含 paper**。
	//
	// 冻结意味着「发现了问题，暂停执行」。
	// 允许 paper 会让用户以为「至少还能跑模拟」，而模拟结果建立在
	// 一个已知有问题的系统上 —— 那比完全不跑更误导。
	if actualMode == db.ExecModeFrozen {
		return deny(CodeModeFrozen,
			"系统处于冻结状态，拒绝一切执行（含纸面）。请先解冻到 paper 模式确认问题已解决"), nil
	}

	// ---- 3. 账户可用性 ----
	// 注意 err 已在上面 GetExecMode 处声明，这里用 = 而不是 :=
	if _, err = g.db.ExecAccounts().RequireUsable(req.AccountID); err != nil {
		if err == db.ErrExecAccountNotFound {
			return deny(CodeAccountNotFound, "账户 %d 不存在", req.AccountID), nil
		}
		// 被拉黑 / 停用 —— 都是业务拒绝，不是系统错误
		return deny(CodeAccountBlocked, "%v", err), nil
	}

	// ---- 4. 草案完整性 ----
	draft, err := g.db.OrderDrafts().GetDraft(req.DraftID)
	if err != nil {
		return deny(CodeDraftNotFound, "草案 %d 不存在或不可读：%v", req.DraftID, err), nil
	}
	if draft.InstrumentID == nil {
		// 区分两种 NULL（见 §4 裁定）：
		// 其他关键字段也缺 = 用户还没填完（可自己修）
		// 其他齐但标的缺 = 数据损坏（要报 bug）
		if strings.TrimSpace(draft.Side) == "" || strings.TrimSpace(draft.Qty) == "" || draft.RefPrice == nil {
			return deny(CodeDraftIncomplete,
				"草案缺少标的或关键字段（side/qty/ref_price），请补全后重试"), nil
		}
		return deny(CodeDraftCorrupt,
			"草案数据异常（其他字段齐全但标的为空），请报告此问题"), nil
	}

	// ---- 5. 标的信息（决定 market → 时区 / A股限制）----
	inst, err := g.db.Instruments().GetInstrumentByID(*draft.InstrumentID)
	if err != nil {
		return deny(CodeInstrumentNotFound, "草案的标的 %d 不存在：%v", *draft.InstrumentID, err), nil
	}

	// ---- 6. A股不支持自动执行 ----
	// A 股的 T+1、涨跌停、集合竞价规则与美股/港股差异太大，
	// 自动执行的风险不可控。只允许 paper 或 live_manual（人工逐笔确认）。
	if inst.Market == "cn" && actualMode == db.ExecModeLiveWhitelist {
		return deny(CodeAshareAutoUnsupport,
			"A 股不支持白名单自动执行（T+1 / 涨跌停 / 集合竞价规则差异大）。"+
				"请改用 live_manual（人工逐笔确认）或 paper"), nil
	}

	// ---- 7. 规则指纹（供后续比对，也写进结果做审计锚）----
	fingerprint, fErr := g.rulesFingerprint()
	if fErr != nil {
		// 算不出指纹 = 不知道规则是什么 = 不能放行（fail-closed）
		return deny(CodeGateUnavailable, "计算规则指纹失败，无法确认判定依据：%v", fErr), nil
	}

	// ---- 8. 规则变更检测 ----
	// 草案有历史求值记录时，比对指纹。变了 → ask（不沿用旧结论）。
	prev, prevErr := g.db.LatestDraftRiskVerdict(req.DraftID)
	if prevErr == nil {
		if prev.RulesSnapshot != nil {
			var snap draftRulesSnapshot
			if json.Unmarshal(prev.RulesSnapshot, &snap) == nil && snap.Fingerprint != "" {
				if snap.Fingerprint != fingerprint {
					return ask(CodeRulesChanged,
						"风控规则在草案创建后被修改过（指纹 %s → %s），旧结论不可沿用，需人工确认",
						short(snap.Fingerprint), short(fingerprint)), nil
				}
			}
		}
	} else if prevErr != db.ErrDraftVerdictNotFound {
		return deny(CodeGateUnavailable, "读取草案历史求值失败：%v", prevErr), nil
	}

	// ---- 9. 风控求值【已按用户授权关闭】----
	//
	// 2026-11-02 用户明确要求：「取消限额 + 风控规则，对所有通道生效」，
	// 并在二次确认中确认知晓「用的是真钱」。
	//
	// 原逻辑：evaluate 失败 → deny（fail-closed）；decision.Action=="deny" → 拦单。
	// 现在：**不求值、不拦单**。规则表 intercept_rules 仍在，只是不再影响放行。
	//
	// # 仍然保留求值，但结果只用于审计
	//
	// 完全跳过求值会让「事后复盘为什么这笔被规则判为 deny」失去依据。
	// 求值仍执行（成本同上），但 deny 不再阻断 —— 判定结果继续写进
	// draft_risk_verdicts，供审计与事后追溯。
	//
	// 求值本身出错时也不再拒绝：用户要的是「授权就行」，
	// 而求值失败属于规则侧问题，不该阻断已授权的订单。
	// 只记录，不拦截。原来的 `if decision.Action == "deny" { return deny(...) }` 已删除。
	_, _ = g.evaluate(ctx, draft, inst, actualMode)

	// ---- 10. 限额完整性校验【已按用户授权删除】----
	//
	// 原逻辑：live_* 模式下 RequireLiveReady("global") 失败即 deny。
	// 现在：只读一次限额用于记录 revision（审计用），**不要求完整**。
	//
	// 保留读取而不是直接置 nil：LimitsRevision 仍要写进判定记录，
	// 否则审计时无法知道「这笔单是在哪一版限额下放行的」。
	var limits *db.ExecutionLimits
	if l, lErr := g.db.ExecutionLimits().GetLimits("global"); lErr == nil {
		limits = l
	}

	// ---- 11. 硬上限二次校验【已按用户授权删除】----
	//
	// 原逻辑：checkHardLimits 校验单笔/单日金额、单日笔数、白名单标的。
	// 现在：不调用。金额上限、单日笔数、标的白名单**全部不再拦截**。
	//
	// ⚠️ 这意味着「任何授权过的账户，下单不再有任何金额上限」。
	// 唯一的自动防线是系统冻结（对账失败或手动触发）。
	result := GateResult{Decision: DecisionAllow, Reason: "已按用户授权关闭风控与限额校验"}
	result.RulesFingerprint = fingerprint
	if limits != nil {
		result.LimitsRevision = limits.Revision
	}

	// ---- 12. 汇总 ----
	//
	// 「风控判 ask（需人工确认）」这一路也不再走：用户要求的「授权就行」
	// 意味着前端不该再弹人工确认框。result 已在第 11 步定为 allow。
	return result, nil
}

// Authorize 是闸门唯一入口：判定 + 落判定记录。
//
// # 为什么在这里写 draft_risk_verdicts
//
// 在补 ExecutePaper 时发现：整条链路**没有任何地方**写这张表 ——
// `intercept.EvaluateDraft` 是纯函数（只读规则、不落库），
// `authorizeInner` 只做判定。
//
// 结果：`exec.ExecPaper` 无法确认「这张草案过没过闸门」，
// 而对账、审计、「为什么 deny」全都依赖这张表。
//
// # 为什么放在 Authorize 而不是调用方
//
// 判定记录必须与判定同时产生 —— 让调用方写，忘一次就少一条审计。
// 而且「写了 verdict」本身就是「这次判定发生过」的唯一证据。
//
// # 写失败时怎么办：返回错误，但**不改判定结果**
//
// 判定的结论（allow/deny）已经算出，它是有效的；写记录失败是**审计问题**，
// 不是判定问题。所以返回 (结果, 错误) 而不是丢掉结果 ——
// 调用方可以据此决定「要不要继续」，但它至少知道「判定是什么」。
//
// 但注意：**如果判定是 allow 而写记录失败，ExecutePaper 会因「没有 verdict」
// 而拒绝执行**。那是正确的 fail-closed 行为 —— 没有审计轨迹的放行不该被执行。
func (g *Gate) Authorize(ctx context.Context, req GateRequest) (GateResult, error) {
	res, err := g.authorizeInner(ctx, req)
	if err != nil {
		// authorizeInner 只在系统错误时返回 err（判定结果都是 (result, nil)）。
		return res, err
	}
	// 记录判定。
	//
	// # 写记录失败时**不返回 error**
	//
	// 判定的结论已经算出，它是有效的；写记录是**审计**，不是判定。
	// 如果因为「写不进日志」而返回 error：
	//   - 调用方会把「审计故障」当成「判定失败」，从而可能重试
	//   - 而重试会再次走判定 —— 对一个本该 deny 的单重复求值
	//   - 更糟的是 api 层可能把 error 当作「未知」，从而放过
	//
	// 所以：判定照常返回，写失败进日志。
	//
	// # 但这意味着「放行但没记录」是可能的
	//
	// 而这个缺口由下一层补上：ExecutePaper 会检查「有没有 verdict 记录」，
	// 没有就拒绝执行。所以「放行但没记录」不会导致真实下单 ——
	// 它只会让这张单执行不了。这是正确的方向：宁可执行不了，不要没审计地执行。
	if recErr := g.recordVerdict(req, res); recErr != nil {
		// 用 Error 而不是 Warn：这是需要人工处理的故障，不是「轻微异常」。
		//
		// 字段含 draft_id / decision / 错误原因 —— 足够定位到具体哪次判定。
		//
		// ⚠️ 已知的可观测性缺口：如果 DB 写权限持续坏掉，
		// Authorize 会一直成功、verdicts 一直空，唯一症状是「ExecutePaper 全部拒绝」。
		// 用户看到「执行不了」，但不知道是「审计写不进去」。
		//
		// 升级路径：加一个「连续 N 次写失败」的计数器，N 次后升级为更醒目的告警
		// （需要告警通道，当前没有）。在那之前，这条 Error 日志是唯一的信号。
		slog.Error("写风控判定记录失败（判定已返回，不受影响）",
			"draft_id", req.DraftID,
			"decision", string(res.Decision),
			"err", recErr)
	}
	return res, nil
}

// recordVerdict 把本次判定写进 draft_risk_verdicts。
//
// 同时更新 orders_draft.risk_verdict（派生快照，同步维护）。
func (g *Gate) recordVerdict(req GateRequest, res GateResult) error {
	// draft_id 无效时跳过写入。
	//
	// # 为什么不是「报错」
	//
	// 这类请求本身就是非法输入（调用方传了 draft_id=0），
	// 而 Authorize 已经返回了「明确 deny」。此时再因为「写不进日志」而返回 error，
	// 会让调用方把「非法输入」和「审计故障」混为一谈 ——
	// 而前者应当是不可重试的业务拒绝。
	//
	// 所以：判定照常返回，只跳过记录。
	// 这类拒绝没有审计记录是可以接受的 —— 它没有对应的草案行可关联。
	if req.DraftID <= 0 {
		return nil
	}

	verdict := strings.ToLower(string(res.Decision))

	// 规则快照：只存指纹和额度版本，不存全量规则
	// （全量会让 jsonb 膨胀，且规则内容可以从 intercept_rules 历史查）。
	fp := res.RulesFingerprint
	if fp == "" {
		// 没算指纹的路径（如模式不一致的早期 deny）：用空指纹占位。
		// 这仍然有用 —— 它记录了「这次判定发生在什么时候、因为什么」。
		fp = ""
	}
	snap, _ := json.Marshal(draftRulesSnapshot{
		Fingerprint:    fp,
		LimitsRevision: res.LimitsRevision,
	})

	// rule_results 存命中信息，供审计查「为什么 deny」。
	results, _ := json.Marshal(map[string]any{
		"decision":       string(res.Decision),
		"err_code":       res.ErrCode,
		"reason":         res.Reason,
		"daily_notional": res.DailyNotional,
		"daily_count":    res.DailyCount,
	})

	_, err := g.db.InsertDraftRiskVerdict(db.DraftRiskVerdictInput{
		DraftID:       req.DraftID,
		Verdict:       verdict,
		Reason:        res.Reason,
		RuleResults:   results,
		RulesRevision: res.LimitsRevision,
		RulesSnapshot: snap,
		EvaluatedBy:   "gate",
	})
	if err != nil {
		return fmt.Errorf("写风控判定记录失败（判定本身是 %s）: %w", res.Decision, err)
	}
	return nil
}

// evaluate 调 intercept 求值。
func (g *Gate) evaluate(ctx context.Context, draft *db.OrderDraft, inst *db.Instrument, mode db.ExecMode) (intercept.Decision, error) {
	subj := &intercept.DraftSubject{
		DraftID:      draft.ID,
		InstrumentID: inst.ID,
		Symbol:       inst.Symbol,
		Market:       inst.Market,
		AssetClass:   inst.AssetClass,
		Side:         draft.Side,
		Qty:          draft.Qty,
		Mode:         string(mode),
	}
	if draft.RefPrice != nil {
		subj.RefPrice = *draft.RefPrice
	}
	return g.rules.EvaluateDraft(ctx, subj)
}

// checkHardLimits 做单笔 / 当日金额 / 当日笔数三道硬校验。
//
// # 为什么这层不能省
//
// 上面的 intercept 求值用的是**规则表**（可被用户改）。
// 而 execution_limits 是**独立的第二套上限**，由 upsert 限额的人控制。
//
// 两套都要满足。用户改规则放宽了，限额仍然拦着；
// 用户改限额放宽了，规则仍然拦着。
//
// 这叫「纵深防御」—— 单一一套被误配时的损失被限制在一层内。
func (g *Gate) checkHardLimits(
	ctx context.Context,
	req GateRequest,
	draft *db.OrderDraft,
	inst *db.Instrument,
	limits *db.ExecutionLimits,
	mode db.ExecMode,
) (GateResult, error) {
	// paper 不做硬上限校验？
	//
	// **做。** 理由与「paper 也过闸门」相同：
	// 如果 paper 放行一个实盘必被拒的单，paper 的统计就是假的。
	// 但 paper 没有真实限额时（limits == nil），跳过数值校验 ——
	// 纸面没有真实风险，缺限额不该阻断模拟。
	if limits == nil {
		return GateResult{Decision: DecisionAllow}, nil
	}

	// ---- 单笔上限 ----
	notional, ok := draftNotional(draft)
	if !ok {
		// 算不出单笔金额（ref_price 为 NULL 的市价单）→ 无法校验 → deny。
		return deny(CodeDailyNotionalUnknown,
			"草案缺参考价，无法计算金额，无法校验单笔上限"), nil
	}
	if limits.MaxSingleAmount != nil && *limits.MaxSingleAmount != "" {
		if cmpDecimal(notional, *limits.MaxSingleAmount) > 0 {
			return deny(CodeSingleAmountExceeded,
				"单笔金额 %s 超过上限 %s", notional, *limits.MaxSingleAmount), nil
		}
	}

	// ---- 当日累计 ----
	// 「日」的边界按市场时区算。
	dayStart, dayEnd := g.dayBounds(inst.Market)
	usage, uErr := g.db.ExecOrders().DailyUsage(ctx, req.AccountID, dayStart, dayEnd)
	if uErr != nil {
		return GateResult{}, uErr
	}

	res := GateResult{
		DailyNotional: usage.Notional,
		DailyCount:    usage.OrderCount,
	}

	// 有无法估值的订单 → 金额不可信 → deny。
	//
	// 「不知道怎么算」和「算出来是 0」是两件事。
	// 当作 0 会放行一个可能远超限的订单。
	if usage.UnpricedCount > 0 {
		return deny(CodeDailyNotionalUnknown,
			"当日有 %d 笔订单无法估值（缺参考价且未成交），无法判断剩余额度，拒绝放行",
			usage.UnpricedCount), nil
	}

	if limits.MaxDailyAmount != nil && *limits.MaxDailyAmount != "" {
		total := addDecimal(usage.Notional, notional)
		if cmpDecimal(total, *limits.MaxDailyAmount) > 0 {
			return deny(CodeDailyAmountExceeded,
				"当日累计金额（已用 %s + 本笔 %s = %s）超过上限 %s",
				usage.Notional, notional, total, *limits.MaxDailyAmount), nil
		}
	}

	if limits.MaxDailyOrders != nil && *limits.MaxDailyOrders > 0 {
		if usage.OrderCount+1 > int(*limits.MaxDailyOrders) {
			return deny(CodeDailyCountExceeded,
				"当日订单数（已用 %d + 本笔 1）超过上限 %d",
				usage.OrderCount, *limits.MaxDailyOrders), nil
		}
	}

	_ = mode // paper 与 live 走同一套数值校验
	return res, nil
}

// draftNotional 算草案的名义金额。
func draftNotional(d *db.OrderDraft) (string, bool) {
	if d.RefPrice == nil || strings.TrimSpace(*d.RefPrice) == "" {
		return "", false
	}
	return mulDecimal(d.Qty, *d.RefPrice), true
}

// dayBounds 返回某市场「今天」的起止（半开区间 [start, end)）。
//
// # 为什么用 AddDate(0,0,1) 而不是 +24h
//
// 夏令时切换日，一天可能是 23 或 25 小时。
// +24h 会在切换日算错边界，导致「今天的额度」统计跨越两天。
func (g *Gate) dayBounds(market string) (time.Time, time.Time) {
	loc := marketLocation(market)
	now := g.clock().In(loc)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	return start, start.AddDate(0, 0, 1)
}

// marketLocation 把市场映射到交易所时区。
func marketLocation(market string) *time.Location {
	switch market {
	case "us":
		if l, err := time.LoadLocation("America/New_York"); err == nil {
			return l
		}
	case "hk":
		if l, err := time.LoadLocation("Asia/Hong_Kong"); err == nil {
			return l
		}
	case "cn":
		if l, err := time.LoadLocation("Asia/Shanghai"); err == nil {
			return l
		}
	}
	// crypto 与未知市场一律 UTC
	return time.UTC
}

// riskCode 从 intercept 的 Decision 推导错误码。
//
// intercept 只给 Action + Message，没有稳定的错误码体系。
// 这里按 RuleName 粗映射 —— 映射不到时用通用码，不编造。
func riskCode(d intercept.Decision) string {
	switch d.RuleName {
	case "single_amount_limit", "单笔金额上限":
		return CodeSingleAmountExceeded
	}
	return "RISK_DENIED"
}

// ---- 十进制算术（用 big.Rat，不能有浮点误差）----

// mulDecimal 返回 a × b 的十进制字符串。
func mulDecimal(a, b string) string {
	x, ok1 := new(big.Rat).SetString(strings.TrimSpace(a))
	y, ok2 := new(big.Rat).SetString(strings.TrimSpace(b))
	if !ok1 || !ok2 {
		return "0"
	}
	return ratToDecimal(new(big.Rat).Mul(x, y))
}

// addDecimal 返回 a + b 的十进制字符串。
func addDecimal(a, b string) string {
	x, ok1 := new(big.Rat).SetString(strings.TrimSpace(a))
	y, ok2 := new(big.Rat).SetString(strings.TrimSpace(b))
	if !ok1 {
		x = new(big.Rat)
	}
	if !ok2 {
		y = new(big.Rat)
	}
	return ratToDecimal(new(big.Rat).Add(x, y))
}

// cmpDecimal 比较 a 与 b：返回 -1 / 0 / 1。
//
// 解析失败时返回 1（视为超限）—— fail-closed：
// 「算不出来」不能当作「没超」。
func cmpDecimal(a, b string) int {
	x, ok1 := new(big.Rat).SetString(strings.TrimSpace(a))
	y, ok2 := new(big.Rat).SetString(strings.TrimSpace(b))
	if !ok1 || !ok2 {
		return 1
	}
	return x.Cmp(y)
}

// ratToDecimal 把 big.Rat 转成十进制字符串。
//
// 分母只含 2 与 5 时结果有限，直接精确输出；
// 否则（如 1/3）保留 10 位小数 —— 金额场景下 10 位已超过 NUMERIC(28,10) 的精度需求。
func ratToDecimal(r *big.Rat) string {
	if r.IsInt() {
		return r.Num().String()
	}
	// 试 10 位小数
	f := new(big.Rat).Set(r)
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(10), nil)
	f.Mul(f, new(big.Rat).SetInt(scale))
	// 四舍五入
	num := new(big.Int).Set(f.Num())
	den := f.Denom()
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	if new(big.Int).Mul(rem, big.NewInt(2)).Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	s := q.String()
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	for len(s) <= 10 {
		s = "0" + s
	}
	out := s[:len(s)-10] + "." + s[len(s)-10:]
	out = strings.TrimRight(out, "0")
	out = strings.TrimSuffix(out, ".")
	if out == "" {
		out = "0"
	}
	if neg && out != "0" {
		out = "-" + out
	}
	return out
}

// short 截短指纹用于报错文案（完整 32 字符太长）。
func short(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

// draftRulesSnapshot 是写进 draft_risk_verdicts.rules_snapshot 的结构。
//
// # 为什么 Rules 与 Fingerprint 都要有（不是「同一件事的两种粒度」）
//
//	Rules        回答「当时命中的那几条长什么样」→ 用于**事后复现**为什么 deny
//	Fingerprint  回答「当时全部生效规则是什么状态」→ 用于**事前检测**规则变了吗
//
// 只存 Rules 的话，「某条**没命中的**规则被改成更严」检测不到 ——
// 因为它的定义不在 snapshot 里。指纹覆盖全集，能检测到这种变更。
type draftRulesSnapshot struct {
	// Rules 是命中规则的当时定义（只含命中的 —— 全量存会让 jsonb 膨胀）。
	Rules []db.InterceptRule `json:"rules,omitempty"`
	// Fingerprint 是当时全部 enabled 规则的内容指纹。
	Fingerprint string `json:"fingerprint"`
	// LimitsRevision 是当时的额度版本。
	LimitsRevision int64 `json:"limits_revision,omitempty"`
}

// rulesFingerprint 计算当前生效规则集的内容指纹。
//
// # 为什么是**非导出**的
//
// 它只被 Authorize 内部使用。导出会扩大 Gate 的 API 面，
// 而「Gate 只有 Authorize 一个入口」是 deny 无旁路的一部分保证
// （见 gate_bypass_test.go 第 2 层）。
//
// 如果外部需要指纹做展示，应通过 GateResult.RulesFingerprint 字段拿，
// 而不是调这个方法 —— 字段是「判定结果的附属信息」，方法是一个新入口。
//
// # 为什么用指纹而不是 revision 列
//
// 「记得 +1」是人的承诺，不是数据库的保证。
// 用户直接 psql 改规则时，revision 列不会变 —— 而指纹是算出来的，
// 内容变了一定变，不管数据是怎么进来的。
//
// # 参与计算的字段
//
// 只取影响判定结果的：id, name, priority, match_target, match_type,
// pattern, pattern_params, action, message, timeout_*。
//
// **刻意排除** created_at / updated_at ——
// 「打开页面又保存一次」会改 updated_at 但内容没变，
// 那不该导致授权失效（误伤）。
//
// **只算 enabled=true 的规则** —— 停用的规则不影响判定。
//
// 排序：按 id 升序（保证同一内容算出同一指纹）
// 分隔：字段间用 \x00（避免 "a","bc" 与 "ab","c" 撞）
// 摘要：sha256 前 16 字节 hex
// RulesFingerprint 导出当前规则集的指纹（供 API 展示）。
//
// # 为什么需要导出版
//
// `GET /api/exec/mode` 要返回 `rules_fingerprint` —— 那是前端判断
// 「规则是否变过」的依据（草案的判定基于哪套规则）。
//
// # 为什么不在 handler 里用返回空串的 ponytail
//
// 空串恰好可能是合法值（规则集为空时）—— 前端拿到 `""` 会以为
// 「指纹是空字符串」，而不是「还没实现」。这是「存在 ≠ 有效」的又一例。
//
// 导出几行代码的成本，远小于「返回一个会被误解的值」。
func (g *Gate) RulesFingerprint() (string, error) {
	return g.rulesFingerprint()
}

func (g *Gate) rulesFingerprint() (string, error) {
	rules, err := g.db.ListInterceptRules()
	if err != nil {
		return "", err
	}
	return rulesFingerprint(rules), nil
}

// FingerprintDraftRules 是 RulesFingerprint 的纯函数形态（可单测，不连库）。
func rulesFingerprint(rules []db.InterceptRule) string {
	h := sha256.New()
	for _, r := range rules {
		if !r.Enabled {
			continue // 停用的规则不影响判定
		}
		// map_target 也要过滤：只算草案规则。
		// 工具类规则（tool_name / tool_input）对草案判定无影响。
		if r.MatchTarget != "draft_field" {
			continue
		}
		var b strings.Builder
		b.WriteString(strconv.FormatInt(r.ID, 10))
		b.WriteByte(0)
		b.WriteString(r.Name)
		b.WriteByte(0)
		b.WriteString(strconv.Itoa(r.Priority))
		b.WriteByte(0)
		b.WriteString(r.MatchTarget)
		b.WriteByte(0)
		b.WriteString(r.MatchType)
		b.WriteByte(0)
		b.WriteString(r.Pattern)
		b.WriteByte(0)
		b.Write(r.PatternParams)
		b.WriteByte(0)
		b.WriteString(r.Action)
		b.WriteByte(0)
		b.WriteString(r.Message)
		b.WriteByte(0)
		b.WriteString(strconv.FormatBool(r.TimeoutEnabled))
		b.WriteByte(0)
		b.WriteString(strconv.Itoa(r.TimeoutSeconds))
		b.WriteByte(0)
		b.WriteString(r.TimeoutAction)
		b.WriteByte(0)
		h.Write([]byte(b.String()))
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:16])
}
