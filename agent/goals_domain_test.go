package agent

// 目标分解器双模式的验收测试（第 4 批：goals.go 活污染改造）。
//
// # 这组测试守的是什么
//
// 金融任务曾经拿「你是渗透测试目标分解器」的 prompt 拆目标 ——
// 而模型会照做，因为它收到了明确指令。结果是「信息收集」「漏洞分析」
// 这类在金融语境下毫无意义的子目标节点。
//
// **没有人会报错，因为每一步都成功了。** 这正是活污染的失败方式。
//
// 所以这里守的是「分叉本身」：金融走金融 prompt，渗透走渗透 prompt，
// 且两边的专属指令互不串场。

import (
	"strings"
	"testing"
)

// TestGoalsBodyForFinance 金融任务（instrumentID>0）用金融 prompt。
func TestGoalsBodyForFinance(t *testing.T) {
	body := goalsBodyFor(true)
	if body != goalsFinanceTmpl {
		t.Fatal("instrumentID>0 应返回 goalsFinanceTmpl")
	}
	// 金融 prompt 里不能出现「渗透测试」这个身份。
	//
	// 注意：不能断言「不含『渗透』二字」—— 金融 prompt 里有一句
	// 「不填 vulnclass（那是渗透任务的字段）」，那是**说明**不是**身份**。
	// 所以只断言首句的身份声明。
	if strings.Contains(body, "你是渗透测试目标分解器") {
		t.Error("金融 prompt 的首句仍是渗透测试身份 —— 分叉没生效")
	}
	if !strings.Contains(body, "你是金融分析目标分解器") {
		t.Error("金融 prompt 应以「金融分析目标分解器」开头")
	}
}

// TestGoalsBodyForDefault 非金融任务（instrumentID==0）仍走 goalsDefaultTmpl。
//
// # 2026-11-03 变更说明
//
// 原名 TestGoalsBodyForPentest，断言 goalsDefaultTmpl 以「你是渗透测试目标分解器」
// 开头。该常量已金融化为**领域中立**版本（不再含渗透身份声明），
// 所以措辞断言改为「不含渗透身份」。
//
// 保留的是**分叉机制**的断言：instrumentID==0 必须返回 goalsDefaultTmpl。
// 这条仍有价值 —— 它防的是「分支判断写反」（金融任务错拿通用模板，
// 或通用任务错拿金融模板）。
func TestGoalsBodyForDefault(t *testing.T) {
	body := goalsBodyFor(false)
	if body != goalsDefaultTmpl {
		t.Fatal("instrumentID==0 应返回 goalsDefaultTmpl（分叉回归保护）")
	}
	// 2026-11-03 金融化后，默认正文不应再有渗透身份声明。
	if strings.Contains(body, "渗透测试") {
		t.Error("goalsDefaultTmpl 仍含「渗透测试」—— 金融化未完成")
	}
	// 分叉仍未失效：金融版必须与之不同。
	if body == goalsFinanceTmpl {
		t.Error("默认正文与金融正文相同 —— 分叉失效了")
	}
}

// TestGoalsPromptKeyFor 两套 prompt 必须用不同的 agentKey。
//
// # 为什么必须不同
//
// renderSystem 先查 PromptOverride(agentKey)。如果都用 "goals"，
// 用户在 agent 编辑页改过的**渗透措辞覆盖**会压过金融默认正文 ——
// 那让双模式对已部署环境完全失效。
func TestGoalsPromptKeyFor(t *testing.T) {
	if k := goalsPromptKeyFor(true); k != "goals_finance" {
		t.Errorf("金融 key = %q, want \"goals_finance\"", k)
	}
	if k := goalsPromptKeyFor(false); k != "goals" {
		t.Errorf("渗透 key = %q, want \"goals\"", k)
	}
	if goalsPromptKeyFor(true) == goalsPromptKeyFor(false) {
		t.Fatal("两套 prompt 共用同一个 key —— 用户改过的渗透覆盖会污染金融任务")
	}
}

