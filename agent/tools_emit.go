package agent

// W5 的四个写库工具：emit_fact / emit_thesis / emit_signal / emit_rating。
//
// # 为什么必须先有这四个工具
//
// PROMPTS-W5.md 的 13 个 prompt 全部要求输出结构化 findings（claim / evidence /
// snapshot_ids），但 ARTEX 里原本一个 emit 工具都没有。没有它们，执行器产出的
// 是一份没有被任何人读的 JSON —— prompt 规格写得再好也落不了地。
//
// # role 的来源（关键设计）
//
// ctx 里**没有** role：RunInfo 只有 TaskID/ExplorationID/IntentID/SessionID。
// 唯一可信来源是 intent 节点的 payload.role（worker 跑某条 intent 时系统写入的）。
//
// 因此分两类：
//   - emit_signal / emit_rating：**只认 DB 查出的 role**。IntentID == 0 直接拒绝。
//     理由：这两个决定「要不要下单」，若允许入参自报 role，planner 传个
//     role:"trader" 就能发信号 —— 那是提权。
//   - emit_fact / emit_thesis：允许入参 role 作为 fallback（planner/mainagent
//     的 IntentID 为 0，但它们也需要记事实/论点），并在 payload 里记录
//     role_source（"db" | "param"）以便审计区分可信度。
//
// # 所有写入都过 redact.MaskJSON
//
// evidence / rationale / confidence_reason 可能夹带 API key（模型有时会把原始
// 响应整段贴进来）。redact 包已就绪，直接用。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	actool "github.com/Autumn-27/norma/tool"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/redact"
)

// emitFactAllowedRoles 允许写 fact 的角色。
//
// 为什么需要白名单：加白名单前 emit_fact 是「谁都能调」——trader 也能写「事实」，
// 角色边界形同虚设。fact 是「观察到的事实」，只有做观察的角色能写。
//
// 注意 planner / mainagent 不在表内：它们的调用走 fallback（无 intent 上下文），
// 而本表只在**有 intent 上下文**时校验（见 checkRoleAllowed）。
var emitFactAllowedRoles = map[string]bool{
	"analyst_market":           true,
	"analyst_news":             true,
	"analyst_sentiment":        true,
	"analyst_fundamental":      true,
	"analyst_sentiment_crypto": true, // crypto 专有的情绪分析师（PROMPTS-W5 §4）
	"research_manager":         true, // 研究经理汇总研究结论
	"planner":                  true, // 规划阶段记录观察（如「标的已退市」）
	"mainagent":                true,
	// screener 是「选品员」，只在任务未绑标的时出现（pipeline.go 阶段 0）。
	//
	// # 为什么必须加（2026-11-05 实测）
	//
	// 它的核心产出就是候选清单（一条 fact）。漏加会导致：
	//
	//	emit_fact 不允许角色 "screener" 调用
	//	→ 选品员算完了 AAPL 349.51 / RSI14=57.8，却写不进去
	//	→ 它开始试 mainagent / 省略 role 等各种绕过
	//	→ 白烧十几轮，候选清单丢失
	//
	// 它写的是「观察到的事实」（价格、动量、波动率），
	// 与分析师写 fact 的性质相同，不是主张（thesis）。所以进 fact 白名单。
	"screener": true,
}

// emitThesisAllowedRoles 允许写 thesis 的角色。
//
// thesis 是「可证伪的主张」，只有辩论、研究经理、风控能写 ——
// 分析师只陈述事实不下论点。
//
// research_manager 也在表内：它的输出是 `conclusion: bull_wins|bear_wins`，
// 那是对辩论的**裁决** —— 是可证伪的主张（若后续走势相反，裁决就错了），
// 不是客观事实（「RSI=72.3」那种）。所以用 emit_thesis 而不是 emit_fact。
var emitThesisAllowedRoles = map[string]bool{
	"debate_bull":       true,
	"debate_bear":       true,
	"research_manager":  true, // 裁决是特殊论点，不是事实
	"risk_aggressive":   true,
	"risk_conservative": true,
	"risk_neutral":      true,
}

