package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// =====================================================================
// 执行限额(execution_limits)数据层
// =====================================================================
//
// ⚠ 本表禁止暴露给 LLM 工具层,见 ARCH-W2-W5 §3.8。
// 执行限额是「人给系统定的硬约束」,一旦能被 agent 读写,agent 就能自己
// 放宽或清空上限,风控形同虚设。读写入口只允许 server 侧的人工/后台路径调用。
//
// ⚠ 另一条绕过路径:okx 二进制当前在系统 PATH
// (/c/Users/Administrator/.workbuddy/binaries/node/versions/22.22.2-6/okx)。
// LLM 可以直接用 Bash 工具调它下单,完全绕过本表与风控闸门。处置要求见
// ARCH-W2-W5 §3.7:决策进程的 PATH 必须剔除 okx。剔除逻辑由 W4 实现,
// 此处只记录风险点,不做运行时拦截。

// 执行模式。三档,与 schema.sql 的 CHECK 约束一致。
//
// DEPRECATED:mode 列已废弃,执行模式状态由 exec_authorizations 承担。
// 下面是历史取值常量,保留仅为兼容既有调用方,新代码不要再引用。
const (
	// ExecutionModePaper 纯纸面交易,不触达券商。
	ExecutionModePaper = "paper"
	// ExecutionModeLiveWhitelist 仅 whitelist_symbols 内的标的允许实盘。
	ExecutionModeLiveWhitelist = "live_whitelist"
	// ExecutionModeLiveManual 每单需人工确认后放行。
	ExecutionModeLiveManual = "live_manual"
	// ExecutionModeLiveAuto 是实盘自动的限额档。
	//
	// ⚠️ 独立于 live_manual —— 无人值守下调得更松等于放弃最后一道闸。
	ExecutionModeLiveAuto = "live_auto"
)

var (
	// ErrExecutionLimitsInvalid 表示入参非法(模式、金额、单数上限)。
	ErrExecutionLimitsInvalid = errors.New("invalid execution limits")
	// ErrExecutionLimitsNotFound 表示该 key 没有配置。
	ErrExecutionLimitsNotFound = errors.New("execution limits not found")
	// ErrLimitNotConfigured 表示金额上限未配置。
	//
	// 语义:NULL = 未配置 = 禁止进入 live 模式。
	// 禁止把 NULL 解释为"不限制" —— 那会导致无上限的真实下单。
	// paper 模式不涉及真金白银,调用方可以忽略此错误。
	ErrLimitNotConfigured = errors.New("执行上限未配置,禁止进入 live 模式")
)

// validExecutionModes 是执行模式白名单。
var validExecutionModes = map[string]bool{
	ExecutionModePaper: true, ExecutionModeLiveWhitelist: true, ExecutionModeLiveManual: true, ExecutionModeLiveAuto: true,
}

// ExecutionLimits is a row in the execution_limits table.
// 金额字段一律字符串承载 NUMERIC(20,8),禁止 float64。
type ExecutionLimits struct {
	ID  int64  `json:"id"`
	Key string `json:"key"`
	// 三个金额字段对「用户」可见，但**不应该对「LLM 工具层」可见**
	// （§3.8.2 权限矩阵：工具层 ✗）。
	//
	// # 当前为什么可以直接返回
	//
	// 所有 exec API 都要求登录（走 auth 中间件），没有「非用户角色」的调用路径。
	// 所以现在返回完整金额是安全的。
	//
	// # 将来接 LLM 工具层时要做的事
	//
	// 需要两套 DTO：
	//   用户读：完整金额
	//   工具层读：只有 revision（用于比对）+「是否已配置」布尔值
	//
	// ⚠️ 如果那时忘了分，LLM 会看到用户的实盘限额 —— 那是敏感信息。
	MaxSingleAmount *string `json:"max_single_amount,omitempty"`
	MaxDailyAmount  *string `json:"max_daily_amount,omitempty"`
	MaxDailyOrders  *int64  `json:"max_daily_orders,omitempty"`
	Revision        int64   `json:"revision"`
	UpdatedAt       string  `json:"updated_at"`
	CreatedAt       string  `json:"created_at"`

	// Mode 与 WhitelistSymbols 已废弃，**不参与 JSON 序列化**。
	//
	// # 为什么不删字段
	//
	// 它们在 DB 里仍有列（mode 有 NOT NULL + CHECK，whitelist_symbols 是 text[]），
	// INSERT 时必须给值。删字段会让 UpsertLimits 的 SQL 缺参数。
	//
	// 所以：字段保留（供 SQL 用），但 json:"-" 阻止它出现在 API 响应里。
	//
	// # 为什么必须阻止
	//
	// 前端如果看到 mode 字段，会以为「限额有模式概念」——
	// 而模式实际由 exec_authorizations + settings.exec_mode 承担。
	// 这个字段会误导前端去做「按 mode 过滤限额」这种错误设计。
	//
	// 同理 whitelist_symbols：白名单的唯一事实来源是 intercept_rules，
	// 不是这里。留着它会造出「两份白名单」的困惑。
	Mode             string   `json:"-"` // DEPRECATED:见 executionModeDeprecatedNote
	WhitelistSymbols []string `json:"-"` // DEPRECATED:见 whitelistDeprecatedNote
}

