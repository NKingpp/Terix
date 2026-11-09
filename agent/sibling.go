package agent

// 读兄弟节点（同一探索内其它角色的产出）。
//
// # 为什么需要这一层
//
// W5 的 10 个角色构成一条链：分析师产出 fact → 辩论读 fact 产出 thesis
// → 研究经理读 thesis 产出裁决 → 交易员读裁决产出 signal → 风控读提案
// → PM 读全部产出评级。
//
// 「读上游产出」有 5 个角色要做（debate / research_manager / trader /
// risk / PM），每个写一套查询会让「读什么、怎么格式化、过滤什么」各自漂移。
//
// # 三条硬约束（实现在 db 层，见 db.LatestSibling）
//
//  1. **过滤作废态**：superseded / refuted / dismissed / abandoned / voided /
//     expired 不该被读到 —— 否则下游基于过期信息决策。
//  2. **perRoleLimit 而非总数限制**：PM 要「三个风控各一条」，不是「任意三条」。
//  3. **返回 state**：下游需区分 thesis 的 `open`（刚写入待评）与
//     `standing`（未被驳倒）。混同会让 research_manager 把「待评」当「有效」。
//
// # 双层输出（摘要 + 全量）
//
// 大数据变量拆两份：摘要进「可用输入」表格，全量进末尾「完整数据」节。
// 表格单元格塞 JSON 数组会撑断 markdown 表格（多行内容终止表格），
// 而 LLM 判断又必须读到具体数值。这与 AnalystVars.Bars/BarsJSON 同一设计。

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// siblingNode 是一条兄弟节点产出（含格式化好的文本与元信息）。
type siblingNode struct {
	Role  string
	State string
	// Text 是完整 JSON（含 _kind/_role/_state/data），用于全量展示。
	Text string
}

// siblingOutput 读本探索内最新的一条 kind + role 匹配的有效节点。
//
// 返回 (text, role, state, found)。found=false 表示「上游还没产出」——
// **不返回错误**，因为那是正常状态（第 1 轮辩论没有 PriorRoundSummary）。
func siblingOutput(ts *db.ExplorationStore, kind, role string) (string, bool, error) {
	if ts == nil {
		return "", false, nil
	}
	n, err := ts.LatestSibling(kind, role)
	if err != nil || n == nil {
		return "", false, err
	}
	return formatSiblingNode(n.Payload, n.Kind, n.Role(), n.State), true, nil
}

// siblingOutputs 读多个 role 的产出。
//
// perRoleLimit：「每个 role 最多几条」（不是总数）。
//   - 1 → 各取最新（PM 读三方风控、debate 读对手）
//   - 0 → 各取全部（research_manager 裁决全部辩论轮次）
func siblingOutputs(ts *db.ExplorationStore, kind string, roles []string, perRoleLimit int) ([]siblingNode, error) {
	if ts == nil || len(roles) == 0 {
		return nil, nil
	}
	nodes, err := ts.LatestSiblings(kind, roles, perRoleLimit)
	if err != nil {
		return nil, err
	}
	out := make([]siblingNode, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, siblingNode{
			Role:  n.Role(),
			State: n.State,
			Text:  formatSiblingNode(n.Payload, n.Kind, n.Role(), n.State),
		})
	}
	return out, nil
}

