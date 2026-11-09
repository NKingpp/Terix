package exec

// 执行模式状态机（WE-3）。
//
// # 这个包管什么，不管什么
//
// 管：
//   - 模式转换的**校验**（五条硬规则）
//   - 进程启动时的**降级**（非 paper → paper，带审计）
//
// 不管：
//   - 落库（那是 db 包的 TransitionMode）
//   - 下单（那需要通道适配器，是 WE-5/WE-6）
//
// 这样拆的理由：模式校验是纯逻辑（输入一组事实，输出放行/拒绝 + 理由），
// 可以用假数据把五条规则全部测到，不需要真通道。

import (
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// 模式校验的错误码。与文档 §3.3 的错误码风格一致（SNAKE_CASE）。
const (
	// CodeModeFrozenRejectsLive frozen → live_* 被拒。
	CodeModeFrozenRejectsLive = "EXEC_MODE_FROZEN"
	// CodeModeTransitionInvalid 转换不在状态机表里。
	CodeModeTransitionInvalid = "EXEC_MODE_TRANSITION_INVALID"
	// CodeAuthRequired 升级到 live 缺少有效授权。
	CodeAuthRequired = "EXEC_AUTH_REQUIRED"
	// CodeAuthStale 授权快照与当前限额/规则不一致，需重新授权。
	CodeAuthStale = "EXEC_AUTH_STALE"
	// CodeLimitsIncomplete 限额不完整（任一 max_* 为 NULL）。
	CodeLimitsIncomplete = "EXEC_LIMITS_INCOMPLETE"
)

// ModeError 是模式校验失败的结构化错误。
//
// # 为什么用结构化错误而不是 fmt.Errorf
//
// 调用方（API handler / 前端）需要区分「被冻结」和「授权过期」——
// 前者要去解冻，后者要重新授权。字符串匹配做不到这个，
// 而且文案会改。
type ModeError struct {
	Code   string
	Reason string
}

func (e *ModeError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Reason)
}

// Is 让 errors.Is 能按 code 匹配。
//
// 用法：errors.Is(err, &exec.ModeError{Code: exec.CodeModeFrozenRejectsLive})
func (e *ModeError) Is(target error) bool {
	t, ok := target.(*ModeError)
	if !ok {
		return false
	}
	return e.Code == t.Code
}

// Auditor 是审计写入的抽象。
//
// # 为什么是接口而不是直接写 activity 表
//
// 实际 schema 里 `activity` 表有 `exploration_id NOT NULL` —— 它绑定到
// 探索会话，而模式变更是**系统级**事件，没有探索上下文可挂。
//
// 把审计做成接口：
//   - 生产实现可以写 activity（挂某个系统级 exploration）或 server_logs
//   - 测试实现可以只记录调用次数
//   - 存储位置待定时，代码可以先写完
//
// **审计失败必须导致操作失败**（硬规则 5）—— 所以 Write 返回 error，
// 调用方必须检查，不能吞掉。
type Auditor interface {
	// WriteModeChange 记录一次模式变更。
	//
	// from/to 是模式值，reason 是变更原因（给人看）。
	// 实现方负责补时间戳与操作者。
	WriteModeChange(from, to db.ExecMode, reason string) error
}

// TransitionRequest 是一次模式转换请求的全部事实。
//
// # 为什么事实都用值传进来，而不是让本包去查库
//
// 目的是让五条规则成为**纯函数**，可以用任意组合测试：
// 「授权快照过期」「限额缺一个字段」这些场景不需要真的造数据。
type TransitionRequest struct {
	From db.ExecMode
	To   db.ExecMode

	// Auth 是当前有效授权；为 nil 表示没有授权。
	// 升级到 live_* 时必须有。
	Auth *db.ExecAuthorization

	// CurrentLimits 是当前生效的限额；为 nil 表示未配置。
	// 升级到 live_* 时必须有完整的 max_*。
	CurrentLimits *db.ExecutionLimits

	// CurrentRulesRevision 是当前规则版本。
	// 与 Auth.RulesRevision 不一致时，授权视为过期（需重新授权）。
	CurrentRulesRevision int64

	// SkipLiveGuards 跳过「升级 live_* 需有效授权 + 限额完整」两条前置校验。
	// 用于执行模式页的「点应用即切换」：选好目标模式后直接生效。
	// 合法转换校验（规则 1）与审计写入（规则 5）仍然保留。
	// 默认 false —— 其它调用方仍走完整的授权/限额链。
	SkipLiveGuards bool
}