// executionModeDeprecatedNote 记录 mode 列已废弃的原因,供阅读代码的人快速定位。
const executionModeDeprecatedNote = "mode 列已废弃,执行模式状态由 exec_authorizations 承担"

// whitelistDeprecatedNote 记录 whitelist_symbols 列已废弃的原因。
const whitelistDeprecatedNote = "whitelist_symbols 已废弃,白名单唯一事实来源为 intercept_rules"

// ExecutionLimitsInput 是 UpsertLimits 的入参。
// 空字符串表示不限制(NULL);WhitelistSymbols 为 nil 时落成 NULL。
type ExecutionLimitsInput struct {
	Key              string
	Mode             string
	MaxSingleAmount  string
	MaxDailyAmount   string
	MaxDailyOrders   *int64
	WhitelistSymbols []string
}

// ExecutionLimitsStore operates on the execution_limits table.
type ExecutionLimitsStore struct{ db *DB }

// ExecutionLimits returns the execution-limits store associated with this DB.
func (d *DB) ExecutionLimits() *ExecutionLimitsStore { return &ExecutionLimitsStore{db: d} }

// executionLimitsColumns 与 scanExecutionLimits 的扫描顺序必须一致。
// whitelist_symbols 在 SQLite 里是 JSON 数组文本（原 text[]）。
// NULL 与 '{}'（PG 语法遗留的默认值）都折成空数组，交给 decodeStringArray 统一处理。
const executionLimitsColumns = `id, key, mode, max_single_amount, max_daily_amount,
	max_daily_orders,
	CASE WHEN COALESCE(whitelist_symbols,'') IN ('','{}') THEN NULL ELSE whitelist_symbols END,
	revision, updated_at, created_at`

// scanExecutionLimits 从一行结果读出一条 ExecutionLimits。
func scanExecutionLimits(row interface{ Scan(...any) error }) (*ExecutionLimits, error) {
	var (
		l         ExecutionLimits
		single    sql.NullString
		daily     sql.NullString
		orders    sql.NullInt64
		whitelist sql.NullString
	)
	if err := row.Scan(
		&l.ID, &l.Key, &l.Mode, &single, &daily,
		&orders, &whitelist, &l.Revision, &l.UpdatedAt, &l.CreatedAt,
	); err != nil {
		return nil, err
	}
	l.MaxSingleAmount = nullStringPtr(single)
	l.MaxDailyAmount = nullStringPtr(daily)
	l.MaxDailyOrders = nullInt64Ptr(orders)
	syms, err := decodeStringArray(whitelist)
	if err != nil {
		return nil, err
	}
	l.WhitelistSymbols = syms
	return &l, nil
}

// decodeStringArray 把 to_json(text[]) 的结果还原成 []string。
// NULL 与空数组都归一化成空切片,调用方不必判空。
func decodeStringArray(raw sql.NullString) ([]string, error) {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return []string{}, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw.String), &out); err != nil {
		return nil, fmt.Errorf("decode whitelist_symbols: %w", err)
	}
	if out == nil {
		return []string{}, nil
	}
	return out, nil
}

// GetLimits 按 key 取一组限额。
//
// 两层失败语义,调用方必须区分:
//   - 行不存在 / 参数非法 → 返回 nil 记录 + 普通错误,没有数据可用。
//   - 金额上限为 NULL     → 返回**有效记录** + ErrLimitNotConfigured。
//     记录本身可读(paper 模式要展示「未配置」),但 live 模式必须据此拒绝。
//
// 之所以错误与数据同时返回:paper 路径需要读限额做模拟撮合,但同样需要知道
// 「未配置」这个事实。live 路径请直接用 RequireLiveReady,不要自己判 NULL。
func (s *ExecutionLimitsStore) GetLimits(key string) (*ExecutionLimits, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, fmt.Errorf("%w: key 不能为空", ErrExecutionLimitsInvalid)
	}
	row := s.db.QueryRow(`
SELECT `+executionLimitsColumns+` FROM execution_limits WHERE key = $1`, key)
	l, err := scanExecutionLimits(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrExecutionLimitsNotFound
		}
		return nil, err
	}
	if l.MaxSingleAmount == nil || l.MaxDailyAmount == nil {
		return l, ErrLimitNotConfigured
	}
	return l, nil
}

