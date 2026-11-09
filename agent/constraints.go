package agent

import (
	"strings"

	"github.com/Autumn-27/artex/db"
)

// constraintBlock renders this task's operation constraints (task_constraints) as a
// high-priority block appended to the planner/worker system prompt. allow/deny are
// grouped; empty string when there are no constraints (or ts is nil). The framing
// deliberately puts these ABOVE the exploration/expansion heuristics so a declared
// boundary wins the tug-of-war against "chase another entry surface".
func constraintBlock(ts *db.ExplorationStore, agentKey string) string {
	if ts == nil {
		return ""
	}
	rows, err := ts.ListConstraints()
	if err != nil || len(rows) == 0 {
		return ""
	}
	var allow, deny []string
	for _, c := range rows {
		text := strings.TrimSpace(c.Text)
		if text == "" {
			continue
		}
		if c.Kind == "allow" {
			allow = append(allow, "- "+text)
		} else {
			deny = append(deny, "- "+text)
		}
	}
	if len(allow) == 0 && len(deny) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【操作约束（最高优先级，凌驾于下方一切探索/拓面启发式；每生成一条意图、每执行一个动作前都必须先自检是否违反，违反即不得进行）】：")
	if len(allow) > 0 {
		b.WriteString("\n允许的操作：\n")
		b.WriteString(strings.Join(allow, "\n"))
	}
	if len(deny) > 0 {
		b.WriteString("\n禁止的操作：\n")
		b.WriteString(strings.Join(deny, "\n"))
	}
	// 结尾段的措辞必须按角色分叉：渗透说「新目标/新端口/新主机 → out-of-scope 事实 →
	// 不派生意图」；金融任务没有「端口/主机」，也没有 planner 在跑（意图已由
	// MaterializeGraph 建好），说「派生意图」会让模型以为它可以扩图。
	b.WriteString(constraintTailFor(agentKey))
	return b.String()
}

// constraintTailFor 返回约束块的结尾段（按角色分叉）。
//
// 抽成独立函数是为了可测：测试直接断言这段文字，不必构造带约束的 store。
func constraintTailFor(agentKey string) string {
	if isFinancialPrompt(agentKey) {
		return "\n（约束之外的分析维度，不等于获得了许可：若结论需要用到被禁止的数据或方法，" +
			"记为 data_gap 并说明缺什么，不得自行扩大分析范围或改用替代口径绕过约束。）"
	}
	return "\n（发现约束之外的新目标/新端口/新主机，不等于获得授权：除非它落在上述允许范围内，" +
		"否则记为 out-of-scope 事实并跳过，不得为其派生意图或执行动作。）"
}
