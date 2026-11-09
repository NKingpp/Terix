package agent

// 阶段图落库。
//
// # 为什么必须在一个事务里
//
// 图的节点与边要么全部建成，要么全部不建。**半成的图比没有图更危险**：
// 它有部分节点，engine 的 Frontier 会开始派发，但依赖关系不全 ——
// 某些阶段永远触发不到，而界面上看「有图在跑」。
//
// 所以本函数的签名接收 *sql.Tx 而不是 *DB：这是「同事务」的物理约束。
// 传 *DB 会让调用方可能忘了开事务。
//
// # 为什么只读 g.Edges，不读 StageNode.Await
//
// Await 只覆盖 join 的直接上游（join_analysts / join_risk），
// 而 Edges 覆盖**全部**阶段依赖（含 trader → risk_* 这类非 join 之间的边）。
// 读 Await 建图会漏掉后者，导致门控失效 —— 所有节点同时可跑。
//
// # 关于依赖方向
//
// `src --gates--> dst` = src 完成后 dst 才能 claim。
// StageEdge{From, To} 的 From 是上游、To 是下游，与 gates 方向一致，直接映射。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// stageKinds 是允许出现在 stage graph 里的节点类型。
//
// 为什么只允许 intent/join：gates 是「执行顺序」依赖，只有能被派发的节点
// （intent）和汇合点（join）才参与。fact/thesis/signal/conclusion 是
// emit_* 工具写的**产物**，不是 stage 节点。
//
// **v1.6 修过一次**：research_manager/trader/portfolio_manager 曾误用
// fact/thesis/signal 作为节点 kind，而 Frontier 的 SQL 硬编码 kind='intent'，
// 导致这 4 个阶段永远不会被 claim —— 图跑到 join_risk 就停，
// 而节点数、边数、所有测试全过，直到真实跑分析流程才发现有阶段不执行。
//
// 放宽这个集合会掩盖「又有人把产物类型当节点类型」这类错误。
var stageKinds = map[string]bool{
	KindIntent: true,
	KindJoin:   true,
}

// MaterializeResult 是一次落库的结果（供日志与测试断言）。
type MaterializeResult struct {
	// NodeIDs 是 Ref → 节点 id 的映射。
	NodeIDs map[string]int64
	// NodeCount / EdgeCount 便于断言（与 graph.Nodes/Edges 长度比对）。
	NodeCount int
	EdgeCount int
}

