package db

// 账户冻结的原子操作（WE-10）。
//
// # 为什么这个复合操作必须在 db 层
//
// 它是「两张表 + 一个 settings key」的原子操作：
//
//	exec_authorizations  —— 撤掉凭据
//	exec_accounts        —— 拉黑账户
//	settings(exec_mode)  —— 降级为 paper
//
// 拆到 exec 层去写 SQL 会让语句散落，而项目的既有范式是
// 「事务封在 db 方法内部」（见 config.go:420 / 597 / 763 / 894 的 4 处先例）。
//
// **分层被第一次例外打破** —— 如果这里破例，下次调度器也会说「就这一次」。

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/redact"
)

// ErrFreezeAlreadyDone 表示账户已经是冻结状态（幂等，不是错误）。
//
// 单独一个哨兵值而不是返回 nil：调用方需要区分
// 「这次真的冻了」和「早就冻了」—— 审计与告警的文案不同。
var ErrFreezeAlreadyDone = errors.New("account already frozen")

// FreezeResult 描述一次冻结实际做了什么。
//
// 三个字段都是「本次是否真的改了」，而不是「当前状态是什么」。
// 幂等重跑时全为 false，调用方据此决定是否写审计。
type FreezeResult struct {
	// AuthorizationRevoked 本次是否撤销了活跃授权。
	AuthorizationRevoked bool
	// AccountBlocked 本次是否拉黑了账户。
	AccountBlocked bool
	// ModeDowngraded 本次是否把 exec_mode 从 live_* 降到了 paper。
	ModeDowngraded bool
}

// Changed 报告本次冻结是否有任何实际改动。
func (r FreezeResult) Changed() bool {
	return r.AuthorizationRevoked || r.AccountBlocked || r.ModeDowngraded
}

