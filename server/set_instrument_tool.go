package server

// set_task_instrument：选品员把「选中哪个标的」写进任务。
//
// # 为什么必须存在（2026-11-05 全流程断裂）
//
// 选品员（screener）在无标的任务里挑出候选，产出是**一条 fact**（自然语言）：
//
//	emit_fact: "首选 AAPL，近20日 +3.8%，RSI14=57.8"
//
// 而下游分析师读的是 **intent.payload.instrument_id**：
//
//	analyst_market      → {"asset_class":"stock","instrument_id":0}
//	analyst_fundamental → "标的未知...无法进行分析" → stance=unavailable
//
// 两条通道不通。实测三个分析师**全部**在空标的下判 unavailable，
// 整条链到此断掉，交易员拿不到任何信号。选品结果丢了。
//
// # 为什么是独立工具而不是让 emit_fact 兼职
//
// emit_fact 的语义是「记录一条观察」，它不该修改任务状态 ——
// 那会让「谁改了标的」这件事无法审计。独立工具语义清晰，日志也查得清。
//
// # 什么时候用
//
// 只在**任务尚未绑定标的**时有效。已绑标的的任务调用它会被拒绝 ——
// 否则选品员能改写用户明确指定的标的，那是不可接受的越权。

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Autumn-27/artex/exec/okxrest"
	"log"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// SetInstrumentToolSet 暴露 set_task_instrument。
type SetInstrumentToolSet struct {
	pg *db.DB
}

// NewSetInstrumentToolSet 构造工具集。
func NewSetInstrumentToolSet(pg *db.DB) *SetInstrumentToolSet {
	return &SetInstrumentToolSet{pg: pg}
}

// Tools 返回工具列表（目前只有一个）。
func (s *SetInstrumentToolSet) Tools() []actool.CoreTool {
	return []actool.CoreTool{s.setTaskInstrument()}
}

func (s *SetInstrumentToolSet) setTaskInstrument() actool.CoreTool {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"symbol": map[string]any{
				"type":        "string",
				"description": "标的完整 instId，如 BTC-USDT-SWAP / AAPL-USDT-SWAP。自动下单只支持永续（必须带 -SWAP 后缀，market 填 crypto）",
			},
			"market": map[string]any{
				"type":        "string",
				"description": "市场：crypto|us|hk|cn。永续一律填 crypto（含美股永续）",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "标的名称（可选），如 Apple Inc。缺省用 symbol",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "选它的依据（可审计）。写数字，如「近20日 -18%，RSI14=31，符合超跌条件」",
			},
		},
		"required": []string{"symbol", "market", "reason"},
	}
	return actool.Build(actool.Spec{
		Name: "set_task_instrument",
		Description: "把【本任务要分析的标的】确定下来 —— 选品员唯一能改动任务状态的工具。\n" +
			"调用后，下游所有分析师会拿到这个标的（symbol/market/asset_class），\n" +
			"不再出现「标的未知，无法分析」。\n" +
			"\n" +
			"⚠ 只在本任务**尚未绑定标的**时用来**选**标的。\n" +
			"任务已有标的时会拒绝改标的（那说明用户明确指定了，你不该覆盖）；\n" +
			"但若上次恰好在下游展开时失败，再次调用会**补建**下游阶段而不是报错。\n" +
			"\n" +
			"调用前务必先用 get_quote 验证标的真实存在（能取到价格），\n" +
			"否则会把不存在的代码写进任务，下游分析师全部白跑。",
		Schema: schema,
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision {
			return acperm.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return s.runSetTaskInstrument(ctx, in)
		},
	})
}

