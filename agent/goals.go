package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// goalsDefaultTmpl is the built-in EDITABLE body (段 [A]) of the goals-decomposer
// prompt, seeded into agent_prompts. No template vars are used today.
//
// 正文刻意保持**领域中立**：只讲「抽约束 → 拆可交付结论」这件事，不假定任务领域。
// 金融任务按 instrumentID 分叉到 goalsFinanceTmpl（见 goalsBodyFor），所以这里
// 不需要、也不应该带任何单一领域的措辞 —— 一旦带上，另一领域的任务就会读到
// 与之矛盾的指令（如把「分析过程」列为禁止项）。
const goalsDefaultTmpl = `你是目标分解器。你的职责是从用户输入中识别出**最终要达成的结果**，而不是规划执行步骤。

**第一步（拆分目标之前先做）：抽取操作约束**
从「任务目标 / 任务描述」里识别操作员对【可以做什么、不可以做什么操作】的明确规定，调用 set_constraints 逐条登记（如果描述、目标中不涉及操作约束可以不进行提取操作约束）：
- type=deny：禁止的动作（如「不做任何写操作」「不得改动生产数据」「不访问某范围」）。
- type=allow：明确允许/限定的范围（如「只读」「仅针对某对象」）。
- 约束 ≠ 目标，也 ≠ 执行步骤：它是对操作行为边界的规定。
- **约束必须【自包含、写死具体对象】**：把「当前目标/当前对象/本系统/本站」这类**指代词**替换成任务目标/描述里的**具体值**。约束会被单独注入到执行阶段的提示里，脱离上下文后指代词无法判断指谁。
  例：目标是「评估供应商 A 的合规状况」→ 写「只评估供应商 A」而不是「只评估当前对象」；「仅看供应商 A 的二级供应商」而不是「只看当前对象的次级范围」。若原文只说「当前目标」但对象已明确，就把具体值填进去。
- **只登记目标/描述里【明确写出或强调】的约束，严禁臆造**；拿不准类型时用 deny（更保守）。
- 若目标/描述里确实没有任何操作约束，则**不要**调用 set_constraints。
登记完约束（如有）后，再进行下面的目标拆分。

**目标 = 最终可交付/可核验的结论**

**不是目标的内容（禁止列为子目标）**：
- 资料收集、数据获取、范围摸底
- 分析与验证过程
- 执行步骤、具体手段
- 结果复核步骤

**拆分原则**：
- 用户描述的最终目标只有一个 → 输出一个
- 存在多个**相互独立**的最终交付物 → 分别列出
- 能对应明确问题类别的标注 vulnclass；其余留空
- 严禁臆造用户未提及的目标

调用 set_goals 提交结果。`

