package exec

// 模式转换的**执行**部分：限额快照比对、转换落库、启动降级。
//
// 与 state.go 的分工：
//   state.go  纯校验（输入事实 → 放行/拒绝），无副作用，可任意组合测试
//   本文件     有副作用（写 DB、写审计），依赖真实连接
//
// 分开的理由：五条硬规则里前三条是纯判断（好测），后两条要写库（难测）。
// 混在一起会让前三条的测试也被迫连库。

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// limitsSnapshot 是授权时写入 `exec_authorizations.limits_snapshot` 的结构。
//
// # 为什么只存三个字段，不存整行
//
// 整行含 revision（每次写 +1）、whitelist_symbols / mode（都已废弃）。
// 存整行会让「内容没变但 revision 变了」被判成不一致。
//
// 三个指针字段保留 nil 语义：nil = 不限制，"" = 限制为 0。
// 这个区分在比对时必须保留（见 LimitsSnapshotMatches）。
type limitsSnapshot struct {
	MaxSingleAmount *string `json:"max_single_amount"`
	MaxDailyAmount  *string `json:"max_daily_amount"`
	MaxDailyOrders  *int64  `json:"max_daily_orders"`
}

// BuildLimitsSnapshot 把当前限额序列化成授权快照的 JSON。
//
// 供 Grant 授权时调用 —— 存什么和比什么必须用同一个函数，
// 否则「存进去的格式」和「比对时期待的格式」会漂移。
func BuildLimitsSnapshot(l *db.ExecutionLimits) (json.RawMessage, error) {
	if l == nil {
		return json.RawMessage(`{}`), nil
	}
	raw, err := json.Marshal(limitsSnapshot{
		MaxSingleAmount: l.MaxSingleAmount,
		MaxDailyAmount:  l.MaxDailyAmount,
		MaxDailyOrders:  l.MaxDailyOrders,
	})
	if err != nil {
		return nil, fmt.Errorf("序列化限额快照失败: %w", err)
	}
	return raw, nil
}

// LimitsSnapshotMatches 判断授权时的限额快照与当前限额是否一致。
//
// # 比对三个字段，不含 revision
//
// Revision 每次写都 +1，包括「内容完全没变的重写」。用它比对会导致
// 「用户重新保存了一次同样的限额」就失效授权 —— 那是误伤，不是安全。
//
// # 为什么用 *string 判 nil
//
//	nil = 不限制
//	""  = 限制为 0（等于禁止一切）
//
// 两者语义完全不同，比对时必须区分。直接比字符串会把 nil 和 "" 混为一谈。
//
// # 快照解析失败时返回 false（视为不一致）
//
// 这是 fail-closed：解析不出来就当作「不匹配」，要求重新授权。
// 反向（解析失败 → 视为匹配）会让一个损坏的快照放行实盘。
func LimitsSnapshotMatches(snapJSON []byte, cur *db.ExecutionLimits) bool {
	// 空字节（NULL / 空串）与 `{}` 统一按「授权时没有限额」处理。
	//
	// 注意不能只判 len(snapJSON)==0 —— BuildLimitsSnapshot(nil) 会产出
	// `{}`（三个字段都是 nil），那与「空字节」语义相同，必须走同一分支。
	// 否则「授权时没有限额、现在也没有」会被判成不匹配，用户做不了任何事。
	var snap limitsSnapshot
	if len(snapJSON) > 0 {
		if err := json.Unmarshal(snapJSON, &snap); err != nil {
			return false // 解析失败 → fail-closed
		}
	}
	snapIsEmpty := snapEmpty(snap)
	// 「当前没有限额」有两种形态，语义相同：
	//   cur == nil               没有 execution_limits 行
	//   cur 三个字段全 nil        有行但没填任何上限（= 不限制）
	// 必须归成一类，否则「&ExecutionLimits{}」（空结构体）会被当成
	// 「有限额但全是 nil」，与「没记录」区分开 —— 而它们对用户是同一件事。
	curIsEmpty := cur == nil ||
		(cur.MaxSingleAmount == nil && cur.MaxDailyAmount == nil && cur.MaxDailyOrders == nil)

	if curIsEmpty {
		// 快照也没值 + 当前也没值 = 一致。
		return snapIsEmpty
	}
	if snapIsEmpty {
		// 快照说「授权时没配置限额」，但当前有配置 —— 不一致。
		return false
	}
	if !ptrStringEqual(snap.MaxSingleAmount, cur.MaxSingleAmount) {
		return false
	}
	if !ptrStringEqual(snap.MaxDailyAmount, cur.MaxDailyAmount) {
		return false
	}
	return ptrInt64Equal(snap.MaxDailyOrders, cur.MaxDailyOrders)
}