// FreezeAccountTx 在一个事务里冻结账户：撤授权 + 拉黑 + 降模式。
//
// # 为什么必须同事务
//
// 「冻结」的语义是「停止一切自动执行」，不只是「这个账户不能用了」。
//
//	只撤授权不拉黑 → 账户还能被重新授权，凭据断得不彻底
//	只拉黑不降模式 → 系统还在 live，其它账户仍可自动下单，
//	                  「冻结」没达到目的
//
// 三件事拆开做，任何一步失败都会留下半成品状态。同事务保证
// 「要么全冻，要么一点没动」。
//
// # 为什么顺序是「撤授权 → 拉黑 → 降模式」
//
// **从内向外断**：
//
//	① 撤授权：先断「这个账户能下单的凭据」（最贴近操作层）
//	② 拉黑账户：再断「这个账户本身」
//	③ 降模式：最后断「全局所有自动执行」（影响面最大，放最后）
//
// 反过来（先降模式）会让「降模式成功但撤授权失败」的窗口里，
// 账户处于「系统在 paper 但凭据还在」的状态 —— 虽然最终会回滚，
// 但降模式改的是 `settings`，那张表有并发写入（用户在 UI 切模式），
// 持有它的行锁越久，冲突概率越高。
//
// 先做最快、最不容易冲突的两步，最后做最可能冲突的那步 —— 事务更短。
//
// # 为什么降模式要判 `value LIKE 'live%'`
//
//   - 已经是 paper：不用改（避免无谓写库 + 覆盖 updated_at）
//   - 已经是 frozen：也不用改成 paper —— frozen 比 paper 更严格，
//     把它「降级」成 paper 反而是放宽，与冻结的意图相反
//   - 只有 live 档位（whitelist / manual / auto）才需要降
//
// # 幂等
//
// 重复冻结不报错：三步各自的 WHERE 子句已经排除「已完成」的行。
// 全部没改动时返回 FreezeResult{}（Changed()==false），
// 调用方据此跳过审计 —— 但不返回 ErrFreezeAlreadyDone，
// 因为「已经冻了」对调用方来说仍是成功。
func (d *DB) FreezeAccountTx(accountID int64, channel, accountRef, reason string) (FreezeResult, error) {
	var res FreezeResult

	if accountID <= 0 {
		return res, fmt.Errorf("account_id 必须为正，实际 %d", accountID)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		// 冻结必须带原因 —— 事后无法解释「为什么冻的」比不冻更糟。
		return res, fmt.Errorf("冻结必须给出原因")
	}

	// reason 是调用方传进来的自由文本 —— 它可能包含
	// 「通道返回 401: api_key=sk-xxx 无效」这类错误信息。
	//
	// # 为什么必须在落库前脱敏
	//
	// reason 会写进三个地方：exec_accounts.block_reason、
	// exec_authorizations.revoked_reason、exec_audit.detail。
	// 这三处都是**长期保留**的（审计表是永久的）。
	//
	// 如果不脱敏，「错误信息带凭证」就变成「数据库里躺着凭证」，而且删不掉 ——
	// 审计的设计目的就是不许删。
	reason = redact.Mask(reason)

	tx, err := d.Begin()
	if err != nil {
		return res, err
	}
	// 提交后 Rollback 是 no-op，所以这里无条件 defer 是安全的。
	defer func() { _ = tx.Rollback() }()

	// ---- ① 降模式 ----
	//
	// 应急响应的顺序是「先关总闸，再查细节」。
	// 降模式是全局关闸 —— 它失败时后面两步不该发生：
	// 即使事务回滚能撤销数据，也撤销不了「行锁/触发器已产生」这件事。
	//
	// # 只改 live_*
	//
	//   paper  不动：避免无谓写库
	//   frozen 不动：**那是更严格的状�态，改成 paper 是放宽，与冻结意图相反**
	//
	// 如果用户需要冻结后在 paper 上验证，应由他显式 TransitionMode(frozen, paper)——
	// 而不是冻结动作顺手放宽。
	//
	// # 已知前提：单实例部署
	//
	// PG 默认 read committed：事务未提交时其他连接读不到中间状态。
	// 所以「降模式后失败回滚」在单实例下 UI 看不到闪回。
	//
	// 如果将来多实例部署，这个前提失效 —— 那时要么用 advisory lock 让降模式
	// 全局可见，要么改成「先封账户→再降模式」接受锁持有更久。
	//
	// # 为什么不用 ON CONFLICT DO UPDATE
	//
	// settings 里可能根本没有 exec_mode 这个 key（首次启动前 GetExecMode 也当它是 paper）。
	// 裸 UPDATE 在 key 不存在时是 no-op，语义正确 —— 「不存在」等于 paper。
	// 用 ON CONFLICT 会凭空创建一行 exec_mode=paper，而「没配过」和「配置成 paper」
	// 在 GetExecMode 里虽然等价，但审计上不是一回事。
	r, err := tx.Exec(`UPDATE settings SET value = $1, updated_at = now()
WHERE key = $2 AND value IN ($3, $4, $5)`,
		string(ExecModePaper), ExecModeKey, string(ExecModeLiveWhitelist), string(ExecModeLiveManual), string(ExecModeLiveAuto))
	if err != nil {
		return res, fmt.Errorf("降模式失败: %w", err)
	}
	if n, _ := r.RowsAffected(); n > 0 {
		res.ModeDowngraded = true
	}

	// ---- ② 撤授权 ----
	//
	// 授权表按 (channel, account_ref) 定位，不是 account_id —— 那是它的自然键。
	// 没有活跃授权时不算失败：账户可能从未授权过（paper 模式不需要授权）。
	if strings.TrimSpace(channel) != "" && strings.TrimSpace(accountRef) != "" {
		r, err := tx.Exec(`UPDATE exec_authorizations
SET revoked_at = now(), revoked_reason = $3
WHERE channel = $1 AND account_ref = $2 AND revoked_at IS NULL`,
			strings.TrimSpace(channel), strings.TrimSpace(accountRef), reason)
		if err != nil {
			return res, fmt.Errorf("撤授权失败: %w", err)
		}
		if n, _ := r.RowsAffected(); n > 0 {
			res.AuthorizationRevoked = true
		}
	}

	// ---- ③ 拉黑账户 ----
	//
	// WHERE blocked_at IS NULL 让重复冻结成为 no-op（而不是覆盖首次的
	// blocked_at 与 block_reason —— 首次的原因才是审计要的）。
	r, err = tx.Exec(`UPDATE exec_accounts
SET blocked_at = now(), block_reason = $2
WHERE id = $1 AND blocked_at IS NULL`, accountID, reason)
	if err != nil {
		return res, fmt.Errorf("拉黑账户失败: %w", err)
	}
	n, _ := r.RowsAffected()
	if n > 0 {
		res.AccountBlocked = true
	} else {
		// 没改到行有两种可能：账户不存在，或已经被拉黑。
		// 必须区分 —— 前者是真错误，后者是幂等。
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM exec_accounts WHERE id=$1)`, accountID).Scan(&exists); err != nil {
			return res, fmt.Errorf("校验账户存在性失败: %w", err)
		}
		if !exists {
			return res, ErrExecAccountNotFound
		}
	}

	// ---- ④ 审计（必须在事务内）----
	//
	// # 为什么在事务内（ARCH §3.5 硬约束 1、2）
	//
	// 「审计是事实来源」。如果冻结生效但没记录，
	// 事后无法回答「谁在什么时候因为什么冻结的」——
	// 而对账发现差异这种事件，必须可追溯。
	//
	// # 「审计失败导致冻结失败」不等于「永不冻结」
	//
	// 事务回滚 → 下一 tick（60s）重试。
	// 所以「回滚」不是「放弃冻结」，是「下一次再来」。
	// 宁可冻结晚 60s，不可「冻结了但没人知道」。
	//
	// # 为什么写 exec_audit 而不是 activity
	//
	// activity.exploration_id 是 NOT NULL + CASCADE —— 执行层事件没有
	// exploration 上下文，且审计必须永久保留（不能随任务删除）。
	// 见 schema.sql 里 exec_audit 的注释。
	if err := d.WriteExecAudit(tx, AuditExecFrozen, "reconciler", map[string]any{
		"account_id":            accountID,
		"channel":               strings.TrimSpace(channel),
		"account_ref":           strings.TrimSpace(accountRef),
		"reason":                reason,
		"authorization_revoked": res.AuthorizationRevoked,
		"account_blocked":       res.AccountBlocked,
		"mode_downgraded":       res.ModeDowngraded,
	}); err != nil {
		// 审计写不进 → 整个事务回滚 → 冻结不生效。
		// 调用方（对账）会在下一 tick 重试。
		return res, fmt.Errorf("冻结审计写入失败（整个冻结已回滚，下一 tick 重试）: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return res, err
	}
	return res, nil
}