// MaterializeGraph 把 StageGraph 落库到指定 exploration（在调用方的事务内）。
//
// 步骤：
//  1. 校验所有节点 kind 属于 stageKinds
//  2. 逐条 INSERT 节点，建立 Ref → id 映射
//     （逐条而非批量：PG 的 RETURNING 不保证与 VALUES 顺序一致，
//     批量需要额外回填 _ref 再清理；16 条的一次性开销不值得那个复杂度）
//  3. 校验每条边的两端 Ref 都存在且 kind 匹配
//  4. 批量 INSERT gates 边
//  5. skipped_by_design 写进对应 join 的 payload
func MaterializeGraph(tx *sql.Tx, expID int64, g *StageGraph) (MaterializeResult, error) {
	// nil 检查必须在任何字段访问之前 —— len(g.Nodes) 在 nil 上会 panic。
	if g == nil {
		return MaterializeResult{NodeIDs: map[string]int64{}}, fmt.Errorf("阶段图为空")
	}
	res := MaterializeResult{NodeIDs: make(map[string]int64, len(g.Nodes))}
	if expID <= 0 {
		return res, fmt.Errorf("exploration id 无效: %d", expID)
	}
	if len(g.Nodes) == 0 {
		return res, fmt.Errorf("阶段图没有任何节点")
	}

	// ---- 1. 校验 kind + Ref 唯一 ----
	kindOf := make(map[string]string, len(g.Nodes))
	for _, n := range g.Nodes {
		if strings.TrimSpace(n.Ref) == "" {
			return res, fmt.Errorf("节点缺少 Ref（role=%q kind=%q）", n.Role, n.Kind)
		}
		if _, dup := kindOf[n.Ref]; dup {
			return res, fmt.Errorf("Ref %q 重复 —— Ref 是图内唯一标识，不能重复", n.Ref)
		}
		if !stageKinds[n.Kind] {
			return res, fmt.Errorf(
				"节点 %q 的 kind=%q 不是 stage 节点（只允许 intent/join）。"+
					"fact/thesis/signal/conclusion 是 emit_* 的产物，不是阶段节点 —— "+
					"用它们做节点会导致 Frontier 永不派发该阶段（v1.6 事故）",
				n.Ref, n.Kind)
		}
		kindOf[n.Ref] = n.Kind
	}

	// ---- 2. 逐条 INSERT 节点 ----
	//
	// state 的选择：
	//   intent → 'open'（等待 ClaimIntent 认领）
	//   join   → 'open'（等待 ReconcileJoins 兑现）
	// 两者都从 open 开始，符合 ck_node_state。
	//
	// origin 用 'system'：这些节点是系统建图时创建的，不是 worker 或人写的。
	for _, n := range g.Nodes {
		payload := n.Payload
		if payload == nil {
			payload = map[string]any{}
		}
		// role 写进 payload：emit_* 工具靠 payload->>'role' 取角色做权限判断，
		// ReconcileJoins 也靠它识别失败上游的名字。
		if n.Role != "" {
			payload["role"] = n.Role
		}
		if n.GroupID != "" {
			payload["group_id"] = n.GroupID
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return res, fmt.Errorf("序列化节点 %q 的 payload 失败: %w", n.Ref, err)
		}
		// ⚠️ owner 列必须与 payload.role 同步写入。
		//
		// # 为什么这里也必须写（与 AddNode 同样的理由）
		//
		// `owner` 是「产出该节点的角色」的**独立列**（不是只在 payload 里）。
		// 读取侧（如 LatestSibling / 依赖 role 的判定）直接查 owner 列，
		// 不解析 payload。
		//
		// # 不写的后果（静默失效）
		//
		// 本函数是「建节点」的**第二条写入路径**（第一条是 AddNode）。
		// 如果这里漏写 owner：
		//   - 阶段图建的节点 owner 恒为 NULL
		//   - 而任何「按 owner 判断 role」的逻辑对它们永远不成立
		//   - 没有报错，只是「本该发生的事没发生」
		//
		// 这正是 L16 的失败方式。
		//
		// 用 NULLIF 让空 role 落成 NULL（与 AddNode 一致：
		// 数据库里「没有 owner」只有一种表示）。
		owner := strings.TrimSpace(n.Role)

		var id int64
		if err := tx.QueryRow(`
INSERT INTO exploration_nodes(exploration_id, kind, payload, priority, state, origin, owner)
VALUES ($1, $2, $3, $4, 'open', 'system', NULLIF($5, ''))
RETURNING id`, expID, n.Kind, string(raw), n.Priority, owner).Scan(&id); err != nil {
			return res, fmt.Errorf("插入节点 %q 失败: %w", n.Ref, err)
		}
		res.NodeIDs[n.Ref] = id
		res.NodeCount++
	}

	// ---- 3. 校验边的两端 ----
	type edgeIDs struct{ src, dst int64 }
	edges := make([]edgeIDs, 0, len(g.Edges))
	for _, e := range g.Edges {
		srcID, ok := res.NodeIDs[e.From]
		if !ok {
			return res, fmt.Errorf("边引用了不存在的上游 Ref=%q（边 %q→%q）", e.From, e.From, e.To)
		}
		dstID, ok := res.NodeIDs[e.To]
		if !ok {
			return res, fmt.Errorf("边引用了不存在的下游 Ref=%q（边 %q→%q）", e.To, e.From, e.To)
		}
		// 两端 kind 已在第 1 步校验过（都是 stageKinds），这里再确认一次
		// 是为了给出更明确的错误（哪条边、哪一端）。
		if !stageKinds[kindOf[e.From]] || !stageKinds[kindOf[e.To]] {
			return res, fmt.Errorf("边 %q→%q 的端点不是 stage 节点（%q/%q）",
				e.From, e.To, kindOf[e.From], kindOf[e.To])
		}
		edges = append(edges, edgeIDs{src: srcID, dst: dstID})
	}

	// ---- 4. 批量 INSERT gates 边 ----
	//
	// # 为什么用 json_each 而不是 unnest（SQLite 迁移）
	//
	// SQLite 没有数组类型，`unnest($2::bigint[], $3::bigint[])` 无等价物。
	// 绑定 []int64 也直接失败（实测 `unsupported type []int64`）。
	// 所以把两端序列化成 JSON 数组（各自一个参数），再用 json_each 按位置对齐。
	//
	// # 为什么用 key 对齐而不是 rowid 对齐
	//
	// `json_each` 的 `key` 是数组下标（0 起），拿它 JOIN 比依赖"两表扫描顺序
	// 一致"可靠得多 —— 后者在 SQLite 里没有保证。
	//
	// # 分块
	//
	// 每条边 2 个整数 + 1 个 expID 全在 JSON 字符串里，参数恒为 3 个。
	// 分块只为控制单条 SQL 的大小，取 30000 与 PG 版一致。
	if len(edges) > 0 {
		const chunk = 30000
		for start := 0; start < len(edges); start += chunk {
			end := start + chunk
			if end > len(edges) {
				end = len(edges)
			}
			srcs := make([]int64, 0, end-start)
			dsts := make([]int64, 0, end-start)
			for i := start; i < end; i++ {
				srcs = append(srcs, edges[i].src)
				dsts = append(dsts, edges[i].dst)
			}
			srcJSON, mErr := json.Marshal(srcs)
			if mErr != nil {
				return res, fmt.Errorf("序列化 src 数组失败: %w", mErr)
			}
			dstJSON, mErr := json.Marshal(dsts)
			if mErr != nil {
				return res, fmt.Errorf("序列化 dst 数组失败: %w", mErr)
			}
			// ON CONFLICT DO NOTHING 必须带冲突目标：
			// SQLite 下不带目标的 `ON CONFLICT DO NOTHING` 对复合主键报错
			// （实测 `near "DO": syntax error` 的同族问题）。
			if _, err := tx.Exec(`
INSERT INTO exploration_edges(exploration_id, src_id, dst_id, rel)
SELECT $1, s.value, d.value, 'gates'
  FROM json_each($2) s
  JOIN json_each($3) d ON d.key = s.key
ON CONFLICT (exploration_id, src_id, rel, dst_id) DO NOTHING`,
				expID, string(srcJSON), string(dstJSON)); err != nil {
				return res, fmt.Errorf("批量插入 gates 边失败（%d 条，偏移 %d）: %w",
					end-start, start, err)
			}
			res.EdgeCount += end - start
		}
	}

	// ---- 5. skipped_by_design 写进 join payload ----
	if len(g.SkippedAnalysts) > 0 {
		if err := writeSkippedByDesign(tx, expID, res.NodeIDs, g.SkippedAnalysts); err != nil {
			return res, err
		}
	}

	return res, nil
}