// ValidateTransition 校验一次模式转换（五条硬规则的 1/2/3 在这里）。
//
// # 五条规则的分工
//
//	规则 1 frozen → live_* 拒绝         → 本函数（纯查表 + 显式分支）
//	规则 2 升级 live 需有效授权           → 本函数（isLiveUpgrade + 三重一致）
//	规则 3 限额不完整时拒绝 live          → 本函数（ValidateLimits）
//	规则 4 重启降 paper                  → DowngradeOnStartup
//	规则 5 审计失败导致转换失败            → ApplyTransition
//
// 前三条是**纯判断**（不改任何状态），所以放这里；
// 后两条涉及副作用，放 ApplyTransition / DowngradeOnStartup。
func ValidateTransition(req TransitionRequest) error {
	if !db.CanTransitionExecMode(req.From, req.To) {
		// frozen → live_* 是最常见的一条，给它单独的 code，
		// 因为调用方的处置完全不同（要去解冻，而不是「转换写错了」）。
		if req.From == db.ExecModeFrozen && isLive(req.To) {
			return &ModeError{
				Code:   CodeModeFrozenRejectsLive,
				Reason: "系统处于冻结状态，必须先回到 paper 模式确认问题已解决",
			}
		}
		return &ModeError{
			Code:   CodeModeTransitionInvalid,
			Reason: fmt.Sprintf("%s → %s 不是合法转换", req.From, req.To),
		}
	}

	// 只有「升级到 live」才需要授权与限额。
	// 降级（live → paper）不需要 —— 收紧永远允许，这是 fail-safe 的方向。
	if !isLiveUpgrade(req.From, req.To) {
		return nil
	}

	// 直接切换模式：跳过规则 2/3（授权仪式、三重一致、限额完整性）。
	// 规则 1（合法转换）在上面已校验，规则 5（审计）在 ApplyTransition。
	if req.SkipLiveGuards {
		return nil
	}

	// 规则 2：必须有未撤销的授权 + 三重一致。
	if req.Auth == nil {
		return &ModeError{
			Code:   CodeAuthRequired,
			Reason: "升级到实盘模式需要一次完整的授权确认（阅读风险告知并确认）",
		}
	}
	// 授权的目标模式必须覆盖这次要升到的模式。
	//
	// 为什么要有这层：用户可能只授权了 live_whitelist（白名单），
	// 现在想升 live_manual（每笔人工确认）。后者约束更松（不需要白名单），
	// 所以必须重新授权。
	if !authCovers(req.Auth.Mode, string(req.To)) {
		return &ModeError{
			Code: CodeAuthStale,
			Reason: fmt.Sprintf("当前授权只覆盖 %s，升级到 %s 需要重新授权",
				req.Auth.Mode, req.To),
		}
	}
	// 三重一致的第三条：规则版本。
	if req.Auth.RulesRevision != req.CurrentRulesRevision {
		return &ModeError{
			Code: CodeAuthStale,
			Reason: fmt.Sprintf("风控规则已变更（授权时 rev=%d，当前 rev=%d），请重新阅读并授权",
				req.Auth.RulesRevision, req.CurrentRulesRevision),
		}
	}
	// 三重一致的第二条：限额快照。
	if !LimitsSnapshotMatches(req.Auth.LimitsSnapshot, req.CurrentLimits) {
		return &ModeError{
			Code:   CodeAuthStale,
			Reason: "限额配置在授权后被修改过，请重新确认限额并授权",
		}
	}

	// 规则 3：升级到 live_* 时限额必须完整。
	//
	// 注意这与「三重一致」是两件事：
	//   - 三重一致 = 授权时的限额与现在一致（前后相同）
	//   - 规则 3   = 现在的限额本身完整（不是 NULL）
	// 两者都满足才能实盘。举例：授权时限额就是空的，现在还是空的 ——
	// 一致，但不完整，仍然拒绝。
	if err := ValidateLimits(req.CurrentLimits); err != nil {
		return err
	}
	return nil
}