// checkRoleAllowed 校验 role 是否在允许集合内。
//
// **只在 role 来自 DB（intent 上下文）时校验**：来自入参 fallback 的 role 是
// 模型自报的（role_source=param），拿它做权限判断等于让模型自己给自己授权 ——
// 那比不校验更糟（会给人「有校验」的错觉）。
//
// 所以 fallback 路径不校验白名单，但 payload 里会记 role_source=param，
// 审计时能看出这条记录的角色未经系统确认。
func checkRoleAllowed(tool string, role string, source string, allowed map[string]bool) error {
	if source != "db" {
		// 自报 role 不参与权限判断（见上）。
		return nil
	}
	if !allowed[role] {
		return fmt.Errorf("%s 不允许角色 %q 调用（允许：%s）",
			tool, role, allowedRoleList(allowed))
	}
	return nil
}

// allowedRoleList 把白名单排成稳定的可读列表（错误信息用）。
func allowedRoleList(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, " / ")
}

// 允许的 action（与 db 的 ck_signal_action 约束一致）。
//
// action 与 signal_type 是**正交维度**：
//   - signal_type（自由文本）：信号来自哪个分析维度（technical/fundamental/news/...）
//   - action（枚举）：准备执行什么动作（entry/exit/...）—— 它驱动执行流程，必须受限
//
// 压成一个字段就无法表达「技术面信号建议离场」。
var validSignalActions = map[string]bool{
	"entry": true, "exit": true, "scale_in": true, "scale_out": true,
	"hold": true, "review": true,
}

// 允许的 direction（与 db 的 direction CHECK 一致）。
var validSignalDirections = map[string]bool{
	"long": true, "short": true, "neutral": true,
}

// 允许的 rating（与 db 的 rating CHECK 一致）。
var validRatingValues = map[string]bool{
	"buy": true, "overweight": true, "hold": true,
	"underweight": true, "sell": true, "review": true,
}

// intentRoleFromCtx 用 RunInfo.IntentID 查 intent 节点的 payload.role。
//
// 返回 ("", 0, nil) 表示「没有 intent 上下文」——调用方据此决定是否允许 fallback。
// 返回 error 表示「有 intent 但查询失败」，这类错误必须上抛，不能当「没 role」处理
// （否则 DB 抖动会让 role 校验静默失效）。
func intentRoleFromCtx(ctx context.Context, ts *db.ExplorationStore) (role string, intentID int64, err error) {
	ri := RunInfoFrom(ctx)
	if ri.IntentID == 0 {
		return "", 0, nil
	}
	if ts == nil {
		return "", ri.IntentID, fmt.Errorf("有 intent 上下文(%d)但 ExplorationStore 未初始化，无法校验 role", ri.IntentID)
	}
	nodes, err := ts.NodesByIDs([]int64{ri.IntentID})
	if err != nil {
		return "", ri.IntentID, fmt.Errorf("查询 intent %d 失败: %w", ri.IntentID, err)
	}
	if len(nodes) == 0 {
		return "", ri.IntentID, fmt.Errorf("intent %d 不存在", ri.IntentID)
	}
	var p struct {
		Role string `json:"role"`
	}
	if len(nodes[0].Payload) > 0 {
		// payload 解析失败不算致命：老数据可能没有 role 字段。
		_ = json.Unmarshal(nodes[0].Payload, &p)
	}
	return strings.TrimSpace(p.Role), ri.IntentID, nil
}

// resolveRoleForWrite 为 emit_fact / emit_thesis 解析 role（允许 fallback）。
func resolveRoleForWrite(ctx context.Context, ts *db.ExplorationStore, paramRole string) (role, source string, err error) {
	dbRole, _, err := intentRoleFromCtx(ctx, ts)
	if err != nil {
		return "", "", err
	}
	if dbRole != "" {
		return dbRole, "db", nil
	}
	p := strings.TrimSpace(paramRole)
	if p == "" {
		return "", "", errors.New("无法确定 role：没有 intent 上下文且未提供 role 入参")
	}
	return p, "param", nil
}