// writeSkippedByDesign 把「按资产类别裁掉的维度」写进对应 join 的 payload。
//
// # 为什么按 join 分流而不是写在一处
//
// join_analysts 与 join_risk 是两个不同的汇合点：
//   - 分析师被裁（如 crypto 无 fundamental）→ 影响 join_analysts
//   - 风控角色被裁（如 crypto 无 aggressive）→ 影响 join_risk
//
// 下游的 prompt 会读自己那个 join 的 skipped_by_design，来判断「数据不全」是否
// 属于预期（本就不存在）还是异常（本该有但没得到）。混在一处会让两者无法区分。
//
// # 分不清归属就报错
//
// 若某个被裁的 Role 既不是分析师也不是风控，说明 Expand 加了新维度但这里没跟上。
// 静默丢弃会让下游读不到该缺失 —— 宁可报错。
func writeSkippedByDesign(tx *sql.Tx, expID int64, nodeIDs map[string]int64,
	skipped []Skipped) error {
	for _, s := range skipped {
		targetRef := joinRefForRole(s.Role)
		if targetRef == "" {
			return fmt.Errorf(
				"skipped 角色 %q 没有在 joinForRole 里声明归属 —— "+
					"Expand 新增了维度但 materialize 的映射表没跟上。"+
					"静默丢弃会让下游读不到「本就被裁掉的维度」，所以这里报错而不是猜测",
				s.Role)
		}
		joinID, ok := nodeIDs[targetRef]
		if !ok {
			return fmt.Errorf("阶段图缺少 %q 节点，无法写入 skipped_by_design（role=%q）",
				targetRef, s.Role)
		}
		// 用 JSONB 数组追加元素。这里是逐条 UPDATE 而不是聚合后一次写，
		// 因为 skipped 通常只有 1-2 条，且逐条更容易在出错时定位。
		entry, err := json.Marshal([]map[string]string{{"role": s.Role, "reason": s.Reason}})
		if err != nil {
			return fmt.Errorf("序列化 skipped 失败: %w", err)
		}
		// # SQLite 改写要点
		//
		// PG 的 `jsonb_set(payload,'{k}', COALESCE(payload->'k','[]') || $2)`
		// 不能照抄两处：
		//
		//  1. JSON 文本用 `||` 拼出来是**字符串**（实测得到
		//     `"[][{\"role\":\"x\"}]"`），不是数组 —— `||` 在 SQLite 里对
		//     JSON 值没有「拼接数组」语义。
		//  2. `json_set(payload,'$.k[#]', …)` 往**不存在的**键追加时能建数组，
		//     但「键存在且值不是数组」时会失败，且 [#] 对数组/非数组的
		//     处理不一致 —— 半个 payload 是对象的场景不可靠。
		//
		// 所以用显式重建：json_each 展开旧数组 + UNION ALL 拼新元素，
		// 再用 json_group_array 收成数组。实测两种初始形态（无该键 / 已有数组）
		// 都得到正确的追加结果，顺序是「旧元素在前、新元素在后」。
		if _, err := tx.Exec(`
UPDATE exploration_nodes
   SET payload = json_set(
         COALESCE(NULLIF(payload, ''), '{}'),
         '$.skipped_by_design',
         (
           SELECT json_group_array(json(value)) FROM (
             SELECT value FROM json_each(
               COALESCE(json_extract(COALESCE(NULLIF(payload, ''), '{}'),
                                     '$.skipped_by_design'), '[]'))
             UNION ALL
             SELECT value FROM json_each($2)
           )
         )
       ),
       content_version = content_version + 1
 WHERE id = $1 AND exploration_id = $3`,
			joinID, string(entry), expID); err != nil {
			return fmt.Errorf("写入 skipped_by_design 到 %q 失败: %w", targetRef, err)
		}
	}
	return nil
}

