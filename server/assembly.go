package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"path/filepath"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/skill"
	actool "github.com/Autumn-27/norma/tool"
)

// wireAgentAugment connects the PG agent_visibility table into the agent runtime:
// an agent's visible skills are loaded from the filesystem and packed into one
// Skill meta-tool; its visible stdio MCP servers are spawned and expanded to
// mcp__server__tool. skillDir is the root directory of all skill subdirectories.
// hostTools, if set, returns runtime host tools (currently the traffic tools when
// capture is on) to add to EVERY agent's base list — the DB tools table then
// filters them per-agent binding. Empty/nil → no host tools this run (capture off).
func wireAgentAugment(pg *db.DB, skillDir string, hostTools func() ([]actool.CoreTool, map[string][]string)) {
	agent.ToolAugment = func(ctx context.Context, agentKey string) ([]actool.CoreTool, agent.DeferredInfo, func()) {
		a, err := pg.GetAgentByKey(agentKey)
		if err != nil || a == nil {
			return nil, agent.DeferredInfo{}, nil
		}
		var extra []actool.CoreTool

		// --- skills: load visible skills into reg (used for the Skill meta-tool
		// and to know which MCP servers are skill-gated). ---
		var reg *skill.Registry
		if names, _ := pg.AgentSkillNames(a.ID); len(names) > 0 {
			nameSet := make(map[string]bool, len(names))
			for _, n := range names {
				nameSet[n] = true
			}
			if allReg, err := skill.LoadDir(skillDir); err == nil && allReg != nil {
				reg = skill.NewRegistry()
				for _, s := range allReg.List() {
					// match by directory name (Base of Dir), not by skill display Name
					if s.Dir != "" && nameSet[filepath.Base(s.Dir)] {
						reg.Add(s)
					}
				}
				if len(reg.List()) == 0 {
					reg = nil
				}
			}
		}
		// A server named by any visible skill's `mcps:` is skill-gated: its tools
		// are deferred + locked (not in the global block) until that skill loads.
		gated := map[string]bool{}
		if reg != nil {
			for _, s := range reg.List() {
				for _, srv := range s.MCPs {
					gated[srv] = true
				}
			}
		}

		// --- mcp: connect enabled servers that are directly visible to this agent
		// OR skill-gated (named in a visible skill's mcps field). Directly-visible
		// and NOT gated → global (unlocked from session start). Skill-gated →
		// deferred until that skill is invoked (regardless of direct visibility).
		var closers []io.Closer
		serverTools := map[string][]string{} // server name → its tool names
		var allNames, globalNames []string
		globalSet := map[string]bool{}
		{
			mcpIDs, _ := pg.AgentVisible(a.ID, "mcp")
			want := idSet(mcpIDs)
			all, _ := pg.ListMCP()
			for _, m := range all {
				if !m.Enabled {
					continue
				}
				directVisible := want[m.ID]
				skillGated := gated[m.Name]
				if !directVisible && !skillGated {
					continue // neither directly visible nor referenced by a visible skill
				}
				cl, err := connectMCP(ctx, m)
				if err != nil {
					log.Printf("[mcp] %s 连接失败: %v", m.Name, err)
					continue
				}
				closers = append(closers, cl)
				ts, err := cl.Tools(ctx)
				if err != nil {
					log.Printf("[mcp] %s tools/list 失败: %v", m.Name, err)
					continue
				}
				for _, t := range ts {
					extra = append(extra, t)
					allNames = append(allNames, t.Name())
					serverTools[m.Name] = append(serverTools[m.Name], t.Name())
					if directVisible && !skillGated {
						// directly visible and not gated → available from session start
						globalNames = append(globalNames, t.Name())
						globalSet[t.Name()] = true
					}
					// skill-gated tools stay out of globalNames; unlocked via unlockSkill()
				}
			}
		}

		// Shared call-gate: global MCP tools are unlocked from the start; skill-gated
		// ones are unlocked when their skill loads (or replayed from history — C2).
		unlock := actool.NewUnlockSet(globalNames...)
		unlockSkill := func(skillName string) {
			if reg == nil {
				return
			}
			if s, ok := reg.Get(skillName); ok {
				for _, srv := range s.MCPs {
					unlock.Add(serverTools[srv]...)
				}
			}
		}

		// Skill meta-tool: on load, unlock the skill's MCPs and reveal their names
		// (the ones not already in the global block).
		if reg != nil {
			reg.OnInvoke = func(s skill.Skill) string {
				unlockSkill(s.Name)
				var reveal []string
				for _, srv := range s.MCPs {
					for _, n := range serverTools[srv] {
						if !globalSet[n] {
							reveal = append(reveal, n)
						}
					}
				}
				return actool.RenderDeferredToolsBlock(reveal)
			}
			// Attribution for the usage ledger: ToolAugment only gets (ctx, agentKey),
			// so the run's task/session ids ride in on the ctx (agent.RunInfo). Read it
			// once here — this closure is rebuilt per run, so the captured value always
			// belongs to this run.
			extra = append(extra, meterSkillTool(reg.Tool(), pg, reg, a.Key, agent.RunInfoFrom(ctx)))
		}

		// host tools (traffic / orchestration / custom) — added to every agent's base;
		// ToolResolve then keeps them only for agents the tool is bound to. Custom
		// tools flagged deferred contribute their names to the deferred wiring so
		// their schema is withheld (SearchExtraTools/ExecuteExtraTool), same as MCP.
		if hostTools != nil {
			ht, deferredBinds := hostTools()
			extra = append(extra, ht...)
			for name, boundAgents := range deferredBinds {
				if !contains(boundAgents, a.Key) {
					continue // only defer names this agent is actually bound to
				}
				allNames = append(allNames, name)       // schema withheld from the prompt
				globalNames = append(globalNames, name) // advertised in the deferred block
				globalSet[name] = true
				if unlock != nil {
					unlock.Add(name) // global deferred → callable from the start
				}
			}
		}

		cleanup := func() {
			for _, c := range closers {
				_ = c.Close()
			}
		}
		def := agent.DeferredInfo{
			Deferred:    allNames,
			GlobalNames: globalNames,
			Unlock:      unlock,
			UnlockSkill: unlockSkill,
		}
		return extra, def, cleanup
	}
}