// requireDBRole 为 emit_signal / emit_rating 解析 role（**不允许 fallback**）。
func requireDBRole(ctx context.Context, ts *db.ExplorationStore) (role string, err error) {
	role, intentID, err := intentRoleFromCtx(ctx, ts)
	if err != nil {
		return "", err
	}
	if intentID == 0 {
		return "", errors.New("该工具只能在 intent(worker)上下文内调用：缺少 intent 上下文，无法校验 role")
	}
	if role == "" {
		return "", fmt.Errorf("intent %d 的 payload 里没有 role 字段", intentID)
	}
	return role, nil
}

// maskString 过 redact，防止证据里夹带密钥。
func maskString(s string) string {
	if s == "" {
		return s
	}
	return string(redact.MaskJSON([]byte(s)))
}

// maskAny 把任意 payload 序列化后正则脱敏，再解回。
// 失败时返回原值（脱敏不该阻断写入）。
func maskAny(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	masked := redact.MaskJSON(b)
	var out any
	if err := json.Unmarshal(masked, &out); err != nil {
		return v
	}
	return out
}

// ---------------------------------------------------------------------
// emit_fact
// ---------------------------------------------------------------------

func (t *ToolSet) emitFact() actool.CoreTool {
	return t.writeExpTool(
		"emit_fact",
		"登记一条**已核实的事实**（fact）。写入 exploration_nodes(kind='fact', state='confirmed')。\n"+
			"claim 与 evidence 都必填：evidence 必须能在你看到的输入里找到，不能是推测。\n"+
			"snapshot_ids 是证据回溯的唯一通道 —— 每个 id 必须真实存在于快照表，悬空 id 会被拒绝。\n"+
			"confitence 用 0..1；不确定时给低值而不是省略。\n"+
			"若你不在 intent 上下文内（planner/mainagent），可传 role 显式声明身份，"+
			"系统会在 payload 记 role_source=param 以便审计区分。",
		obj(map[string]any{
			"claim":        str("必填。一句话结论"),
			"evidence":     str("必填。支撑该结论的证据，必须能在输入中找到"),
			"snapshot_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "证据来源的快照 id 列表（每个都必须存在）"},
			"confidence":   map[string]any{"type": "number", "description": "0..1 的置信度"},
			"role":         str("可选。不在 intent 上下文时显式声明角色，如 analyst_market"),
		}, "claim", "evidence"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Claim       string  `json:"claim"`
				Evidence    string  `json:"evidence"`
				SnapshotIDs []int64 `json:"snapshot_ids"`
				Confidence  float64 `json:"confidence"`
				Role        string  `json:"role"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(fmt.Sprintf("入参解析失败: %v", err)), nil
			}
			claim := strings.TrimSpace(a.Claim)
			evidence := strings.TrimSpace(a.Evidence)
			if claim == "" {
				return actool.Errorf("claim 不能为空"), nil
			}
			if evidence == "" {
				return actool.Errorf("evidence 不能为空（必须能在输入中找到支撑）"), nil
			}
			if a.Confidence < 0 || a.Confidence > 1 {
				return actool.Errorf(fmt.Sprintf("confidence 必须在 0..1，实际 %v", a.Confidence)), nil
			}

			// snapshot_ids 必须真实存在 —— 悬空 id 等于放弃可审计性。
			if len(a.SnapshotIDs) > 0 {
				store := t.ts.DB()
				if store == nil {
					return actool.Errorf("无法校验 snapshot_ids：DB 未初始化"), nil
				}
				existing, err := store.Snapshots().ExistingIDs(a.SnapshotIDs)
				if err != nil {
					return actool.Errorf(fmt.Sprintf("校验 snapshot_ids 失败: %v", err)), nil
				}
				var missing []int64
				for _, id := range a.SnapshotIDs {
					if !existing[id] {
						missing = append(missing, id)
					}
				}
				if len(missing) > 0 {
					return actool.Errorf(fmt.Sprintf(
						"snapshot_ids 中有 %d 个不存在: %v —— 证据必须可回溯到真实快照",
						len(missing), missing)), nil
				}
			}

			role, roleSource, err := resolveRoleForWrite(ctx, t.ts, a.Role)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if err := checkRoleAllowed("emit_fact", role, roleSource, emitFactAllowedRoles); err != nil {
				return actool.Errorf(err.Error()), nil
			}

			payload := map[string]any{
				"claim":       maskString(claim),
				"evidence":    maskString(evidence),
				"confidence":  a.Confidence,
				"role":        role,
				"role_source": roleSource, // db=系统写入可信；param=模型自报，审计时注意
			}
			if len(a.SnapshotIDs) > 0 {
				payload["snapshot_ids"] = a.SnapshotIDs
			} else {
				// 无快照证据链。允许存在（盘中看现价产 fact 的场景），
				// 但必须留痕：否则「无证据链的事实」与「有完整证据链的事实」
				// 在库里长得一样，审计时无法区分。
				//
				// 字段名刻意不用 data_gaps：那是 prompt 规格里「输入维度不可用」
				// 的数组，语义不同、形态不同（这里是单值原因码）。混用会让下游解析出错。
				payload["snapshot_absence_reason"] = "no_snapshot_captured"
			}

			// state='confirmed'：分析师产出的事实视为已确认（辩论阶段可引用）。
			// 注意不能用 "pending" —— ck_node_state 对 fact 只允许 confirmed/dismissed/origin。
			id, err := t.ts.AddNode(db.KindFact, payload, 5, "confirmed", "worker", nil)
			if err != nil {
				return actool.Errorf(fmt.Sprintf("写入 fact 失败: %v", err)), nil
			}
			return jsonResult(map[string]any{"node_id": id, "kind": db.KindFact, "role": role})
		},
	)
}

// ---------------------------------------------------------------------
// emit_thesis
// ---------------------------------------------------------------------

func (t *ToolSet) emitThesis() actool.CoreTool {
	return t.writeExpTool(
		"emit_thesis",
		"登记一条**论点**（thesis），用于多空辩论与风控讨论。\n"+
			"invalid_if（失效条件）**必填** —— 没有失效条件的论点不可证伪，也就不可评估。\n"+
			"supports 指向 fact 节点 id；refutes 指向 thesis 节点 id。类型不匹配会被拒绝。\n"+
			"round 表示辩论轮次（从 1 开始）。",
		obj(map[string]any{
			"thesis":     str("必填。论点陈述"),
			"supports":   map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "支撑本论点的 fact 节点 id"},
			"refutes":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "本论点反驳的 thesis 节点 id"},
			"invalid_if": str("必填。什么情况下本论点失效（可证伪条件）"),
			"round":      map[string]any{"type": "integer", "description": "辩论轮次，从 1 开始"},
			"role":       str("可选。不在 intent 上下文时显式声明角色，如 debate_bull"),
		}, "thesis", "invalid_if"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Thesis    string  `json:"thesis"`
				Supports  []int64 `json:"supports"`
				Refutes   []int64 `json:"refutes"`
				InvalidIf string  `json:"invalid_if"`
				Round     int     `json:"round"`
				Role      string  `json:"role"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(fmt.Sprintf("入参解析失败: %v", err)), nil
			}
			thesis := strings.TrimSpace(a.Thesis)
			invalidIf := strings.TrimSpace(a.InvalidIf)
			if thesis == "" {
				return actool.Errorf("thesis 不能为空"), nil
			}
			if invalidIf == "" {
				// 硬约束：辩论的 argument_quality.falsifiability 打分依赖它。
				return actool.Errorf("invalid_if 不能为空 —— 没有失效条件的论点不可证伪，无法评估"), nil
			}

			// 引用类型必须匹配：supports→fact、refutes→thesis。
			// 不校验会让图谱串（下游按 kind 分类读节点时会读到错的东西）。
			if len(a.Supports) > 0 {
				if err := t.assertNodeKinds(a.Supports, db.KindFact, "supports"); err != nil {
					return actool.Errorf(err.Error()), nil
				}
			}
			if len(a.Refutes) > 0 {
				if err := t.assertNodeKinds(a.Refutes, KindThesisStr, "refutes"); err != nil {
					return actool.Errorf(err.Error()), nil
				}
			}

			role, roleSource, err := resolveRoleForWrite(ctx, t.ts, a.Role)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if err := checkRoleAllowed("emit_thesis", role, roleSource, emitThesisAllowedRoles); err != nil {
				return actool.Errorf(err.Error()), nil
			}

			payload := map[string]any{
				"thesis":      maskString(thesis),
				"invalid_if":  maskString(invalidIf),
				"role":        role,
				"role_source": roleSource,
			}
			if len(a.Supports) > 0 {
				payload["supports"] = a.Supports
			}
			if len(a.Refutes) > 0 {
				payload["refutes"] = a.Refutes
			}
			if a.Round > 0 {
				payload["round"] = a.Round
			}

			// state='open'：刚写入待评（ck_node_state 允许 open/standing/refuted/superseded）
			id, err := t.ts.AddNode(KindThesisStr, payload, 10, "open", "worker", nil)
			if err != nil {
				return actool.Errorf(fmt.Sprintf("写入 thesis 失败: %v", err)), nil
			}
			return jsonResult(map[string]any{"node_id": id, "kind": KindThesisStr, "role": role})
		},
	)
}

