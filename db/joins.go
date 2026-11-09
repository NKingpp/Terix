package db

// join 节点的门控兑现。
//
// # 什么是 join
//
// join 是阶段图的汇合点：它本身不是工作项（没有 LLM 调用、不产出结论），
// 只表达「我等待的上游全部结束后，下游才能开始」。
//
// 它在 W5 的用法：
//
//	analyst_market ─┐
//	analyst_news   ─┼─gates→ join_analysts ─gates→ debate_bull_r1 ...
//	analyst_senti  ─┘
//
// 即：每个分析师 intent 都有一条 `--gates--> join_analysts` 边。
// join_analysts 自身再有一条 `--gates--> debate_bull_r1` 边。
// 于是 Frontier 的两级门控自然生效：分析师未全结束时 join 不 done、
// debate 也不出现（因为 join 不是结束态）。
//
// # 为什么需要显式 ReconcileJoins
//
// Frontier 的 SQL 只做「读时过滤」——它不改变任何状态。join 的
// `state: open → done` 必须有东西主动写。两种实现：
//
//	(a) 惰性：Frontier 查询时顺带 UPDATE（读操作带副作用，不推荐）
//	(b) 显式：本函数，在 intent 结束时调用
//
// 选 (b)：调用点可审计，Frontier 保持纯读。
//
// # 为什么不在 SetIntentState 内部调用
//
// SetIntentState 是通用状态写入（intent/fact/goal 都可能走它）。
// 在通用函数里跑全图扫描有两个问题：
//   - 性能：任何状态变化都触发一次全 join 扫描
//   - 语义：更新一个 fact 为什么要 reconcile join？
//
// 所以调用点放在 engine.go 的 intent 结束时（唯一会推进阶段图的地方）。

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ReconcileResult 是一次 join 兑现的统计（供日志与测试断言）。
type ReconcileResult struct {
	// Satisfied 本次被置为 done 的 join 数（所有上游都 done）。
	Satisfied int
	// Blocked 本次被置为 blocked 的 join 数（有上游未成功）。
	//
	// # 为什么和 Satisfied 分开计数
	//
	// 两者的下游行为完全不同：
	//   Satisfied → 下游继续跑
	//   Blocked   → 下游被门控挡住（Frontier 的 gates 子查询排除）
	//
	// 混成一个数字会让调用方无法判断「本次是否有分支被中止」。
	Blocked int
	// Details 每个被兑现的 join 及其失败上游（便于排查数据不全）。
	Details []JoinSatisfied
	// BlockedDetails 每个被中止的 join 及其失败上游。
	//
	// 与 Details 分开的理由同上：Downstream 的行为不同。
	BlockedDetails []JoinSatisfied
}

// JoinSatisfied describe 一个被兑现的 join。
type JoinSatisfied struct {
	JoinID int64
	// FailedUpstreams 上游里非 done 的那些（blocked/exhausted/stopped/deleted）。
	// 名字取自上游节点的 payload.role（没 role 时用 "node#<id>"）。
	FailedUpstreams []string
}