// joinSiblings 把多条产出拼成一段（全量展示用）。
func joinSiblings(nodes []siblingNode) string {
	if len(nodes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		parts = append(parts, n.Text)
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// summarizeSiblings 生成「N 份 X（role1 / role2 / ...）」这样的摘要。
//
// 不塞全量 JSON —— 表格单元格会被撑断。这是双层设计的上半层。
//
// noun 是量词（"份事实" / "条论点" / "份意见"）。
func summarizeSiblings(nodes []siblingNode, noun string) string {
	if len(nodes) == 0 {
		// 「上游还没产出」与「数据源没接」不同：前者等一会可能有。
		return "（上游尚未产出 —— 该阶段可能还没执行完，或本轮被跳过）"
	}
	roles := make([]string, 0, len(nodes))
	states := make(map[string]int, 2)
	for _, n := range nodes {
		roles = append(roles, n.Role)
		states[n.State]++
	}
	// 有非 standing/confirmed 的 state 时附注 —— 下游要知道「其中有待评的」
	var stateNote string
	for st, cnt := range states {
		if st != "standing" && st != "confirmed" {
			stateNote += fmt.Sprintf("；%d 条 state=%s", cnt, st)
		}
	}
	return fmt.Sprintf("%d %s（%s%s）", len(nodes), noun, strings.Join(roles, " / "), stateNote)
}

// formatSiblingNode 把一个节点的 payload 格式化成给 LLM 读的 JSON。
//
// # 为什么用 JSON 而不是 Markdown 列表
//
// LLM 对 JSON 的字段边界识别比 Markdown 更精确 —— 证据字符串里常含换行、
// 冒号、破折号，Markdown 列表会被撑断（`- claim: xxx` 后跟多行证据，
// 渲染出来像多个列表项）。
//
// # 为什么把 state / role / kind 一起输出
//
// state 决定这条产出能不能用（standing vs open），role 决定是谁说的
// （辩论要区分多空）。两者都是下游判断依据，藏在 payload 里等于没给。
func formatSiblingNode(raw []byte, kind, role, state string) string {
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Sprintf(`{"_kind":%q,"_role":%q,"_state":%q,"_error":"payload 解析失败: %v"}`,
			kind, role, state, err)
	}
	wrapped := map[string]any{
		"_kind":  kind,
		"_role":  role,
		"_state": state,
		"data":   p,
	}
	out, err := json.Marshal(wrapped)
	if err != nil {
		return fmt.Sprintf(`{"_kind":%q,"_role":%q,"_state":%q,"_error":"序列化失败: %v"}`,
			kind, role, state, err)
	}
	return string(out)
}

// analystRoles 是全部分析师角色（供 debate 读全部事实）。
//
// **必须与实际展开的分析师一致** —— 列了没注册的角色，debate 会读到空结果
// 并误以为「该维度没产出」；漏了已注册的角色，debate 会看不到那份事实。
// 所以这里同时包含股票侧（analyst_market/news/fundamental）与 crypto 侧
// （analyst_sentiment_crypto）：siblingOutputs 按 role 过滤，
// 不存在的角色只是不返回节点，不会报错。
//
// analyst_sentiment（股票版情绪）**不在表内** —— 它没有数据源，未注册。
var analystRoles = []string{
	RoleAnalystMarket,
	RoleAnalystNews,
	RoleAnalystFundamental,
	RoleAnalystSentimentCrypto,
}

// debateRoles 是辩论双方。
var debateRoles = []string{RoleDebateBull, RoleDebateBear}

// riskRoles 是三方风控。
var riskRoles = []string{
	RoleRiskAggressive,
	RoleRiskNeutral,
	RoleRiskConservative,
}

// otherDebateRole 返回辩论对手的角色名（bull ↔ bear）。
//
// 用于第 2 轮辩论的 .PriorRoundSummary —— bull 要读 bear 的上一轮，
// 反之亦然。未知角色返回空串（调用方据此跳过）。
func otherDebateRole(role string) string {
	switch role {
	case RoleDebateBull:
		return RoleDebateBear
	case RoleDebateBear:
		return RoleDebateBull
	}
	return ""
}

// notWiredSibling 是「兄弟节点还没有产出」的说明文案。
//
// 与 notWiredGap（数据源未接入）区分：那个是代码没写，这个是上游还没跑完。
// 混成一句会让运维分不清「等一会就有了」还是「永远不会有」。
const notWiredSibling = "（上游尚未产出 —— 该阶段可能还没执行完，或本轮被跳过）"
