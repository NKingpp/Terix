package agent

// prompt 模板的渲染与变量校验测试。
//
// 核心目的：**证明模板里每个 {{.X}} 都能被 vars 解析**。
//
// # 为什么必须测这个
//
// renderTmpl 用 `missingkey=error`：模板引用一个 vars 里不存在的字段时，
// template.Execute 返回错误。而 renderSystem 的错误处理是「回退到默认模板」，
// 默认模板若有同样问题则**返回未渲染的原始模板字符串** —— prompt 里就出现了
// `{{.NotAField}}` 这样的字面量。
//
// 那是**静默失败**：不报错、不 panic、测试也可能全绿（因为没人断言输出内容），
// 但 LLM 看到的是模板语法而非数据，会困惑、会胡编、或者直接说「数据缺失」。
//
// 所以这里做两件事：
//  1. 静态校验：遍历模板 AST，检查每个 FieldNode 在 vars 类型里存在
//  2. 动态渲染：用真数据渲染一次，断言关键值出现、无残留 {{

import (
	"reflect"
	"strings"
	"testing"
	"text/template"
	"text/template/parse"
)

// TestRenderAnalystMarketPrompt 渲染 analyst_market 的完整链路。
func TestRenderAnalystMarketPrompt(t *testing.T) {
	tmpl := BuiltinPromptSeeds()["analyst_market"]
	if tmpl == "" {
		t.Fatal("BuiltinPromptSeeds 缺 analyst_market")
	}

	vars := AnalystVars{
		Instrument: InstrumentView{
			Symbol: "QQQ", Name: "Invesco QQQ Trust", AssetClass: "etf",
			Market: "us", Currency: "USD", Exchange: "NASDAQ",
		},
		AsOf:        "2026-10-26",
		Bars:        "20 根日线（2026-09-28 ~ 2026-10-24）",
		BarsJSON:    `[{"trading_date":"2026-10-24","close":"1335.0"}]`,
		Indicators:  `{"macd":1.82,"rsi14":65.4}`,
		SnapshotIDs: []string{"42"},
		DataGaps:    []string{},
		PastContext: "",
	}

	out := renderSystem("analyst_market", tmpl, vars)

	// 1. 变量真被替换
	if !strings.Contains(out, "QQQ") {
		t.Error("Instrument.Symbol 未渲染 —— 变量名不在 vars 里，或模板写错")
	}
	if !strings.Contains(out, "65.4") {
		t.Error("Indicators 未渲染")
	}
	// 2. 无残留模板语法
	if strings.Contains(out, "{{") {
		idx := strings.Index(out, "{{")
		end := idx + 80
		if end > len(out) {
			end = len(out)
		}
		t.Errorf("有未解析的占位符（位置 %d）：%q", idx, out[idx:end])
	}
	// 3. PastContext 为空的 else 分支生效
	if !strings.Contains(out, "无历史反思") {
		t.Error("PastContext 为空的 else 分支未生效 —— 会让 LLM 不知道是「没有」还是「漏了」")
	}
	// 4. 摘要与全量都在（双层设计生效）
	if !strings.Contains(out, "20 根日线（2026-09-28 ~ 2026-10-24）") {
		t.Error("Bars 摘要未渲染")
	}
	if !strings.Contains(out, `"close":"1335.0"`) {
		t.Error("BarsJSON 全量未渲染 —— 双层设计失效，LLM 拿不到具体数值")
	}
	// 5. 叙述性章节保持文本（反模式清单里的一条）
	//    注意断言要跨过 markdown 粗体标记 **不要**计算 —— 直接搜「不要计算」搜不到。
	if !strings.Contains(out, "里已有的指标再重算一遍") {
		t.Error("反模式节内容缺失或叙述文字被误替换")
	}
	// 6. DataGaps 为空时走 {{else}} → 显示「无」（不是空字符串）
	//    空字符串会让 LLM 分不清「没有缺口」和「字段没传」
	if !strings.Contains(out, "| 数据缺口 | 无 |") {
		t.Error("DataGaps 为空的 else 分支未生效 —— LLM 会分不清「没有缺口」和「字段没传」")
	}
	// 7. 关键指令在（防止模板被截断）
	for _, must := range []string{"技术面分析师", "emit_fact", "stance", "confidence_reason", "is_partial"} {
		if !strings.Contains(out, must) {
			t.Errorf("prompt 缺少关键内容 %q —— 模板可能被截断", must)
		}
	}
}