// RequireLiveReady 是进入 live 模式的唯一门禁。
//
// fail-closed:行不存在、金额上限未配置、单数上限缺失、限额非法,任一条成立
// 都返回错误,调用方据此拒绝下单。只有全部配置齐全才返回 nil。
//
// 不允许调用方自己读 MaxSingleAmount 判空 —— 那是把安全判断散落到各处,
// 漏一处就是一个 fail-open 入口。集中在这里,只有这一个出口。
func (s *ExecutionLimitsStore) RequireLiveReady(key string) (*ExecutionLimits, error) {
	l, err := s.GetLimits(key)
	if err != nil {
		// 包含 ErrLimitNotConfigured:未配置金额上限 = 禁止 live。
		return nil, err
	}
	// 金额上限存在还不够,必须能解析成正数:0 或负数等于没有上限。
	if !positiveDecimal(l.MaxSingleAmount) {
		return l, fmt.Errorf("%w: max_single_amount=%q 必须为正数", ErrLimitNotConfigured, derefOrEmpty(l.MaxSingleAmount))
	}
	if !positiveDecimal(l.MaxDailyAmount) {
		return l, fmt.Errorf("%w: max_daily_amount=%q 必须为正数", ErrLimitNotConfigured, derefOrEmpty(l.MaxDailyAmount))
	}
	if l.MaxDailyOrders == nil || *l.MaxDailyOrders <= 0 {
		return l, fmt.Errorf("%w: max_daily_orders 必须为正", ErrLimitNotConfigured)
	}
	return l, nil
}

// positiveDecimal 判断字符串是否表示严格大于 0 的十进制数。
// 用 big.Rat 而非 float64:金额不能因精度误差被判成 0 或负数。
func positiveDecimal(p *string) bool {
	if p == nil {
		return false
	}
	s := strings.TrimSpace(*p)
	if !validDecimalLiteral(s) {
		return false
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return false
	}
	return r.Sign() > 0
}

// derefOrEmpty 把可能为 nil 的字符串指针转成便于报错的展示值。
func derefOrEmpty(p *string) string {
	if p == nil {
		return "<NULL>"
	}
	return *p
}

// validateExecutionLimitsInput 校验模式与金额字面量。
func validateExecutionLimitsInput(in ExecutionLimitsInput) error {
	if strings.TrimSpace(in.Key) == "" {
		return fmt.Errorf("%w: key 不能为空", ErrExecutionLimitsInvalid)
	}
	if !validExecutionModes[strings.TrimSpace(in.Mode)] {
		return fmt.Errorf("%w: mode %q 不在 paper/live_whitelist/live_manual/live_auto 之内",
			ErrExecutionLimitsInvalid, in.Mode)
	}
	for name, val := range map[string]string{
		"max_single_amount": in.MaxSingleAmount,
		"max_daily_amount":  in.MaxDailyAmount,
	} {
		if strings.TrimSpace(val) == "" {
			continue
		}
		if !validDecimalLiteral(val) {
			return fmt.Errorf("%w: %s %q 不是合法十进制数", ErrExecutionLimitsInvalid, name, val)
		}
		// 限额是上限,负数没有意义 —— 会变成「负额度」绕过所有判断。
		if strings.HasPrefix(strings.TrimSpace(val), "-") {
			return fmt.Errorf("%w: %s 不能为负", ErrExecutionLimitsInvalid, name)
		}
	}
	if in.MaxDailyOrders != nil && *in.MaxDailyOrders < 0 {
		return fmt.Errorf("%w: max_daily_orders 不能为负", ErrExecutionLimitsInvalid)
	}
	return nil
}

// UpsertLimits 按 key 写入或更新一组限额,返回落库后的记录。
// updated_at 只在真正更新时刷新,避免 upsert 空转改时间戳。
// revision 每次调用 +1,供审计链证明限额被改过几次。
func (s *ExecutionLimitsStore) UpsertLimits(in ExecutionLimitsInput) (*ExecutionLimits, error) {
	if err := validateExecutionLimitsInput(in); err != nil {
		return nil, err
	}
	// 列现在是 JSON 数组文本；nil 表示「不设白名单」（NULL），
	// 空切片也落 NULL（与 PG 版声明里「nil 时落成 NULL」一致）。
	var whitelist any
	if len(in.WhitelistSymbols) > 0 {
		raw, err := json.Marshal(in.WhitelistSymbols)
		if err != nil {
			return nil, fmt.Errorf("序列化 whitelist_symbols 失败: %w", err)
		}
		whitelist = string(raw)
	}
	row := s.db.QueryRow(`
INSERT INTO execution_limits(key, mode, max_single_amount, max_daily_amount,
                            max_daily_orders, whitelist_symbols)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (key) DO UPDATE SET
    mode              = EXCLUDED.mode,
    max_single_amount = EXCLUDED.max_single_amount,
    max_daily_amount  = EXCLUDED.max_daily_amount,
    max_daily_orders  = EXCLUDED.max_daily_orders,
    whitelist_symbols = EXCLUDED.whitelist_symbols,
    revision          = execution_limits.revision + 1,
    updated_at        = now()
RETURNING `+executionLimitsColumns,
		strings.TrimSpace(in.Key), strings.TrimSpace(in.Mode),
		normalizeOptDecimal(in.MaxSingleAmount, decimalScaleNotional),
		normalizeOptDecimal(in.MaxDailyAmount, decimalScaleNotional),
		in.MaxDailyOrders, whitelist,
	)
	l, err := scanExecutionLimits(row)
	if err != nil {
		return nil, err
	}
	return l, nil
}