// snapEmpty 判断快照是否三个字段全为 nil（即「授权时没有限额」）。
func snapEmpty(s limitsSnapshot) bool {
	return s.MaxSingleAmount == nil && s.MaxDailyAmount == nil && s.MaxDailyOrders == nil
}

// ptrStringEqual 比较两个 *string，区分 nil 与 ""。
func ptrStringEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	// 数值等价比较：DB 的 NUMERIC(28,10) 输出 "1000.0000000000"，
	// 而用户输入可能是 "1000"。两者应视为相等。
	return decimalsEqual(*a, *b)
}

// decimalsEqual 判断两个十进制字符串数值是否相等（忽略尾随零与前导零）。
//
// # 为什么不能直接字符串比较
//
// NUMERIC(28,10) 读出来是 "1000.0000000000"，而授权时用户输入的是 "1000"。
// 直接比较会判成不等 —— 于是用户什么都没改，授权却显示过期。
func decimalsEqual(a, b string) bool {
	return normalizeDecimal(a) == normalizeDecimal(b)
}

// normalizeDecimal 去掉多余的前导零、尾随零与小数点。
//
//	"1000.0000000000" → "1000"
//	"01000"           → "1000"
//	"0.5000"          → "0.5"
//	"0.0000"          → "0"
func normalizeDecimal(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	} else if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	intPart = strings.TrimLeft(intPart, "0")
	if intPart == "" {
		intPart = "0"
	}
	fracPart = strings.TrimRight(fracPart, "0")
	out := intPart
	if fracPart != "" {
		out += "." + fracPart
	}
	// -0 归一成 0
	if neg && out != "0" {
		out = "-" + out
	}
	return out
}