// TestRenderNewsAndFundamentalPrompts 两个新分析师的动态渲染。
//
// 与 TestRenderAnalystMarketPrompt 同一套断言：变量真被替换、无残留 {{、
// 关键指令在。为什么两个角色各测一遍而不是合并：它们的**全量节**变量不同
// （NewsJSON/MacroJSON vs FundamentalsJSON/FilingsJSON），合并测会漏掉
// 「某个角色引用了自己没填的字段」这类错误。
func TestRenderNewsAndFundamentalPrompts(t *testing.T) {
	inst := InstrumentView{
		Symbol: "600519", Name: "贵州茅台", AssetClass: "stock",
		Market: "cn", Currency: "CNY",
	}
	cases := []struct {
		key  string
		vars AnalystVars
		want []string // 渲染后必须出现的字符串
	}{
		{
			key: "analyst_news",
			vars: AnalystVars{
				Instrument:  inst,
				AsOf:        "2026-10-30",
				News:        "12 条新闻（财新 8 / 财联社 4）",
				NewsJSON:    `[{"title":"罚款","source":"caixin","published":"2026-10-23"}]`,
				Macro:       "CPI（%）最新一期 2026-09 = 3.2",
				MacroJSON:   `{"series_name":"CPI","points":[{"period":"2026-09","value":"3.2"}]}`,
				SnapshotIDs: []string{"61"},
			},
			want: []string{"新闻与宏观分析师", "12 条新闻（财新 8 / 财联社 4）",
				`"title":"罚款"`, "emit_fact", "stance", "excluded_count",
				"无历史反思"},
		},
		{
			key: "analyst_fundamental",
			vars: AnalystVars{
				Instrument:       inst,
				AsOf:             "2026-10-30",
				Fundamentals:     "40 行 × 4 期（最新报告期 2026-09-30）",
				FundamentalsJSON: `{"columns":["指标","2026-09-30"],"rows":[{"指标":"净利润","2026-09-30":"1230000000"}]}`,
				Filings:          "（申报文件通道未接入）",
				FilingsJSON:      "[]",
				SnapshotIDs:      []string{"71"},
			},
			want: []string{"基本面分析师", "40 行 × 4 期", `"净利润"`,
				"申报文件通道未接入", "not_applicable", "stance", "无历史反思"},
		},
	}
	for _, c := range cases {
		tmpl, ok := BuiltinPromptSeeds()[c.key]
		if !ok {
			t.Errorf("BuiltinPromptSeeds 缺 %s", c.key)
			continue
		}
		out := renderSystem(c.key, tmpl, c.vars)
		if strings.Contains(out, "{{") {
			i := strings.Index(out, "{{")
			end := i + 80
			if end > len(out) {
				end = len(out)
			}
			t.Errorf("%s: 有未解析占位符（位置 %d）：%q", c.key, i, out[i:end])
		}
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: 渲染结果缺少 %q —— 变量没替换或模板被截断", c.key, w)
			}
		}
	}
}

// TestRenderAnalystMarketWithPastContext PastContext 非空时 if 分支生效。
func TestRenderAnalystMarketWithPastContext(t *testing.T) {
	tmpl := BuiltinPromptSeeds()["analyst_market"]
	vars := AnalystVars{
		Instrument:  InstrumentView{Symbol: "QQQ"},
		AsOf:        "2026-10-26",
		Bars:        "20 根日线",
		BarsJSON:    `[]`,
		Indicators:  `{}`,
		PastContext: "上次分析在 RSI 76 时判断超买，事后 3 日回调 4%，判断正确。",
	}
	out := renderSystem("analyst_market", tmpl, vars)
	if !strings.Contains(out, "上次分析在 RSI 76") {
		t.Error("PastContext 非空时 if 分支未生效")
	}
	if strings.Contains(out, "这是该标的的首次分析") {
		t.Error("PastContext 非空时不该出现 else 分支文案")
	}
}

