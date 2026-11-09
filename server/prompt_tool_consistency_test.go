package server

// 「prompt 点名的工具必须在工具面里」的一致性守卫。
//
// # 为什么需要这个测试
//
// renderIntentTask 曾无条件说「写回用 record_fact / report_finding」：
//   - `record_fact` **这个工具名根本不存在**（recordFact() 是 Go 方法名，
//     注册的工具名是 emit_fact）
//   - `report_finding` 存在，但分析师没绑定
//
// 这类错误**不会报错**：模型看到工具列表里有 emit_fact，会自己纠正过去。
// 所以它一直没被发现，只是降低了一次成功率。
//
// # 三层断言
//
//  1. emitToolFor(role) 返回非空（渗透角色除外，它们用 emit_fact/report_finding 二元）
//  2. 该工具在 tools 表里**真实绑定了**这个角色
//  3. **交叉验证**：与 financialToolBindings[role].Tools 里声明的 emit_* 一致
//
// 第 3 条是关键：两处硬编码（emitToolFor 的 switch 和 financialToolBindings 的
// 工具清单）必须一致，否则 mapping 会漂移 —— 而这种漂移不会有任何运行时症状，
// 只有当 LLM 照着 prompt 去调不存在的工具时才暴露。

import (
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// financialRolesForTest 是 10 个金融角色（顺序无关，遍历用）。
var financialRolesForTest = []string{
	"analyst_market", "analyst_news", "analyst_fundamental", "analyst_sentiment_crypto",
	"debate_bull", "debate_bear", "research_manager",
	"trader", "risk_aggressive", "risk_neutral", "risk_conservative",
	"portfolio_manager",
}

// penetrationRolesForTest 是渗透角色（它们不走 emitToolFor，用二元写回）。
var penetrationRolesForTest = []string{
	"worker", "planner", "mainagent", "goals", "auto",
}

// TestEmitToolMentionedInPromptIsBound 三层断言，覆盖全部 16 个角色。
func TestEmitToolMentionedInPromptIsBound(t *testing.T) {
	d := setupToolResolve(t)

	t.Run("金融角色有 emit 工具且已绑定", func(t *testing.T) {
		for _, role := range financialRolesForTest {
			emitTool := agent.EmitToolForTest(role)
			if emitTool == "" {
				t.Errorf("%s: emitToolFor 返回空 —— 该角色的写回工具未定义，"+
					"renderIntentTask 会退化成渗透文案（说「只产生事实」）", role)
				continue
			}
			if !toolBoundTo(t, d, role, emitTool) {
				t.Errorf("%s: renderIntentTask 会说「写回用 %s」，"+
					"但该工具没绑定到这个角色 —— LLM 会调它然后得到「工具不存在」",
					role, emitTool)
			}
		}
	})

	t.Run("渗透角色不走 emitToolFor", func(t *testing.T) {
		for _, role := range penetrationRolesForTest {
			if got := agent.EmitToolForTest(role); got != "" {
				t.Errorf("%s: emitToolFor 应返回空（渗透走 emit_fact/report_finding 二元文案），"+
					"实际返回 %q", role, got)
			}
		}
	})

	t.Run("交叉验证 emitToolFor 与 financialToolBindings 一致", func(t *testing.T) {
		for role, binding := range financialToolBindings {
			emitTool := agent.EmitToolForTest(role)
			if emitTool == "" {
				continue
			}
			// binding.Tools 里必须恰有一个 emit_* 且等于 emitToolFor 的结果
			var declared []string
			for _, k := range binding.Tools {
				if strings.HasPrefix(k, "emit_") {
					declared = append(declared, k)
				}
			}
			if len(declared) != 1 {
				t.Errorf("%s: financialToolBindings 里应有且仅有 1 个 emit_* 工具，实际 %v",
					role, declared)
				continue
			}
			if declared[0] != emitTool {
				t.Errorf("%s: emitToolFor 说 %q，但 financialToolBindings 绑的是 %q —— "+
					"两处硬编码漂移了，prompt 会点名一个没绑定的工具", role, emitTool, declared[0])
			}
		}
	})
}

// TestRenderIntentTaskPerRole 各角色的意图块文案。
func TestRenderIntentTaskPerRole(t *testing.T) {
	cases := []struct {
		role     string
		wantTool string
		denyText string
	}{
		{"analyst_market", "emit_fact", "只产生事实"}, // 金融不该说「只产生事实」
		{"debate_bull", "emit_thesis", "只产生事实"},
		{"research_manager", "emit_thesis", "只产生事实"},
		{"trader", "emit_signal", "只产生事实"},
		{"risk_neutral", "emit_thesis", "只产生事实"},
		{"portfolio_manager", "emit_rating", "只产生事实"},
		{"worker", "emit_fact", "record_fact"}, // 渗透保留二元文案，但不该出现 record_fact 这个名字
	}
	for _, c := range cases {
		got := agent.RenderIntentTaskForTest(c.role, 42)
		if !strings.Contains(got, c.wantTool) {
			t.Errorf("%s: 文案应含 %q，实际: %s", c.role, c.wantTool, got)
		}
		if c.denyText != "" && strings.Contains(got, c.denyText) {
			t.Errorf("%s: 文案不该含 %q，实际: %s", c.role, c.denyText, got)
		}
	}
}

// TestRenderIntentTaskNeverMentionsRecordFact record_fact 这个工具名从不存在。
//
// 这是本次事故的直接回归 —— 所有角色都不该再看到它。
func TestRenderIntentTaskNeverMentionsRecordFact(t *testing.T) {
	all := append(append([]string{}, financialRolesForTest...), penetrationRolesForTest...)
	for _, role := range all {
		got := agent.RenderIntentTaskForTest(role, 1)
		if strings.Contains(got, "record_fact") {
			t.Errorf("%s: 文案含 record_fact —— 该工具不存在（应为 emit_fact），"+
				"模型会调用失败。实际: %s", role, got)
		}
	}
}

// TestConstraintBlockPerRole 约束块结尾段按角色分叉。
func TestConstraintBlockPerRole(t *testing.T) {
	fin := agent.ConstraintBlockTailForTest("analyst_market")
	if strings.Contains(fin, "端口") || strings.Contains(fin, "主机") {
		t.Errorf("金融角色不该看到「端口/主机」（渗透资产类型）：%s", fin)
	}
	if strings.Contains(fin, "派生意图") {
		t.Errorf("金融角色不该看到「派生意图」（金融任务没有 planner 在跑）：%s", fin)
	}
	if !strings.Contains(fin, "data_gap") {
		t.Errorf("金融角色的结尾段应说「记为 data_gap」：%s", fin)
	}

	pen := agent.ConstraintBlockTailForTest("worker")
	if !strings.Contains(pen, "端口") || !strings.Contains(pen, "主机") {
		t.Errorf("渗透角色的结尾段应保留「端口/主机」：%s", pen)
	}
	if !strings.Contains(pen, "派生意图") {
		t.Errorf("渗透角色的结尾段应保留「派生意图」：%s", pen)
	}
}

// toolBoundTo 查 tools 表里 toolKey 是否绑定了 agentKey 角色。
func toolBoundTo(t *testing.T, d *db.DB, agentKey, toolKey string) bool {
	t.Helper()
	var bound bool
	// PG 用 `agents ? $1`（jsonb 键存在操作符）判断数组里有没有该 agent；
	// SQLite 的 `?` 是占位符，会撞语法（`near "?"`）。改用 json_each 成员判断。
	err := d.QueryRow(`SELECT EXISTS (SELECT 1 FROM json_each(agents) WHERE value = $1)
    FROM tools WHERE key = $2`, agentKey, toolKey).Scan(&bound)
	if err != nil {
		t.Fatalf("查 tools 表失败（%s/%s）: %v", agentKey, toolKey, err)
	}
	return bound
}
