package db

// 测试残留守卫（C1 的机械化版本，见《KNOWN-LIMITATIONS》C1，
// docs 仓库：quantex/KNOWN-LIMITATIONS.md）。
//
// # 为什么需要它
//
// C1 写在文档里，但写代码时想不起来 —— 至少已踩 9 次同一个坑
// （llm_profiles / company_scope / activity 死锁 / 158 处批量修正 /
// blackboard × 2 / notify 裸 DELETE / 故障注入先关池 / emit 测试漏清 snapshot）。
// 文档靠人「记得读」，守卫靠机器「强制拦截」。
//
// # 设计
//
// TestMain 在 m.Run() 前后各数一次四张关键表的行数，delta != 0 时
// 让整个包失败（即使所有测试本身 PASS）。把「残留」从「需要主动查」
// 变成「默认暴露」。
//
// # 为什么是这六张 —— 以及两类残留的危害分级
//
// **动态危害（优先覆盖）**：
//
//	notification_channels / notification_events
//	  → 残留会**改变其他测试的行为**，让测试结果不可复现。
//	    第 10 次残留就是这样：一条遗留的 digest 频道（filter={}）会接收
//	    本该被过滤的事件，让 TestFanOutMarksEventsWithNoMatchingChannel 失败。
//	    而那个测试自己只建了 realtime 频道 —— 它「依赖库的全局状态」了。
//
// **静态危害**：
//
//	tasks / explorations / exploration_nodes / snapshots
//	  → 残留让行数增长、占空间，但不影响正确性（测试按 id/task_id 隔离）。
//
// **exec 组（2026-10-29 新增）**：
//
//	orders_draft / orders_execution / draft_risk_verdicts / exec_reconciliations
//	exec_audit / exec_accounts
//	  → 静态危害为主（测试按 draft_id / account_id 隔离），但有两个例外：
//
//	    ① exec_audit 是 append-only 审计，正常 cleanup 很难覆盖
//	       （它按 detail->>'account_id' 关联，账户删了审计还在）→ 会永久累积。
//	    ② exec_accounts 残留会让 dashboard 显示「账户 N 个」而实际只有 1 个真实的 ——
//	       那是**用户可见的错误**，不只是脏数据。
//
// ⚠️ 这轮的实证：修完 orders_draft 的泄漏后，exec_audit 当天又涨了 25 条，
// 而它不在原六张表的检查范围内 —— 所以那 25 条从来没被任何检查发现过。
// 检查范围必须覆盖「所有会被测试写入的表」，而不是「当前正在漏的那张」。
//
// **动态危害优先级更高**：它让「测试通过」这个信号本身失去意义 ——
// 因为你不知道通过是「代码对了」还是「环境碰巧合适」。

import (
	"database/sql"
	"fmt"
	"os"
)