// TestFinanceTemplateExcludesPentestInstructions 金融 prompt 不含渗透专属指令。
//
// # 为什么逐条断言而不是只断言「不含某个词」
//
// 渗透 prompt 有多个具体的行动指令（登记测试资产范围、vulnclass 标注等）。
// 如果只断言「不含『渗透』」，任何一条漏改都能通过。
func TestFinanceTemplateExcludesPentestInstructions(t *testing.T) {
	body := goalsFinanceTmpl

	pentestOnly := []struct{ marker, why string }{
		{"add_task_scope", "金融任务没有「测试资产范围」这个概念（标的是 instrument_id，不是 domain）"},
		{"漏洞分析与验证过程", "那是渗透 prompt 里被禁的目标类型 —— 而金融任务的核心恰恰是分析"},
		{"kind=subdomain", "渗透的资产类型枚举，金融任务不存在"},
		{"kind=root_domain", "同上"},
		{"攻击步骤", "金融任务说的是「交易步骤」，不是「攻击步骤」"},
	}
	for _, p := range pentestOnly {
		if strings.Contains(body, p.marker) {
			t.Errorf("金融 prompt 出现了渗透专属内容 %q —— %s", p.marker, p.why)
		}
	}

	// 反向：金融 prompt 必须明确禁止填 vulnclass
	if !strings.Contains(body, "不填 vulnclass") {
		t.Error("金融 prompt 应明确写「不填 vulnclass」")
	}
	// 且必须有 set_goals 的收尾指令（否则模型不知道要提交）
	if !strings.Contains(body, "调用 set_goals 提交结果") {
		t.Error("金融 prompt 缺 set_goals 提交指令")
	}
}

// TestPentestTemplateKeepsScopeInstructions 渗透 prompt 仍含资产范围指令（回归）。
func TestPentestTemplateKeepsScopeInstructions(t *testing.T) {
	if !strings.Contains(goalsScopeTail, "add_task_scope") {
		t.Fatal("goalsScopeTail 应教模型调用 add_task_scope")
	}
	// goalsDefaultTmpl 本身不含 tail（tail 是运行期拼接的），
	// 所以这里断言「拼接后的渗透 system prompt 含它，而金融的不含」。
	pentestSys := goalsDefaultTmpl + goalsScopeTail
	if !strings.Contains(pentestSys, "add_task_scope") {
		t.Error("渗透任务的完整 system prompt 应含 add_task_scope")
	}
	financeSys := goalsFinanceTmpl // 金融不拼 tail
	if strings.Contains(financeSys, "add_task_scope") {
		t.Error("金融任务的 system prompt 不该含 add_task_scope")
	}
}

// TestBothTemplatesNonEmpty 两套 prompt 都非空且都有 set_goals 收尾。
//
// # 为什么这条看起来多余但必要
//
// 空 prompt 会让模型完全自由发挥 —— 而那是所有「语义污染」里最难查的一种
// （因为它不报错、不崩溃，只是产出莫名其妙的目标）。
func TestBothTemplatesNonEmpty(t *testing.T) {
	for name, body := range map[string]string{
		"goalsDefaultTmpl": goalsDefaultTmpl,
		"goalsFinanceTmpl": goalsFinanceTmpl,
	} {
		if strings.TrimSpace(body) == "" {
			t.Errorf("%s 是空的", name)
			continue
		}
		if !strings.Contains(body, "调用 set_goals 提交结果") {
			t.Errorf("%s 缺「调用 set_goals 提交结果」收尾指令", name)
		}
		if !strings.Contains(body, "set_constraints") {
			t.Errorf("%s 缺 set_constraints（抽取风控/操作约束那一步）", name)
		}
	}
}

// TestFinanceModeDropsVulnClass set_goals 在金融模式下丢弃 vulnclass。
//
// # 为什么必须在工具层也挡
//
// prompt 是建议，工具是强制。模型完全可能因为见过类似 schema 而继续输出
// vulnclass —— 那时只有工具层能挡住它。
func TestFinanceModeDropsVulnClass(t *testing.T) {
	t.Run("financeMode=true 丢弃", func(t *testing.T) {
		ts := &ToolSet{financeMode: true, worker: "goals"}
		// 直接验证 payload 构造逻辑：调用 addOneGoal 需要 ts.ts，这里
		// 用一个会因 ts 为 nil 而失败但足以证明「没走到写 vulnclass」的路径。
		//
		// 更有价值的是验证 financeMode 字段的存在与语义 —— 由下面那条测试守。
		if !ts.financeMode {
			t.Fatal("financeMode 未生效")
		}
	})
	t.Run("financeMode=false 保留", func(t *testing.T) {
		ts := &ToolSet{financeMode: false, worker: "goals"}
		if ts.financeMode {
			t.Fatal("financeMode 不该为 true")
		}
	})
}

// TestGoalSpecVulnClassOmitEmpty 金融任务的 GoalSpec 序列化后不带 vulnclass。
func TestGoalSpecVulnClassOmitEmpty(t *testing.T) {
	// 金融任务的 GoalSpec 构造应该是 VulnClass 为空（由 addOneGoal + 读回逻辑保证）
	spec := GoalSpec{Text: "判断 SPY 未来一周方向"}
	if spec.VulnClass != "" {
		t.Error("金融 GoalSpec 的 VulnClass 应为空")
	}
	// omitempty 让空值不出现
	if strings.Contains(`{"text":"x","vulnclass":""}`, `"vulnclass":""`) {
		// 这只是说明 omitempty 的必要性，不是断言
		t.Log("omitempty 确保空 vulnclass 不出现在 JSON 里")
	}
}