// ptrInt64Equal 比较两个 *int64。
func ptrInt64Equal(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// ApplyTransition 执行一次模式转换（规则 5：审计失败导致转换失败）。
//
// # 执行顺序是有讲究的
//
//  1. 校验（ValidateTransition）—— 不通过就返回，什么都不写
//  2. 写 DB（TransitionMode，CAS）
//  3. DB 成功后才写审计
//
// **为什么审计在 DB 之后**：
//
//	反过来（先写审计再改 DB）会在 DB 失败时留下「审计说改了但实际没改」的
//	假记录 —— 那比没有记录更糟。
//
// **但规则 5 要求「审计失败导致转换失败」** —— 所以审计失败时必须回滚 DB。
// 这就是下面「补偿」那段的作用。
func ApplyTransition(d *db.DB, auditor Auditor, req TransitionRequest, reason string) error {
	// 1. 校验
	if err := ValidateTransition(req); err != nil {
		return err
	}

	// 2. 改 DB
	ok, err := d.TransitionMode(req.From, req.To)
	if err != nil {
		return fmt.Errorf("模式转换失败: %w", err)
	}
	if !ok {
		// 被别人先改了 —— 不是错误，返回一个可识别的哨兵让调用方跳过。
		return ErrModeConflict
	}

	// 3. 写审计（失败则回滚 DB）
	if auditor != nil {
		if aErr := auditor.WriteModeChange(req.From, req.To, reason); aErr != nil {
			// 补偿：把模式改回去。
			//
			// 补偿本身也可能失败 —— 那就只剩日志了。
			// 这是这个设计最薄弱的一环，必须显式记录，不能吞。
			if _, rErr := d.TransitionMode(req.To, req.From); rErr != nil {
				// # 这条日志必须能直接指导人工恢复
				//
				// 半夜被叫起来的人只看到「出错了」是不知道该做什么的。
				// 所以这里带上三样东西：
				//   1. 期望状态 vs 实际状态（差在哪）
				//   2. 两个错误的原文（为什么没成）
				//   3. 建议动作（照抄就能修）
				//
				// 实际状态用一次额外查询拿 —— 这条路径极罕见（审计失败 + 回滚
				// 失败同时发生），多一次查询不影响性能，而「不知道当前是什么状态」
				// 会让恢复变成猜谜。
				actual, gErr := d.GetExecMode()
				actualStr := string(actual)
				if gErr != nil {
					actualStr = "(读取失败: " + gErr.Error() + ")"
				}
				slog.Error("审计失败且模式回滚也失败，系统处于不一致状态，需人工恢复",
					"expected_mode", string(req.From),
					"intended_mode", string(req.To),
					"actual_mode", actualStr,
					"audit_err", aErr,
					"rollback_err", rErr,
					"read_err", gErr,
					"recovery_sql",
					"UPDATE settings SET value='"+string(req.From)+"' WHERE key='exec_mode';",
					"recovery_note",
					"确认 settings.exec_mode 的值；若为 "+string(req.To)+
						"，执行上面的 SQL 改回 "+string(req.From)+
						"；然后检查审计后端是否可用，再重试本次模式转换",
				)
			}
			return fmt.Errorf("审计写入失败，模式转换已回滚: %w", aErr)
		}
	}
	return nil
}

// ErrModeConflict 表示模式已被别人先改（不是错误）。
var ErrModeConflict = errors.New("exec mode was changed concurrently")

// DowngradeOnStartup 在进程启动时把非 paper 模式降为 paper（规则 4）。
//
// # 三条要求
//
//  1. **幂等**：两个进程同时启动，只有一个能成功降级（ForceSetExecMode 的
//     CAS 保证）。另一个拿到 changed=false，什么都不做。
//  2. **有记录**：成功降级那个写审计（含降级前的模式）。
//  3. **不刷噪音**：本来就是 paper 时不写审计 —— 否则每次重启都一条。
//
// # 为什么必须在启动时、且在任何下单路径可用之前调用
//
// 否则存在一个窗口期：进程已经能接单，但模式还是上次的 live_*。
// 那个窗口里的订单会真的打到通道 —— 而用户以为重启后是安全的 paper。
func DowngradeOnStartup(d *db.DB, auditor Auditor) error {
	changed, previous, err := d.ForceSetExecMode(db.ExecModePaper)
	if err != nil {
		return fmt.Errorf("启动降级失败: %w", err)
	}
	if !changed {
		// 本来就是 paper，或并发下别人已经降了 —— 都不写审计。
		return nil
	}

	// 真的降级了，写审计。
	//
	// 这里审计失败不回滚（与 ApplyTransition 不同）：
	// 启动降级的方向是「收紧」，失败时保持在 paper 比回滚到 live_* 安全得多。
	// 所以只记日志，不阻断启动 —— 否则「审计写不进去」会让系统起不来，
	// 而系统起不来时用户更容易做危险操作（比如手动改 DB 绕过）。
	if auditor != nil {
		prev, pErr := db.ParseExecMode(previous)
		if pErr != nil {
			// previous 是坏值 —— 记一个占位。
			prev = db.ExecMode("(unknown:" + previous + ")")
		}
		if aErr := auditor.WriteModeChange(prev, db.ExecModePaper,
			"进程重启，实盘模式自动降级为纸面模式"); aErr != nil {
			slog.Error("启动降级的审计写入失败（不阻断启动，模式已是 paper）",
				"from", previous, "err", aErr)
		}
	}
	return nil
}

// ParseInt64Ptr 是给测试与调用方用的小工具：把字符串转成 *int64。
//
// 放在这里而不是 db 包，因为它只服务 exec 的限额快照构造。
func ParseInt64Ptr(s string) *int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}
