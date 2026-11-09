package db

// 执行模式（settings.exec_mode）数据层。
//
// # 为什么模式存在 settings 而不是 exec_accounts 或新表
//
// 模式是**系统级**开关（整个进程当前跑纸面还是实盘），不是账户属性 ——
// 同一账户可以同时跑 paper 与 live，那由 orders_execution.mode 表达。
// 系统级开关放 settings 是项目既有做法（auth.password_hash 等也在那里）。
//
// # 为什么只存一个 key，不存 mode_at / mode_reason
//
// 变更历史属于 activity（那里已有完整审计链）。在 settings 里再存一份
// 「最后一次的原因」必然漂移 —— activity 写了但 settings 没更新，或反之。
//
// 分工：settings 存**当前值**，activity 存**事件流**。
// 这是「当前状态」与「事件流」的正确分工，不是「两处冗余备份」。

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ExecModeKey 是 settings 表里存执行模式的键名。
const ExecModeKey = "exec_mode"

// ExecMode 是运行时的执行模式。
type ExecMode string

const (
	// ExecModePaper 纸面模式：订单走虚拟账本，不触达真实通道。
	ExecModePaper ExecMode = "paper"
	// ExecModeLiveWhitelist 实盘白名单：只允许白名单内标的下单。
	ExecModeLiveWhitelist ExecMode = "live_whitelist"
	// ExecModeLiveManual 实盘手动：每笔需人工确认。
	ExecModeLiveManual ExecMode = "live_manual"
	// ExecModeLiveAuto 实盘自动：**无人值守**，系统按策略信号自行下单/撤单/平仓。
	//
	// # 与 live_manual 的本质差别
	//
	// live_manual 的每一笔都要人点确认；本模式**不再询问**。
	// 所以它是整个系统里权限最高、也最危险的档位 —— 一旦策略失效，
	// 亏损会在无人干预下持续放大。
	//
	// 因此进入本模式需要独立的授权仪式（见 exec/live_auto.go 的 Arming），
	// 且运行期有自动降级条件（对账失败 / 单笔超限）。
	ExecModeLiveAuto ExecMode = "live_auto"
	// ExecModeFrozen 冻结：拒绝一切实盘，唯一出口是回到 paper。
	ExecModeFrozen ExecMode = "frozen"
)

// ErrExecModeUnknown 表示读到的模式值不在白名单内。
var ErrExecModeUnknown = errors.New("unknown exec mode")

// ErrExecModeTransitionInvalid 表示模式转换不合法。
var ErrExecModeTransitionInvalid = errors.New("invalid exec mode transition")

// validExecModes 是模式白名单。
var validExecModes = map[ExecMode]bool{
	ExecModePaper:         true,
	ExecModeLiveWhitelist: true,
	ExecModeLiveManual:    true,
	ExecModeLiveAuto:      true,
	ExecModeFrozen:        true,
}

// execModeTransitions 是模式状态机的合法转换表。
//
//	paper ──► live_whitelist ──► live_manual
//	  ▲              │                │
//	  │              ▼                ▼
//	  └────────── frozen ◄────────────┘
//	                │
//	                └──► paper（唯一出口）
//
// # 四条设计选择
//
//  1. **frozen → live_* 被拒**：冻结意味着「发现了问题」。直接恢复实盘
//     等于「问题没解决就继续下注」。强制经过 paper 是一道人工确认。
//  2. **frozen → paper 是唯一出口**：必须先回模拟盘。
//  3. **paper → frozen 合法**：在纸面上发现问题也该能立即全局冻结，
//     不必先升档再冻。
//  4. **live_manual → live_whitelist 是「降档」**：允许的 —— 收紧比放宽安全。
//     反向（whitelist → manual）是放宽，需要重新授权（由调用方校验授权）。
var execModeTransitions = map[ExecMode][]ExecMode{
	ExecModePaper:         {ExecModeLiveWhitelist, ExecModeLiveManual, ExecModeLiveAuto, ExecModeFrozen},
	ExecModeLiveWhitelist: {ExecModeLiveManual, ExecModeLiveAuto, ExecModePaper, ExecModeFrozen},
	ExecModeLiveManual:    {ExecModeLiveWhitelist, ExecModeLiveAuto, ExecModePaper, ExecModeFrozen},
	// live_auto 的出口刻意**不包含**任何 live 档位的直接互转。
	//
	// 出去只能到 paper 或 frozen —— 想从自动回到手动必须经过 paper。
	// 理由：live_auto → live_manual 看起来是「收紧」，但操作者的心智
	// 还停留在「系统在自动跑」，容易误以为自己没在下单。强制经 paper 是
	// 一次明确的「停下来」确认。
	ExecModeLiveAuto: {ExecModePaper, ExecModeFrozen},
	ExecModeFrozen:   {ExecModePaper},
}