// joinForRole 显式映射「角色 → 它归属的 join 节点 Ref」。
//
// # 为什么用映射表而不是字符串匹配
//
// 曾用 `strings.Contains(role, "analyst")` 这类推断，问题：
//   - 新增角色 "macro_analyst" 会被分到 join_analysts（碰巧对）
//   - 新增角色 "data_quality" 会被分到哪？两个都不含 → 报错（对）
//   - 但 "risk_data" 会被分到 join_risk（错，它是数据角色）
//
// 字符串推断的正确性依赖「命名恰好含关键词」，而命名会变。
// 显式表让「新增角色必须同时声明归属」成为强制动作 —— 漏了会报错。
var joinForRole = map[string]string{
	RoleAnalystMarket:          "join_analysts",
	RoleAnalystNews:            "join_analysts",
	RoleAnalystSentiment:       "join_analysts",
	RoleAnalystSentimentCrypto: "join_analysts",
	RoleAnalystFundamental:     "join_analysts",

	RoleRiskAggressive:   "join_risk",
	RoleRiskNeutral:      "join_risk",
	RoleRiskConservative: "join_risk",
}

// joinRefForRole 返回角色归属的 join Ref；未知角色返回空串（调用方报错）。
func joinRefForRole(role string) string {
	return joinForRole[role]
}