// goalsFinanceTmpl 是金融分析任务的默认正文。
//
// # 为什么金融任务需要自己的 prompt 而不是复用渗透的
//
// 渗透 prompt 的「不是目标的内容」清单里写着「漏洞分析与验证过程」——
// 而金融任务的核心恰恰是「分析」。如果复用，模型会被明确禁止列出
// 「分析」类目标 —— 那是反向的。
//
// 另外渗透 prompt 要求「登记测试资产范围（domain/IP/CIDR）」，
// 而金融任务分析的是标的（SPY / BTC-USDT），那是 instrument_id，不是 domain。
//
// # 与 goalsDefaultTmpl 的关系
//
// 两者是**平级**的两套正文，按任务的 instrumentID 分叉（见 goalsBodyFor）。
// 不是「金融版是渗透版的补丁」—— 因为渗透的相对应段落删不掉，
// 补丁式写法会留下矛盾指令。
const goalsFinanceTmpl = `你是金融分析目标分解器。你的职责是从用户输入中识别出**最终要产出的分析结论**，而不是规划交易步骤。

**第一步（拆分目标之前先做）：抽取风控约束**
从「任务目标 / 任务描述」里识别操作员对【可以做什么、不可以做什么】的明确规定，调用 set_constraints 逐条登记（若不涉及约束则跳过）：
- type=deny：禁止的行为（如「不做空」「单笔不超过 1 万」「不碰加密」）。
- type=allow：明确允许/限定的范围（如「只分析美股」「仅做 ETF」）。
- 约束 ≠ 目标，也 ≠ 交易步骤：它是对行为边界的规定。
- **约束必须自包含、写死具体值**：把「当前标的」「这个市场」这类指代词替换成具体标的代码。
  例：分析 SPY → 写「只分析 SPY」，不写「只分析当前标的」。
- **只登记目标/描述里明确写出的约束，严禁臆造**；拿不准类型用 deny（更保守）。
- 若确实没有任何约束，**不要**调用 set_constraints。

登记完约束（如有）后，再进行下面的目标拆分。

**目标 = 最终可交付的分析结论**

**不是目标的内容（禁止列为子目标）**：
- 具体取数动作（拉 K 线、查财报、查舆情）
- 指标计算过程（MA / ATR / RSI 怎么算）
- 下单执行步骤（怎么买、买多少）
- 单一中间结论（如「技术面偏多」「估值偏高」—— 那是一条证据，不是目标本身）

**拆分原则**：
- 用户描述的最终目标只有一个 → 输出一个
- 存在多个相互独立的最终结论（如「技术面判断」+「风险敞口评估」）→ 分别列出
- **不填 vulnclass**（那是渗透任务的字段，金融任务必须留空）
- 严禁臆造用户未提及的目标

调用 set_goals 提交结果。`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away — same pattern as the trafficTool tail.
const goalsScopeTail = `

**额外职责：登记测试资产范围**
除拆分目标外，你还要从「任务目标 / 任务描述」里识别出**明确给出的测试资产范围**，调用 add_task_scope 登记（本任务的授权边界，也是资产测试覆盖度的分母）。**最小范围原则：只登记用户明确点到的那一个目标，绝不擅自放大。**
- 目标是 URL 或带主机名的地址（如 https://xxx.example.com/path、app.example.com）→ 取其**完整主机名**，kind=subdomain，value=完整主机名。
  例：目标 https://a1b2c3.lab.example.net/path → kind=subdomain，value=a1b2c3.lab.example.net（**不是** example.net）。
  **严禁**把带子域的主机名缩成根域名——看到 xxx.example.com 就登记整个 example.com 会把范围扩到用户目标之外，违背最小范围原则。
- 仅当用户给的就是**裸根域名、且不含任何子域**（如直接写 example.com），或明确说“整个站点 / 所有子域 / 全域名” → 才用 kind=root_domain，value=example.com。
- 纯 IP 或网段 → kind=ip / cidr，value=IP 或 CIDR。
- **不要**登记公司范围（company）——任务刚建立、资产系统里通常还没有这家公司，登记不上，公司级范围交由后续 plan 阶段处理。
其它规则：
- 只登记**目标/描述里明确写出**的范围；严禁臆造或推断未提及的域名/IP。
- reason 简述依据来自哪句话，便于审计。
- 若目标/描述中没有任何明确资产范围，则**不要**调用 add_task_scope。
先用 add_task_scope 登记范围（如有），再调用 set_goals 提交目标。`

// GoalSpec is one decomposed objective.
type GoalSpec struct {
	Text string `json:"text"`
	// VulnClass 只在渗透任务里有值（对应 CWE / OWASP 分类）。
	//
	// # 为什么金融任务必须为空
	//
	// 「漏洞类」对金融分析没有意义 —— 而如果模型被渗透 prompt 误导填了一个值，
	// 那个值会出现在 goal 节点的 payload 里，让整条分析链看起来像在找漏洞。
	//
	// 由 set_goals 在金融模式下主动丢弃（见 ToolSet.setGoals 的 financeMode）。
	VulnClass string `json:"vulnclass,omitempty"`
}

