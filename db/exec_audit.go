package db

// 执行层审计（WE-12）。
//
// # 为什么不复用 activity 表
//
// 三个原因（详见 schema.sql 里 exec_audit 的注释）：
//   ① activity.exploration_id 是 NOT NULL —— 执行层事件不属于任何任务
//   ② activity 有 ON DELETE CASCADE —— 审计记录必须永久保留
//   ③ 语义不同：activity 是「任务执行过程」，exec_audit 是「系统状态变更」
//
// # 为什么 WriteExecAudit 接受 nil tx
//
// 有些事件是「独立记录」（如模式切换后补记），可以自己开事务。
// 但**与状态变更绑定的事件必须传 tx**：冻结、授权、解冻。
//
// 如果那些事件在事务外写审计，就会出现「状态变了但没记录」——
// 而那正是 ARCH §3.5 硬约束 2 明令禁止的。
//
// 所以：调用方要问自己「这个事件和某个状态变更是一体的吗」。
//   是一体 → 传 tx（审计失败则状态变更也回滚）
//   不是 → 传 nil

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/redact"
)

// 审计动作类型。与 schema 的 CHECK 约束一一对应。
const (
	AuditExecModeChange   = "exec_mode_change"
	AuditExecAuthorize    = "exec_authorize"
	AuditExecUnfreeze     = "exec_unfreeze"
	AuditExecFrozen       = "exec_frozen"
	AuditExecLimitsChange = "exec_limits_change"
)

// validAuditActions 是动作白名单，与 schema 的 CHECK 一致。
//
// 在 Go 侧也校验一遍：SQL 的 CHECK 会给出一条「violates check constraint」的
// 低层错误，而这里能给出「未知审计动作 X」的可读错误。
var validAuditActions = map[string]bool{
	AuditExecModeChange:   true,
	AuditExecAuthorize:    true,
	AuditExecUnfreeze:     true,
	AuditExecFrozen:       true,
	AuditExecLimitsChange: true,
}

// auditWriteHook 仅供测试注入：非 nil 时替代真实写入。
//
// # 为什么用包级变量而不是 DB 结构体字段
//
// 加 DB 字段会影响所有构造点（Open / New / 每个测试实例），
// 而包级变量只在本包可见，生产路径完全不变（nil 时走原逻辑）。
//
// # 为什么需要它（而不是用「传非法参数」测回滚）
//
// 「非法 action」被 Go 白名单拦、「非法 detail」被 json.Marshal 拦 ——
// 两者都发生在 DB 之前，所以 FreezeAccountTx 还没走到第四步就返回了，
// 谈不上「前三步成功、第四步失败 → 回滚」。
//
// 要验证 ARCH §3.5 硬约束 2（审计失败 → 冻结回滚），
// 必须让「第四步真的失败」而「前三步已成功」—— 只能注入。
//
// ⚠️ 与 t.Parallel() 不兼容（全局状态）。
//
//	当前 db 包只有 task_context_unit_test.go 用 Parallel，且它不碰审计。
//	如果将来有并行测试要碰审计，改成 per-DB 字段或 context 传递。
var auditWriteHook func(tx *sql.Tx, action, actor string, detail map[string]any) error

// SetAuditWriteHookForTest 注入/清除审计写入钩子。仅供测试使用。
//
// 必须在 t.Cleanup 里传 nil 清零 —— 否则会污染后续测试。
func SetAuditWriteHookForTest(fn func(tx *sql.Tx, action, actor string, detail map[string]any) error) {
	auditWriteHook = fn
}