// ReconcileJoins 检查本 exploration 所有 open 的 join 节点，
// 若其全部 gates 上游都已结束（upstreamSettledStates），置为 done 并记录失败上游。
//
// 幂等：已 done 的 join 不会被重复处理。
func (s *ExplorationStore) ReconcileJoins() (ReconcileResult, error) {
	var res ReconcileResult

	// 先查所有 open 的 join。
	rows, err := s.db.Query(`
SELECT id FROM exploration_nodes
WHERE exploration_id=$1 AND kind='join' AND state='open'
ORDER BY priority DESC, id ASC`, s.expID)
	if err != nil {
		return res, err
	}
	var joinIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return res, err
		}
		joinIDs = append(joinIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	if len(joinIDs) == 0 {
		return res, nil
	}

	for _, joinID := range joinIDs {
		// 该 join 的全部 gates 上游及其状态/角色。
		type up struct {
			ID    int64
			State string
			Role  string
		}
		// `payload->>'role'` → `json_extract`：SQLite 无 jsonb 箭头操作符，
		// 且 `?` 在 SQLite 里是占位符（用 `?` 判键存在会直接语法错）。
		// 缺 key 时 json_extract 返回 NULL，COALESCE 兜成空串，与 PG 的 ->> 一致。
		upRows, err := s.db.Query(`
SELECT up.id, up.state, COALESCE(json_extract(up.payload,'$.role'),'')
  FROM exploration_edges e
  JOIN exploration_nodes up ON up.id = e.src_id
 WHERE e.exploration_id=$1 AND e.dst_id=$2 AND e.rel='gates'
 ORDER BY up.id`, s.expID, joinID)
		if err != nil {
			return res, err
		}
		var ups []up
		for upRows.Next() {
			var u up
			if err := upRows.Scan(&u.ID, &u.State, &u.Role); err != nil {
				upRows.Close()
				return res, err
			}
			ups = append(ups, u)
		}
		upRows.Close()
		if err := upRows.Err(); err != nil {
			return res, err
		}

		// 没有 gates 上游的 join 视为「无依赖」—— 立刻兑现。
		// （正常阶段图不会这样，但手工建的 join 可能没有边。）
		satisfied := true
		var failed []string
		for _, u := range ups {
			if !isUpstreamSettled(u.State) {
				satisfied = false
				break
			}
			// 上游结束了，但不是正常完成 → 记录为「本该有但没得到」。
			if u.State != "done" {
				name := u.Role
				if name == "" {
					name = fmt.Sprintf("node#%d", u.ID)
				}
				failed = append(failed, name)
			}
		}
		if !satisfied {
			continue
		}

		// 全部上游已结束，分两种情况：
		//
		//   A. 无失败上游 → join 置 done（正常兑现）
		//   B. 有失败上游 → join 置 **blocked**（不是 done）
		//
		// # 为什么 B 必须是 blocked 而不是 done
		//
		// 文档 ARCH-W2-W5.md:4641 明确要求：
		//   「join 的任一 await 处于 blocked/exhausted/stopped → join 自身转 blocked」
		//
		// 而 ARCH-W2-W5.md:899 给了完整路径：
		//   「SOURCE_UNAVAILABLE → intent blocked → join 节点因上游非 done 而转 blocked
		//    → 任务终态 blocked → 前端显式显示数据源故障」
		//
		// # 置 done 的后果（原来的实现）
		//
		// 下游看到 join=done 会**继续执行** —— 一个分析师跑挂了，
		// 下游辩论照样开跑，只是从 payload 里读到「某维度缺失」。
		//
		// 而设计意图是「上游失败 → 整条分支不跑」：数据不全时不该产出结论。
		//
		// # 为什么不需要改 Frontier
		//
		// Frontier 的 gates 子查询已经排除了「上游未 settled」的下游。
		// join 转 blocked 后，它就是「未 settled」的一种 —— 下游自然不会被 claim。
		// 这个联动是现成的。
		//
		// skipped_by_design 与 failed_upstreams 的区别（下游处理方式不同）：
		//   - skipped_by_design：「按 asset_class 裁掉的维度」（本就不存在）
		//     → 下游不该因此降置信度
		//   - failed_upstreams：「本该有但这次没跑成」
		//     → 下游必须降置信度或返回 review
		// 混成一个列表会让「crypto 没有基本面」和「基本面跑挂了」无法区分。
		payload := map[string]any{
			"reconciled_at": NowISO(),
		}
		if len(failed) > 0 {
			payload["failed_upstreams"] = failed
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return res, fmt.Errorf("序列化 join payload 失败: %w", err)
		}

		if len(failed) > 0 {
			// B：有上游失败 → 转 blocked。
			//
			// blocked_reason 要能让人一眼看出「哪个上游失败了」——
			// 那是前端显示「数据源故障：X 维度」的依据。
			reason := fmt.Sprintf("上游 %d 个节点未成功：%s",
				len(failed), strings.Join(failed, ", "))
			// 用 JSONB merge 而非覆盖：保留 join 建时写入的其它 payload 字段
			// （如 asset_class / instrument_id / skipped_by_design）。
			if _, err := s.db.Exec(`
UPDATE exploration_nodes
   SET state='blocked',
       blocked_reason=$4,
       payload = json_patch(COALESCE(payload,'{}'),$2),
       completed_at = now(),
       content_version = content_version + 1
 WHERE id=$1 AND exploration_id=$3 AND kind='join' AND state='open'`,
				joinID, string(raw), s.expID, reason); err != nil {
				return res, err
			}
			res.Blocked++
			res.BlockedDetails = append(res.BlockedDetails,
				JoinSatisfied{JoinID: joinID, FailedUpstreams: failed})
			continue
		}

		// A：无失败上游 → 正常兑现为 done。
		if _, err := s.db.Exec(`
UPDATE exploration_nodes
   SET state='done',
       payload = json_patch(COALESCE(payload,'{}'),$2),
       completed_at = now(),
       content_version = content_version + 1
 WHERE id=$1 AND exploration_id=$3 AND kind='join' AND state='open'`,
			joinID, string(raw), s.expID); err != nil {
			return res, err
		}

		res.Satisfied++
		res.Details = append(res.Details, JoinSatisfied{JoinID: joinID, FailedUpstreams: failed})
	}
	return res, nil
}