// goalsBodyFor 按任务的领域选出目标分解器的正文。
//
// # 判据：instrumentID > 0 ⇔ 金融任务
//
// `tasks.instrument_id` 是「本任务分析哪个标的」的外键（WE-8 加的）。
// 而渗透任务没有标的 —— 它分析的是资产（domain/IP/CIDR），存在 asset store 里。
//
// 所以：
//
//	instrumentID > 0  → 金融分析任务 → goalsFinanceTmpl
//	instrumentID == 0 → 渗透任务（或无标的）→ goalsDefaultTmpl
//
// # 为什么用 instrumentID 而不是别的标记
//
// 库里目前**没有**「任务类型」列，而 instrument_id 是唯一的、有外键约束的、
// 语义明确的领域信号。加一个 kind 列会引入「两个字段互相矛盾」的可能
// （kind=finance 但 instrument_id 为 NULL），而 instrument_id 本身不会自相矛盾。
//
// # 2026-11-05 修正：判定已从 instrumentID 解耦为 financeMode
//
// 原实现只认 instrumentID > 0，于是「不绑定标的的金融任务」
// （如「在 OKX 上做交易，标的自己挑」）会走渗透 prompt ——
// 而用户明确要求允许不指定标的、由模型自行选品。
//
// 现由调用方传 financeMode，判据是「标的已知 **或** 任务文本是金融语境」
// （见 server/goals.go 的 taskIsFinance）。加标的只是金融任务的一种，
// 不是唯一形式。
func goalsBodyFor(financeMode bool) string {
	if financeMode {
		return goalsFinanceTmpl
	}
	return goalsDefaultTmpl
}

// goalsPromptKeyFor 返回 renderSystem 用的 agentKey。
//
// # 为什么金融模式要用不同的 key
//
// renderSystem 会先查 PromptOverride(agentKey) —— 那是**用户在 agent 编辑页
// 改过的正文**。而现有的 "goals" 覆盖是渗透措辞。
//
// 如果金融任务也用 "goals"，那么：
//   - 用户改过 "goals" → 金融任务拿到渗透措辞（旧覆盖压过新默认值）
//   - 用户没改过 → 拿到 goalsFinanceTmpl（正常）
//
// 前一种会让「改造成双模式」对已部署环境失效。所以金融模式必须用独立 key
// （"goals_finance"），让两套覆盖互不干扰。
func goalsPromptKeyFor(financeMode bool) string {
	if financeMode {
		return "goals_finance"
	}
	return "goals"
}

// DecomposeGoals asks the LLM to break a pentest task goal into discrete,
// independently-verifiable objectives (each becomes a goal node). Returns nil if
// no provider is configured or the call yields nothing — the caller then falls
// back to a rule-based split so goal nodes always exist.
//
// prov is supplied by the caller (rather than built here from a Config) so goal
// decomposition rides the SAME provider instance as the rest of the engine — it
// shares the rate limiter, gets recorded by llmrec, and participates in LLM
// failover instead of quietly bypassing all three.
//
// desc is the task's free-text description (背景：分析范围/时间窗口/数据源等).
// It is fed alongside the goal so the decomposer no longer splits blind — the
// prompt still forbids inventing anything the two texts don't state.
//
// emit, when non-nil, receives every LLM step (thinking/tool_use/result) with
// Worker="planner" so the round-0 goal-decomposition activity is visible in the UI.
//
// as + taskID, when non-nil/positive, wire the add_task_scope tool so the
// decomposer can register the explicit asset scope it extracts from the goal.
//
// ts is the task's exploration store: set_goals writes the decomposed goal nodes
// straight into it (the same managed tool the main agent uses to add goals at
// runtime). The returned specs are read back from the store so callers can emit
// per-goal activity and detect the "LLM produced nothing" case for their fallback.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, 0, false, false, 0, emit)
}

