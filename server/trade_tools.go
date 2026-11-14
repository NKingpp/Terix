package server

// 交易执行工具：把「下单」暴露成 agent 可调的工具。
//
// # 为什么必须做这件事（2026-11-02 用户决策）
//
// 在此之前 agent **没有任何下单工具**。它想交易只能调 OKX skill，
// 而 skill 内部直接 exec `okx` CLI —— 那条路绕过整个 exec 层：
//   - 没有闸门（冻结/草案完整性/风控规则都不生效）
//   - 没有草案与审计记录
//   - 凭证来自 ~/.okx/config.toml（与凭证页脱节）
//
// 用户的原话是「通过工具实现下单，否则后面无法进行拦截」——这就是判据：
// **下单必须经过 Gate**，否则拦截无从谈起。
//
// # 为什么放在 server 包而不是 agent 包
//
// exec/gate.go 已经 import 了 agent（它要求 Draft/Instrument 等类型）。
// 若 agent 反过来 import exec 就是循环依赖。server 同时依赖两者，是天然的
// 装配层 —— 工具定义放在这里，agent 通过 buildDomainReg 拿到它们，
// 与 MarketToolSet 的装配方式一致。
//
// # 三个工具的分工
//
//	create_order_draft  提案：写草案（不执行、无副作用）
//	authorize_order     过闸：Gate.Authorize 判定 + 落判定记录
//	execute_paper_order 执行：仅 paper 模式真正记账
//
// 为什么拆三步而不是一个 place_order：闸门需要草案 ID 才能求值风控、
// 比对规则指纹、写 draft_risk_verdicts。所以「先有草案」是硬约束。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/exec"
	"github.com/Autumn-27/artex/exec/okxrest"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// tradeToolNames 是交易工具名清单。
//
// 独立导出给测试与 buildDomainReg 用 —— 名字散落在两处时，
// 改一个忘另一个会让 agent 的 tools 绑定静默失效（名字不存在时
// ToolResolve 只是跳过，不会报错）。
var tradeToolNames = []string{
	"create_order_draft",
	"authorize_order",
	"execute_paper_order",
	"get_positions",
	"get_task_trades",
	"modify_bracket",
}

// draftScopedTradeToolNames 是「以草案为中心」的工具子集。
//
// # 为什么要这个子集（2026-11-14）
//
// 验收测试原本遍历 tradeToolNames 断言「所有交易工具都必须校验
// task/exploration/intent/signal 五层上下文」。那是三个草案工具的契约。
// 新增的 get_positions / get_task_trades / modify_bracket 不接 signal_id ——
// 让它们也满足那条断言，只能靠凭空加一个用不上的参数，或者让测试
// 断言一个不成立的性质。
//
// 分开之后两边各自精确：草案工具必须做全量上下文校验（那是防跨任务下单），
// 只读/改保护工具只需 task + instrument。
var draftScopedTradeToolNames = []string{
	"create_order_draft",
	"authorize_order",
	"execute_paper_order",
}

// TradeToolSet 持有交易工具需要的依赖。
type TradeToolSet struct {
	pg   *db.DB
	gate *exec.Gate
	// creds 读 OKX 交易凭据。nil 表示未装配 —— 只有需要打交易所的
	// 工具（get_positions / modify_bracket）会因此报错，其余照常可用。
	creds func() (*db.OkxTradeCred, error)
}

// NewTradeToolSet 构造交易工具集。
//
// 任一依赖为 nil 时**仍返回可用对象**：工具名必须在目录里存在，
// 因为 agent 的 tools 表可能已绑定了这些名字，名字消失会让整轮规划失败。
// 调用时才报「未装配」的明确错误。
func NewTradeToolSet(pg *db.DB, gate *exec.Gate) *TradeToolSet {
	return &TradeToolSet{pg: pg, gate: gate}
}

// SetCreds 注入 OKX 凭据读取器。
//
// 传 nil 安全：需要凭证的工具会返回「未装配」，而不是 panic。
func (t *TradeToolSet) SetCreds(fn func() (*db.OkxTradeCred, error)) {
	if t != nil {
		t.creds = fn
	}
}

// Tools 返回全部交易工具。
func (t *TradeToolSet) Tools() []actool.CoreTool {
	return []actool.CoreTool{
		t.createOrderDraft(),
		t.authorizeOrder(),
		t.executePaperOrder(),
		t.getPositions(),
		t.getTaskTrades(),
		t.modifyBracket(),
	}
}

// Name 判断某工具名是否属于交易工具集。
func (t *TradeToolSet) Has(name string) bool {
	for _, n := range tradeToolNames {
		if n == name {
			return true
		}
	}
	return false
}

// guard 统一处理依赖缺失。
func (t *TradeToolSet) guard() error {
	if t == nil || t.pg == nil {
		return fmt.Errorf("交易工具未装配数据库（server 启动时未初始化）")
	}
	if t.gate == nil {
		return fmt.Errorf("交易工具未装配闸门（server 启动时未初始化）")
	}
	return nil
}

// ---------------------------------------------------------------------
// 工具构造函数辅助（与 agent 包同名函数保持同样的 schema 形状）
// ---------------------------------------------------------------------

