package server

// 交易执行工具（2026-11-14 架构变更）。
//
// # 为什么从「后台循环」改成「同步工具」
//
// 旧设计是 TradeLoop 每 15 秒扫一遍草案表、机械执行。问题是**控制权不在 LLM**：
// 模型判断完行情、写完草案就结束了，之后能不能成交、什么时候成交、
// 仓位被套了该不该跑，全都由那个循环按固定规则决定。实测的后果：
//
//   - 平仓草案躺在表里永不执行（守卫把 exit 和 entry 一起挡了）
//   - 模型说「止损上移到 167」只能写进草案，等下一轮才可能生效
//   - 一轮行情走完了，模型还不知道自己有没有仓
//
// 现在的模型：LLM 拿到工具后**同步**完成「判断 → 下单 → 拿到回执」。
// 毫秒级的保护仍然挂在交易所（TP/SL 是 OCO 算法单，不依赖任何进程活着），
// 只有「主动决策」回到模型手里。
//
// # 为什么不再写 orders_draft / orders_execution
//
// 那两张表是循环的产物：草案要有 signal 才有 task 归属，执行单的
// draft_id 是 NOT NULL 外键。工具调用的本质是「一次动作」，
// 硬塞进草案模型只能造空壳。所以改用 trade_records（见 db/trade_records.go），
// 它把 task_id 直存，盈亏归属一跳可达。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// execToolNames 是「直接交易」工具名清单。
//
// 与 tradeToolNames 分开：那一组是草案模型（create_order_draft 等），
// 这一组是同步下单。两组共存的唯一理由是兼容旧任务 —— 新任务只需这一组。
var execToolNames = []string{
	"open_position",
	"close_position",
	"set_leverage",
}

// ExecToolSet 持有同步交易工具需要的依赖。
type ExecToolSet struct {
	pg   *db.DB
	gate *exec.Gate
	// creds 为 nil 时退回 clientOverride。
	creds func() (*db.OkxTradeCred, error)
	// clientOverride 是测试注入点：绕过真实凭据与网络。
	// 生产路径永远为 nil。
	clientOverride okxTrader
}

// NewExecToolSet 构造同步交易工具集。nil 依赖安全（调用时才报「未装配」）。
func NewExecToolSet(pg *db.DB, gate *exec.Gate, creds func() (*db.OkxTradeCred, error)) *ExecToolSet {
	return &ExecToolSet{pg: pg, gate: gate, creds: creds}
}

// Tools 返回全部同步交易工具。
func (t *ExecToolSet) Tools() []actool.CoreTool {
	return []actool.CoreTool{
		t.openPosition(),
		t.closePosition(),
		t.setLeverage(),
	}
}

// Has 判断某工具名是否属于本工具集。
func (t *ExecToolSet) Has(name string) bool {
	for _, n := range execToolNames {
		if n == name {
			return true
		}
	}
	return false
}

