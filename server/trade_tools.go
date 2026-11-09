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
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/exec"
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
}

// TradeToolSet 持有交易工具需要的依赖。
type TradeToolSet struct {
	pg   *db.DB
	gate *exec.Gate
}

// NewTradeToolSet 构造交易工具集。
//
// 任一依赖为 nil 时**仍返回可用对象**：工具名必须在目录里存在，
// 因为 agent 的 tools 表可能已绑定了这些名字，名字消失会让整轮规划失败。
// 调用时才报「未装配」的明确错误。
func NewTradeToolSet(pg *db.DB, gate *exec.Gate) *TradeToolSet {
	return &TradeToolSet{pg: pg, gate: gate}
}

// Tools 返回全部交易工具。
func (t *TradeToolSet) Tools() []actool.CoreTool {
	return []actool.CoreTool{
		t.createOrderDraft(),
		t.authorizeOrder(),
		t.executePaperOrder(),
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
		"创建一笔交易草案（不执行、不下单，仅生成待审核的订单意图）。\n"+
			"这是下单的第一步：草案必须先过 authorize_order 的闸门判定才能执行。\n"+
			"返回 draft_id，后续步骤都要用它。\n\n"+
			"参数：\n"+
			"  symbol: 标的代码。加密传 BTC；A股传 600519；美股传 AAPL\n"+
			"  market: crypto | cn | hk | us\n"+
			"  side:   buy | sell\n"+
			"  qty:    数量（字符串，保留精度）\n"+
			"  ref_price: 参考价（字符串，可选）\n\n"+
			"本工具只写草案，**不会**真正下单。",
		tradeObj(map[string]any{
			"symbol":    tradeStr("标的代码，如 BTC / 600519 / AAPL"),
			"market":    tradeStr("市场：crypto|cn|hk|us"),
			"side":      tradeStr("方向：buy|sell"),
			"qty":       tradeStr("数量（字符串）"),
			"ref_price": tradeStr("参考价（字符串，可空）"),
		}, "symbol", "market", "side", "qty"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			if err := t.guard(); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var a struct {
				Symbol   string `json:"symbol"`
				Market   string `json:"market"`
				Side     string `json:"side"`
				Qty      string `json:"qty"`
				RefPrice string `json:"ref_price"`
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

			inst, err := t.pg.Instruments().GetInstrumentByMarketSymbol(market, symbol)
			if err != nil {
				return actool.Errorf(fmt.Sprintf(
					"找不到标的 %q（market=%q）：%v\n"+
						"提示：该标的需先存在于 instruments 表。可先用 get_quote 确认代码正确。",
					symbol, market, err)), nil
			}

			draftID, err := t.pg.OrderDrafts().InsertDraft(db.OrderDraftInput{
				InstrumentID: &inst.ID,
				Side:         side,
				Qty:          qty,
				// RefPrice 是 string（空串 = 未指定，落成 NULL）。
				RefPrice: strings.TrimSpace(a.RefPrice),
			})
			if err != nil {
				return actool.Errorf(fmt.Sprintf("写草案失败: %v", err)), nil
			}

			return tradeJSON(map[string]any{
				"draft_id": draftID,
				"symbol":   inst.Symbol,
				"market":   inst.Market,
				"side":     side,
				"qty":      qty,
				"note":     "草案已创建，尚未执行。下一步调用 authorize_order 过闸门。",
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

// 确保 agent 包被引用（buildDomainReg 用 agent.ToolSet，这里只是编译期锚点）。
var _ = agent.NewToolSet