// ResidueCounts 六张关键表的行数快照（导出供 agent 包复用）。
type ResidueCounts struct {
	// 静态危害组（残留只是脏，不影响正确性）
	Tasks        int64
	Explorations int64
	Nodes        int64
	Snapshots    int64
	// Findings 是渗透功能的漏洞记录。
	//
	// # 为什么它必须被守卫
	//
	// 实测（2026-10-29→10-30）：跑一次五包测试，findings 从 12004 涨到 12085（+81）。
	// 而它原先不在守卫里 —— 所以那 81 条**从来没有被任何检查发现过**。
	//
	// 12085 条全部 task_id=NULL、vulnclass 全是 TEST/SQL注入/TCP 这类渗透语义 ——
	// 即「渗透功能的残留」里最主要的一张表。
	//
	// # 为什么它和 NotifyEvents 是同一类
	//
	// 两个都是「测试经 AddFinding 间接写、而没人负责清」的表。
	// 唯一区别是 NotifyEvents 已纳入守卫并加了 id 上界清理，而 Findings 没有。
	//
	// # 为什么不单独统计它的两个子表
	//
	// finding_traffic_bindings 对 findings 是 ON DELETE CASCADE，
	// 所以它们的行数跟着 findings 涨落 —— 数 findings 本表就足够了。
	Findings int64
	// 动态危害组（残留改变其他测试行为）
	NotifyChannels int64
	NotifyEvents   int64
	// exec 组（2026-10-29 新增，见文件头注释里的两类例外）
	OrdersDraft         int64
	OrdersExecution     int64
	DraftRiskVerdicts   int64
	ExecReconciliations int64
	ExecAudit           int64
	ExecAccounts        int64
	// Agents 是自定义 agent 行数（builtin=false）。
	//
	// # 为什么纳入守卫
	//
	// 实测（2026-10-30）：库里积了 123 条 `lock_agent_*` / `write_lock_agent_*`。
	// 它们是 db/task_context_lock_test.go 用 CreateAgent 建的临时 agent，
	// 而该测试的 t.Cleanup 里有 DeleteAgent —— 但 Cleanup 只在测试返回时跑，
	// 中途 t.Fatal（锁等待超时那类）会让后来的 goroutine 仍在跑，
	// DeleteAgent 撞上 FOR UPDATE 锁后失败，被 `_ =` 吞掉。
	//
	// 那 123 条**从来没有被任何检查发现过**，因为 agents 不在守卫里。
	//
	// # 为什么只统计 builtin=false
	//
	// builtin 的 6 个（goals/planner/...）+ 9 个金融角色是**正常数据**，
	// 行数恒定（15）。把它们算进去只会让总数漂移，掩盖真实残留。
	// 所以查询用 `WHERE builtin=false` —— 只有「测试建的」才可能涨。
	Agents int64
	// Signals / Ratings 是金融链路的产出表（trader 的 emit_signal、
	// portfolio_manager 的 emit_rating）。
	//
	// # 为什么必须纳进来
	//
	// 实测（2026-10-30）：signals 里有一条 2026-10-27 的历史行，而它**不在守卫里** ——
	// 所以任何「跑测试写了一条 signal 但没清」的泄漏都不会被发现。
	//
	// 这两张表的特殊性：`DeleteTask` 不删它们（它们挂在 instrument_id 上，
	// 不挂 task_id）。所以「删任务」这个最常见的 cleanup 动作对它们无效 ——
	// 必须由测试自己按 instrument_id 删，而漏了这个动作没人会发现。
	Signals int64
	Ratings int64
}

func (r ResidueCounts) String() string {
	return fmt.Sprintf("tasks=%d explorations=%d nodes=%d snapshots=%d findings=%d "+
		"channels=%d events=%d "+
		"draft=%d execution=%d verdicts=%d recons=%d audit=%d accts=%d agents=%d "+
		"signals=%d ratings=%d",
		r.Tasks, r.Explorations, r.Nodes, r.Snapshots, r.Findings,
		r.NotifyChannels, r.NotifyEvents,
		r.OrdersDraft, r.OrdersExecution, r.DraftRiskVerdicts, r.ExecReconciliations,
		r.ExecAudit, r.ExecAccounts, r.Agents,
		r.Signals, r.Ratings)
}

func (r ResidueCounts) total() int64 {
	return r.Tasks + r.Explorations + r.Nodes + r.Snapshots + r.Findings +
		r.NotifyChannels + r.NotifyEvents +
		r.OrdersDraft + r.OrdersExecution + r.DraftRiskVerdicts + r.ExecReconciliations +
		r.ExecAudit + r.ExecAccounts +
		r.Agents + r.Signals + r.Ratings
}

// CountResidueRows 查六张表的当前行数。查询失败返回 error（调用方决定是否忽略）。
func CountResidueRows(conn *sql.DB) (ResidueCounts, error) {
	var r ResidueCounts
	err := conn.QueryRow(`
SELECT (SELECT count(*) FROM tasks),
       (SELECT count(*) FROM explorations),
       (SELECT count(*) FROM exploration_nodes),
       (SELECT count(*) FROM snapshots),
       (SELECT count(*) FROM findings),
       (SELECT count(*) FROM notification_channels),
       (SELECT count(*) FROM notification_events),
       (SELECT count(*) FROM orders_draft),
       (SELECT count(*) FROM orders_execution),
       (SELECT count(*) FROM draft_risk_verdicts),
       (SELECT count(*) FROM exec_reconciliations),
       (SELECT count(*) FROM exec_audit),
       (SELECT count(*) FROM exec_accounts),
       (SELECT count(*) FROM agents WHERE builtin=false),
       (SELECT count(*) FROM signals),
       (SELECT count(*) FROM ratings)`).Scan(
		&r.Tasks, &r.Explorations, &r.Nodes, &r.Snapshots, &r.Findings,
		&r.NotifyChannels, &r.NotifyEvents,
		&r.OrdersDraft, &r.OrdersExecution, &r.DraftRiskVerdicts,
		&r.ExecReconciliations, &r.ExecAudit, &r.ExecAccounts, &r.Agents,
		&r.Signals, &r.Ratings)
	return r, err
}