// okxClient / defaultOkxAccount 与 TradeToolSet 共用同一套实现。
func (t *ExecToolSet) okxClient() (okxTrader, error) {
	if t == nil {
		return nil, fmt.Errorf("交易工具未装配")
	}
	// ⚠️ 不能直接 `return buildOkxClient(...)`。
	//
	// buildOkxClient 返回 (*okxrest.Client, error)，出错时那个指针是 nil。
	// 把 typed-nil 装进接口，接口本身**不等于 nil** —— 于是调用方
	// `if ec.client != nil` 会通过，然后在 nil receiver 上调用方法 panic。
	// 实测 TestOpenPositionUpgradesSpotToPerp 就是这样崩在 okxrest/market.go:34。
	if t.clientOverride != nil {
		return t.clientOverride, nil
	}
	c, err := buildOkxClient(t.creds)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (t *ExecToolSet) defaultOkxAccount() (*db.ExecAccount, error) {
	if t == nil {
		return nil, fmt.Errorf("交易工具未装配")
	}
	return pickOkxAccount(t.pg)
}

// taskIDPtr 把任务 id 转成可空指针（0 = 不归属任何任务）。
func taskIDPtr(id int64) *int64 {
	if id <= 0 {
		return nil
	}
	return &id
}

func (t *ExecToolSet) ready() error {
	if t == nil || t.pg == nil {
		return fmt.Errorf("交易工具未装配数据库（server 启动时未初始化）")
	}
	if t.gate == nil {
		return fmt.Errorf("交易工具未装配闸门（server 启动时未初始化）")
	}
	if t.creds == nil {
		return fmt.Errorf("交易工具未装配凭据读取器（server 启动时未初始化）")
	}
	return nil
}

// ---------------------------------------------------------------------
// 公共前置：上下文、账户、标的、客户端
// ---------------------------------------------------------------------

// execCtx 是一次工具调用解析出的执行上下文。
type execCtx struct {
	taskID  int64
	task    *db.Task
	acct    *db.ExecAccount
	params  db.ExecAccountParams
	inst    *db.Instrument
	mode    db.ExecMode
	client  okxTrader
}

// prepareExec 把工具入参解析成可直接下单的上下文。
//
// 顺序刻意是「DB 能判的先判」：上下文 → 标的 → 账户 → 凭据。
// 早期版本把凭据放最前，导致「任务没绑标的」这种纯配置错误也要先打一次
// 交易所才发现（实测主 agent 连续 4 次失败后开始乱试别的工具）。
func (t *ExecToolSet) prepareExec(ctx context.Context, symbol string) (*execCtx, error) {
	if t == nil || t.pg == nil {
		return nil, fmt.Errorf("交易工具未装配数据库（server 启动时未初始化）")
	}
	if t.gate == nil {
		return nil, fmt.Errorf("交易工具未装配闸门（server 启动时未初始化）")
	}
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

	sym := strings.TrimSpace(symbol)
	if sym == "" {
		return nil, fmt.Errorf("缺少 symbol（完整 instId，如 BTC-USDT-SWAP）")
	}
	// 统一解析（精确 → 补 -USDT-SWAP → 补 -SWAP → 自动登记）。
	// 自动登记这一层是关键：instruments 表是选品阶段写的，而交易阶段
	// 可能对一个从未被分析过的永续下单 —— 那时表里没有行，
	// 按「找不到标的」拒绝等于「只能交易被分析过的那几个」。
	inst, instErr := resolveTradeInstrument(t.pg, sym)
	if instErr != nil {
		return nil, instErr
	}
	// 只做永续：现货持仓不在 positions 接口里，无法巡检也无法平仓。
	// 不拦住的话请求会以 tdMode=cross 打到 OKX，回来是看不出原因的 51000。
	if exec.InstTypeOf(inst.Symbol) != "SWAP" {
		return nil, fmt.Errorf("%q 不是永续合约（需 -SWAP 后缀）。本系统只做 OKX 永续："+
			"现货持仓在 OKX 里是余额不是 position，无法自动平仓与保护", inst.Symbol)
	}
	// 任务已绑标的时必须一致 —— 防跨标的操作。
	//
	// 但有一个例外必须放行并**自动改绑**：任务绑的是**同一资产的现货写法**，
	// 而本次要交易的是它的永续。
	//
	// # 为什么必须自愈（2026-11-14 任务 29）
	//
	// 选品阶段把 BTC-USDT（现货）绑成了任务标的，那条链从此不可交易。
	// 光修「新任务不再绑现货」救不了存量任务 —— 用户面对的是一个
	// 已经绑错、且「改标的是被禁止的」（set_task_instrument 的硬约束）。
	// 结果就是任务永远卡死，只能新建任务重跑整条链。
	//
	// 自愈的判据很窄：只有「去掉 -SWAP 后完全相同」才改绑。
	// BTC-USDT 与 BTC-USDT-SWAP 是同一资产，改绑不改变用户意图；
	// 而 BTC-USDT 与 ETH-USDT-SWAP 不满足这个条件，仍然拒绝。
	if task.InstrumentID != nil && *task.InstrumentID > 0 && inst.ID != *task.InstrumentID {
		bound, bErr := t.pg.Instruments().GetInstrumentByID(*task.InstrumentID)
		if bErr != nil || bound == nil || !sameUnderlying(bound.Symbol, inst.Symbol) {
			return nil, fmt.Errorf("标的 %q 与任务绑定的 instrument_id=%d 不一致", inst.Symbol, *task.InstrumentID)
		}
		if _, uErr := t.pg.Exec(`UPDATE tasks SET instrument_id=$1 WHERE id=$2`, inst.ID, task.ID); uErr != nil {
			return nil, fmt.Errorf("任务标的从 %s 改绑到 %s 失败: %v", bound.Symbol, inst.Symbol, uErr)
		}
		slog.Info("交易工具：任务标的从现货改绑为永续",
			"task", task.ID, "from", bound.Symbol, "to", inst.Symbol)
		task.InstrumentID = &inst.ID
	}

	acct, aErr := t.defaultOkxAccount()
	if aErr != nil {
		return nil, aErr
	}
	params, pErr := t.pg.GetExecAccountParams(acct.ID)
	if pErr != nil {
		return nil, fmt.Errorf("读账户参数失败: %v", pErr)
	}

	mode, mErr := t.pg.GetExecMode()
	if mErr != nil {
		return nil, fmt.Errorf("读执行模式失败: %v", mErr)
	}
	out := &execCtx{
		taskID: task.ID, task: task, acct: acct, params: params,
		inst: inst, mode: mode,
	}
	client, cErr := t.okxClient()
	if cErr != nil {
		return nil, cErr
	}
	out.client = client
	return out, nil
}

// newClOrdID 生成 OKX 可接受的幂等键。
//
// 约束（实测）：只允许字母数字，≤32 字符。带连字符会被 51000 拒。
// 用「时间戳纳秒 + 动作 + 标的」做种子而不是纯随机：同一次调用重试
// （网络超时后重发）应命中同一张单，而不是下第二张。
func newClOrdID(prefix, action, instID string) string {
	seed := fmt.Sprintf("%s:%s:%d", action, instID, time.Now().UnixNano())
	h := sha256.Sum256([]byte(seed))
	return prefix + hex.EncodeToString(h[:8])
}

// ---------------------------------------------------------------------
// open_position
// ---------------------------------------------------------------------

func (t *ExecToolSet) openPosition() actool.CoreTool {
	return tradeWriteTool(
		"open_position",
		"对目标标的**立即下单**（真实下单，同步返回交易所回执）。\n\n"+
			"这是唯一下单方式：没有「草案」中间态，调用即提交（市价单）。\n\n"+
			"**这一单的真实效果由现有持仓决定**，回执里的 position_effect 会告诉你：\n"+
			"  open     无仓，纯开新仓\n"+
			"  scale_in 同向已有仓，本次是**加仓**（均价被摊平，工具会自动重挂止盈止损）\n"+
			"  reduce   反向已有仓且数量不超过持仓，交易所实际做的是**减仓**\n"+
			"  reverse  反向已有仓且数量超过持仓，先平后反手（**不自动挂保护**）\n\n"+
			"参数：\n"+
			"  symbol:   完整 instId，如 BTC-USDT-SWAP（必须带 -SWAP）\n"+
			"  side:     buy（做多/减空）| sell（做空/减多）\n"+
			"  qty:      合约张数（字符串）。先调 get_positions / get_account_balance 算清楚\n"+
			"  tp_price: 止盈触发价。多单必须 > 现价，空单必须 < 现价\n"+
			"  sl_price: 止损触发价。多单必须 < 现价，空单必须 > 现价\n"+
			"            （open / scale_in / reduce 必填；reverse 可省略，因为方向刚反过来）\n"+
			"  leverage: 可选，设置该标的杠杆倍数（3 / 5 / 10）。不填沿用账户默认\n"+
			"  reason:   为什么做这一单（必填，写进交易台账）\n\n"+
			"回执里必须看两个字段：\n"+
			"  position_effect —— 这一单到底干了什么（见上）\n"+
			"  protection      —— protected 已挂保护 / naked **当前无保护**（必须立刻调 modify_bracket）\n"+
			"                     / double 两组单并存（危险）/ unknown 读不到算法单\n\n"+
			"本地会拒绝的情况：非永续标的、任务绑定标的不同、账户冻结、"+
			"TP/SL 相对现价方向不对（会立刻触发，等于市价平仓）。",
		tradeObj(map[string]any{
			"symbol":   tradeStr("完整 instId，如 BTC-USDT-SWAP"),
			"side":     tradeStr("buy（做多/减空）| sell（做空/减多）"),
			"qty":      tradeStr("合约张数（字符串）"),
			"tp_price": tradeStr("止盈触发价（reverse 时可省略）"),
			"sl_price": tradeStr("止损触发价（reverse 时可省略）"),
			"leverage": tradeStr("可选。杠杆倍数，如 3"),
			"reason":   tradeStr("为什么做这一单（必填，写进台账）"),
		}, "symbol", "side", "qty", "reason"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Symbol   string `json:"symbol"`
				Side     string `json:"side"`
				Qty      string `json:"qty"`
				TpPrice  string `json:"tp_price"`
				SlPrice  string `json:"sl_price"`
				Leverage string `json:"leverage"`
				Reason   string `json:"reason"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(fmt.Sprintf("入参解析失败: %v", err)), nil
			}
			side := strings.ToLower(strings.TrimSpace(a.Side))
			qty := strings.TrimSpace(a.Qty)
			tp := strings.TrimSpace(a.TpPrice)
			sl := strings.TrimSpace(a.SlPrice)
			reason := strings.TrimSpace(a.Reason)
			if side != "buy" && side != "sell" {
				return actool.Errorf(fmt.Sprintf(`side 必须是 "buy" 或 "sell"，收到 %q`, a.Side)), nil
			}
			if qty == "" {
				return actool.Errorf("缺少 qty（合约张数）"), nil
			}
			if reason == "" {
				return actool.Errorf("缺少 reason —— 开仓决策必须可审计"), nil
			}

			ec, err := t.prepareExec(ctx, a.Symbol)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}

			cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
			defer cancel()

			posSide := "long"
			if side == "sell" {
				posSide = "short"
			}
			oppSide := "short"
			if posSide == "short" {
				oppSide = "long"
			}

			// ---- 读现有持仓，判定这一单的真实效果 ----
			//
			// 读失败必须拒绝：没有持仓快照就无法知道要不要撤旧 OCO、
			// 会不会反手 —— 盲下单会留下两组保护单或按旧方向平掉新仓。
			snap, sErr := t.snapshotPositions(cctx, ec)
			if sErr != nil {
				return actool.Errorf(fmt.Sprintf(
					"%v —— 无法判定加仓/减仓/反手，拒绝盲下单。稍后重试", sErr)), nil
			}
			plan, cErr := exec.ClassifyEntry(ec.params.PosMode, side, qty,
				snap.qtyOf(posSide), snap.qtyOf(oppSide))
			if cErr != nil {
				return actool.Errorf(cErr.Error()), nil
			}

			// TP/SL 必填性按 effect 决定：反手方向刚翻转，模型可能还不知道
			// 新仓的合理价位，强制必填只会逼它编一个。
			if plan.Effect != exec.EffectReverse && (tp == "" || sl == "") {
				return actool.Errorf("开仓/加仓/减仓必须同时给 tp_price 与 sl_price —— " +
					"没有保护的仓不允许开（只有 reverse 可以省略）"), nil
			}

			// ---- 现价与方向校验 ----
			px := ""
			if ec.client != nil {
				if p, pErr := ec.client.LastPrice(cctx, ec.inst.Symbol); pErr == nil {
					px = p
				}
			}
			if px == "" {
				slog.Info("开仓：未取到现价，跳过 TP/SL 方向校验", "inst", ec.inst.Symbol)
			} else if tp != "" || sl != "" {
				if why := bracketSideInvalid(posSide, px, tp, sl); why != "" {
					return actool.Errorf(fmt.Sprintf("%s（开仓被本地拒绝，未提交交易所）", why)), nil
				}
			}

			// ---- 实盘：设杠杆 → settleBracket 按 effect 分派 ----
			lever := strings.TrimSpace(a.Leverage)
			if lever == "" {
				lever = ec.params.Lever
			}
			if lErr := ec.client.SetLeverage(cctx, ec.inst.Symbol, lever, ec.params.MgnMode); lErr != nil {
				// 杠杆失败不阻断：可能是已有仓位不允许改。按旧杠杆下单
				// 比完全不下单更接近用户意图。
				slog.Warn("开仓：设置杠杆失败（继续下单）",
					"inst", ec.inst.Symbol, "lever", lever, "err", lErr)
			}

			cid := newClOrdID("artexo", plan.Effect, ec.inst.Symbol)
			rec := db.TradeRecord{
				TaskID: ec.taskID, AccountID: ec.acct.ID, InstrumentID: ec.inst.ID,
				InstID: ec.inst.Symbol, Action: "open", Side: side, PosSide: posSide,
				Qty: qty, OrdType: "market", Leverage: lever, TpPrice: tp, SlPrice: sl,
				ClientOrderID: cid, State: db.TradeRecordSubmitted,
				Mode: string(ec.mode), Operator: "agent",
				PositionEffect: plan.Effect,
			}

			oc, oErr := t.settleBracket(cctx, ec.client, settleParam{
				InstID: ec.inst.Symbol, TdMode: ec.params.MgnMode,
				PosMode: ec.params.PosMode, PosSide: posSide,
				Side: side, Qty: qty, Tp: tp, Sl: sl, MarkPx: px,
				Effect: plan.Effect, ClOrdID: cid,
			})
			if oErr != nil {
				rec.State, rec.Error = db.TradeRecordFailed, oErr.Error()
				rid, iErr := t.pg.InsertTradeRecord(rec)
				if iErr == nil {
					_ = t.pg.UpdateTradeRecordResult(rid, "", db.TradeRecordFailed, oErr.Error())
				}
				return actool.Errorf(oErr.Error()), nil
			}

			rec.ChannelOrderID = oc.OrdID
			// 台账 state 必须反映**实际成交**，不能一律写 submitted。
			//
			// # 为什么（2026-11-15 对账事故）
			//
			// 原来这里固定写 submitted，而 settleBracket 早就拿到了
			// fill_state=filled。后果是台账永远显示「已提交」，
			// 事后核对「这单到底成交没有、成交多少」只能去交易所查 ——
			// 而账本存在的意义正是让人不用查交易所。
			//
			// 实测 rec=26（open buy 150 张）当时全量成交，台账却写 submitted，
			// 被误读成「0 成交」，进而怀疑整条链路。
			if oc.FillState == "filled" {
				rec.State = db.TradeRecordFilled
			} else if oc.FillState == "canceled" {
				rec.State = db.TradeRecordFailed
				rec.Error = "交易所撤销（未成交）"
			}
			if _, iErr := t.pg.InsertTradeRecord(rec); iErr != nil {
				// 台账写失败不回滚：钱已经动了。但必须让调用方知道，
				// 否则这一笔永远不会出现在盈亏报表里。
				return actool.Errorf(fmt.Sprintf(
					"**单已下出（ordId=%s）但台账写入失败**: %v —— 请人工记录，并检查 trade_records 表",
					oc.OrdID, iErr)), nil
			}

			slog.Info("工具下单成功", "inst", ec.inst.Symbol, "side", side, "qty", qty,
				"effect", plan.Effect, "protection", oc.Protection,
				"ordId", oc.OrdID, "task", ec.taskID)

			out := map[string]any{
				"result": "submitted", "symbol": ec.inst.Symbol, "side": side,
				"pos_side": posSide, "qty": qty, "ord_id": oc.OrdID,
				"client_order_id": cid, "leverage": lever, "tp": tp, "sl": sl,
				"mark_px_at_submit": px,
				"position_effect":   plan.Effect,
				"protection":        oc.Protection,
				"fill_state":        oc.FillState,
				"pre_position":      positionView(snap, posSide),
				"post_position":     positionViewAfter(oc.Post, plan, posSide),
			}
			if oc.AlgoID != "" {
				out["algo_id"] = oc.AlgoID
			}
			if oc.OldAlgoID != "" {
				out["old_algo_id"] = oc.OldAlgoID
			}
			note := "市价单已提交，止盈止损由交易所 OCO 保护（不依赖本进程）。" +
				"成交明细约 60 秒内出现在交易记录里"
			if oc.Note != "" {
				note = oc.Note
			}
			out["note"] = note
			if oc.naked() {
				out["warning"] = "**当前无保护**：该仓没有止盈止损，立刻用 modify_bracket 补挂"
			}
			if px == "" && (tp != "" || sl != "") {
				out["warning_price"] = "提交时未取到现价，TP/SL 方向未做本地校验 —— " +
					"若价格已越过，交易所会立即触发平仓"
			}
			return tradeJSON(out)
		},
	)
}

// positionView 把快照某方向渲染成给模型看的 {side,qty,avg_px}。
func positionView(s *positionSnapshot, side string) map[string]any {
	if hp, ok := s.side(side); ok {
		return map[string]any{"side": hp.Side, "qty": hp.Qty, "avg_px": hp.AvgPx}
	}
	return map[string]any{"side": "", "qty": "0", "avg_px": ""}
}

// positionViewAfter 优先用下单后重读的快照；拿不到时按 plan 的预计值。
//
// # 为什么不能照抄模型传的 qty
//
// reverse 时实际新仓量是 qty - heldOpp，reduce 时剩余量是 heldOpp - qty。
// 回执里若写 qty，模型会以为自己持有了 qty —— 之后所有仓位计算全错，
// 而且它没有任何办法自己发现。
func positionViewAfter(post *positionSnapshot, plan exec.EntryPlan, side string) map[string]any {
	if post != nil {
		if hp, ok := post.side(side); ok {
			return map[string]any{"side": hp.Side, "qty": hp.Qty, "avg_px": hp.AvgPx}
		}
	}
	return map[string]any{"side": side, "qty": plan.PostNet, "avg_px": "", "estimated": true}
}


// ---------------------------------------------------------------------
// close_position
// ---------------------------------------------------------------------

func (t *ExecToolSet) closePosition() actool.CoreTool {
	return tradeWriteTool(
		"close_position",
		"**立即平仓**（reduceOnly 市价单，同步返回回执）。\n\n"+
			"不传 qty 表示全平。平多必须 pos_side=long、平空 pos_side=short。\n"+
			"平仓前会读交易所真实持仓，数量超过持仓会被本地拒绝。\n\n"+
			"平仓会同时撤掉该仓的止盈止损（交易所侧 OCO 随仓消失）。",
		tradeObj(map[string]any{
			"symbol":   tradeStr("完整 instId，如 BTC-USDT-SWAP"),
			"pos_side": tradeStr("被平的持仓方向：long | short"),
			"qty":      tradeStr("可选。要平的数量，留空=全平"),
			"reason":   tradeStr("为什么平（必填，写进台账）"),
		}, "symbol", "pos_side", "reason"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Symbol  string `json:"symbol"`
				PosSide string `json:"pos_side"`
				Qty     string `json:"qty"`
				Reason  string `json:"reason"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(fmt.Sprintf("入参解析失败: %v", err)), nil
			}
			posSide := strings.ToLower(strings.TrimSpace(a.PosSide))
			if posSide != "long" && posSide != "short" {
				return actool.Errorf(fmt.Sprintf("pos_side 必须是 long 或 short，收到 %q", a.PosSide)), nil
			}
			if strings.TrimSpace(a.Reason) == "" {
				return actool.Errorf("缺少 reason —— 平仓决策必须可审计"), nil
			}

			ec, err := t.prepareExec(ctx, a.Symbol)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
			defer cancel()

			// ---- 实盘：用统一快照定数量 ----
			//
			// 数量取 availPos（可平量）而不是 pos：long_short 模式下 pos 是该方向
			// 的总持仓，其中可能有一部分被挂单占用，按 pos 下 reduceOnly 会被拒。
			snap, sErr := t.snapshotPositions(cctx, ec)
			if sErr != nil {
				return actool.Errorf(fmt.Sprintf("%v —— 不能凭猜测平仓", sErr)), nil
			}
			hp, has := snap.side(posSide)
			if !has {
				return actool.Errorf(fmt.Sprintf(
					"交易所没有 %s 的 %s 持仓（可能已被止盈止损平掉）。先用 get_positions 核对",
					ec.inst.Symbol, posSide)), nil
			}
			held := hp.Avail
			if held == "" {
				held = hp.Qty
			}
			qty := strings.TrimSpace(a.Qty)
			if qty == "" {
				qty = held
			} else if !lessOrEqualDecimal(qty, held) {
				if held != hp.Qty {
					return actool.Errorf(fmt.Sprintf(
						"平仓量 %s 超过可平量 %s（该方向总持仓 %s，其中部分被挂单占用；"+
							"可先撤掉挂单或按可平量下单）", qty, held, hp.Qty)), nil
				}
				return actool.Errorf(fmt.Sprintf("平仓量 %s 超过真实持仓 %s", qty, held)), nil
			}

			side := "sell"
			if posSide == "short" {
				side = "buy"
			}
			cid := newClOrdID("artexc", "close", ec.inst.Symbol)
			req := okxrest.OrderRequest{
				InstID: ec.inst.Symbol, TdMode: ec.params.MgnMode, Side: side,
				PosSide: exec.DerivePosSide(ec.params.PosMode, side, true, posSide),
				OrdType: "market", Sz: qty, ClOrdID: cid, ReduceOnly: true,
			}
			rec := db.TradeRecord{
				TaskID: ec.taskID, AccountID: ec.acct.ID, InstrumentID: ec.inst.ID,
				InstID: ec.inst.Symbol, Action: "close", Side: side, PosSide: posSide,
				Qty: qty, OrdType: "market", ReduceOnly: true,
				ClientOrderID: cid, State: db.TradeRecordSubmitted,
				Mode: string(ec.mode), Operator: "agent",
			}
			ack, oErr := ec.client.PlaceOrder(cctx, req)
			if oErr != nil && exec.IsDuplicateErr(oErr) {
				if st, qErr := ec.client.QueryOrderByClOrdID(cctx, ec.inst.Symbol, cid); qErr == nil && st != nil {
					ack = &okxrest.OrderAck{OrdID: st.OrdID, ClOrdID: st.ClOrdID}
					oErr = nil
				}
			}
			if oErr != nil {
				rec.State, rec.Error = db.TradeRecordFailed, oErr.Error()
				rid, _ := t.pg.InsertTradeRecord(rec)
				_ = t.pg.UpdateTradeRecordResult(rid, "", db.TradeRecordFailed, oErr.Error())
				return actool.Errorf(fmt.Sprintf("平仓下单失败: %v", oErr)), nil
			}
			rec.ChannelOrderID = ack.OrdID
			// 与开仓同理：台账要反映实际成交，不能一律 submitted。
			// 平仓单成不成交直接决定「还剩多少仓」，猜错会让模型的
			// 下一次决策建立在错误的敞口上。
			switch st, _, _ := awaitFill(cctx, ec.client, ec.inst.Symbol, cid); st {
			case "filled":
				rec.State = db.TradeRecordFilled
			case "canceled":
				rec.State, rec.Error = db.TradeRecordFailed, "交易所撤销（未成交）"
			}
			if _, iErr := t.pg.InsertTradeRecord(rec); iErr != nil {
				return actool.Errorf(fmt.Sprintf(
					"**平仓单已下出（ordId=%s）但台账写入失败**: %v —— 请人工记录",
					ack.OrdID, iErr)), nil
			}
			slog.Info("工具平仓成功", "inst", ec.inst.Symbol, "posSide", posSide,
				"qty", qty, "ordId", ack.OrdID, "task", ec.taskID)
			return tradeJSON(map[string]any{
				"result": "submitted", "symbol": ec.inst.Symbol, "pos_side": posSide,
				"side": side, "qty": qty, "ord_id": ack.OrdID, "client_order_id": cid,
				"note": "reduceOnly 市价平仓单已提交；该仓的交易所侧止盈止损随仓消失",
			})
		},
	)
}