// promptVarsCase 是一个「模板 key → 它期望的 vars 类型」的配对。
type promptVarsCase struct {
	key  string
	vars any
}

// varsCasesForTest 返回「金融类 prompt → 它的 vars 类型」的完整配对表。
//
// 抽成函数而不是包级变量，是为了让 TestPromptSeedAndVarsCasesInSync 能同时
// 拿到 key 集合（做交叉校验）和类型（做静态校验）。
//
// **新增 prompt 时必须在这里加一行** —— 漏加不会让任何测试失败，
// 但会让那个 prompt 失去静态校验保护。TestPromptSeedAndVarsCasesInSync 兜这个底。
func varsCasesForTest() []promptVarsCase {
	return []promptVarsCase{
		{"analyst_market", AnalystVars{}},
		// news / fundamental 也走 AnalystVars —— 三个角色共享
		// Instrument/AsOf/SnapshotIDs/DataGaps/PastContext，各自的取数字段
		// 是同一个结构里的不同成员（见 AnalystVars 的注释）。
		{"analyst_news", AnalystVars{}},
		{"analyst_fundamental", AnalystVars{}},
		{"analyst_sentiment_crypto", CryptoSentimentVars{}},
		{"debate_bull", DebateVars{}},
		{"debate_bear", DebateVars{}},

		{"research_manager", ResearchManagerVars{}},
		{"trader", TraderVars{}},
		{"risk_aggressive", RiskVars{}},
		{"risk_neutral", RiskVars{}},
		{"risk_conservative", RiskVars{}},
		{"portfolio_manager", PMVars{}},
		// screener 只在无标的任务里出现（阶段 0）。它的 vars 刻意极简 ——
		// 选品员能依赖的只有 user 消息里的意图摘要与自己调的 get_quote。
		{"screener", ScreenerVars{}},
	}
}

// isFinancialPrompt 见 agent/role_prompt.go（生产代码需要它，故移出测试文件）。

// TestBuiltinPromptVarsResolve 静态校验：模板引用的每个变量都能在 vars 里解析。
//
// 遍历模板 AST 的所有 FieldNode（如 {{.Instrument.Symbol}} 对应
// []string{"Instrument", "Symbol"}），逐级用 reflect 查 vars 是否有该字段。
//
// 这个测试能在**不渲染**的情况下发现「模板写了 .Foo 但 AnalystVars 没有 Foo」——
// 而那正是 renderSystem 会静默吞掉、最终返回未渲染模板的情况。
func TestBuiltinPromptVarsResolve(t *testing.T) {
	seeds := BuiltinPromptSeeds()
	for _, c := range varsCasesForTest() {
		tmpl, ok := seeds[c.key]
		if !ok {
			t.Errorf("%s: BuiltinPromptSeeds 里没有这个 key", c.key)
			continue
		}
		if err := validateTemplateFields(tmpl, c.vars); err != nil {
			t.Errorf("%s: %v", c.key, err)
		}
	}
}

// validateTemplateFields 解析模板并逐个检查 FieldNode 在 vars 里存在。
func validateTemplateFields(tmplText string, vars any) error {
	t, err := template.New("v").Option("missingkey=error").Parse(tmplText)
	if err != nil {
		return &promptVarError{msg: "模板语法错误: " + err.Error()}
	}
	root := t.Tree.Root
	if root == nil {
		return &promptVarError{msg: "模板没有根节点"}
	}
	v := reflect.ValueOf(vars)
	return walkNodes(root, v, "")
}

// promptVarError 让错误信息带上出错位置。
type promptVarError struct{ msg string }

func (e *promptVarError) Error() string { return e.msg }