// checkResidue 对比前后行数，有残留时打印并返回 true（表示应判失败）。
//
// 单独成函数以便两个包的 TestMain 共用，也便于单测。
func CheckResidue(conn *sql.DB, before ResidueCounts) (hasResidue bool, msg string) {
	after, err := CountResidueRows(conn)
	if err != nil {
		// 查不出来不阻断（数据库可能已被某测试关掉），但要说出来。
		return false, fmt.Sprintf("⚠ 残留检查失败（无法查询行数）: %v", err)
	}
	if after.total() == before.total() {
		return false, ""
	}
	return true, fmt.Sprintf(
		"\n❌ 测试残留 %d 行\n   前: %s\n   后: %s\n   见《KNOWN-LIMITATIONS》C1（docs 仓库：quantex/KNOWN-LIMITATIONS.md）\n"+
			"   常见原因: 1) defer d.Close() 与 t.Cleanup 顺序冲突 "+
			"2) 裸 DELETE FROM tasks 绕过 DeleteTask 3) t.Cleanup 里吞掉错误\n",
		after.total()-before.total(), before.String(), after.String())
}

// reportResidue 是 TestMain 里的一句话封装：打印并返回是否应判失败。
func ReportResidue(conn *sql.DB, before ResidueCounts) bool {
	has, msg := CheckResidue(conn, before)
	if msg != "" {
		fmt.Fprint(os.Stderr, msg)
	}
	return has
}

// NotificationWatermark 记录「测试开始前」通知类表的 id 上界。
//
// # 为什么需要「上界 + 统一删」而不是「逐个测试加 cleanup」
//
// notification_events 没有外键指向 findings（finding_id 是裸 bigint），
// 而 findings.task_id 是 ON DELETE SET NULL —— 所以「删 task/finding」
// 永远清不掉这些事件行，它们会永久累积。
//
// 而一个测试可能经 report_finding / AddFinding **间接**写事件，
// 逐个加 cleanup 必然漏（server 包实测漏了 29 行）。
//
// # 为什么 id 上界是可靠分界
//
// 各包测试都持有同一把 advisory lock（7337741002），所以测试串行执行；
// 而 id 是单调递增的。所以「跑测试前的 max(id)」之后的所有行，
// 都是本包测试产生的。
//
// ⚠️ **前提：持有 advisory lock。** 没持有时上界会被别的包的写入污染 ——
// 那时 Purge 会误删别人正在用的行，或留下本该删的行。
type NotificationWatermark struct {
	EventMax   int64
	ChannelMax int64
	// FindingMax 是 findings 表的 id 上界。
	//
	// # 为什么 findings 也要上界清理（与通知表同一类）
	//
	// findings.task_id 是 ON DELETE SET NULL —— 删 task 时 finding 会保留
	// （漏洞有审计价值，这是有意的设计）。所以任何「建了 finding」的测试都必须
	// 自己清，否则 findings 表永久累积。
	//
	// 实测（2026-10-30）：五包跑一次让 findings 涨 81 条
	// （db +6 / agent +4 / server +71），而它原先不在守卫也不在上界清理里 ——
	// 那 81 条从来没被发现过。
	//
	// 用上界清理而不是逐个测试加 cleanup：server 包有 8 个测试文件经
	// report_finding 间接写 findings，逐个加会漏（这正是 notification_events 的经验）。
	FindingMax int64
}

// TakeNotificationWatermark 记录通知两张表的 id 上界。
//
// 返回 error 时调用方应**跳过 Purge 与守卫**（而不是用零值继续）——
// 因为零上界会让 Purge 删掉全表。
func TakeNotificationWatermark(conn *sql.DB) (NotificationWatermark, error) {
	var w NotificationWatermark
	if err := conn.QueryRow(
		`SELECT COALESCE(max(id),0) FROM notification_events`).Scan(&w.EventMax); err != nil {
		return w, err
	}
	if err := conn.QueryRow(
		`SELECT COALESCE(max(id),0) FROM notification_channels`).Scan(&w.ChannelMax); err != nil {
		return w, err
	}
	if err := conn.QueryRow(
		`SELECT COALESCE(max(id),0) FROM findings`).Scan(&w.FindingMax); err != nil {
		return w, err
	}
	return w, nil
}