// ---------------------------------------------------------------------
// set_leverage
// ---------------------------------------------------------------------

func (t *ExecToolSet) setLeverage() actool.CoreTool {
	return tradeWriteTool(
		"set_leverage",
		"设置某个标的的杠杆倍数（账户级设置，对该标的所有仓位生效）。\n\n"+
			"杠杆只影响保证金占用与强平价，**不改变仓位数量**。\n"+
			"已有仓位时交易所可能拒绝降杠杆（低于维持保证金要求）—— 那时会返回明确错误。\n\n"+
			"纸面模式不做任何事（纸面账本不计保证金），直接返回成功。",
		tradeObj(map[string]any{
			"symbol":   tradeStr("完整 instId，如 BTC-USDT-SWAP"),
			"leverage": tradeStr("杠杆倍数，如 3 / 5 / 10"),
			"reason":   tradeStr("为什么调杠杆（必填，写进台账）"),
		}, "symbol", "leverage", "reason"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Symbol   string `json:"symbol"`
				Leverage string `json:"leverage"`
				Reason   string `json:"reason"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(fmt.Sprintf("入参解析失败: %v", err)), nil
			}
			lever := strings.TrimSpace(a.Leverage)
			if lever == "" {
				return actool.Errorf("缺少 leverage"), nil
			}
			if strings.TrimSpace(a.Reason) == "" {
				return actool.Errorf("缺少 reason —— 调杠杆必须可审计"), nil
			}

			ec, err := t.prepareExec(ctx, a.Symbol)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}

			rec := db.TradeRecord{
				TaskID: ec.taskID, AccountID: ec.acct.ID, InstrumentID: ec.inst.ID,
				InstID: ec.inst.Symbol, Action: "set_leverage", Leverage: lever,
				State: db.TradeRecordFilled, Mode: string(ec.mode), Operator: "agent",
			}
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			if lErr := ec.client.SetLeverage(cctx, ec.inst.Symbol, lever, ec.params.MgnMode); lErr != nil {
				rec.State, rec.Error = db.TradeRecordFailed, lErr.Error()
				rid, _ := t.pg.InsertTradeRecord(rec)
				_ = t.pg.UpdateTradeRecordResult(rid, "", db.TradeRecordFailed, lErr.Error())
				return actool.Errorf(fmt.Sprintf("设置杠杆失败: %v", lErr)), nil
			}
			_, _ = t.pg.InsertTradeRecord(rec)
			return tradeJSON(map[string]any{
				"result": "ok", "symbol": ec.inst.Symbol, "leverage": lever,
				"mgn_mode": ec.params.MgnMode,
			})
		},
	)
}

// 编译期锚点：setLeverage 的 Run 用不到 acperm，但 tradeWriteTool 依赖它。
var _ = acperm.Allowed

// sameUnderlying 判断两个 instId 是否指同一资产（忽略 -SWAP 后缀）。
//
// 用于「任务绑了现货、要交易对应永续」的自愈判据。
// 只有完全相同才算 —— 宁可少自愈，也不要因为名字相近把标的改错。
func sameUnderlying(a, b string) bool {
	base := func(s string) string {
		s = strings.ToUpper(strings.TrimSpace(s))
		// 取第一段：NVDA-USDT-SWAP → NVDA，BTC-USDT → BTC，600519 → 600519。
		//
		// # 为什么按第一段比而不是只剥 -SWAP（2026-11-14 任务 30）
		//
		// 任务是 "NVDA"（market=us 的裸股票代码），要交易的是
		// "NVDA-USDT-SWAP"。只剥 -SWAP 得到 "NVDA" vs "NVDA-USDT" ——
		// 判为不同资产，自愈失效，任务永久卡死。而它们显然是同一资产
		// （OKX 的 NVDA 永续就是以 NVDA 为标的）。
		//
		// 按第一段比足够窄：BTC-USDT 与 ETH-USDT-SWAP 得到 BTC / ETH，
		// 仍然判不同。宁可漏掉少数合法情况，也不能把标的改错。
		if i := strings.Index(s, "-"); i > 0 {
			return s[:i]
		}
		return s
	}
	na, nb := base(a), base(b)
	return na != "" && na == nb
}

// mulDecimal 两个十进制字符串相乘，保持精度（不走 float64）。
//
// 名义金额要拿去和权益比大小，float64 的尾数误差足以让
// 「刚好等于上限」的单被误判成超限或漏放。
func mulDecimal(a, b string) string {
	x, ok1 := new(big.Rat).SetString(strings.TrimSpace(a))
	y, ok2 := new(big.Rat).SetString(strings.TrimSpace(b))
	if !ok1 || !ok2 {
		return "0"
	}
	return decimalString(new(big.Rat).Mul(x, y))
}