// walkNodes 递归遍历模板节点树。
func walkNodes(node parse.Node, v reflect.Value, path string) error {
	switch n := node.(type) {
	case *parse.ListNode:
		if n == nil {
			return nil
		}
		for _, c := range n.Nodes {
			if err := walkNodes(c, v, path); err != nil {
				return err
			}
		}
	case *parse.ActionNode:
		return walkNodes(n.Pipe, v, path)
	case *parse.PipeNode:
		if n == nil {
			return nil
		}
		for _, c := range n.Cmds {
			if err := walkNodes(c, v, path); err != nil {
				return err
			}
		}
	case *parse.CommandNode:
		for _, a := range n.Args {
			if err := walkNodes(a, v, path); err != nil {
				return err
			}
		}
	case *parse.FieldNode:
		return checkFieldPath(n.Ident, v)
	case *parse.VariableNode:
		// 模板局部变量（$x := ...），不是数据字段，跳过
		return nil
	case *parse.IfNode:
		if err := walkNodes(n.Pipe, v, path); err != nil {
			return err
		}
		if err := walkNodes(n.List, v, path); err != nil {
			return err
		}
		if err := walkNodes(n.ElseList, v, path); err != nil {
			return err
		}
	case *parse.RangeNode:
		// range 会改变上下文（. 变成元素），这里只校验 pipe 本身
		return walkNodes(n.Pipe, v, path)
	case *parse.WithNode:
		return walkNodes(n.Pipe, v, path)
	case *parse.TemplateNode:
		return nil
	case *parse.TextNode, *parse.CommentNode, *parse.ChainNode, *parse.BoolNode,
		*parse.DotNode, *parse.NilNode, *parse.NumberNode, *parse.StringNode,
		*parse.IdentifierNode, *parse.BreakNode, *parse.ContinueNode:
		return nil
	}
	return nil
}

// checkFieldPath 检查 ["Instrument","Symbol"] 这样的路径在 v 里能否逐级解析。
func checkFieldPath(ident []string, v reflect.Value) error {
	cur := v
	for i, name := range ident {
		// 解引用指针/接口
		for cur.Kind() == reflect.Ptr || cur.Kind() == reflect.Interface {
			if cur.IsNil() {
				// 零值指针：无法继续，但类型上合法 —— 视为通过
				// （渲染时若真的为 nil，template 会报 nil pointer，那是运行时问题）
				return nil
			}
			cur = cur.Elem()
		}
		if cur.Kind() != reflect.Struct {
			return &promptVarError{msg: "在 ." + strings.Join(ident[:i], ".") +
				" 上取字段 " + name + "，但它的类型是 " + cur.Kind().String() + "，不是 struct"}
		}
		f, ok := cur.Type().FieldByName(name)
		if !ok {
			avail := make([]string, 0, cur.NumField())
			for j := 0; j < cur.NumField(); j++ {
				avail = append(avail, cur.Type().Field(j).Name)
			}
			return &promptVarError{msg: "模板引用了 {{." + strings.Join(ident, ".") +
				"}}，但 " + cur.Type().Name() + " 没有字段 " + name +
				"（现有字段: " + strings.Join(avail, ", ") + "）"}
		}
		_ = f
		cur = cur.FieldByName(name)
	}
	return nil
}

// TestPromptSeedAndVarsCasesInSync 交叉校验：BuiltinPromptSeeds 里的每个金融 prompt
// 都必须在 varsCasesForTest() 里有对应行。
//
// 为什么需要：漏加一行不会让任何测试失败 —— TestBuiltinPromptVarsResolve 只会
// 检查它在 cases 里的那些，漏掉的 prompt 静默失去保护，而测试仍全绿。
// 这个测试把「漏加」变成可见失败。
func TestPromptSeedAndVarsCasesInSync(t *testing.T) {
	inCases := make(map[string]bool)
	for _, c := range varsCasesForTest() {
		inCases[c.key] = true
	}
	for key := range BuiltinPromptSeeds() {
		if isFinancialPrompt(key) && !inCases[key] {
			t.Errorf("%s 在 BuiltinPromptSeeds 里但不在 varsCasesForTest() 里 —— "+
				"静态变量校验对它失效（TestBuiltinPromptVarsResolve 不会检查它）", key)
		}
	}
	// 反向检查：cases 里的每个 key 都必须真的有模板（防止拼错 key）
	seeds := BuiltinPromptSeeds()
	for _, c := range varsCasesForTest() {
		if _, ok := seeds[c.key]; !ok {
			t.Errorf("varsCasesForTest() 里有 %s 但 BuiltinPromptSeeds 没有 —— key 拼错了？", c.key)
		}
	}
}