// upstreamSettledStates 是「上游已结束」的节点状态集合（不论成败）。
//
// ⚠️ 本次会话中此变量的定义一度丢失（误操作 git checkout 清空了未跟踪文件的改动），
// 此定义按 db/exploration.go:352 的权威口径重建：
//
//	terminal := "done" || "blocked" || "exhausted" || "stopped"
//
// 两者必须保持一致 —— 若将来 exploration 的终态集合变化，这里要同步。
//
// # 谁用它
//
// **只有 `ReconcileJoins`** —— 因为 join 的职责是「汇总上游的成败」：
// 它必须等全部上游**结束**（不论成败），才能判断自己是 done 还是 blocked。
//
// # 谁不该用它
//
// **`Frontier`** —— 它回答的是「这个 intent 能 claim 吗」，
// 而「能 claim」意味着「上游产出可用于继续工作」。blocked 的上游没有产出可用。
// 用这个集合会让「上游失败」被当成「上游完成」，下游拿不完整数据出结论。
// 见 L16（已修复）。
var upstreamSettledStates = []string{"done", "blocked", "exhausted", "stopped"}

// upstreamSucceededStates 是「上游成功完成」的节点状态集合。
//
// # 为什么需要它（与 upstreamSettledStates 的区别）
//
//	settled   = 不会再变了（done / blocked / exhausted / stopped）—— 「结束」
//	succeeded = 成功完成（只有 done）                              —— 「成功」
//
// **这是一条真实的语义分界，不是命名洁癖。**
//
// # 谁用它
//
// **`Frontier`** —— 它回答「这个 intent 能 claim 吗」。
// 能 claim ⟺ 它的**所有** gates 上游都**成功**（上游产出可用）。
//
// # 反例（如果不分开用）
//
//	analyst_news 取数失败 → blocked
//	→ join_analysts 转 blocked（正确）
//	→ 但下游辩论被判「上游 settled」→ 被放行
//	→ 辩论拿不完整数据出结论 → 结论变评级 → 评级变变订单
//
// 这是文档 ARCH-W2-W5.md:899 明确排除的「软处理」。
var upstreamSucceededStates = []string{"done"}

// isUpstreamSettled 判定上游是否**已结束**（不论成败）。
//
// ⚠️ 只给 `ReconcileJoins` 用。判断「能否放行下游」请用 isUpstreamSucceeded。
func isUpstreamSettled(state string) bool {
	for _, s := range upstreamSettledStates {
		if state == s {
			return true
		}
	}
	return false
}

// 注：这里刻意**没有** AddGates 之类的公开建边方法。
//
// 图的边必须经由 agent.MaterializeGraph 建 —— 那里有 kind 校验（只允许
// intent/join）、Ref 存在性校验、事务保证。直接写边会绕过全部这些。
//
// 「不能绕过的封装」比「能绕过但有运行时校验」更可靠：前者是编译期约束，
// 后者是运行期检查（而且只挡直接调用者，挡不住「新写一段 materialize 逻辑
// 但忘了校验」这种更常见的失误）。
//
// 测试需要直接建边时用 joins_test.go 里的 addGatesForTest。