// assertNodeKinds 校验 ids 指向的节点都存在且 kind 匹配。
func (t *ToolSet) assertNodeKinds(ids []int64, wantKind, field string) error {
	nodes, err := t.ts.NodesByIDs(ids)
	if err != nil {
		return fmt.Errorf("校验 %s 失败: %v", field, err)
	}
	byID := make(map[int64]*db.Node, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
	}
	for _, id := range ids {
		n, ok := byID[id]
		if !ok {
			return fmt.Errorf("%s 引用的节点 %d 不存在", field, id)
		}
		if n.Kind != wantKind {
			return fmt.Errorf("%s 引用的节点 %d 是 %q，但要求 %q", field, id, n.Kind, wantKind)
		}
	}
	return nil
}

// ---------------------------------------------------------------------
// emit_signal
// ---------------------------------------------------------------------

func (t *ToolSet) emitSignal() actool.CoreTool {
	return t.writeExpTool(
		"emit_signal",
		"登记一个**待执行信号**。**仅 trader 角色可调用**（role 从 intent 上下文读取，不接受自报）。\n"+
			"signal_type: entry(建仓) / exit(平仓) / scale_in(加仓) / scale_out(减仓) / hold(持有) / review(待复核)。\n"+
			"direction: long / short / neutral。\n"+
			"horizon: intraday(日内) / swing(波段) / position(长持)。\n"+
			"rationale 写清「为什么现在发这个信号」，会用于事后归因。",
		obj(map[string]any{
			"instrument_id": map[string]any{"type": "integer", "description": "标的 id"},
			"signal_type":   str("必填。信号来源分类，自由文本（technical / fundamental / news / macro / flow 等）"),
			"action":        map[string]any{"type": "string", "enum": []string{"entry", "exit", "scale_in", "scale_out", "hold", "review"}, "description": "必填。准备执行的动作"},
			"direction":     map[string]any{"type": "string", "enum": []string{"long", "short", "neutral"}},
			"confidence":    map[string]any{"type": "number", "description": "0..1 的置信度"},
			"horizon":       map[string]any{"type": "string", "enum": []string{"intraday", "swing", "position"}},
			"rationale":     str("必填。为什么发这个信号"),
		}, "instrument_id", "signal_type", "action", "direction", "rationale"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				InstrumentID int64   `json:"instrument_id"`
				SignalType   string  `json:"signal_type"`
				Action       string  `json:"action"`
				Direction    string  `json:"direction"`
				Confidence   float64 `json:"confidence"`
				Horizon      string  `json:"horizon"`
				Rationale    string  `json:"rationale"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(fmt.Sprintf("入参解析失败: %v", err)), nil
			}

			// role 守卫：只认 DB，不接受自报（防提权）。
			role, err := requireDBRole(ctx, t.ts)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if role != "trader" {
				return actool.Errorf(fmt.Sprintf(
					"emit_signal 仅允许 trader 角色调用（当前 role=%q）—— 信号是「准备行动」的声明，分析师不该发", role)), nil
			}

			// signal_type 只要求非空，不限制具体值 —— 它是分类标签，
			// 未来会加 sentiment/macro/flow，限制枚举会让每次加维度都要改 schema。
			st := strings.TrimSpace(a.SignalType)
			if st == "" {
				return actool.Errorf("signal_type 不能为空（如 technical / fundamental / news）"), nil
			}
			// action 必须受限 —— 它驱动执行流程。
			act := strings.TrimSpace(a.Action)
			if !validSignalActions[act] {
				return actool.Errorf(fmt.Sprintf(
					"action %q 非法（entry|exit|scale_in|scale_out|hold|review）", a.Action)), nil
			}
			dir := strings.TrimSpace(a.Direction)
			if !validSignalDirections[dir] {
				return actool.Errorf(fmt.Sprintf("direction %q 非法（long|short|neutral）", a.Direction)), nil
			}
			if a.InstrumentID <= 0 {
				return actool.Errorf("instrument_id 必须为正"), nil
			}
			if a.Confidence < 0 || a.Confidence > 1 {
				return actool.Errorf(fmt.Sprintf("confidence 必须在 0..1，实际 %v", a.Confidence)), nil
			}
			rationale := strings.TrimSpace(a.Rationale)
			if rationale == "" {
				return actool.Errorf("rationale 不能为空"), nil
			}

			ri := RunInfoFrom(ctx)
			instID := a.InstrumentID
			var taskID *int64
			if ri.TaskID > 0 {
				tid := ri.TaskID
				taskID = &tid
			}
			store := t.ts.DB()
			if store == nil {
				return actool.Errorf("无法写入信号：DB 未初始化"), nil
			}
			id, err := store.Signals().InsertSignal(db.SignalInput{
				TaskID:       taskID,
				InstrumentID: &instID,
				SignalType:   st,
				Action:       act,
				Direction:    dir,
				Confidence:   strconv.FormatFloat(a.Confidence, 'f', 4, 64),
				Horizon:      strings.TrimSpace(a.Horizon),
				Rationale:    maskString(rationale),
			})
			if err != nil {
				return actool.Errorf(fmt.Sprintf("写入 signal 失败: %v", err)), nil
			}
			return jsonResult(map[string]any{"signal_id": id, "signal_type": st, "action": act, "direction": dir, "role": role})
		},
	)
}

// ---------------------------------------------------------------------
// emit_rating
// ---------------------------------------------------------------------

// ReviewReasons 是 rating='review' 时必须给出的四类原因。
type ReviewReasons struct {
	DataGaps         []string `json:"data_gaps"`
	ConflictingFacts []string `json:"conflicting_facts"`
	SkippedAnalysts  []string `json:"skipped_analysts"`
	NextStep         string   `json:"next_step"`
}

func (t *ToolSet) emitRating() actool.CoreTool {
	return t.writeExpTool(
		"emit_rating",
		"登记最终评级。**仅 portfolio_manager 角色可调用**（role 从 intent 上下文读取，不接受自报）。\n"+
			"rating: buy / overweight / hold / underweight / sell / review。\n"+
			"confidence_reason 必填且至少 10 字 —— 写「ok」这类敷衍会被拒绝。\n"+
			"当 rating='review' 时必须给出 review_reasons 四项（data_gaps / conflicting_facts / "+
			"skipped_analysts / next_step），说明「为什么还不能下结论」。\n"+
			"当 rating 不是 review 时**不要**给 review_reasons —— 给了会被拒绝（说明判断混乱）。",
		obj(map[string]any{
			"instrument_id":     map[string]any{"type": "integer", "description": "标的 id"},
			"rating":            map[string]any{"type": "string", "enum": []string{"buy", "overweight", "hold", "underweight", "sell", "review"}},
			"confidence":        map[string]any{"type": "number", "description": "0..1 的置信度"},
			"confidence_reason": str("必填，≥10 字。为什么是这个置信度"),
			"decided_at":        str("可选。RFC3339 时间；省略则用当前时间"),
			"evidence_node_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "支撑本次评级的节点 id"},
			"review_reasons": obj(map[string]any{
				"data_gaps":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "哪些数据缺失"},
				"conflicting_facts": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "哪些事实互相矛盾"},
				"skipped_analysts":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "哪些维度被裁掉"},
				"next_step":         str("下一步做什么才能得出结论"),
			}, "data_gaps", "conflicting_facts", "skipped_analysts", "next_step"),
		}, "instrument_id", "rating", "confidence_reason"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				InstrumentID     int64          `json:"instrument_id"`
				Rating           string         `json:"rating"`
				Confidence       float64        `json:"confidence"`
				ConfidenceReason string         `json:"confidence_reason"`
				DecidedAt        string         `json:"decided_at"`
				EvidenceNodeIDs  []int64        `json:"evidence_node_ids"`
				ReviewReasons    *ReviewReasons `json:"review_reasons"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(fmt.Sprintf("入参解析失败: %v", err)), nil
			}

			role, err := requireDBRole(ctx, t.ts)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if role != "portfolio_manager" {
				return actool.Errorf(fmt.Sprintf(
					"emit_rating 仅允许 portfolio_manager 角色调用（当前 role=%q）", role)), nil
			}

			// 用 ParseRating 而不是直接查表 —— 它能区分「模型输出合法」和
			// 「模型输出非法但被归一」。
			//
			// # 为什么这是升级而不是等价替换
			//
			// 原来的 `strings.TrimSpace + validRatingValues[rt]` 是**一次性校验**：
			// 它拒绝非法值，但不记录「模型到底输出了什么」。
			//
			// 而 ParseRating 返回 (Rating, Source, Raw)：
			//   Source=SourceLiteral  → 模型输出合法（逐字匹配六值之一）
			//   Source=SourceFallback → 模型输出非法，已归一为 review，且 Raw 被记进日志
			//
			// 这让我们能回答「prompt 是否有效」—— 而原来的写法把线索丢了。
			//
			// # 为什么非法值不报错而是继续写入 review
			//
			// 报错会让整条链中断，而 review 是「需要人工看」—— 那正是非法值的正确归宿。
			// 文档 §3.4:196 要求 review 不生成草案，所以写 review 是安全的。
			res := ParseRating(a.Rating)
			rt := res.Rating
			// ParseRating 在 fallback 时已打过 slog.Warn（含 raw）——
			// 这里不再重复打，但要把「为什么是 review」写进 review_reasons（见下）。
			if a.InstrumentID <= 0 {
				return actool.Errorf("instrument_id 必须为正"), nil
			}
			if a.Confidence < 0 || a.Confidence > 1 {
				return actool.Errorf(fmt.Sprintf("confidence 必须在 0..1，实际 %v", a.Confidence)), nil
			}
			reason := strings.TrimSpace(a.ConfidenceReason)
			if len([]rune(reason)) < 10 {
				return actool.Errorf(fmt.Sprintf(
					"confidence_reason 至少 10 个字（写「ok」这类敷衍不可接受），实际 %d 字",
					len([]rune(reason)))), nil
			}

			// review 与非 review 的双向约束（DB 层 ck_rating_review_reasons 也兜底）。
			var reviewJSON json.RawMessage
			if rt == RatingReview {
				// # fallback 时自动填 review_reasons
				//
				// 模型输出非法值时，它不会提供 review_reasons（它连格式都没搞对）。
				// 而 DB CHECK 要求 review 必须带四个键，缺一个 INSERT 就失败。
				//
				// # 为什么由系统填而不是报错
				//
				// 模型没给理由，但**系统知道理由**（格式非法）——
				// 而这条系统填的理由比模型自己写的更有价值：
				// 它精确指出了 raw 是什么，是改进 prompt 的直接依据。
				//
				// # 为什么只填三项（不填 conflicting_facts）
				//
				// 「有冲突的证据」这件事系统看不出来 —— 那需要读 facts 内容。
				// 填一个空数组是诚实的（确实没检测到冲突），而编造内容是错的。
				if res.Source == SourceFallback && a.ReviewReasons == nil {
					a.ReviewReasons = &ReviewReasons{
						DataGaps: []string{fmt.Sprintf(
							"模型输出非法评级 %q（合法值：buy/overweight/hold/underweight/sell/review，逐字匹配小写）",
							res.Raw)},
						ConflictingFacts: []string{},
						SkippedAnalysts:  []string{},
						NextStep: "检查 prompt 是否明确要求「只输出六个值之一，逐字匹配小写」；" +
							"若已在 prompt 中要求，说明模型未遵循格式指令，考虑换模型或降低 temperature",
					}
				}
				if a.ReviewReasons == nil {
					return actool.Errorf("rating='review' 时必须提供 review_reasons（data_gaps/conflicting_facts/skipped_analysts/next_step）"), nil
				}
				rr := a.ReviewReasons
				if len(rr.DataGaps) == 0 && len(rr.ConflictingFacts) == 0 && len(rr.SkippedAnalysts) == 0 && strings.TrimSpace(rr.NextStep) == "" {
					return actool.Errorf("review_reasons 四项不能全空 —— 至少说明缺什么或下一步做什么"), nil
				}
				if strings.TrimSpace(rr.NextStep) == "" {
					return actool.Errorf("review_reasons.next_step 不能为空 —— 必须说明「下一步做什么才能得出结论」"), nil
				}
				b, err := json.Marshal(rr)
				if err != nil {
					return actool.Errorf(fmt.Sprintf("序列化 review_reasons 失败: %v", err)), nil
				}
				reviewJSON = redact.MaskJSON(b)
			} else {
				if a.ReviewReasons != nil {
					return actool.Errorf(fmt.Sprintf(
						"rating 不是 'review' 时不应提供 review_reasons —— 给了说明判断混乱，"+
							"请确认是要复核(review)还是给出评级(%s)", rt)), nil
				}
			}

			ri := RunInfoFrom(ctx)
			instID := a.InstrumentID
			var taskID *int64
			if ri.TaskID > 0 {
				tid := ri.TaskID
				taskID = &tid
			}
			// intent_id 记录「这条评级是哪个 intent 产出的」。
			//
			// # 为什么需要它
			//
			// PM intent 结束时（server/engine.go）要精确知道「这个 intent 产出的
			// 评级是哪一条」，从而决定是否生成订单草案。
			//
			// 时间窗口（created_at >= state_at）是猜测 —— 同一秒内两条 intent
			// 结束时会取错。而 intent_id 是直接记录，无歧义。
			//
			// # 为什么可能为 nil
			//
			// ri.IntentID == 0 表示「没有 intent 上下文」（planner/mainagent/chat）。
			// 而那种上下文里 role 校验会先拒绝（emit_rating 只允许 portfolio_manager），
			// 所以理论上走不到这里 —— 但为了不为 0 写进 DB（0 不是合法 node id），
			// 这里显式转成 nil。
			var intentID *int64
			if ri.IntentID > 0 {
				iid := ri.IntentID
				intentID = &iid
			}
			store := t.ts.DB()
			if store == nil {
				return actool.Errorf("无法写入评级：DB 未初始化"), nil
			}
			id, err := store.Ratings().InsertRating(db.RatingInput{
				TaskID:           taskID,
				InstrumentID:     &instID,
				IntentID:         intentID,
				Rating:           rt,
				Confidence:       strconv.FormatFloat(a.Confidence, 'f', 4, 64),
				DecidedAt:        strings.TrimSpace(a.DecidedAt),
				EvidenceNodeIDs:  a.EvidenceNodeIDs,
				ConfidenceReason: maskString(reason),
				ReviewReasons:    reviewJSON,
			})
			if err != nil {
				return actool.Errorf(fmt.Sprintf("写入 rating 失败: %v", err)), nil
			}
			return jsonResult(map[string]any{"rating_id": id, "rating": rt, "role": role})
		},
	)
}

// kindThesisStr 与 pipeline.go 的 KindThesis 保持一致。
// 单独定义常量避免两处字符串字面量漂移（pipeline.go 用的是无前缀常量）。
const KindThesisStr = "thesis"

var _ = maskAny // 保留以便后续对整块 payload 脱敏