func idSet(ids []int64) map[int64]bool {
	m := make(map[int64]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// seedPrompts writes each built-in agent's code-default prompt body into
// agent_prompts on startup — first-insert only (SeedPromptIfEmpty is a no-op once
// any version exists), so the DB becomes the authoritative editable source while
// user edits survive restarts. Runs after seedBuiltins has created the agent rows.
func seedPrompts(pg *db.DB) {
	for key, tmpl := range agent.BuiltinPromptSeeds() {
		a, err := pg.GetAgentByKey(key)
		if err != nil || a == nil {
			log.Printf("[prompts] seed %s 跳过: agent 不存在 (%v)", key, err)
			continue
		}
		if err := pg.SeedPromptIfEmpty(a.ID, tmpl); err != nil {
			log.Printf("[prompts] seed %s 失败: %v", key, err)
		}
	}
}

// wireTools seeds the built-in tool catalog (idempotent, first-insert only so page
// edits survive restart) and wires the DB tools table into the agent runtime: at
// tool-assembly time each built-in tool is filtered by its agent binding / enabled
// flag and, if kept, wrapped so the model sees the DB-overridden description/schema
// and缺省入参 get injected. MCP/skill/host tools have no row and pass through.
func wireTools(pg *db.DB, domainReg map[string]actool.CoreTool) {
	agent.FindingTrafficBindingEnabled = func() bool { return pg.GetBool(settingAgentTrafficBinding, false) }
	// Seed the built-in domain tools (first-insert only; DO NOTHING preserves edits).
	// No startup prune: rows we didn't seed are left alone so future user-defined
	// custom tools (system=false, added via the UI) survive restarts.
	for _, s := range agent.BuiltinToolSeeds() {
		schema, _ := json.Marshal(s.Schema)
		agents, _ := json.Marshal(s.Agents)
		if err := pg.SeedTool(s.Key, s.Desc, schema, agents); err != nil {
			log.Printf("[tools] seed %s 失败: %v", s.Key, err)
		}
	}
	// 流量捕获已删（2026-10-29）—— 不再 seed traffic_search / traffic_get 等宿主工具。
	// bashInteractiveShellNote is appended to Bash's description ONLY for agents whose
	// interactive_shell is on, so Bash points at shell_open for interactive programs
	// without ever referencing a tool that isn't injected (§14.1/§14.2).
	const bashInteractiveShellNote = "\n\n需要【交互输入】的程序（msfconsole / ssh 交互登录 / mysql、psql、python 等 REPL / 密码或 yes/no 提示 / nc 反弹 shell）不要用 Bash（它没有 stdin、会卡住），改用 shell_open 开交互会话（用完 shell_close）。一次性、非交互命令仍用 Bash。"
	agent.ToolResolve = func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool {
		rows, err := pg.ListTools()
		if err != nil {
			log.Printf("[tools] 读取工具表失败，按代码默认放行: %v", err)
			return tools
		}
		byKey := make(map[string]*db.Tool, len(rows))
		for _, t := range rows {
			byKey[t.Key] = t
		}
		runInfo := agent.RunInfoFrom(ctx)
		resolve := func(t actool.CoreTool, row *db.Tool) actool.CoreTool {
			var schema map[string]any
			if len(row.Schema) > 0 {
				_ = json.Unmarshal(row.Schema, &schema)
			}
			return meterTool(agent.DecorateTool(t, row.Description, schema), pg, row.Key, agentKey, runInfo)
		}
		out := tools[:0:0]
		for _, t := range tools {
			row, known := byKey[t.Name()]
			if !known { // MCP/skill/host tool: no row → untouched
				out = append(out, t)
				continue
			}
			if !row.Enabled || !contains(row.Agents, agentKey) {
				continue // disabled globally or not bound to this agent → drop
			}
			out = append(out, resolve(t, row))
		}
		// inject: domain tools bound to this agent in the DB but absent from the
		// incoming list. Covers agents (Auto, custom) whose base only has DefaultTools
		// and therefore never includes ToolSet-backed domain tools. Per-task instances
		// in the base always win: inList is built from the original incoming list so a
		// worker's own upsert_asset is never shadowed by the server-level registry copy.
		if len(domainReg) > 0 {
			inList := make(map[string]bool, len(tools))
			for _, t := range tools {
				inList[t.Name()] = true
			}
			for _, row := range rows {
				if row.Kind == "shell" || !row.Enabled || !contains(row.Agents, agentKey) || inList[row.Key] {
					continue
				}
				inst, ok := domainReg[row.Key]
				if !ok {
					continue // not a domain tool; custom/host tools are injected via hostTools()
				}
				out = append(out, resolve(inst, row))
			}
		}
		// shell hints: user-defined kind="shell" tools are not callable — they are
		// environment declarations that tell the model which command-line tools are
		// installed. Collect the ones bound to this agent and append to Bash's description.
		var shellHints []string
		for _, row := range rows {
			if row.Kind == "shell" && row.Enabled && contains(row.Agents, agentKey) {
				shellHints = append(shellHints, "- "+row.Key+": "+row.Description)
			}
		}
		if len(shellHints) > 0 {
			note := "\n\n以下工具已安装在此 bash 环境中，可直接通过 Bash 调用：\n" + strings.Join(shellHints, "\n")
			for i, t := range out {
				if t.Name() == "Bash" {
					out[i] = agent.DecorateTool(t, t.Description()+note, t.InputSchema())
					break
				}
			}
		}
		// interactive shell: gated purely by the agent's interactive_shell flag (like
		// web_search), NOT by tools-table binding. When on, inject the 5 shell_* tools
		// and COUPLE the Bash description addendum so it points at shell_open — and never
		// dangles when off. See docs/交互式shell设计.md §14.2.
		if !actool.InteractiveShellDisabled() {
			if a, err := pg.GetAgentByKey(agentKey); err == nil && a != nil && a.InteractiveShell {
				out = append(out, actool.ShellSessionTools()...)
				for i, t := range out {
					if t.Name() == "Bash" {
						out[i] = agent.DecorateTool(t, t.Description()+bashInteractiveShellNote, t.InputSchema())
						break
					}
				}
			}
		}
		return out
	}
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

// buildDomainReg builds a name→CoreTool registry for ToolResolve to inject tools
// into agents (Auto, custom, and W5 financial roles) that don't own a per-task
// ToolSet.
//
// 两个来源：ToolSet（资产/探索图）与 MarketToolSet（金融行情）。
// 任一为 nil 就跳过该来源（graceful degradation）。
//
// The nil ExplorationStore is deliberate — these instances are task-less by
// construction — so every tool here must tolerate it. Asset/company tools do
// (they only need the AssetStore); the exploration-graph tools refuse with a
// clear message via ToolSet.needExploration. Binding one of them to a task-less
// agent in the tools table is therefore a useless tool, not a crash.
func buildDomainReg(as *db.AssetStore, mts *agent.MarketToolSet, tts *TradeToolSet, sits *SetInstrumentToolSet) map[string]actool.CoreTool {
	reg := make(map[string]actool.CoreTool)

	// Asset/company + 探索图工具。
	if as != nil {
		serverTS := agent.NewToolSet(nil, "")
		serverTS.SetAssetStore(as, as.Companies())
		for _, t := range serverTS.AllDomainTools() {
			reg[t.Name()] = t
		}
	}

	// 金融工具（get_quote/get_kline/compute_indicator/get_macro/get_volatility/
	// get_crypto_flow）。
	//
	// **为什么必须在这里注册**：这 6 个工具属于 MarketToolSet，而 MarketToolSet
	// 是独立类型，不在 ToolSet.AllDomainTools() 的视野里。不注册的话，
	// 它们只存在于 builtinToolsByAgent() 的种子壳（用于写 tools 表），
	// 运行时没有任何装配路径能把它们给到 agent —— 而 ToolResolve 对
	// 「DB 有行但 domainReg 没有」的工具是**静默跳过**（`if !ok { continue }`），
	// 表现就是「工具在 UI 里能勾选，但 agent 永远拿不到」。
	//
	// 这是 C1.6 的实例：工具注册了（tools 表有行）→ 但没人派发（domainReg 没有）。
	if mts != nil {
		// 用 sourceQuoteTools()（按可用性过滤）而不是 Tools()。
		//
		// # 为什么这里要单独处理
		//
		// mts.Tools() 返回**全部**按源工具（含不可用源）—— 那是为写 tools 表
		// 设计的（种子壳 client 全 nil，不过滤就没法建表行）。
		//
		// 但 domainReg 是**运行期**注册表，ToolResolve 会从它取实现去执行。
		// 如果把不可用源也放进去，agent 会看到一个必然失败的工具，
		// 在规划里反复尝试且无法解释。所以这里再过滤一次。
		for _, t := range mts.Tools() {
			if agent.IsSourceQuoteTool(t.Name()) && !mts.AvailableSourceTool(t.Name()) {
				continue // 该源当前不可用，不注入
			}
			reg[t.Name()] = t
		}
	}

	// 交易执行工具（create_order_draft / authorize_order / execute_paper_order）。
	//
	// # 这一处注册是「下单能被拦截」的前提
	//
	// 2026-11-02 之前 agent 没有任何下单工具，只能调 OKX skill ——
	// 而 skill 内部直接 exec `okx` CLI，完全绕过 exec 层：
	// 无闸门、无草案、无审计记录。用户的要求是「通过工具实现下单，
	// 否则后面无法进行拦截」，所以这三个工具必须经由 Gate。
	//
	// 与 MarketToolSet 同理：只写进 tools 表而不在这里注册，
	// ToolResolve 会静默跳过（`if !ok { continue }`），
	// 表现是「UI 能勾选但 agent 永远拿不到」。
	if tts != nil {
		for _, t := range tts.Tools() {
			reg[t.Name()] = t
		}
	}

	// 选品结果回填（set_task_instrument）。
	//
	// # 为什么必须注册（2026-11-05 全流程断裂）
	//
	// 选品员的产出是 fact（自然语言），而分析师读 intent.payload.instrument_id。
	// 没有这个工具，选品结果永远传不到下游 —— 实测三个分析师全部判
	// 「标的未知，无法分析」，整条链断在 analysis 阶段。
	//
	// 与上面两个 ToolSet 同理：只写进 tools 表而不在这里注册，
	// ToolResolve 会静默跳过，表现是「模型看得到工具名但调用不了」。
	if sits != nil {
		for _, t := range sits.Tools() {
			reg[t.Name()] = t
		}
	}
	return reg
}

func jsonStrSlice(raw json.RawMessage) []string {
	var out []string
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func jsonStrMap(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

// financialToolBindings 是 W5 金融角色的工具子集（ARCH-W2-W5 §4.3.2）。
//
// 为什么需要单独的迁移而不是靠 SeedTool：
// SeedTool 用 `ON CONFLICT (key) DO NOTHING` —— 已存在的 tools 行**永远不会**
// 更新 agents 列（那是「UI 改过的绑定不被重启覆盖」的设计）。
// 所以给已有行补绑必须走 AddAgentToToolBinding + setting flag 的一次性迁移。
// toolBinding 是一个角色的工具绑定 + **用途说明**。
//
// # 为什么需要 Notes（而不只是工具名清单）
//
// 只有 `role → [工具名]` 会让后来人困惑：「analyst_market 是执行者，
// 为什么能 list_facts 读别人的事实？」
//
// 答案在**用途**不在**权限**：
//   - planner 用 list_facts = 复盘（看历史做了什么决定）
//   - analyst 用 list_facts = 读输入（看上游产出什么）
//
// 同一个工具、两种语义。只写工具名会让人按「这个角色该有什么权限」去猜，
// 而按权限猜会得出「worker 也该能读事实」的错误结论。
//
// # 为什么 Notes 只在数据结构里、且只给易误解的工具
//
// 放在结构体字段里而不是代码注释：注释容易被删，字段会被维护。
// 只给易误解的工具（list_facts / list_thesis / emit_signal / emit_rating）：
// 全写等于没写 —— 每个都注释会让人跳过所有注释。
type toolBinding struct {
	Tools []string
	Notes map[string]string
}

var financialToolBindings = map[string]toolBinding{
	// screener：只在任务未绑标的时出现（阶段 0）。
	//
	// 与 analyst_market 的差别：**不给 mx_query**。
	// 选品要逐个候选比对，而妙想是自然语言问句接口（一次一问），
	// 用它筛 5-8 个候选会变成 5-8 次高延迟调用 —— 而价格/涨跌幅/波动率
	// 这些纯数值条件用 get_quote + compute_indicator 更快也更准。
	"screener": {
		Tools: []string{"get_quote", "get_kline", "compute_indicator", "get_volatility", "list_facts", "emit_fact", "set_task_instrument"},
		Notes: map[string]string{
			"get_quote":           "逐个候选查最新价 —— 只用它验证标的真实存在，不猜代码",
			"get_kline":           "取近期序列算筛选条件（涨跌幅、均线位置）",
			"compute_indicator":   "算 RSI/波动率等可量化条件",
			"get_volatility":      "波动率筛选（低波动策略需要）",
			"list_facts":          "看有没有上游已产出的约束（如风险偏好）",
			"emit_fact":           "落候选清单 —— 必须含「首选」，下游分析师据此选标的",
			"set_task_instrument": "把首选标的写进任务 —— 不调它，下游分析师拿不到标的会全部判「无法分析」",
		},
	},
	"analyst_market": {
		Tools: []string{"get_quote", "get_kline", "compute_indicator", "get_volatility", "mx_query", "list_facts", "emit_fact"},
		Notes: map[string]string{
			"list_facts":     "读上游已产出的事实作输入 —— 不是复盘（worker 才不做复盘，分析师是消费者）",
			"get_volatility": "ATR% + ^VIX 对照，判断标的波动相对大盘的水平",
			"mx_query":       "妙想（东财权威库）自然语言问句：换手率/量比/主力资金流这类 akshare 拿不到的结构化字段",
			"emit_fact":      "写本角色的分析结论（带 snapshot_ids 指向证据）",
		},
	},
	// 新闻分析师：新闻 + 宏观背景。不做技术面、不看财报。
	"analyst_news": {
		Tools: []string{"get_quote", "get_news", "get_macro", "mx_news", "list_facts", "emit_fact"},
		Notes: map[string]string{
			"get_quote": "仅用于确认标的当前价所属交易日，不做技术面分析",
			"get_news":  "取本标的相关的财新/财联社新闻；过滤后为空是合法结果（NO_DATA），不要 fallback 到全量，而应写进 data_gaps",
			"get_macro": "宏观背景（cpi/pmi 等）；与新闻方向冲突时必须写出，不得只挑一边",
			"mx_news":   "妙想资讯：混合返回新闻/公告/研报（含机构评级），覆盖面比 get_news 宽，且研报评级是 get_news 没有的维度",
			"emit_fact": "每条保留的新闻/宏观序列写一条事实，带 snapshot_ids",
		},
	},
	// 基本面分析师：报表 + 申报文件。不做技术面、不解读新闻。
	"analyst_fundamental": {
		Tools: []string{"get_quote", "get_fundamentals", "mx_query", "list_facts", "emit_fact"},
		Notes: map[string]string{
			"get_quote":        "仅用于确认交易日/最新价（PE 需要股价，但本轮不给股本，故不能算 PE）",
			"get_fundamentals": "取财报数据；目前只有 A 股通道，美股/港股返回 NO_DATA 时如实写进 data_gaps（那是能力缺口，不是源故障）",
			"mx_query":         "get_fundamentals 给不了的问句（「近三年毛利率」「单季度扣非净利润」），且覆盖港股/美股",
			"emit_fact":        "每个判断维度（盈利质量/财务健康/数据时效）写一条事实，evidence 必须含科目名+报告期+数值",
		},
	},
	// 加密情绪分析师：资金费率/持仓量 + 宏观背景（get_news 未实现，故不含）
	"analyst_sentiment_crypto": {
		Tools: []string{"get_crypto_flow", "get_macro", "list_facts", "emit_fact"},
		Notes: map[string]string{
			"get_macro":  "加密也受宏观影响（美元流动性、利率），但只作背景不作主导",
			"list_facts": "读上游事实作输入",
		},
	},
	"debate_bull": {
		Tools: []string{"list_facts", "list_thesis", "emit_thesis"},
		Notes: map[string]string{
			"list_facts":  "读全部分析师事实，作为构造多头论证的原料",
			"list_thesis": "读对手（空头）已产出的论点，第 2 轮起用于针对性反驳",
		},
	},
	"debate_bear": {
		Tools: []string{"list_facts", "list_thesis", "emit_thesis"},
		Notes: map[string]string{
			"list_facts":  "读全部分析师事实，用于找多头的证据缺口",
			"list_thesis": "读对手（多头）已产出的论点，用于证伪",
		},
	},
	"research_manager": {
		Tools: []string{"list_facts", "list_thesis", "emit_thesis"},
		Notes: map[string]string{
			"list_thesis": "读**全部轮次**的辩论论据（不只看最新）—— 裁决依赖论证的演变过程",
			"emit_thesis": "写裁决。裁决是可证伪主张（若后续走势相反就错了），不是客观事实，故用 thesis 不用 fact",
		},
	},
	"trader": {
		Tools: []string{"get_quote", "mx_query", "list_thesis", "emit_signal",
			"create_order_draft", "authorize_order"},
		Notes: map[string]string{
			"list_thesis": "读 research_manager 的裁决（含 conclusion 与 confidence），作为发信号的唯一依据",
			"get_quote":   "确认最新价，判断信号的可执行性（不重新分析）",
			"mx_query":    "下单前要一个可复核的最新价/涨跌幅口径（东财权威库）",
			// ⚠️ 只给 trader 前两步，**不给 execute_paper_order**。
			//
			// 交易员的职责是「提出并论证一笔交易」，不是「自己拍板执行」。
			// 执行权留给用户在 UI 上确认（或未来的独立执行角色）。
			// 这与 PRD 的三级模式一致：paper 也应有「提案 → 人工确认 → 执行」的痕迹。
			"create_order_draft": "把交易意图写成草案（不执行）。这是下单的提案步骤",
			"authorize_order":    "让草案过风控闸门。返回 decision=allow 才说明这笔可以执行",
		},
	},
	"risk_aggressive": {
		Tools: []string{"list_thesis", "list_facts", "emit_thesis"},
		Notes: map[string]string{
			"list_thesis": "读 trader 的提案 + 另两方的风控意见（避免重复已有人说过的风险）",
		},
	},
	"risk_neutral": {
		Tools: []string{"list_thesis", "list_facts", "emit_thesis"},
		Notes: map[string]string{
			"list_thesis": "读提案与另两方意见，检查它们与事实之间是否自洽",
		},
	},
	"risk_conservative": {
		Tools: []string{"list_thesis", "list_facts", "emit_thesis"},
		Notes: map[string]string{
			"list_thesis": "读提案与另两方意见，评估最坏情形的不可逆损失",
		},
	},
	"portfolio_manager": {
		Tools: []string{"list_facts", "list_thesis", "list_signals", "emit_rating"},
		Notes: map[string]string{
			"list_signals": "读 trader 的交易信号（结构化字段：direction/horizon/confidence）",
			"list_thesis":  "读三方风控意见 + 研究裁决",
			"emit_rating":  "唯一的评级写入者。若 trader 也能写评级，就成了「既提案又决定」",
		},
	},
}

// seedFinancialToolBindings 给 10 个金融角色补工具绑定（幂等，一次性）。
//
// 为什么用 setting flag 而不是每次启动都跑：
// AddAgentToToolBinding 本身幂等（`NOT (agents ? $1)` 守卫），但每次启动跑 10 个角色
// × 4-6 个工具的 UPDATE 是白白开销。flag 让它在首次启动后不再执行。
//
// 但 flag 也意味着「改了 financialToolBindings 不会自动生效」—— 需要 bump flag 版本。
// 这是刻意的：默认绑定是「初始状态」，用户后续在 UI 里的调整不该被重启抹掉。
func (s *Server) seedFinancialToolBindings() {
	// v2（2026-10-30）：新增 analyst_news / analyst_fundamental 两个角色的绑定。
	//
	// **必须 bump 版本**：v1 在旧库上早已置 true，不换 flag 名这段绑定对
	// 已部署的库永远不生效 —— 而新角色拿不到工具，节点会派发但立刻失败。
	//
	// flag 的语义仍是「一次性补绑」，不是「每次启动对齐」：v1 上用户手动改过的
	// 绑定不会被 v2 抹掉（AddAgentToToolBinding 只补不减，见其 SQL 守卫）。
	// v3（2026-10-31）：给 analyst_market / analyst_news / analyst_fundamental /
	// trader 补绑妙想工具（mx_query / mx_news）。
	//
	// **必须 bump**：v2 在旧库上已置 true，不换 flag 名这段绑定对已部署的库
	// 永远不生效 —— 而 prompt 里已经告诉模型「你有 mx_query」，它调用后会
	// 因为 tools.agents 没绑而拿不到工具（ToolResolve 按 DB 过滤）。
	const flag = "financial_tool_bindings_v5"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	for agentKey, binding := range financialToolBindings {
		if err := s.m.pg.AddAgentToToolBinding(agentKey, binding.Tools); err != nil {
			log.Printf("[financial] %s 默认绑定失败: %v", agentKey, err)
			return // 不设 flag，下次启动重试
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedTradeToolBindings 给金融角色补绑交易执行工具。
//
// # 为什么要单独的 flag 而不是并进 seedFinancialToolBindings
//
// 那个函数已经有 v1/v2/v3 三个版本语境（bump 一次才生效一次）。
// 交易工具是**新能力**，与「妙想工具补绑」是两回事：混在一起意味着
// 用户为了拿到交易工具必须先让某个旧 flag 失效 —— 而旧 flag 早已置 true。
//
// 独立 flag 让「本次新增可单独生效」，且不影响既有绑定。
//
// # 只绑 trader，不绑 execute_paper_order
//
// 见 financialToolBindings["trader"] 的注释：交易员提案 + 过闸，
// 执行权留给用户。若未来引入「自动执行角色」，在那个角色上单独绑第 3 个工具。
func (s *Server) seedTradeToolBindings() {
	const flag = "trade_tool_bindings_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	// ① 先把工具行写进 tools 表。
	//
	// 没有这一步，AddAgentToToolBinding 会更新 0 行（绑定语句按 key 匹配），
	// 而它不报错 —— 表现就是「flag 设了、绑定看起来做了、agent 还是没工具」。
	// 这是 SeedTool 的 ON CONFLICT DO NOTHING 语义决定的：已存在的行保留用户编辑。
	tts := NewTradeToolSet(nil, nil) // 只需要工具名与描述，nil 依赖足够
	for _, tl := range tts.Tools() {
		schema, _ := json.Marshal(tl.InputSchema())
		// agents 列先留空：实际绑定由下一步 AddAgentToToolBinding 完成，
		// 这里写死会让「用户后来解绑」被下次 seed 覆盖。
		if err := s.m.pg.SeedTool(tl.Name(), tl.Description(), schema, nil); err != nil {
			log.Printf("[trade] seed %s 失败: %v", tl.Name(), err)
			return // 不设 flag，下次启动重试
		}
	}

	// ② 只给 trader。executor 类角色当前不存在，将来有了在这里加。
	if err := s.m.pg.AddAgentToToolBinding("trader",
		[]string{"create_order_draft", "authorize_order"}); err != nil {
		log.Printf("[trade] trader 交易工具绑定失败: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// refreshSourceQuoteToolDescriptions 每次启动把按源行情工具的描述刷成代码默认值。
//
// # 为什么不能塞进 seedTradeToolBindings
//
// 那个函数受 flag 守卫（trade_tool_bindings_v1）—— 一次性迁移。
// 而描述刷新**必须每次启动都跑**：工具的返回值契约会随代码演进
// （如 2026-11-03 给 quote_mx 加了「空值不是 0」的说明），
// 不刷新则 DB 里永远停留在第一次 seed 时的旧文案，
// UI 与 /api/tools 都看不到新契约。
//
// # 为什么用 RefreshToolDefaults 而不是 SeedTool
//
// SeedTool 是 ON CONFLICT DO NOTHING —— 行已存在时新描述写不进去。
// RefreshToolDefaults 只更新 description + schema，**保留用户改过的
// agents 绑定与 enabled 开关**，正是这里要的语义。
func (s *Server) refreshSourceQuoteToolDescriptions() {
	for _, key := range agent.SourceQuoteToolNames {
		desc, schemaMap, ok := agent.SourceQuoteToolDefaults(key)
		if !ok {
			continue
		}
		schemaJSON, err := json.Marshal(schemaMap)
		if err != nil {
			log.Printf("[market] 序列化 %s schema 失败: %v", key, err)
			continue
		}
		if err := s.m.pg.RefreshToolDefaults(key, desc, schemaJSON); err != nil {
			log.Printf("[market] 刷新 %s 描述失败: %v", key, err)
		}
	}
}

// refreshComputeIndicatorDefaults 把 compute_indicator 的 schema 刷成代码默认值。
//
// # 为什么需要（2026-11-05 事故）
//
// SeedTool 是 `ON CONFLICT DO NOTHING` —— 首次插入后永不再改。于是给
// compute_indicator 加了 symbol 模式后，已有环境的 tools.schema 仍是旧版：
//
//	模型看到的是 DB 里的旧 schema（无 symbol 参数）
//	→ 它继续手抄 250 根 OHLCV 数组
//	→ 抄出 high=248/low=249/close=249，工具拒绝
//	→ 反复重试 6 次
//
// 而 agent 的实时工具定义是新的（symbol 模式可用），**两边不一致**。
// 这种「代码改了但模型看不见」的问题极难从现象推断，所以启动时强制对齐。
//
// # 为什么用 RefreshToolDefaults 而不是 UpsertToolForce
//
// RefreshToolDefaults 只改 description/schema，**保留 agents 与 enabled**。
// UpsertToolForce 会把 agents 覆盖成传入值 —— 实测传 nil 时把
// compute_indicator 的绑定清成了 []，导致所有角色都拿不到这个工具。
// 用户在 UI 上改过的绑定也不该被一次 schema 升级抹掉。
func (s *Server) refreshComputeIndicatorDefaults() {
	mts := agent.NewMarketToolSet(nil) // 种子壳：只读 Name/Desc/Schema，不调用
	for _, t := range mts.Tools() {
		if t.Name() != "compute_indicator" {
			continue
		}
		schemaJSON, err := json.Marshal(t.InputSchema())
		if err != nil {
			log.Printf("[market] 序列化 compute_indicator schema 失败: %v", err)
			return
		}
		if err := s.m.pg.RefreshToolDefaults(t.Name(), t.Description(), schemaJSON); err != nil {
			log.Printf("[market] 刷新 compute_indicator 定义失败: %v", err)
		}
		return
	}
}

// marketClientForIntent 按 intent 所属标的的市场选行情客户端。
//
// 为什么在 engine 层做而不是 Worker 里：Worker 是执行器，「用哪个 client」
// 是 server 的装配决定。而且 Worker 已有 18 个字段，再加一个是继续膨胀。
//
// 返回值可能为 nil（client 未装配）—— 调用方 WithMarketClient 对 nil 是 no-op，
// vars 构造器会走 stub 分支（给「数据未接入」的说明，不给假数据）。
func (e *Engine) marketClientForIntent(intent *db.Node) agent.MarketClient {
	if intent == nil || len(intent.Payload) == 0 {
		return nil
	}
	var p map[string]any
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return nil
	}
	// crypto 走 OKX，其余走 akshare。渗透 intent 没有 asset_class，会落空。
	if ac, _ := p["asset_class"].(string); ac == agent.AssetClassCrypto {
		return e.cryptoCl
	}
	return e.marketCl
}

// SetMarketClients 给 Engine 注入行情客户端（供 W5 金融角色的 vars 构造器取数）。
//
// 为什么 Engine 要自己持有而不是从 Server 拿：Engine 是独立类型，没有 Server 引用
// （那是刻意的分层 —— Engine 只管编排，不依赖 HTTP 装配）。setter 注入保持这个分层。
//
// 不调用 = 全部 nil，vars 构造器走 stub 分支（「数据未接入」的说明）。
func (e *Engine) SetMarketClients(market, crypto agent.MarketClient) {
	e.marketCl, e.cryptoCl = market, crypto
}

// SetExecAccount 给 Engine 注入默认下单账户。
//
// # 为什么是「默认账户」而不是「按任务选」
//
// 账户是执行配置（exec_accounts 里可能有多条），而任务不该关心它 ——
// 用户建任务时说「买入 BTC」，不是「用 11569 号账户买 BTC」。
// 系统按 active 标记选默认账户，与 exec API 的 default_account 同一判据。
//
// 不调用 = 无账户，trader 只产信号不建草案（见 TraderVars.AccountID 注释）。
func (e *Engine) SetExecAccount(acct agent.ExecAccount) {
	e.execAccount = acct
}