func tradeObj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func tradeStr(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func tradeInt(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

// tradeJSON 把 payload 序列化成工具成功结果。
//
// 用 actool.Text（与 agent.jsonResult 一致）而不是手拼 Result ——
// Result 的 Content 是 []llm.ContentBlock，直接塞 string 编译不过。
func tradeJSON(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(fmt.Sprintf("序列化结果失败: %v", err)), nil
	}
	return actool.Text(string(b)), nil
}

// tradeWriteTool 构造一个写工具（非只读、不可并发）。
//
// 与 agent.writeTool 的差异：这里权限直接放行。
// 理由：能不能下单由 Gate 判定，不是由工具权限判定 ——
// 两处都判会让「为什么被拒」有两个互不一致的答案。
func tradeWriteTool(name, desc string, schema map[string]any,
	run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision {
			return acperm.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// ---------------------------------------------------------------------
// create_order_draft：写入草案
// ---------------------------------------------------------------------

func (t *TradeToolSet) createOrderDraft() actool.CoreTool {
	return tradeWriteTool(
		"create_order_draft",
		"⚠️ **已停用（2026-11-14）**：草案不再有任何执行者。要下单请直接用 open_position / close_position。\n\n"+
			"本工具仍会写一条草案记录（仅供留痕与人工复核），但**不会成交**：\n"+
			"旧的后台循环（TradeLoop 与 paper 自动成交）已下线 —— 那是「模型写完草案就\n"+
			"结束、之后成交与否全由固定规则决定」的旧设计，控制权不在模型手里。\n"+
			"若你调用它，唯一的后果是台账里多一条永远不会执行的记录。\n\n"+
			"参数：\n"+
			"  symbol: 完整 instId（如 BTC-USDT-SWAP）\n"+
			"  market: crypto | cn | hk | us\n"+
			"  side:   buy | sell\n"+
			"  qty:    合约张数（字符串）\n"+
			"  ref_price: 参考价（字符串，可选）\n"+
			"  signal_id: 交易信号 ID（可选；提供时必须与标的匹配）\n\n"+
			"=== 以下是旧版说明，保留供审计 ===\n"+
			"创建一笔交易草案（不执行、不下单，仅生成待审核的订单意图）。\n"+
			"这是下单的第一步：草案必须先过 authorize_order 的闸门判定才能执行。\n"+
			"返回 draft_id，后续步骤都要用它。\n\n"+
			"本工具只写草案，**不会**真正下单、也不会被任何人执行。",
		tradeObj(map[string]any{
			"symbol":      tradeStr("标的代码，如 BTC / 600519 / AAPL"),
			"market":      tradeStr("市场：crypto|cn|hk|us"),
			"side":        tradeStr("方向：buy|sell"),
			"qty":         tradeStr("数量（字符串）"),
			"ref_price":   tradeStr("参考价（字符串，可空）"),
			"signal_id":   tradeInt("交易信号 ID（可选；必须与标的匹配）"),
			"action":      tradeStr("entry(开仓) | exit(平仓) | scale_in(加仓) | scale_out(减仓) | hold。默认 entry"),
			"tp_price":    tradeStr("止盈触发价（字符串）。开仓/加仓必填；多单必须 > ref_price，空单 < ref_price"),
			"sl_price":    tradeStr("止损触发价（字符串）。开仓/加仓必填；多单必须 < ref_price，空单 > ref_price"),
			"reduce_only": map[string]any{"type": "boolean", "description": "平仓/减仓必须 true（会自动补上）"},
			"pos_side":    tradeStr("被平的持仓方向 long|short（平仓时必填）；留空则按 side 推导"),
		}, "symbol", "market", "side", "qty"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			if err := t.guard(); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var a struct {
				Symbol     string `json:"symbol"`
				Market     string `json:"market"`
				Side       string `json:"side"`
				Qty        string `json:"qty"`
				RefPrice   string `json:"ref_price"`
				SignalID   int64  `json:"signal_id"`
				Action     string `json:"action"`
				TpPrice    string `json:"tp_price"`
				SlPrice    string `json:"sl_price"`
				ReduceOnly bool   `json:"reduce_only"`
				PosSide    string `json:"pos_side"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(fmt.Sprintf("入参解析失败: %v", err)), nil
			}

			symbol := strings.TrimSpace(a.Symbol)
			market := strings.ToLower(strings.TrimSpace(a.Market))
			side := strings.ToLower(strings.TrimSpace(a.Side))
			qty := strings.TrimSpace(a.Qty)
			if symbol == "" {
				return actool.Errorf("缺少 symbol"), nil
			}
			if side != "buy" && side != "sell" {
				return actool.Errorf(fmt.Sprintf(`side 必须是 "buy" 或 "sell"，收到 %q`, side)), nil
			}
			if qty == "" {
				return actool.Errorf("缺少 qty"), nil
			}

			// crypto 走统一解析（容忍 BTC / BTC-USDT / BTC-USDT-SWAP 三种写法，
			// 并在表里没有该永续时自动登记）；其它市场保持严格精确匹配 ——
			// A股/美股的代码体系不同，自动补后缀只会造出错标的。
			var inst *db.Instrument
			var instErr error
			if market == "crypto" {
				inst, instErr = resolveTradeInstrument(t.pg, symbol)
				if instErr != nil {
					return actool.Errorf(instErr.Error()), nil
				}
				symbol = inst.Symbol
			} else {
				inst, instErr = t.pg.Instruments().GetInstrumentByMarketSymbol(market, symbol)
				if instErr != nil {
					return actool.Errorf(fmt.Sprintf(
						"找不到标的 %q（market=%q）：%v\n"+
							"提示：该标的需先存在于 instruments 表。",
						symbol, market, instErr)), nil
				}
			}

			var signalID *int64
			if a.SignalID < 0 {
				return actool.Errorf("signal_id 无效"), nil
			}
			if a.SignalID > 0 {
				signalID = &a.SignalID
			}
			if err := t.validateTradeContext(ctx, inst.ID, signalID); err != nil {
				return actool.Errorf(err.Error()), nil
			}

			action := strings.ToLower(strings.TrimSpace(a.Action))
			if action == "" {
				action = "entry"
			}
			switch action {
			case "entry", "exit", "scale_in", "scale_out", "hold", "review":
			default:
				return actool.Errorf(fmt.Sprintf("action %q 非法（entry|exit|scale_in|scale_out|hold|review）", a.Action)), nil
			}
			reduce := action == "exit" || action == "scale_out"
			if reduce {
				// 平仓不填 reduce_only 是常见疏漏，直接补上；显式填 false 才是矛盾。
				if !a.ReduceOnly && strings.Contains(strings.ToLower(string(in)), `"reduce_only":false`) {
					return actool.Errorf(fmt.Sprintf("action=%s 必须配合 reduce_only=true", action)), nil
				}
				posSide := strings.ToLower(strings.TrimSpace(a.PosSide))
				if posSide != "long" && posSide != "short" {
					return actool.Errorf("平仓必须指定 pos_side（long 或 short，即被平的持仓方向）"), nil
				}
				want := "sell"
				if posSide == "short" {
					want = "buy"
				}
				if side != want {
					return actool.Errorf(fmt.Sprintf("平 %s 仓只能 %s，收到 %s", posSide, want, side)), nil
				}
				// 有跟踪记录时顺手校验数量；没有就交给交易循环在下发前用真实持仓再核一次。
				if rows, rErr := t.pg.ListOpenLivePositions(0); rErr == nil {
					for _, row := range rows {
						if row.InstID != inst.Symbol {
							continue
						}
						if strings.TrimSpace(row.Qty) != "" && !lessOrEqualDecimal(qty, row.Qty) {
							return actool.Errorf(fmt.Sprintf("平仓量 %s 超过跟踪持仓 %s", qty, row.Qty)), nil
						}
					}
				}
			} else {
				// 开仓/加仓：TP/SL 必填，方向与幅度护栏在 exec 包里（硬编码，不靠 prompt）。
				// pos_side 若填了就必须与 side 一致 —— 否则会通过 derivePosSide 的
				// hint 优先级（那个优先级是为平仓设计的）泄露到开仓单，
				// 变成「buy + posSide=short」这种必然被 51000 拒的组合。
				posSideIn := strings.ToLower(strings.TrimSpace(a.PosSide))
				if posSideIn != "" {
					want := "long"
					if side == "sell" {
						want = "short"
					}
					if posSideIn != want {
						return actool.Errorf(fmt.Sprintf(
							"开仓时 pos_side 必须与 side 对应（%s → %s），收到 %q；不填则由系统推导",
							side, want, a.PosSide)), nil
					}
				}
				if err := exec.ValidateBracket(exec.BracketInput{
					Side: side, Action: action, Qty: qty,
					RefPrice: a.RefPrice, TpPrice: a.TpPrice, SlPrice: a.SlPrice,
				}); err != nil {
					return actool.Errorf(err.Error()), nil
				}
			}

			draftID, err := t.pg.OrderDrafts().InsertDraft(db.OrderDraftInput{
				SignalID:     signalID,
				InstrumentID: &inst.ID,
				Side:         side,
				Qty:          qty,
				// RefPrice 是 string（空串 = 未指定，落成 NULL）。
				RefPrice: strings.TrimSpace(a.RefPrice),
			})
			if err != nil {
				return actool.Errorf(fmt.Sprintf("写草案失败: %v", err)), nil
			}
			// 执行参数落侧表：market/posSide/reduceOnly/TP-SL 是下单必需，
			// 而 orders_draft 没有这些列（SQLite 加列不可幂等）。
			if err := t.pg.UpsertDraftExecParams(db.DraftExecParams{
				DraftID: draftID, OrderType: "market",
				PosSide:    strings.ToLower(strings.TrimSpace(a.PosSide)),
				ReduceOnly: reduce,
				TpPrice:    strings.TrimSpace(a.TpPrice),
				SlPrice:    strings.TrimSpace(a.SlPrice),
			}); err != nil {
				return actool.Errorf(fmt.Sprintf("写草案执行参数失败: %v", err)), nil
			}

			return tradeJSON(map[string]any{
				"draft_id": draftID,
				"symbol":   inst.Symbol,
				"market":   inst.Market,
				"side":     side,
				"qty":      qty,
				"action":   action,
				"tp_price": strings.TrimSpace(a.TpPrice),
				"sl_price": strings.TrimSpace(a.SlPrice),
				"note":     "草案已创建，尚未执行。下一步调用 authorize_order 过闸门；过闸后交易循环会自动下发。",
			})
		},
	)
}

// ---------------------------------------------------------------------
// authorize_order：过闸门
// ---------------------------------------------------------------------

func (t *TradeToolSet) authorizeOrder() actool.CoreTool {
	return tradeWriteTool(
		"authorize_order",
		"让订单草案通过风控闸门判定（不执行下单）。\n"+
			"闸门检查：执行模式、账户冻结、草案完整性、标的信息、风控规则。\n"+
			"返回 decision=allow 才可进入执行步骤；deny 时 reason 说明原因。\n\n"+
			"account_id 取自 exec_accounts 表。",
		tradeObj(map[string]any{
			"draft_id":   tradeInt("草案 ID（create_order_draft 返回的 draft_id）"),
			"account_id": tradeInt("执行账户 ID（exec_accounts.id）"),
		}, "draft_id", "account_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			if err := t.guard(); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var a struct {
				DraftID   int64 `json:"draft_id"`
				AccountID int64 `json:"account_id"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(fmt.Sprintf("入参解析失败: %v", err)), nil
			}
			if a.DraftID <= 0 {
				return actool.Errorf("draft_id 无效"), nil
			}
			if a.AccountID <= 0 {
				return actool.Errorf("account_id 无效（需 > 0，取自 exec_accounts.id）"), nil
			}

			if err := t.validateDraftContext(ctx, a.DraftID); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			mode, err := t.pg.GetExecMode()
			if err != nil {
				return actool.Errorf(fmt.Sprintf("读执行模式失败: %v", err)), nil
			}

			res, err := t.gate.Authorize(ctx, exec.GateRequest{
				DraftID:   a.DraftID,
				AccountID: a.AccountID,
				Mode:      mode,
			})
			if err != nil {
				return actool.Errorf(fmt.Sprintf("闸门判定失败: %v", err)), nil
			}

			payload := map[string]any{
				"decision":          string(res.Decision),
				"reason":            res.Reason,
				"mode":              string(mode),
				"rules_fingerprint": res.RulesFingerprint,
				"limits_revision":   res.LimitsRevision,
			}
			if res.ErrCode != "" {
				payload["err_code"] = res.ErrCode
			}
			return tradeJSON(payload)
		},
	)
}

// ---------------------------------------------------------------------
// execute_paper_order：paper 模式执行
// ---------------------------------------------------------------------

func (t *TradeToolSet) executePaperOrder() actool.CoreTool {
	return tradeWriteTool(
		"execute_paper_order",
		"在纸面（paper）模式执行已过闸门的订单，记入纸面账本。**不涉及真实资金**。\n"+
			"前置：该草案必须先通过 authorize_order（decision=allow）。\n"+
			"若当前是 live 模式，本工具会拒绝 —— 真实下单走独立通道。\n\n"+
			"为什么 live 不走这里：真实资金下发由 exec/vaultd 独立进程承担，\n"+
			"它只从环境变量读密钥、不持有数据库连接（隔离设计）。",
		tradeObj(map[string]any{
			"draft_id":   tradeInt("草案 ID"),
			"account_id": tradeInt("执行账户 ID"),
		}, "draft_id", "account_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			if err := t.guard(); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var a struct {
				DraftID   int64 `json:"draft_id"`
				AccountID int64 `json:"account_id"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(fmt.Sprintf("入参解析失败: %v", err)), nil
			}
			if a.DraftID <= 0 || a.AccountID <= 0 {
				return actool.Errorf("draft_id / account_id 无效"), nil
			}

			if err := t.validateDraftContext(ctx, a.DraftID); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			mode, err := t.pg.GetExecMode()
			if err != nil {
				return actool.Errorf(fmt.Sprintf("读执行模式失败: %v", err)), nil
			}
			// 只允许 paper。live 的真实下发不在进程内，不能在此静默降级 ——
			// 那会让 agent 以为「已下单」而实际什么都没发生。
			if mode != db.ExecModePaper {
				return actool.Errorf(fmt.Sprintf(
					"当前是 %s 模式，本工具只执行 paper 单。真实下单需走 live 通道（未接入本工具）",
					mode)), nil
			}

			paperID, err := t.gate.ExecutePaper(ctx, a.DraftID, a.AccountID)
			if err != nil {
				return actool.Errorf(fmt.Sprintf("纸面执行失败: %v", err)), nil
			}

			return tradeJSON(map[string]any{
				"paper_order_id": paperID,
				"draft_id":       a.DraftID,
				"account_id":     a.AccountID,
				"mode":           string(mode),
				"note":           "已记入纸面账本，不涉及真实资金。",
			})
		},
	)
}

// requireTaskInstrument 校验任务上下文并返回任务绑定的 instrument_id。
//
// 与 validateTradeContext 的分工：这个只回答「上下文是否自洽 + 有没有标的」，
// 不比对某个具体标的。用于「可能要列出多个标的」的只读工具（get_positions）
// 与「按任务查账」的工具（get_task_trades）。
//
// 错误文案必须可区分（任务不存在 / exploration 不一致 / 意图不一致 /
// 未绑定标的）：调用方要靠它判断是「传错上下文」还是「还没选品」，
// 统一成一句「参数错误」会让这两类故障看起来一样。
// requireTaskContext 校验任务上下文自洽（不要求标的绑定），返回任务。
//
// # 为什么与 requireTaskInstrument 分开（2026-11-14 实测踩过）
//
// 只读工具（get_positions / get_task_trades）不该因为「任务还没绑标的」
// 就拒绝服务：用户问「我现在有仓吗」，读的是交易所事实，与任务绑没绑无关。
// 实测中主 agent 因为这道多余的绑定要求，连续 4 次 get_positions 全部失败，
// 然后开始用 add_task_scope / insert_assets 乱试 —— 把工具用坏比不给工具更糟。
func (t *TradeToolSet) requireTaskContext(ctx context.Context) (*db.Task, error) {
	ri := agent.RunInfoFrom(ctx)
	if ri.TaskID <= 0 || ri.ExplorationID <= 0 || ri.IntentID < 0 {
		return nil, fmt.Errorf("交易任务上下文无效，需明确 task_id 与 exploration_id")
	}
	task, err := t.pg.GetTask(ri.TaskID)
	if err != nil {
		return nil, fmt.Errorf("读取交易任务 %d 失败: %v", ri.TaskID, err)
	}
	if task == nil {
		return nil, fmt.Errorf("交易任务上下文无效，任务 %d 不存在", ri.TaskID)
	}
	if task.ExplorationID != ri.ExplorationID {
		return nil, fmt.Errorf("交易任务 %d 与 exploration_id=%d 不一致", ri.TaskID, ri.ExplorationID)
	}
	if ri.IntentID > 0 {
		if _, iErr := tradeIntentInstrumentID(t.pg, ri.ExplorationID, ri.IntentID); iErr != nil {
			return nil, iErr
		}
	}
	return task, nil
}

func (t *TradeToolSet) requireTaskInstrument(ctx context.Context) (int64, error) {
	ri := agent.RunInfoFrom(ctx)
	if ri.TaskID <= 0 || ri.ExplorationID <= 0 || ri.IntentID < 0 {
		return 0, fmt.Errorf("交易任务上下文无效，需明确 task_id 与 exploration_id")
	}
	task, err := t.pg.GetTask(ri.TaskID)
	if err != nil {
		return 0, fmt.Errorf("读取交易任务 %d 失败: %v", ri.TaskID, err)
	}
	if task == nil {
		return 0, fmt.Errorf("交易任务上下文无效，任务 %d 不存在", ri.TaskID)
	}
	if task.ExplorationID != ri.ExplorationID {
		return 0, fmt.Errorf("交易任务 %d 与 exploration_id=%d 不一致", ri.TaskID, ri.ExplorationID)
	}
	if task.InstrumentID == nil || *task.InstrumentID <= 0 {
		return 0, fmt.Errorf("交易任务 %d 未明确 instrument_id，拒绝猜测标的", ri.TaskID)
	}
	if ri.IntentID > 0 {
		intentInstID, iErr := tradeIntentInstrumentID(t.pg, ri.ExplorationID, ri.IntentID)
		if iErr != nil {
			return 0, iErr
		}
		if intentInstID <= 0 {
			return 0, fmt.Errorf("交易意图未明确 instrument_id，拒绝猜测标的；请先绑定明确标的")
		}
	}
	return *task.InstrumentID, nil
}

func (t *TradeToolSet) validateDraftContext(ctx context.Context, draftID int64) error {
	draft, err := t.pg.OrderDrafts().GetDraft(draftID)
	if err != nil {
		return fmt.Errorf("读取交易草案 %d 失败: %v", draftID, err)
	}
	if draft.InstrumentID == nil || *draft.InstrumentID <= 0 {
		return fmt.Errorf("交易草案 %d 未明确 instrument_id", draftID)
	}
	return t.validateTradeContext(ctx, *draft.InstrumentID, draft.SignalID)
}

func (t *TradeToolSet) validateTradeContext(ctx context.Context, instrumentID int64, signalID *int64) error {
	ri := agent.RunInfoFrom(ctx)
	// 无任务的直接纸面调用保留；部分任务上下文不能按无上下文放行。
	if ri.TaskID != 0 || ri.ExplorationID != 0 || ri.IntentID != 0 {
		if ri.TaskID <= 0 || ri.ExplorationID <= 0 || ri.IntentID < 0 {
			return fmt.Errorf("交易任务上下文无效，需明确 task_id 与 exploration_id")
		}
		task, err := t.pg.GetTask(ri.TaskID)
		if err != nil {
			return fmt.Errorf("读取交易任务 %d 失败: %v", ri.TaskID, err)
		}
		if task == nil {
			return fmt.Errorf("交易任务上下文无效，任务 %d 不存在", ri.TaskID)
		}
		if task.ExplorationID != ri.ExplorationID {
			return fmt.Errorf("交易任务 %d 与 exploration_id=%d 不一致", ri.TaskID, ri.ExplorationID)
		}
		if task.InstrumentID == nil || *task.InstrumentID <= 0 {
			return fmt.Errorf("交易任务 %d 未明确 instrument_id，拒绝猜测标的", ri.TaskID)
		}
		if *task.InstrumentID != instrumentID {
			return fmt.Errorf("交易任务 instrument_id=%d 与请求或草案标的不一致（instrument_id=%d）", *task.InstrumentID, instrumentID)
		}
		if ri.IntentID > 0 {
			intentInstID, err := tradeIntentInstrumentID(t.pg, ri.ExplorationID, ri.IntentID)
			if err != nil {
				return err
			}
			if intentInstID <= 0 {
				return fmt.Errorf("交易意图未明确 instrument_id，拒绝猜测标的；请先绑定明确标的")
			}
			if intentInstID != instrumentID {
				return fmt.Errorf("交易意图 instrument_id=%d 与请求或草案标的不一致（instrument_id=%d）", intentInstID, instrumentID)
			}
		}
	}
	if signalID == nil {
		return nil
	}
	sig, err := t.pg.Signals().GetSignal(*signalID)
	if err != nil {
		return fmt.Errorf("signal_id=%d 不存在或不可读: %v", *signalID, err)
	}
	if sig.InstrumentID == nil || *sig.InstrumentID != instrumentID {
		return fmt.Errorf("signal_id=%d 与请求或草案标的不一致", *signalID)
	}
	if ri.TaskID > 0 && (sig.TaskID == nil || *sig.TaskID != ri.TaskID) {
		return fmt.Errorf("signal_id=%d 不属于当前任务 %d", *signalID, ri.TaskID)
	}
	if sig.TaskID != nil {
		task, err := t.pg.GetTask(*sig.TaskID)
		if err != nil {
			return fmt.Errorf("读取 signal_id=%d 所属任务失败: %v", *signalID, err)
		}
		if task == nil || task.InstrumentID == nil || *task.InstrumentID <= 0 {
			return fmt.Errorf("signal_id=%d 所属任务未明确 instrument_id", *signalID)
		}
		if *task.InstrumentID != instrumentID {
			return fmt.Errorf("signal_id=%d 与所属任务标的不一致", *signalID)
		}
	}
	return nil
}

func tradeIntentInstrumentID(pg *db.DB, expID, intentID int64) (int64, error) {
	if pg == nil || expID <= 0 || intentID <= 0 {
		return 0, fmt.Errorf("交易意图上下文无效，拒绝猜测标的")
	}
	intent, err := pg.Exploration(expID).GetNode(intentID)
	if err != nil {
		return 0, fmt.Errorf("读取交易意图 %d 失败: %v", intentID, err)
	}
	if intent == nil || intent.Kind != db.KindIntent {
		return 0, fmt.Errorf("交易意图 %d 不存在或类型无效", intentID)
	}
	var payload struct {
		InstrumentID int64 `json:"instrument_id"`
	}
	if err := json.Unmarshal(intent.Payload, &payload); err != nil {
		return 0, fmt.Errorf("读取交易意图 %d 标的失败: %v", intentID, err)
	}
	return payload.InstrumentID, nil
}

// 确保 agent 包被引用（buildDomainReg 用 agent.ToolSet，这里只是编译期锚点）。
var _ = agent.NewToolSet

// lessOrEqualDecimal 比较两个十进制字面量 a <= b。
//
// 解析失败返回 false（fail-closed：算不出来不能算「没超」）。
func lessOrEqualDecimal(a, b string) bool {
	x, ok1 := new(big.Rat).SetString(strings.TrimSpace(a))
	y, ok2 := new(big.Rat).SetString(strings.TrimSpace(b))
	if !ok1 || !ok2 {
		return false
	}
	return x.Cmp(y) <= 0
}

// ---------------------------------------------------------------------
// okxClient：需要打交易所的工具共用的客户端构造
// ---------------------------------------------------------------------

func (t *TradeToolSet) okxClient() (*okxrest.Client, error) {
	if t == nil {
		return nil, fmt.Errorf("交易工具未装配")
	}
	return buildOkxClient(t.creds)
}

// buildOkxClient 按凭据读取器构造客户端（两个工具集共用）。
func buildOkxClient(creds func() (*db.OkxTradeCred, error)) (*okxrest.Client, error) {
	if creds == nil {
		return nil, fmt.Errorf("OKX 凭据未装配（server 启动时未初始化），无法读写交易所")
	}
	cred, err := creds()
	if err != nil {
		return nil, fmt.Errorf("读取 OKX 交易凭据失败: %w", err)
	}
	if cred == nil || strings.TrimSpace(cred.APIKey) == "" {
		return nil, fmt.Errorf("没有可用的 OKX 交易凭据，请先在凭证页配置")
	}
	return okxrest.NewClient(okxrest.Credentials{
		APIKey: cred.APIKey, SecretKey: cred.SecretKey, Passphrase: cred.Passphrase,
		Demo: cred.Demo, BaseURL: cred.BaseURL,
	}), nil
}

// defaultOkxAccount 取一个可用于实盘的执行账户（排掉 paper-auto 与冻结账户）。
func (t *TradeToolSet) defaultOkxAccount() (*db.ExecAccount, error) {
	if t == nil {
		return nil, fmt.Errorf("交易工具未装配")
	}
	return pickOkxAccount(t.pg)
}

// pickOkxAccount 选一个可用于实盘的 OKX 执行账户（两个工具集共用）。
func pickOkxAccount(pg *db.DB) (*db.ExecAccount, error) {
	if pg == nil {
		return nil, fmt.Errorf("交易工具未装配数据库")
	}
	accts, err := pg.ExecAccounts().ListAccounts()
	if err != nil {
		return nil, fmt.Errorf("读执行账户失败: %w", err)
	}
	for _, a := range accts {
		if a.Channel != "okx" || !a.Active || a.BlockedAt != nil {
			continue
		}
		if strings.HasPrefix(a.ChannelAcctID, "paper-auto") {
			continue
		}
		return a, nil
	}
	return nil, fmt.Errorf("没有可用于实盘的 OKX 执行账户（uid 不能是 paper-auto）")
}

// ---------------------------------------------------------------------
// resolveTradeInstrument：交易工具的统一标的解析
// ---------------------------------------------------------------------

// resolveTradeInstrument 把用户/模型给的标的写法解析成一个可交易的标的行。
//
// # 为什么需要它（2026-11-14 任务 29 全链路卡死）
//
// 三处写法不一致，每一处都能让交易失败：
//   - create_order_draft 文档说传 "BTC"（基础币），实测表里只有 "BTC-USDT"
//   - open_position 要完整 instId "BTC-USDT-SWAP"，而表里根本没有这一行
//   - instruments 表是**选品阶段**才写入的，可交易标的必须先去那里登记
//
// 实测 work#1 试了 `create_order_draft {symbol:"BTC-USDT-SWAP"}` →
// "找不到标的"；试 "BTC-USDT" → 命中但那是现货，被止盈止损校验挡住。
// 两个写法各错一半，模型无从下手。
//
// # 规则（按顺序试，第一个命中即用）
//
//  1. 精确匹配
//  2. 裸代码（无 "-"）→ 补 "-USDT-SWAP"（BTC → BTC-USDT-SWAP）
//  3. "X-USDT"（现货写法）→ 补 "-SWAP"（BTC-USDT → BTC-USDT-SWAP）
//  4. 都没命中，但形如 "X-USDT-SWAP" → **自动登记**
//
// 第 3 条是有争议的：用户写 BTC-USDT 可能真想交易现货。但本系统只支持
// 永续（现货持仓在 OKX 是余额、不在 positions 接口里，无法巡检与保护），
// 所以「意图是交易」这个前提下，把现货写法映射到对应永续是唯一可执行的
// 解释。若用户真要分析现货，走 get_quote 那条只读路径，不会到这里。
func resolveTradeInstrument(pg *db.DB, raw string) (*db.Instrument, error) {
	if pg == nil {
		return nil, fmt.Errorf("交易工具未装配数据库")
	}
	sym := strings.ToUpper(strings.TrimSpace(raw))
	if sym == "" {
		return nil, fmt.Errorf("缺少 symbol（如 BTC-USDT-SWAP）")
	}
	store := pg.Instruments()

	// ⚠️ 只接受 **-SWAP 结尾** 的标的行。
	//
	// # 为什么不能「精确匹配优先」（2026-11-14 任务 29 的形态）
	//
	// instruments 表里已经有 BTC-USDT（现货，选品阶段写的）。
	// 若精确匹配优先，请求 BTC-USDT-SWAP 会被「软化」成 BTC-USDT，
	// 然后被永续闸挡回来 —— 错误信息看着像「你选错了标的」，
	// 而真实情况是「表里有现货行，把永续请求吃掉了」。
	//
	// 所以过滤掉非永续行，让 BTC/USDT 这些写法一律走永续候选。
	isPerp := func(inst *db.Instrument) bool {
		return inst != nil && strings.HasSuffix(strings.ToUpper(inst.Symbol), "-SWAP")
	}
	tryExact := func(s string) *db.Instrument {
		inst, err := store.GetInstrumentByMarketSymbol("crypto", s)
		if err == nil && isPerp(inst) {
			return inst
		}
		return nil
	}

	candidates := []string{sym}
	if !strings.Contains(sym, "-") {
		candidates = append(candidates, sym+"-USDT-SWAP")
	} else if strings.HasSuffix(sym, "-USDT") {
		candidates = append(candidates, sym+"-SWAP")
	}
	for _, c := range candidates {
		if inst := tryExact(c); inst != nil {
			return inst, nil
		}
	}

	// 自动登记：只有形如 X-USDT-SWAP 才登记。
	//
	// 不加这个判断的话，模型把 "BTC" 或 "BTC-USDT" 写错时会被登记成
	// 一堆永远不可交易的假标的，污染 instruments 表。
	final := candidates[len(candidates)-1]
	if !strings.HasSuffix(final, "-SWAP") {
		return nil, fmt.Errorf("找不到可交易的永续 %q。本系统只交易 OKX 永续（形如 BTC-USDT-SWAP）—— "+
			"请用带 -SWAP 后缀的完整 instId", raw)
	}
	id, _, err := store.UpsertInstrument(db.InstrumentInput{
		Symbol: final, Name: final, Market: "crypto",
		AssetClass: agent.AssetClassCrypto, Currency: "USDT",
	})
	if err != nil {
		return nil, fmt.Errorf("登记标的 %q 失败: %v", final, err)
	}
	inst, err := store.GetInstrumentByID(id)
	if err != nil || inst == nil {
		return nil, fmt.Errorf("登记后仍读不到标的 %q", final)
	}
	slog.Info("交易工具：自动登记永续标的", "symbol", final, "id", id, "requested", raw)
	return inst, nil
}

// ---------------------------------------------------------------------
// get_positions：读交易所真实持仓 + 保护单
// ---------------------------------------------------------------------

func (t *TradeToolSet) getPositions() actool.CoreTool {
	return tradeReadTool(
		"get_positions",
		"读 OKX 永续的**真实持仓**（含未实现盈亏）与当前挂着的止盈止损。\n"+
			"这是判断「该不该平仓 / 该不该挪止损」的唯一事实来源 —— 不要凭记忆或草案推断持仓。\n\n"+
			"返回每个仓的：instId / posSide / pos（净数量，负数=空头）/ avgPx（开仓均价）/\n"+
			"markPx（标记价）/ upl（未实现盈亏 USDT）/ lever / protection（tp、sl、algoId）。\n\n"+
			"protection 为空数组表示**这个仓没有保护单**（裸仓），应立刻用 modify_bracket 补挂。",
		tradeObj(map[string]any{
			"symbol": tradeStr("可选。只看某个标的（完整 instId，如 BTC-USDT-SWAP）"),
		}),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			if err := t.guard(); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var a struct {
				Symbol string `json:"symbol"`
			}
			_ = json.Unmarshal(in, &a)
			want := strings.TrimSpace(a.Symbol)

			// 上下文先校验：它在 DB 里就能判，不该等到打交易所才发现没任务。
			// 只要求上下文自洽，不要求标的已绑定（读持仓与绑定与否无关）。
			task, ctxErr := t.requireTaskContext(ctx)
			if ctxErr != nil {
				return actool.Errorf(ctxErr.Error()), nil
			}
			// 标的过滤：**能解析就解析，解析不出来也不拒绝**。
			//
			// # 为什么不能拒绝（2026-11-14 任务 29）
			//
			// 读交易所持仓与「本地 instruments 表里有没有这一行」无关。
			// 实测 get_positions {"symbol":"BTC-USDT-SWAP"} 直接报
			// "instrument not found"，而那一刻仓可能真的存在 —— 一个只读工具
			// 因为本地目录缺一行而拒绝报告事实，是最没道理的失败。
			//
			// 解析成功时才做「与任务绑定标的一致」的校验（那是防跨标的）；
			// 解析失败就按用户给的字面 instId 去查，让交易所回答。
			if want != "" {
				if inst, iErr := resolveTradeInstrument(t.pg, want); iErr == nil {
					want = inst.Symbol
					if task.InstrumentID != nil && *task.InstrumentID > 0 && inst.ID != *task.InstrumentID {
						return actool.Errorf(fmt.Sprintf(
							"标的 %q 与任务绑定的 instrument_id=%d 不一致", want, *task.InstrumentID)), nil
					}
				} else {
					want = strings.ToUpper(want)
				}
			}

			// 纸面模式读**纸面账本**，不是交易所。
			//
			// # 为什么（2026-11-14 任务 30）
			//
			// 纸面仓在交易所不存在，所以按交易所读永远返回空。而模型拿到
			// 「count:0」就断定「没有持仓」，于是它不会去平仓/改保护 ——
			// 实测任务 30 的 trader 连续两次报「三处都找不到该仓」，
			// 而纸面账本里明明有一条 short 0.1。
			//
			// 这不是小事：「看不见自己的仓」会让后续所有决策都建立在
			// 错误前提上，而模型没有任何办法发现这一点。
			if mode, mErr := t.pg.GetExecMode(); mErr == nil && mode == db.ExecModePaper {
				return t.getPaperPositions(ctx)
			}

			client, cErr := t.okxClient()
			if cErr != nil {
				return actool.Errorf(cErr.Error()), nil
			}
			cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
			defer cancel()

			positions, err := client.GetPositions(cctx, "SWAP")
			if err != nil {
				return actool.Errorf(fmt.Sprintf("读持仓失败: %v", err)), nil
			}
			algos, err := client.ListAlgoOrders(cctx, "SWAP", "oco")
			if err != nil {
				// 持仓能读到、保护单读不到 —— 必须明确说「保护状态未知」，
				// 不能当成「没有保护」（那会诱使模型重复挂单）。
				return actool.Errorf(fmt.Sprintf(
					"读持仓成功但读保护单失败: %v。**保护状态未知，不要据此判断无保护**", err)), nil
			}

			type protection struct {
				AlgoID string `json:"algo_id"`
				Tp     string `json:"tp"`
				Sl     string `json:"sl"`
			}
			type row struct {
				InstID  string `json:"inst_id"`
				PosSide string `json:"pos_side"`
				// Pos 是带符号净量（net 模式下负数=空），Qty 是绝对值，
				// Avail 是可平量（long_short 下 pos 恒正，只有它能反映挂单占用）。
				Pos        string       `json:"pos"`
				Qty        string       `json:"qty"`
				Avail      string       `json:"avail"`
				AvgPx      string       `json:"avg_px"`
				MarkPx     string       `json:"mark_px"`
				Upl        string       `json:"upl"`
				Lever      string       `json:"lever"`
				Protection []protection `json:"protection"`
			}
			out := make([]row, 0, len(positions))
			bare := make([]string, 0, 2)
			// 方向归一化走 exec.SideQtyOf（三处各写一份会互相矛盾）。
			// 这里用它把 net 模式的 posSide="net" 翻译成 long/short，
			// 让模型看到的方向与它自己下单时要填的 pos_side 一致。
			posMode := ""
			if acct, aErr := pickOkxAccount(t.pg); aErr == nil && acct != nil {
				if prm, pErr := t.pg.GetExecAccountParams(acct.ID); pErr == nil {
					posMode = prm.PosMode
				}
			}
			for _, p := range positions {
				if strings.TrimSpace(p.Pos) == "" || p.Pos == "0" {
					continue
				}
				if want != "" && p.InstID != want {
					continue
				}
				side, absQty, ok := exec.SideQtyOf(posMode, p.PosSide, p.Pos)
				if !ok {
					continue
				}
				r := row{InstID: p.InstID, PosSide: side, Pos: signedQtyStr(side, p.Pos), Qty: absQty,
					Avail: exec.AvailOf(p.Pos, p.AvailPos),
					AvgPx: p.AvgPx, MarkPx: p.MarkPx, Upl: p.Upl, Lever: p.Lever,
					Protection: []protection{}}
				for _, ag := range algos {
					if ag.InstID != p.InstID {
						continue
					}
					if !exec.AlgoSideMatches(posMode, ag.PosSide, side) {
						continue
					}
					r.Protection = append(r.Protection, protection{AlgoID: ag.AlgoID, Tp: ag.TpTriggerPx, Sl: ag.SlTriggerPx})
				}
				if len(r.Protection) == 0 {
					bare = append(bare, p.InstID)
				}
				out = append(out, r)
			}
			payload := map[string]any{
				"positions":    out,
				"count":        len(out),
				"mark_is_last": true,
			}
			if len(bare) > 0 {
				payload["warning"] = "以下仓没有止盈止损（裸仓）：" + strings.Join(bare, ", ") +
					" —— 立刻用 modify_bracket 补挂，不要等信号"
			}
			if len(out) == 0 {
				payload["note"] = "交易所当前无持仓（这是交易所返回空，不是读取失败）"
			}
			return tradeJSON(payload)
		},
	)
}

// ---------------------------------------------------------------------
// get_task_trades：本任务已实现盈亏
// ---------------------------------------------------------------------

func (t *TradeToolSet) getTaskTrades() actool.CoreTool {
	return tradeReadTool(
		"get_task_trades",
		"读**本任务**的已实现盈亏（实盘逐笔成交 + 模拟盘纸面账本，分开统计）。\n"+
			"注意：这是**已平仓部分**的盈亏。未平仓的浮动盈亏在 get_positions 的 upl 里。\n"+
			"判断「目标是否达成」「该不该继续加仓」时必须先看这个，不要凭印象。",
		tradeObj(map[string]any{}),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			if err := t.guard(); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if _, ctxErr := t.requireTaskContext(ctx); ctxErr != nil {
				return actool.Errorf(ctxErr.Error()), nil
			}
			ri := agent.RunInfoFrom(ctx)

			fills, err := t.pg.ListFills(db.FillsFilter{TaskID: ri.TaskID, Limit: 1000})
			if err != nil {
				return actool.Errorf(fmt.Sprintf("读实盘成交失败: %v", err)), nil
			}
			livePnL := exec.ComputePnLByInst(fills)

			actions, _ := t.pg.ListTradeRecords(db.TradeRecordFilter{TaskID: ri.TaskID, Limit: 100})
			paper, pErr := db.TaskTradeSummaryFor(ctx, t.pg.DB, ri.TaskID, "paper", 0)
			if pErr != nil {
				return actool.Errorf(fmt.Sprintf("读模拟盘成交失败: %v", pErr)), nil
			}

			return tradeJSON(map[string]any{
				"task_id": ri.TaskID,
				"live": map[string]any{
					"fill_count": len(fills),
					"pnl":        livePnL,
					"synced":     len(fills) > 0,
					"note":       "逐笔成交含手续费；未平仓部分的浮动盈亏不在此处",
				},
				"paper": map[string]any{
					"realized_total": paper.RealizedTotal,
					"fills":          paper.Fills,
					"open_positions": paper.OpenPositions,
					"fee_included":   false,
				},
				// actions 是「我做过什么」的原始台账（含被拒的）。
				// 与 pnl 的差别：pnl 只统计真的成交了的部分。
				"actions": actions,
			})
		},
	)
}

// ---------------------------------------------------------------------
// modify_bracket：改止盈止损（同步，不走草案队列）
// ---------------------------------------------------------------------

func (t *TradeToolSet) modifyBracket() actool.CoreTool {
	return tradeWriteTool(
		"modify_bracket",
		"修改某个持仓的止盈/止损触发价。**同步生效**（秒级），不进草案队列。\n\n"+
			"为什么同步：挪止损是为了跟住行情，走 15 秒队列就失去意义；\n"+
			"而且改保护**不会放大风险敞口**（不增仓），不需要过闸门。\n\n"+
			"校验：多单必须 sl < 现价 < tp；空单必须 tp < 现价 < sl。\n"+
			"违反会被本地拒绝 —— 那是防止「止损设在现价上方」这种立刻触发的误操作。\n\n"+
			"该仓没有保护单时会自动补挂一组新的。\n"+
			"两个价格都可只给一个（只改止盈或只改止损）。",
		tradeObj(map[string]any{
			"symbol":   tradeStr("完整 instId，如 BTC-USDT-SWAP"),
			"pos_side": tradeStr("被保护的持仓方向：long | short"),
			"tp_price": tradeStr("新止盈触发价（字符串）。不改就留空"),
			"sl_price": tradeStr("新止损触发价（字符串）。不改就留空"),
			"reason":   tradeStr("为什么改（必填，写进审计）：如「浮盈 +2U，止损上移到成本价」"),
		}, "symbol", "pos_side", "reason"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			if err := t.guard(); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var a struct {
				Symbol  string `json:"symbol"`
				PosSide string `json:"pos_side"`
				TpPrice string `json:"tp_price"`
				SlPrice string `json:"sl_price"`
				Reason  string `json:"reason"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(fmt.Sprintf("入参解析失败: %v", err)), nil
			}
			// 上下文先校验（DB 就能判），再校验参数，最后才打交易所。
			task, ctxErr := t.requireTaskContext(ctx)
			if ctxErr != nil {
				return actool.Errorf(ctxErr.Error()), nil
			}
			symbol := strings.TrimSpace(a.Symbol)
			posSide := strings.ToLower(strings.TrimSpace(a.PosSide))
			tp := strings.TrimSpace(a.TpPrice)
			sl := strings.TrimSpace(a.SlPrice)
			reason := strings.TrimSpace(a.Reason)
			if symbol == "" {
				return actool.Errorf("缺少 symbol（完整 instId，如 BTC-USDT-SWAP）"), nil
			}
			if posSide != "long" && posSide != "short" {
				return actool.Errorf(fmt.Sprintf("pos_side 必须是 long 或 short，收到 %q", a.PosSide)), nil
			}
			if tp == "" && sl == "" {
				return actool.Errorf("tp_price 与 sl_price 至少要给一个"), nil
			}
			if reason == "" {
				return actool.Errorf("缺少 reason —— 改止盈止损必须可审计"), nil
			}

			inst, iErr := resolveTradeInstrument(t.pg, symbol)
			if iErr != nil {
				return actool.Errorf(iErr.Error()), nil
			}
			symbol = inst.Symbol
			// 任务已绑标的时必须是同一个 —— 防止跨标的改保护。
			if task.InstrumentID != nil && *task.InstrumentID > 0 && inst.ID != *task.InstrumentID {
				return actool.Errorf(fmt.Sprintf(
					"标的 %q 与任务绑定的 instrument_id=%d 不一致", symbol, *task.InstrumentID)), nil
			}

			client, cErr := t.okxClient()
			if cErr != nil {
				return actool.Errorf(cErr.Error()), nil
			}

			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()

			// ---- ① 确认这个仓真的在，并拿现价 ----
			positions, err := client.GetPositions(cctx, "SWAP")
			if err != nil {
				return actool.Errorf(fmt.Sprintf("读持仓失败: %v", err)), nil
			}
			// 方向归一化统一走 exec.SideQtyOf（见 server/trade_positions.go 顶部注释：
			// 三处各写一份会导致 close_position 能平、这里却说没有该持仓）。
			acct, _ := t.defaultOkxAccount()
			acctID := int64(0)
			posMode := ""
			if acct != nil {
				acctID = acct.ID
				if prm, pErr := t.pg.GetExecAccountParams(acct.ID); pErr == nil {
					posMode = prm.PosMode
				}
			}
			snap := snapshotFromExchange(positions, symbol, posMode)
			hp, has := snap.side(posSide)
			if !has {
				return actool.Errorf(fmt.Sprintf(
					"交易所没有 %s 的 %s 持仓（可能已平）。不要凭草案或本地表推断持仓，先用 get_positions 核对",
					symbol, posSide)), nil
			}

			// 价格基准优先用标记价（交易所口径），缺失再拉最新价。
			px := strings.TrimSpace(hp.MarkPx)
			if px == "" {
				if p, pErr := client.LastPrice(cctx, symbol); pErr == nil {
					px = p
				}
			}
			if why := bracketSideInvalid(posSide, px, tp, sl); why != "" {
				return actool.Errorf(why), nil
			}

			// ---- ② 找现有保护单 ----
			algos, aErr := client.ListAlgoOrders(cctx, "SWAP", "oco")
			if aErr != nil {
				return actool.Errorf(fmt.Sprintf("读保护单失败: %v", aErr)), nil
			}
			// 精确匹配：long_short 模式下同标的有 long / short 两条仓、
			// 各挂各的 OCO，改错方向会让另一条仓裸奔且用户看不出原因。
			algo := pickAlgoFor(algos, symbol, posMode, posSide)

			audit := db.BracketAmendment{
				AccountID: acctID, InstID: symbol, PosSide: posSide,
				NewTp: tp, NewSl: sl, Operator: "agent", Reason: reason,
			}

			// ---- ③ 没有保护单 → 直接挂一组 ----
			if algo == nil {
				placed, pErr := t.placeBracket(cctx, client, acct, symbol, posSide, tp, sl)
				audit.Result = "placed"
				if pErr != nil {
					audit.Result, audit.Error = "failed", pErr.Error()
					_ = t.pg.InsertBracketAmendment(audit)
					return actool.Errorf(fmt.Sprintf("补挂止盈止损失败: %v", pErr)), nil
				}
				audit.AlgoID = placed
				_ = t.pg.InsertBracketAmendment(audit)
				return tradeJSON(map[string]any{
					"result": "placed", "symbol": symbol, "pos_side": posSide,
					"tp": tp, "sl": sl, "algo_id": placed,
					"note": "该仓原先没有保护单，已新挂一组",
				})
			}

			audit.AlgoID = algo.AlgoID
			audit.OldTp, audit.OldSl = algo.TpTriggerPx, algo.SlTriggerPx

			// ---- ④ 原子改价；永久失败才退到撤旧挂新 ----
			if amErr := client.AmendAlgoOCO(cctx, symbol, algo.AlgoID, tp, sl); amErr == nil {
				audit.Result = "amended"
				_ = t.pg.InsertBracketAmendment(audit)
				return tradeJSON(map[string]any{
					"result": "amended", "symbol": symbol, "pos_side": posSide,
					"old_tp": audit.OldTp, "old_sl": audit.OldSl, "tp": tp, "sl": sl,
					"algo_id": algo.AlgoID,
				})
			} else if amErr != nil {
				var apiErr *okxrest.APIError
				if !errors.As(amErr, &apiErr) || !errors.Is(amErr, okxrest.ErrPermanent) {
					// 瞬时故障：不动现有保护单。撤单再挂只会让仓在窗口期裸奔，
					// 而现有保护还是有效的（只是没改成新价）。
					audit.Result, audit.Error = "failed", amErr.Error()
					_ = t.pg.InsertBracketAmendment(audit)
					return actool.Errorf(fmt.Sprintf(
						"改价失败（瞬时故障，已保留原保护单 tp=%s sl=%s，可重试）: %v",
						audit.OldTp, audit.OldSl, amErr)), nil
				}
				slog.Warn("改价被永久拒绝，回退撤旧挂新", "inst", symbol, "err", amErr)
			}

			if cErr := client.CancelAlgo(cctx, symbol, algo.AlgoID); cErr != nil {
				audit.Result, audit.Error = "failed", cErr.Error()
				_ = t.pg.InsertBracketAmendment(audit)
				return actool.Errorf(fmt.Sprintf(
					"撤旧保护单失败（原保护仍在 tp=%s sl=%s）: %v", audit.OldTp, audit.OldSl, cErr)), nil
			}
			placed, pErr := t.placeBracket(cctx, client, acct, symbol, posSide, tp, sl)
			if pErr != nil {
				audit.Result, audit.Error = "failed", pErr.Error()
				_ = t.pg.InsertBracketAmendment(audit)
				return actool.Errorf(fmt.Sprintf(
					"**该仓现在没有保护单**（旧单已撤、新单挂失败）: %v —— 立刻用 get_positions 核对并重试",
					pErr)), nil
			}
			audit.Result, audit.AlgoID = "replaced", placed
			_ = t.pg.InsertBracketAmendment(audit)
			return tradeJSON(map[string]any{
				"result": "replaced", "symbol": symbol, "pos_side": posSide,
				"old_tp": audit.OldTp, "old_sl": audit.OldSl, "tp": tp, "sl": sl, "algo_id": placed,
				"note": "原子改价被拒，已撤旧单并挂新单",
			})
		},
	)
}

// placeBracket 给一个已有持仓补挂一组 OCO（closeFraction=1 即全平）。
func (t *TradeToolSet) placeBracket(ctx context.Context, client *okxrest.Client,
	acct *db.ExecAccount, instID, posSide, tp, sl string) (string, error) {
	if acct == nil {
		return "", fmt.Errorf("没有可用执行账户，无法确定保证金模式")
	}
	params, err := t.pg.GetExecAccountParams(acct.ID)
	if err != nil {
		return "", fmt.Errorf("读账户参数失败: %w", err)
	}
	side := "sell"
	if posSide == "short" {
		side = "buy"
	}
	return client.PlaceAlgoOCO(ctx, okxrest.AlgoRequest{
		InstID: instID, TdMode: params.MgnMode, Side: side,
		PosSide: exec.DerivePosSideForAlgo(params.PosMode, posSide),
		OrdType: "oco", CloseFraction: "1",
		TpTriggerPx: tp, TpOrdPx: "-1",
		SlTriggerPx: sl, SlOrdPx: "-1",
	})
}

// bracketSideInvalid 校验新的止盈止损相对现价是否成立，返回原因（空 = 成立）。
//
// # 为什么必须校验方向（而不是交给交易所）
//
// 「多单止损设在现价上方」在 OKX 侧会被接受并**立刻触发平仓** ——
// 请求合法、结果却是「一改就平」。这种误操作的代价是把一个还在盈利的
// 仓按亏损平掉，而错误信息什么都不会说。
func bracketSideInvalid(posSide, price, tp, sl string) string {
	if strings.TrimSpace(price) == "" {
		// 拿不到价就不做方向校验，但不能当成「通过」—— 调用方应知道这一点。
		return ""
	}
	px, ok := new(big.Rat).SetString(strings.TrimSpace(price))
	if !ok || px.Sign() <= 0 {
		return ""
	}
	cmp := func(s string) (*big.Rat, bool) {
		v, ok := new(big.Rat).SetString(strings.TrimSpace(s))
		return v, ok && v.Sign() > 0
	}
	if v, ok := cmp(tp); ok {
		if posSide == "long" && v.Cmp(px) <= 0 {
			return fmt.Sprintf("多单止盈 %s 必须高于现价 %s（否则会立刻触发，等于市价平仓）", tp, price)
		}
		if posSide == "short" && v.Cmp(px) >= 0 {
			return fmt.Sprintf("空单止盈 %s 必须低于现价 %s（否则会立刻触发）", tp, price)
		}
	}
	if v, ok := cmp(sl); ok {
		if posSide == "long" && v.Cmp(px) >= 0 {
			return fmt.Sprintf("多单止损 %s 必须低于现价 %s（否则会立刻触发，等于市价平仓）", sl, price)
		}
		if posSide == "short" && v.Cmp(px) <= 0 {
			return fmt.Sprintf("空单止损 %s 必须高于现价 %s（否则会立刻触发）", sl, price)
		}
	}
	return ""
}

// tradeReadTool 构造一个只读工具（可并发、无副作用）。
func tradeReadTool(name, desc string, schema map[string]any,
	run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision {
			return acperm.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// getPaperPositions 把纸面账本里的未平仓计划渲染成 get_positions 的形状。
//
// 形状与实盘一致是刻意的：模型对同一份字段做同一套判断，
// 不该因为模式不同而换一套读法（那会让 prompt 里的「当前持仓」段落
// 在两种模式下语义不同）。
func (t *TradeToolSet) getPaperPositions(ctx context.Context) (actool.Result, error) {
	plans, err := t.pg.ListOpenPaperTradePlans(0)
	if err != nil {
		return actool.Errorf(fmt.Sprintf("读纸面持仓失败: %v", err)), nil
	}
	type protection struct {
		AlgoID string `json:"algo_id"`
		Tp     string `json:"tp"`
		Sl     string `json:"sl"`
	}
	type row struct {
		InstID     string       `json:"inst_id"`
		PosSide    string       `json:"pos_side"`
		Pos        string       `json:"pos"`
		AvgPx      string       `json:"avg_px"`
		MarkPx     string       `json:"mark_px"`
		Upl        string       `json:"upl"`
		Lever      string       `json:"lever"`
		Protection []protection `json:"protection"`
	}
	out := make([]row, 0, len(plans))
	bare := make([]string, 0, 2)
	for _, p := range plans {
		r := row{InstID: p.Symbol, PosSide: p.PosSide, Pos: p.Qty, Protection: []protection{}}
		// 纸面没有交易所侧 OCO，用计划里的 TP/SL 表达「保护是否存在」。
		if p.TpPrice != "" || p.SlPrice != "" {
			r.Protection = append(r.Protection, protection{
				AlgoID: fmt.Sprintf("paper-plan-%d", p.ID), Tp: p.TpPrice, Sl: p.SlPrice,
			})
		} else {
			bare = append(bare, p.Symbol)
		}
		out = append(out, r)
	}
	payload := map[string]any{
		"positions": out, "count": len(out), "mode": "paper",
	}
	if len(bare) > 0 {
		payload["warning"] = "以下纸面仓没有止盈止损：" + strings.Join(bare, ", ") +
			" —— 用 modify_bracket 补挂，或到点调 close_position"
	}
	if len(out) == 0 {
		payload["note"] = "纸面账本当前无未平仓持仓（这是纸面账本返回空，不是读取失败）"
	} else {
		payload["note"] = "纸面持仓，不涉及真实资金；TP/SL 是记录值，不自动触发，需按判断调 close_position"
	}
	return tradeJSON(payload)
}