// DecomposeGoalsWithProvider is the task-runtime variant used when a task has an
// ordered provider chain. It preserves the same tools and write behavior while
// letting the caller own provider selection/failover. maxTokens is the profile's
// per-reply output cap (0 = send none).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID, instrumentID int64, financeMode bool, nonStreaming bool, maxTokens int, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	// 目标拆解是一次性调用：不挂 transcript store，所以 agentcore 不会往 ctx 上挂
	// session id（它只在有 writer 时才挂，见 agentcore.Prompt）。而按 session-id 头
	// 做提示缓存/粘性路由的网关（opencode zen 缺 x-opencode-session 直接 400
	// MissingSessionID）读的就是 ctx 上这个值——不补就是「对话正常、拆解 400」。
	// 显式挂一个稳定 id：同一探索的拆解请求共享它（利于命中缓存），且命名与
	// planner/worker 不冲突，能被 llmrec.parseSession 正确归因。
	if ts != nil {
		ctx = transcript.WithSessionID(ctx, fmt.Sprintf("exp%d-goals", ts.ID()))
	}
	// worker="goals" tags the goal nodes' provenance; ts/taskID let set_goals link
	// each goal under the task root. This is the catalog's real set_goals tool, so a
	// web-edited description/schema on it applies here too.
	// financeMode 让 set_goals 丢弃 vulnclass（见 ToolSet.setGoals）。
	// 这里是「工具行为」的一部分，不是 prompt 的一部分 —— 因为 prompt 只是建议，
	// 而工具是强制的。
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals", financeMode: financeMode}
	// Description rides in the user message (same channel as the goal), NOT via the
	// {{.EngagementDescription}} template var — else a prompt that references the var
	// would inject the description twice. System prompt stays pure static instructions.
	sys := renderSystem(goalsPromptKeyFor(financeMode), goalsBodyFor(financeMode),
		GoalsVars{DataDir: dataDir, Now: nowStr()})
	// set_constraints 始终可用(不依赖 asset store):正文已含「先抽操作约束再拆目标」这步
	// (可在 agent 编辑页改措辞),这里只需接上工具。
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	// Wire add_task_scope only when we have a real asset store + task to write to.
	// The scope-extraction tail is appended in lockstep so the prompt never asks for
	// a tool that isn't present.
	//
	// # 为什么金融任务不能附加 goalsScopeTail
	//
	// goalsScopeTail 教模型调用 add_task_scope 登记「测试资产范围」（domain/IP/CIDR）。
	// 而金融任务没有「测试资产」这个概念 —— 它分析的是标的（SPY / BTC-USDT），
	// 那是 instrument_id，不是 domain。附加它会让模型在金融任务里尝试登记
	// 一个不存在的 domain。
	if as != nil && taskID > 0 && !financeMode {
		tools = append(tools, tsx.addTaskScope())
		sys += goalsScopeTail
	}
	userMsg := "任务目标：\n" + goalText
	if d := strings.TrimSpace(desc); d != "" {
		// 2026-11-03 去领域化：原措辞把任务描述框定成单一领域专用的背景字段，
		// 会向模型暗示领域。现在统一成中性表述 —— 任务描述里装的是哪类背景，
		// 由用户自己写，prompt 不预设。
		userMsg += "\n\n任务描述（背景信息，可能含分析范围/时间窗口/数据源偏好等；仅供参考，不要臆造其中未提及的内容）：\n" + d
	}
	// Use captureRun so every LLM step is emitted as an activity record (visible in
	// the plan tab under the round-0 marker). Falls back gracefully when emit is nil.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	captureRun(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// 3 步(抽约束 → 登记范围 → 拆目标)各需一次工具调用,给足回合避免收尾前漏调 set_goals。
		MaxTurns:     8,
		NonStreaming: nonStreaming, // 该 profile 选非流式时走 Provider.Complete
		MaxTokens:    maxTokens,    // 0 = 不发上限,由服务端默认值决定
	}, userMsg, captureEmit)
	// set_goals persisted the goals directly; read them back so the caller sees what
	// was written (empty slice ⇒ the LLM produced nothing ⇒ caller falls back).
	if ts == nil {
		return nil
	}
	nodes, _ := ts.ListByKind(db.KindGoal, 10000)
	var out []GoalSpec
	for _, n := range nodes {
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if strings.TrimSpace(p.Text) == "" {
			continue
		}
		// 金融模式下 VulnClass 必须为空 —— 即使库里那条 goal 带了值
		// （可能是 prompt 改造之前写的，或模型仍在幻觉）。
		//
		// 在「读回」这一层也挡，而不是只靠 set_goals 的写入拦截 ——
		// 因为库里可能已有历史数据，而那些数据的 vulnclass 无意义。
		vc := p.VulnClass
		if instrumentID > 0 {
			vc = ""
		}
		out = append(out, GoalSpec{Text: p.Text, VulnClass: vc})
	}
	return out
}