// ValidateLimits 检查限额是否完整（规则 3）。
//
// # 为什么不直接复用 db.RequireLiveReady
//
// `RequireLiveReady(key)` 会去查库。而这里是纯校验，输入已经是
// `*ExecutionLimits`。复用它会引入一次多余的 DB 往返，且让本函数
// 从纯函数变成有 IO 的函数 —— 那会破坏「五条规则可任意组合测试」这个前提。
//
// 判定口径与 RequireLiveReady 一致（我读了那份实现后对齐的）：
// 三个 max_* 都必须存在且为正。
func ValidateLimits(l *db.ExecutionLimits) error {
	if l == nil {
		return &ModeError{
			Code:   CodeLimitsIncomplete,
			Reason: "尚未配置任何交易限额。请先设置单笔上限、日上限与日单数上限",
		}
	}
	var missing []string
	// 金额必须存在且为正（0 或负数等于没有上限）。
	if !isPositiveDecimal(l.MaxSingleAmount) {
		missing = append(missing, "max_single_amount")
	}
	if !isPositiveDecimal(l.MaxDailyAmount) {
		missing = append(missing, "max_daily_amount")
	}
	if l.MaxDailyOrders == nil || *l.MaxDailyOrders <= 0 {
		missing = append(missing, "max_daily_orders")
	}
	if len(missing) > 0 {
		return &ModeError{
			Code:   CodeLimitsIncomplete,
			Reason: "交易限额不完整或非正数，缺少/非法：" + strings.Join(missing, ", "),
		}
	}
	return nil
}

// isLive 判断是否为实盘模式（live_whitelist / live_manual）。
// isLive 判断某模式是不是「实盘档」。
//
// # 2026-11-05 修正：漏了 live_auto
//
// 原来只认 whitelist / manual。加入 live_auto 后没同步这里，后果是：
// 切到 live_auto 时 isLive(to)=false → isLiveUpgrade=false →
// **授权校验与限额校验全被跳过**，模式直接切换成功。
//
// 这是整个自动交易功能里最危险的一处遗漏：无人值守下单是风险最高的档位，
// 却成了唯一不需要授权的实盘档。
//
// ⚠️ 加任何新的实盘档位时，**必须**同步这个函数 ——
// 它决定了「要不要查授权 / 要不要查限额」。
func isLive(m db.ExecMode) bool {
	switch m {
	case db.ExecModeLiveWhitelist, db.ExecModeLiveManual, db.ExecModeLiveAuto:
		return true
	default:
		return false
	}
}

// isLiveUpgrade 判断这次转换是不是「往实盘方向升」。
//
// # 定义要精确，因为它决定「要不要查授权」
//
// 升级 = 目标比来源更接近实盘：
//
//	paper → live_*            ✓ 升级
//	live_whitelist → live_manual  ✓ 升级（约束更松）
//	live_manual → live_whitelist  ✗ 降档（约束更紧，不需要重新授权）
//	live_* → paper               ✗ 降级
//	任意 → frozen                ✗ 降级
//	frozen → paper               ✗ 降级（但这是唯一合法出口）
func isLiveUpgrade(from, to db.ExecMode) bool {
	if !isLive(to) {
		return false
	}
	if from == db.ExecModeFrozen {
		// frozen 不能直接到 live（转换表已拦，这里再兜一次防止调用方绕过）
		return false
	}
	if !isLive(from) {
		return true // paper → live_*
	}
	// live → live：只有 whitelist → manual 是升级（约束变松）
	return from == db.ExecModeLiveWhitelist && to == db.ExecModeLiveManual
}

// authCovers 判断授权记录的目标模式是否覆盖本次要升到的模式。
//
// 约束强度：live_manual（每笔人工确认）比 live_whitelist（白名单内自动放行）
// 更**宽松**吗？
//
// 不是。两者的约束维度不同：
//
//	live_whitelist = 只能买白名单内的标的（对标的的限制）
//	live_manual    = 每笔都要人工点确认（对每次操作的限制）
//
// 所以「授权了 manual」不等于「授权了 whitelist」—— 反之亦然。
// **必须精确匹配**，不能假设包含关系。
func authCovers(authMode, target string) bool {
	return authMode == target
}

// isPositiveDecimal 判断字符串是否表示严格大于 0 的十进制数。
//
// 与 db 包的同名函数口径一致，但那是私有的，这里重新实现（3 行）。
// 不导出 db 的私有函数是为了不扩大它的 API 面。
func isPositiveDecimal(p *string) bool {
	if p == nil {
		return false
	}
	s := strings.TrimSpace(*p)
	if s == "" {
		return false
	}
	// 简单判定：允许前导 +，必须有至少一个非零数字，且只含数字/小数点。
	// 不用 big.Rat 是因为这里只要「是不是正数」，
	// 而 db 包的实现已经处理了完整的精度校验（那是执行路径的事）。
	hasNonZero := false
	dotSeen := false
	for i, r := range s {
		switch {
		case r == '+' && i == 0:
			continue
		case r == '.':
			if dotSeen {
				return false
			}
			dotSeen = true
		case r >= '1' && r <= '9':
			hasNonZero = true
		case r == '0':
			// 0 不改变 hasNonZero
		default:
			return false
		}
	}
	return hasNonZero
}