// CanTransitionExecMode 判断 from→to 是否为合法模式转换。
//
// 导出给 exec 包用：调用方要在写 DB 之前给出可读的拒绝理由，
// 而不是让一次非法配置静默返回 ok=false。
func CanTransitionExecMode(from, to ExecMode) bool {
	for _, next := range execModeTransitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// ParseExecMode 把字符串解析成 ExecMode（去空白 + 小写）。
//
// 未知值返回 ErrExecModeUnknown 而不是静默回退到 paper ——
// 「配置写错了却按 paper 跑」会让用户以为实盘已启用。
func ParseExecMode(s string) (ExecMode, error) {
	m := ExecMode(strings.ToLower(strings.TrimSpace(s)))
	if !validExecModes[m] {
		return "", fmt.Errorf("%w: %q", ErrExecModeUnknown, s)
	}
	return m, nil
}

// GetExecMode 读当前执行模式。
//
// # 不存在时返回 paper，不返回错误
//
// 首次启动时 settings 里没有这个 key。报错会让系统起不来，
// 而 paper 是安全默认值 —— 符合「默认最保守」。
//
// 但**值非法**时返回错误而不是回退 paper：
// 那是「配置被写坏了」，静默回退会掩盖问题（用户以为在实盘，实际在纸面）。
func (d *DB) GetExecMode() (ExecMode, error) {
	raw, ok, err := d.GetSetting(ExecModeKey)
	if err != nil {
		return "", fmt.Errorf("读 exec_mode 失败: %w", err)
	}
	if !ok || strings.TrimSpace(raw) == "" {
		// 两种情形都归到这里：key 不存在（ok=false）或值为空。
		// 两者语义相同 —— 「没配过」，安全默认是 paper。
		return ExecModePaper, nil
	}
	return ParseExecMode(raw)
}

// TransitionMode 用条件更新做模式转换（乐观并发）。
//
// # 为什么必须条件更新
//
// 两个调度器/两个用户可能同时改模式。只有「当前值确实是 from」才允许改，
// 改不成就说明别人先动了。
//
// 不这样做的话，两个并发请求各自读到 paper，各自写 live_manual 和 frozen，
// 后写的覆盖先写的，且**两个调用方都以为成功** —— 而「frozen 被覆盖成
// live_manual」意味着冻结失效。
//
// # 返回值语义
//
//	(ok=true,  nil) → 转换成功
//	(ok=false, nil) → **当前值不是 from（别人先改了），不是错误**
//	(false, err)    → 系统错误，或模式转换本身不合法
//
// # 校验分工
//
// 这里只管两件事：转换合法性（frozen→live 拒绝）+ 并发安全。
// 授权三重一致、限额完整性这些**由调用方校验** —— 那需要读其他表，
// 放进仓储层会让它承担不属于它的职责。
func (d *DB) TransitionMode(from, to ExecMode) (bool, error) {
	if !validExecModes[from] {
		return false, fmt.Errorf("%w: from=%q", ErrExecModeUnknown, from)
	}
	if !validExecModes[to] {
		return false, fmt.Errorf("%w: to=%q", ErrExecModeUnknown, to)
	}
	// 先判合法性：给出可读拒绝（frozen→live 是这个判断的主要服务对象）。
	if !CanTransitionExecMode(from, to) {
		return false, fmt.Errorf("%w: %s → %s", ErrExecModeTransitionInvalid, from, to)
	}

	// 用 UPDATE ... WHERE value=$from 实现 CAS。
	//
	// 注意 key 不存在时 UPDATE 影响 0 行 —— 调用方据此知道「当前不是 from」。
	// 但首次设置（不存在 → paper）需要能成功，所以 from=paper 时用
	// INSERT ... ON CONFLICT DO UPDATE ... WHERE 处理：
	//   - 不存在      → 插入 from（即 paper），再走下一次转换？不行，会丢一次。
	//   - 用「不存在视为 paper」的语义在 SQL 里表达：
	//     WHERE COALESCE(value,'paper') = $from
	res, err := d.Exec(`UPDATE settings SET value = $2, updated_at = now()
WHERE key = $1 AND COALESCE(NULLIF(value,''), 'paper') = $3`,
		ExecModeKey, string(to), string(from))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 1 {
		return true, nil
	}

	// 影响 0 行有两种可能：key 不存在，或当前值 ≠ from。
	// 若是因为「key 不存在」且 from=paper（默认值），则应插入。
	if from != ExecModePaper {
		return false, nil // 当前值不是 from，或不存在 —— 都不是 paper
	}
	// from=paper：尝试插入（首次设置）。
	insRes, insErr := d.Exec(`INSERT INTO settings (key, value) VALUES ($1, $2)
ON CONFLICT (key) DO NOTHING`, ExecModeKey, string(to))
	if insErr != nil {
		return false, insErr
	}
	if n, _ := insRes.RowsAffected(); n == 1 {
		return true, nil
	}
	// 插入也失败 = 并发下别人先插了/改了 —— 仍然不是错误。
	return false, nil
}

// ForceSetExecMode 无条件写入模式（仅供**启动降级**使用）。
//
// # 为什么需要一个绕过状态机的入口
//
// 启动降级要做的是「从任意非 paper 值强制回到 paper」——
// 而 frozen → paper 是合法的、live_* → paper 也是合法的，
// 所以其实可以走 TransitionMode。但启动时我们**不知道**当前值
// （要先读再转），两步之间有窗口期。
//
// 这个方法用一条 SQL 完成「读 + 条件写」，避免窗口期：
//
//	UPDATE settings SET value='paper' WHERE key='exec_mode' AND value <> 'paper'
//
// 返回 (changed bool, previous string)：
//
//	changed=true  说明确实降级了（调用方据此写 activity）
//	changed=false 说明本来就是 paper（**不写 activity，避免每次重启刷噪音**）
//
// previous 是降级前的值 —— 写 activity 需要它。用 RETURNING 拿到，
// 避免「先 SELECT 再 UPDATE」的竞态。
// ForceSetExecMode 无条件设置执行模式（覆盖当前值）。
//
// # 为什么叫 Force 且确实无条件
//
// 它服务于「紧急降级」：冻结路径要把模式强制拉回 paper，
// 那一刻不需要「当前是不是 live」这个前提 —— 无论当前是什么，都必须变成 paper。
//
// 这与 TransitionMode 的 CAS 语义（`WHERE value=$from`）是两个不同的工具：
//
//	TransitionMode   —— 「我确定当前是 from，改成 to」（用户主动切换，要防并发）
//	ForceSetExecMode —— 「不管当前是什么，改成 to」（故障降级，不允许失败）
//
// # 并发风险（已知，接受）
//
// 两个实例同时 ForceSetExecMode 时，后者覆盖前者。但两者写的都是同一个
// 目标值（因为 Force 的调用方总是「降级到 paper」），所以结果相同。
//
// ⚠️ 如果有人用 ForceSetExecMode 做「升到 live」这类非幂等目标，
// 那就引入了真实的并发风险 —— 那种场景必须走 TransitionMode。
//
// # 已知边界：与升级并发时「最后写入者胜」
//
// 场景：
//
//	时刻 1  对账发现差异 → 开始冻结事务（内部调 ForceSetExecMode(paper)）
//	时刻 2  用户调 POST /api/exec/mode {to: live_manual}（走 TransitionMode）
//	时刻 3  两者各自提交
//
// 若升级后提交，结果是 mode = live_manual **但账户已 blocked** ——
// 用户看到「实盘」，却下不了单（RequireUsable 会拒）。
//
// # 为什么现在不修
//
// 账户 blocked 会挡住所有下单，所以不会产生真实订单。
// 而「mode 显示 live 但账户不可用」是**可见的误导**，不是静默的危险 ——
// 用户点一次下单就会看到明确错误。
//
// # 更根本的问题（见 KNOWN-LIMITATIONS L14）
//
// 当前是「一个账户出问题 → 全局降 paper → 所有账户都停」。
// 单账户场景下这是保守且正确的；多账户下是过度反应。
// 修法需要改成「账户级冻结」而不是「全局降级」，那是架构改动。
func (d *DB) ForceSetExecMode(to ExecMode) (changed bool, previous string, err error) {
	if !validExecModes[to] {
		return false, "", fmt.Errorf("%w: %q", ErrExecModeUnknown, to)
	}
	// # 为什么不用「数据修改 CTE + 读 CTE」（SQLite 迁移）
	//
	// PG 版靠 `WITH old AS (SELECT …), upd AS (UPDATE … RETURNING value)`
	// 一次拿旧值与是否改动。SQLite 的 CTE 不支持数据修改语句
	// （`WITH … UPDATE` 整体语法错误，实测 `near "UPDATE": syntax error`），
	// 也没有 `EXISTS(SELECT … FROM <updatable CTE>)`。
	//
	// 改成事务里三步（单写者下没有竞态窗口，BEGIN IMMEDIATE 已独占写锁）：
	//
	//	1. 读旧值（缺行按 'paper'）
	//	2. 值不同才 UPDATE
	//	3. 提交
	tx, err := d.Begin()
	if err != nil {
		return false, "", err
	}
	defer tx.Rollback() //nolint:errcheck // 提交成功后是 no-op

	var current string
	scanErr := tx.QueryRow(`SELECT COALESCE(NULLIF(value,''), 'paper') FROM settings
WHERE key = $1`, ExecModeKey).Scan(&current)
	if errors.Is(scanErr, sql.ErrNoRows) {
		current = string(ExecModePaper)
	} else if scanErr != nil {
		return false, "", scanErr
	}
	if current == string(to) {
		// 已经是目标值：不写库（避免每次启动刷新 updated_at 产生噪音）。
		return false, current, nil
	}
	if _, err := tx.Exec(`UPDATE settings SET value = $2, updated_at = now()
WHERE key = $1`, ExecModeKey, string(to)); err != nil {
		return false, "", err
	}
	if err := tx.Commit(); err != nil {
		return false, "", err
	}
	return true, current, nil
}