func (s *SetInstrumentToolSet) runSetTaskInstrument(ctx context.Context, in json.RawMessage) (actool.Result, error) {
	if s.pg == nil {
		return actool.Errorf("set_task_instrument 未启用：DB 未注入"), nil
	}
	var a struct {
		Symbol string `json:"symbol"`
		Market string `json:"market"`
		Name   string `json:"name"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(in, &a); err != nil {
		return actool.Errorf(fmt.Sprintf("入参解析失败: %v", err)), nil
	}
	symbol := strings.TrimSpace(a.Symbol)
	market := strings.ToLower(strings.TrimSpace(a.Market))
	if symbol == "" {
		return actool.Errorf("缺少 symbol"), nil
	}
	if market == "" {
		return actool.Errorf("缺少 market（cn|hk|us|crypto）"), nil
	}
	if strings.TrimSpace(a.Reason) == "" {
		return actool.Errorf("缺少 reason —— 标的选定必须可审计"), nil
	}

	// 任务上下文：必须知道自己属于哪个任务。
	// 没有就拒绝 —— 跨任务改标的是严重越权。
	ri := agent.RunInfoFrom(ctx)
	if ri.TaskID <= 0 {
		return actool.Errorf("set_task_instrument 需要任务上下文（当前不在某个任务内运行）"), nil
	}

	// ① 读任务当前标的。
	var current *int64
	if err := s.pg.QueryRowContext(ctx,
		`SELECT instrument_id FROM tasks WHERE id=$1`, ri.TaskID).Scan(&current); err != nil {
		return actool.Errorf(fmt.Sprintf("读取任务失败: %v", err)), nil
	}
	if current != nil && *current > 0 {
		// 已有标的 —— 只放行「补建下游」，不放行改标的。
		//
		// # 为什么不能一律拒绝（2026-11-11 修）
		//
		// 上一次调用可能恰好在下游展开时失败（比如 MaterializeDownstream 报错）。
		// 那时 tasks.instrument_id **已经落库**，而阶段图没建。
		// 一律拒绝会让重试被挡在门外 —— 下游永远补不上，形成死锁。
		//
		// 改标的仍然是禁止的（那是权限边界：用户明确写了标的时选品员不该改写）。
		// 幂等由 MaterializeDownstream 的 join 判据保证：下游已在就返回 0。
		assetClass := agent.AssetClassStock
		if instRow, e := s.pg.Instruments().GetInstrumentByID(*current); e == nil && instRow != nil && instRow.AssetClass != "" {
			assetClass = instRow.AssetClass
		}
		created, mErr := MaterializeDownstream(ctx, s.pg, ri.ExplorationID, *current, assetClass)
		if mErr != nil {
			return actool.Errorf(fmt.Sprintf(
				"标的已绑定（instrument_id=%d）但下游补建失败: %v。请重试或联系运维",
				*current, mErr)), nil
		}
		if created == 0 {
			return actool.Errorf(fmt.Sprintf(
				"本任务已绑定标的（instrument_id=%d），阶段图也已展开，不能覆盖。"+
					"直接分析该标的即可，不要调用本工具", *current)), nil
		}
		return actool.Text(fmt.Sprintf(
			"标的已绑定（instrument_id=%d），已补建 %d 个下游阶段节点。"+
				"分析师现在会针对该标的取数分析。", *current, created)), nil
	}

	// ①.5 任何非永续标的都要**升级为对应永续**，而不是原样绑定。
	//
	// # 为什么不能绑非永续（2026-11-14 任务 29 / 30 两次整链白跑）
	//
	// 任务 29：选品员挑 BTC-USDT（现货）→ 11 个下游节点全为一个
	// 永远不可交易的标的服务，交易员最后只能报「找不到标的」。
	// 任务 30：挑 NVDA（market=us 的裸股票代码）→ 同样的结局，
	// 而且更难查：工具返回的提示里只有一句「该标的只能分析、不能自动执行」，
	// 模型据此认为「那我换个写法试试」，然后在 NVDA / NVDA-USDT /
	// NVDA-USDT-SWAP 之间反复试，最后判「绑定不一致，需要人工介入」。
	//
	// 提示语没用 —— 那是选品员的最后一次工具调用，它不会因此再改一次标的
	// （改标的本身被工具禁止）。所以这里直接**升级**：
	//   BTC       → BTC-USDT-SWAP
	//   BTC-USDT  → BTC-USDT-SWAP
	//   NVDA (us) → NVDA-USDT-SWAP   ← OKX 确实有美股永续
	// 升级前用公开行情验证该永续真实存在，避免凭空造标的。
	var upgradeNote string
	up := strings.ToUpper(strings.TrimSpace(symbol))
	if !strings.HasSuffix(up, "-SWAP") {
		perp, ok := toPerpSymbol(symbol)
		if ok {
			if perpPx, pErr := perpLastPrice(ctx, perp); pErr == nil && perpPx != "" {
				upgradeNote = fmt.Sprintf("（%s 已升级为永续 %s，现价 %s —— 非永续标的无法自动下单）",
					symbol, perp, perpPx)
				symbol, market, up = perp, "crypto", perp
			} else if market == "crypto" {
				// crypto 标的找不到对应永续 → 明确拒绝。
				// 让它绑一个不可交易的标的，等于让整条链白跑。
				return actool.Errorf(fmt.Sprintf(
					"标的 %q 在 OKX 没有对应的永续合约（试过 %s：%v）。"+
						"请改选有永续的标的，如 BTC-USDT-SWAP / NVDA-USDT-SWAP",
					symbol, perp, pErr)), nil
			}
			// 非 crypto（A股/美股）且没有对应永续 → 允许绑（用户可能只要分析），
			// 但把话说清楚：下游交易阶段会拒绝，别让它以为能下单。
			if !strings.HasSuffix(up, "-SWAP") && market != "crypto" {
				upgradeNote = fmt.Sprintf("（%s 在 OKX 没有对应永续，只能分析、不能自动下单）", symbol)
			}
		}
	}
	// 永续的 market 一律是 crypto（OKX 的美股永续也走加密通道）。
	if strings.HasSuffix(up, "-SWAP") {
		market = "crypto"
	}

	// ② 查/建 instruments 行。
	instID, err := ensureInstrument(ctx, s.pg, market, symbol, a.Name)
	if err != nil {
		return actool.Errorf(fmt.Sprintf("登记标的失败: %v", err)), nil
	}

	// ③ 写 tasks.instrument_id。
	if _, err := s.pg.ExecContext(ctx,
		`UPDATE tasks SET instrument_id=$1 WHERE id=$2`, instID, ri.TaskID); err != nil {
		return actool.Errorf(fmt.Sprintf("写入任务标的失败: %v", err)), nil
	}

	// ④ 补建下游阶段。
	//
	// # 为什么在这里建而不是创建任务时建
	//
	// 下游 13 个意图全都要 instrument_id，而标的要等选品才知道。
	// 创建任务时先建出来只会是空标的节点 —— 分析师拿到 instrument_id=0
	// 后全部判「标的无法分析」（实测 2026-11-05）。所以创建时只建选品节点
	// （ExpandOpts.ScreenerOnly），选定后在这里补建。
	assetClass := agent.AssetClassStock
	if market == "crypto" {
		assetClass = agent.AssetClassCrypto
	}
	created, mErr := MaterializeDownstream(ctx, s.pg, ri.ExplorationID, instID, assetClass)
	if mErr != nil {
		// 标的已写成功，但下游没建 —— 必须明确报错，否则用户看到「标的已绑定」
		// 却没有任何分析师开跑，无从判断卡在哪。
		return actool.Errorf(fmt.Sprintf(
			"标的已绑定（instrument_id=%d），但展开下游阶段失败: %v。请重试或联系运维",
			instID, mErr)), nil
	}

	// ⑤ 兜底：把仍为空标的的开放意图补上（正常情况下 ④ 已带好值，这里应为 0）。
	patched, _ := patchOpenIntentInstrument(ctx, s.pg, ri.ExplorationID, instID)

	// ⑥ 可执行性提示（不阻断）。
	hint := instrumentExecutabilityHint(symbol, market)
	if hint != "" {
		hint = " " + hint
	}

	return actool.Text(fmt.Sprintf(
		"标的已绑定：%s（%s，instrument_id=%d）。已展开 %d 个下游阶段，%d 个待启动意图补齐标的。"+
			"分析师现在会针对该标的取数分析。%s%s",
		symbol, market, instID, created, patched, upgradeNote, hint)), nil
}

// toPerpSymbol 把现货/裸代码写法映射成 OKX 永续 instId。
//
//	BTC          → BTC-USDT-SWAP
//	BTC-USDT     → BTC-USDT-SWAP
//	BTC-USDT-SWAP→ 原样
func toPerpSymbol(symbol string) (string, bool) {
	up := strings.ToUpper(strings.TrimSpace(symbol))
	switch {
	case up == "":
		return "", false
	case strings.HasSuffix(up, "-SWAP"):
		return up, true
	case strings.Contains(up, "-"):
		// 已有分隔符但不是 SWAP：只接受 X-USDT 这种两段式。
		parts := strings.Split(up, "-")
		if len(parts) == 2 {
			return up + "-SWAP", true
		}
		return "", false
	default:
		return up + "-USDT-SWAP", true
	}
}

// perpLastPrice 用**公开**接口取最新价（不需要凭据），用于验证永续是否存在。
func perpLastPrice(ctx context.Context, instID string) (string, error) {
	client := okxrest.NewClient(okxrest.Credentials{})
	return client.LastPrice(ctx, instID)
}

// instrumentExecutabilityHint 判断「这个标的能不能被本系统自动下单」，返回提示语（空=没问题）。
//
// # 为什么选品环节就要提示
//
// 选品员容易按「行情能取到」挑标的，而自动下单要求的是 OKX 永续 instId。
// 两者不是一回事：BTC-USDT 能取价但无法自动下单（现货持仓不在 positions
// 接口里），AAPL 能取价但交易通道要的是 AAPL-USDT-SWAP。
//
// 不在这里说清，错误要等到交易员建草案时才暴露 —— 前面整条链已经白跑。
// 但不拒绝：用户可能只要分析不要下单。
func instrumentExecutabilityHint(symbol, market string) string {
	up := strings.ToUpper(strings.TrimSpace(symbol))
	switch {
	case strings.HasSuffix(up, "-SWAP") && market == "crypto":
		return "" // 正常：可直接自动下单
	case strings.HasSuffix(up, "-SWAP"):
		return fmt.Sprintf("注意：%s 是永续，market 建议填 crypto（当前 %s）——"+
			"永续的行情与交易通道都走加密市场，填 %s 会取不到价。", symbol, market, market)
	case market == "crypto":
		return fmt.Sprintf("注意：%s 不是永续 instId（缺 -SWAP 后缀）。"+
			"本系统只能对永续自动下单，该标的只能分析、不能自动执行。", symbol)
	default:
		return fmt.Sprintf("注意：%s（market=%s）是非永续标的。"+
			"本系统只能对 OKX 永续（形如 %s-USDT-SWAP，market=crypto）自动下单；"+
			"该标的只能分析、不能自动执行。", symbol, market, symbol)
	}
}

// ensureInstrument 按 market+symbol 找标的，没有就建一条。
//
// asset_class 由 market 推导：crypto → crypto，其余 → stock。
// 不细分 etf/index 是因为选品员报的是「能交易的东西」，
// 而 get_quote 只关心 market+symbol 能否取到价。
func ensureInstrument(ctx context.Context, pg *db.DB, market, symbol, name string) (int64, error) {
	store := pg.Instruments()
	if inst, err := store.GetInstrumentByMarketSymbol(market, symbol); err == nil && inst != nil {
		return inst.ID, nil
	}
	if strings.TrimSpace(name) == "" {
		name = symbol
	}
	assetClass := agent.AssetClassStock
	if market == "crypto" {
		assetClass = agent.AssetClassCrypto
	}
	id, _, err := store.UpsertInstrument(db.InstrumentInput{
		Symbol:     symbol,
		Name:       name,
		Market:     market,
		AssetClass: assetClass,
		Currency:   currencyForMarket(market),
	})
	return id, err
}

func currencyForMarket(market string) string {
	switch market {
	case "cn":
		return "CNY"
	case "hk":
		return "HKD"
	case "crypto":
		return "USDT"
	default:
		return "USD"
	}
}

// patchOpenIntentInstrument 给尚未启动的意图补 instrument_id。
//
// 返回被修改的意图数。只动 state='open' 的节点：
// running/done 的输入已被消费，改它会让产出无法复现。
func patchOpenIntentInstrument(ctx context.Context, pg *db.DB, expID, instID int64) (int, error) {
	if expID <= 0 {
		return 0, nil // 非任务运行（chat/mainagent），没有图可改
	}
	// # SQLite 改写要点
	//
	// `payload || jsonb_build_object(...)` → `json_patch(payload, json_object(...))`：
	// jsonb 的 `||` 是「右侧覆盖左侧」，json_patch 语义一致（实测）。
	// `(payload->>'k')::bigint` → `CAST(json_extract(payload,'$.k') AS INTEGER)`，
	// 缺失时 json_extract 返回 NULL，COALESCE 兜成 0。
	//
	// `COALESCE(NULLIF(payload,''),'{}')` 兜住空串脏值（json_patch 对空串报错）。
	res, err := pg.ExecContext(ctx, `
UPDATE exploration_nodes
   SET payload = json_patch(
         COALESCE(NULLIF(payload, ''), '{}'),
         json_object('instrument_id', $1)
       )
 WHERE exploration_id = $2
   AND kind = 'intent'
   AND state = 'open'
   AND COALESCE(CAST(json_extract(payload, '$.instrument_id') AS INTEGER), 0) = 0`,
		instID, expID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// SeedSetInstrumentTool 把 set_task_instrument 写进 tools 表并绑给 screener。
//
// # 为什么必须 seed（2026-11-05 全流程实测）
//
// 只在 buildDomainReg 里注册是不够的：ToolResolve 会遍历 tools 表，
// 表里没有这一行 → 工具不会出现在模型的工具列表里 → 模型永远不调用它。
//
// 实测症状：选品员算出「首选 BTC-USDT」并 emit_fact 写了候选清单，
// 但从头到尾没调用 set_task_instrument（它根本没看到这个工具），
// 于是 tasks.instrument_id 一直是空，planner 只好派一个无 role 的意图
// 去「算仓位参数」，整条链再次断裂。
//
// # 为什么用 flag 守卫
//
// 与 seedTradeToolBindings 同一模式：一次性迁移，避免每次启动覆盖
// 用户在 UI 上改过的描述/绑定。种子只做「插入 + 补绑」，不做覆盖。
func (s *Server) SeedSetInstrumentTool() {
	if s.m == nil || s.m.pg == nil {
		return
	}
	const flag = "set_instrument_tool_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}

	// ① 写工具行。nil 依赖足够 —— 只需要 Name/Description/Schema。
	sits := NewSetInstrumentToolSet(nil)
	for _, tl := range sits.Tools() {
		schema, _ := json.Marshal(tl.InputSchema())
		// agents 留空：实际绑定由下一步完成。写死会让「用户后来解绑」
		// 被下次 seed 覆盖。
		if err := s.m.pg.SeedTool(tl.Name(), tl.Description(), schema, nil); err != nil {
			log.Printf("[screener] seed %s 失败: %v", tl.Name(), err)
			return // 不设 flag，下次启动重试
		}
	}

	// ② 只给 screener。
	//
	// 不给 analyst_*：选标的只在选品阶段做。分析师若也能改标的，
	// 会出现「三个分析师各改一次」的竞争，而标的应当唯一确定。
	if err := s.m.pg.AddAgentToToolBinding("screener",
		[]string{"set_task_instrument"}); err != nil {
		log.Printf("[screener] 工具绑定失败: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}