// WriteExecAudit 写一条执行层审计。
//
// tx 为 nil 时自己开事务；非 nil 时加入调用方的事务。
//
// # 为什么返回值必须被检查
//
// 审计失败意味着「这个事件没有被记录下来」。
// 而 ARCH §3.5 硬约束 2 要求：「宁可冻结晚 60s，不可冻结了但没人知道」。
//
// 所以**调用方不能吞这个错误** —— 尤其当 tx != nil 时，
// 必须让整个事务回滚。
func (d *DB) WriteExecAudit(tx *sql.Tx, action, actor string, detail map[string]any) error {
	// 测试注入点：非 nil 时代替真实写入（用于验证「审计失败 → 事务回滚」）。
	if auditWriteHook != nil {
		return auditWriteHook(tx, action, actor, detail)
	}

	if !validAuditActions[action] {
		// 明确报错而不是让 SQL 的 CHECK 去拦 ——
		// 这里的错误信息能直接说「未知动作 X」，而 CHECK 只会说「违反约束」。
		return fmt.Errorf("未知审计动作 %q（合法值：exec_mode_change / exec_authorize / exec_unfreeze / exec_frozen / exec_limits_change）", action)
	}

	payload := detail
	if payload == nil {
		payload = map[string]any{}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		// 可测的失败点：detail 里有 math.Inf / NaN / 不可序列化的类型时。
		// 调用方（FreezeAccountTx）必须让整个事务回滚 —— 见它的注释。
		return fmt.Errorf("序列化审计 detail 失败（detail 可能含 NaN/Inf 或不可序列化的值）: %w", err)
	}

	// 落库前脱敏。
	//
	// # 为什么审计也要脱敏
	//
	// detail 里的字段多数是我们自己填的（account_id / reason / before / after），
	// 但 `reason` 是**调用方传进来的自由文本** —— 而它可能包含
	// 「通道返回 401: api_key=sk-xxx 无效」这类错误信息。
	//
	// 如果审计不脱敏，那「错误信息带凭证」就会变成「审计表里躺着凭证」——
	// 而审计表是**永久保留**的（这正是它的设计目的）。
	//
	// 换句话说：审计表越可靠，泄漏的持久性越强。
	// 所以它必须和日志一样，在写入前脱敏。
	raw = redact.MaskJSON(raw)

	const q = `INSERT INTO exec_audit(action, detail, actor) VALUES ($1, $2, $3)`

	if tx != nil {
		if _, err := tx.Exec(q, action, string(raw), actor); err != nil {
			return fmt.Errorf("写执行层审计失败（action=%s）: %w", action, err)
		}
		return nil
	}

	if _, err := d.Exec(q, action, string(raw), actor); err != nil {
		return fmt.Errorf("写执行层审计失败（action=%s）: %w", action, err)
	}
	return nil
}

// ExecAuditEntry 是一条审计记录（读接口）。
type ExecAuditEntry struct {
	ID        int64           `json:"id"`
	Action    string          `json:"action"`
	Detail    json.RawMessage `json:"detail"`
	Actor     string          `json:"actor"`
	CreatedAt string          `json:"created_at"`
}

// ListExecAudit 读最近的审计记录（按时间倒序）。
//
// action 为空表示不过滤。
func (d *DB) ListExecAudit(action string, limit int) ([]*ExecAuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var rows *sql.Rows
	var err error
	const cols = `id, action, detail, actor, created_at`
	if action == "" {
		rows, err = d.Query(`SELECT `+cols+` FROM exec_audit ORDER BY created_at DESC, id DESC LIMIT $1`, limit)
	} else {
		rows, err = d.Query(`SELECT `+cols+` FROM exec_audit WHERE action=$1 ORDER BY created_at DESC, id DESC LIMIT $2`, action, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*ExecAuditEntry
	for rows.Next() {
		var e ExecAuditEntry
		var detail []byte
		if err := rows.Scan(&e.ID, &e.Action, &detail, &e.Actor, &e.CreatedAt); err != nil {
			return nil, err
		}
		if len(detail) == 0 {
			detail = []byte(`{}`)
		}
		e.Detail = json.RawMessage(detail)
		out = append(out, &e)
	}
	return out, rows.Err()
}