// Purge 删掉上界之后的所有通知行。
//
// ⚠️ **调用方必须先判断 TakeNotificationWatermark 的 error。**
//
// 上界查询失败时 EventMax 是零值，而 `DELETE WHERE id > 0` 会**删光整张表** ——
// 那是「查询失败」变成「数据丢失」。
//
// 正确用法：
//
//	wm, err := TakeNotificationWatermark(conn)
//	if err != nil {
//	    fmt.Fprintf(os.Stderr, "⚠ 上界查询失败，跳过事件清理: %v\n", err)
//	}
//	...
//	if err == nil {
//	    wm.Purge(conn)
//	}
//
// # 为什么 Purge 自己不检查
//
// 因为它拿不到 error —— 那是在 Take 阶段产生的，而 Go 的结构体方法
// 没有「携带调用方错误」的自然方式。所以这个约定只能写在注释里，
// 而注释是唯一能阻止下一个人写错的东西。
//
// # 为什么必须在守卫对比**之前**调用
//
// 否则守卫会把「已知会产生的测试副作用」当成残留报错。那种守卫因为
// 「永远红」会被无视，而「被无视的守卫」等于没有。
//
// # 为什么删除失败只打印不阻断
//
// 清不掉会让守卫在下一步报残留 —— 那已经足够让人知道。
// 在 TestMain 里 Fatal 会掩盖原本的测试结果。
func (w NotificationWatermark) Purge(conn *sql.DB) {
	if _, err := conn.Exec(`DELETE FROM notification_events WHERE id > $1`, w.EventMax); err != nil {
		fmt.Fprintf(os.Stderr, "⚠ 清理 notification_events 失败: %v\n", err)
	}
	if _, err := conn.Exec(`DELETE FROM notification_channels WHERE id > $1`, w.ChannelMax); err != nil {
		fmt.Fprintf(os.Stderr, "⚠ 清理 notification_channels 失败: %v\n", err)
	}
	// findings 的子表（retests / bindings）是 ON DELETE CASCADE，只删本表即可。
	if _, err := conn.Exec(`DELETE FROM findings WHERE id > $1`, w.FindingMax); err != nil {
		fmt.Fprintf(os.Stderr, "⚠ 清理 findings 失败: %v\n", err)
	}
}

// findDecreased 找出行数**减少**的表。
//
// 返回 (是否存在减少, 描述)。描述形如 "agents: 15 → 13 (少 2)"。
//
// # 为什么这个函数是必要的
//
// CountResidueRows 只汇总 total。而「删 2 行、加 2 行」的净变化是 0 ——
// 那正是本次事故的形态：测试删了用户的 OKX 凭据，又插了自己的假凭据。
// 只比 total 永远发现不了。
//
// 逐表比对能指名道姓说出「哪张表少了多少行」，修复方向就明确了。
func findDecreased(before, after ResidueCounts) (bool, string) {
	type pair struct {
		name string
		b, a int64
	}
	pairs := []pair{
		{"tasks", before.Tasks, after.Tasks},
		{"explorations", before.Explorations, after.Explorations},
		{"nodes", before.Nodes, after.Nodes},
		{"snapshots", before.Snapshots, after.Snapshots},
		{"findings", before.Findings, after.Findings},
		{"notification_channels", before.NotifyChannels, after.NotifyChannels},
		{"notification_events", before.NotifyEvents, after.NotifyEvents},
		{"orders_draft", before.OrdersDraft, after.OrdersDraft},
		{"orders_execution", before.OrdersExecution, after.OrdersExecution},
		{"draft_risk_verdicts", before.DraftRiskVerdicts, after.DraftRiskVerdicts},
		{"exec_reconciliations", before.ExecReconciliations, after.ExecReconciliations},
		{"exec_audit", before.ExecAudit, after.ExecAudit},
		{"exec_accounts", before.ExecAccounts, after.ExecAccounts},
		{"agents", before.Agents, after.Agents},
		{"signals", before.Signals, after.Signals},
		{"ratings", before.Ratings, after.Ratings},
	}
	var out string
	any := false
	for _, p := range pairs {
		if p.a < p.b {
			any = true
			out += fmt.Sprintf("%s: %d → %d（少 %d 行）\n   ", p.name, p.b, p.a, p.b-p.a)
		}
	}
	return any, out
}
