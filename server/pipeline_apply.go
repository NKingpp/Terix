package server

// 阶段图落库的编排层（W3-10）。
//
// # 分层
//
//	agent.Expand          —— 纯内存展开，产出 StageGraph（不碰 DB）
//	agent.MaterializeGraph —— 在调用方事务里建 nodes + gates 边（只做「建」）
//	本文件 ApplyStageGraph —— 幂等判断 + 编排（业务知识在这里）
//
// # 为什么幂等判断不在 agent.MaterializeGraph 里
//
// 因为「建过了」的判据依赖 payload 的内容（一个版本标记）——
// 那是**业务知识**。而 MaterializeGraph 只负责「把图落库」这个机械动作。
//
// 把它塞进 MaterializeGraph 会让那个函数同时处理「建」和「判断该不该建」，
// 而后者在测试时需要额外造数据 —— 现在 MaterializeGraph 的 10 个测试
// 都不需要关心幂等。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// StageGraphVersion 是阶段图模板的版本。
//
// # 为什么带版本号而不是只判「标记是否存在」
//
// 判据从「applied 存在吗」变成「applied 的版本是当前版本吗」。
// 这样模板升级后（比如 crypto 加了 analyst_fundamental）可以重建图，
// 而不是永远用第一次建的旧图。
//
// ⚠️ 改这个常量意味着「所有历史任务下次会被视为未建图」——
// 那是有意的，但要确保新的 Expand 产出与旧图的差异是可接受的
// （重建会新增节点，不会删除已有节点）。
const StageGraphVersion = "v1"

// ErrEmptyStageGraph 表示传入的阶段图没有任何节点。
//
// # 为什么这算错误
//
// `Expand` 已经拒绝了未知 asset_class（W3-9 的 TestExpandUnknownAssetClass），
// 所以「0 节点」只可能来自调用方自己构造了空图 —— 那是 bug。
//
// 而如果返回成功，会留下一个「applied=v1 但图是空的」的 exploration ——
// 那个状态下所有下游逻辑都拿不到任何节点，且下次调用会因幂等而跳过重建。
var ErrEmptyStageGraph = errors.New("阶段图为空（0 个节点）：这通常是调用方 bug，" +
	"而不是合法的「空任务」")

// ApplyStageGraph 幂等地把阶段图落到 exploration。
//
// 返回值：
//   - refs:    Ref → nodeID 映射（no-op 时是 nil）
//   - applied: true = 本次真的建了；false = 已建过且版本相同（no-op）
//
// # 为什么用显式的 applied 而不是「nil map 表示已建过」
//
// 把两种状态编码进「map 是否为 nil」需要读注释才知道。
// 而布尔值不需要 —— 这是「一个语义一种表示」。
func ApplyStageGraph(tx *sql.Tx, expID int64, g *agent.StageGraph) (refs map[string]int64, applied bool, err error) {
	if g == nil {
		return nil, false, fmt.Errorf("%w: 阶段图为 nil", ErrEmptyStageGraph)
	}
	if len(g.Nodes) == 0 {
		return nil, false, fmt.Errorf("%w: asset_class 展开结果为空", ErrEmptyStageGraph)
	}

	current, rootID, err := readStageGraphVersion(tx, expID)
	if err != nil {
		return nil, false, err
	}
	if current == StageGraphVersion {
		// 已建过且版本相同 —— no-op。
		return nil, false, nil
	}

	res, mErr := agent.MaterializeGraph(tx, expID, g)
	if mErr != nil {
		return nil, false, fmt.Errorf("落库阶段图: %w", mErr)
	}

	if wErr := writeStageGraphVersion(tx, rootID, current, StageGraphVersion); wErr != nil {
		return nil, false, wErr
	}
	return res.NodeIDs, true, nil
}

// readStageGraphVersion 读根 fact 节点上记录的已应用版本。
//
// 返回当前版本（空串表示还没建过）与根节点 id（0 表示没有根节点）。
//
// # 为什么记在根 fact 上而不是 settings 表
//
// 因为「这个 exploration 的阶段图是什么版本」是**每个 exploration 自己的属性**，
// 而 settings 是全局的。多任务并行时全局 key 会互相覆盖。
func readStageGraphVersion(tx *sql.Tx, expID int64) (version string, rootID int64, err error) {
	// 根 fact 的判定：kind='fact' AND state='origin'（见 db/constants.go 的 StateOrigin）。
	// # 为什么是 json_extract 而不是 ->>
	//
	// `payload->>'k'` 是 PG 的 jsonb 文本操作符，SQLite 不支持（实测
	// `unrecognized token: ">"`）。而 `->>` 在 SQLite 里**恰好**是合法语法糖
	// 吗？不是 —— `->` 要一个路径参数，`->>` 不是单目/双目运算符。
	//
	// 所以统一用显式的 `json_extract(payload,'$.k')`：它对「键不存在」返回 NULL，
	// 外层 COALESCE 兜成空串，与 PG 的 `->>` 语义一致。
	err = tx.QueryRow(`
SELECT id, COALESCE(json_extract(payload,'$.stage_graph_applied'),'')
  FROM exploration_nodes
 WHERE exploration_id=$1 AND kind='fact' AND state='origin'
 ORDER BY id ASC LIMIT 1`, expID).Scan(&rootID, &version)
	if errors.Is(err, sql.ErrNoRows) {
		// 没有根 fact 的 exploration 可能存在（手工建的测试数据）——
		// 那时无法记版本，返回 0 表示「没地方记」。
		return "", 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("读阶段图版本: %w", err)
	}
	return version, rootID, nil
}

// writeStageGraphVersion 把新版本写进根 fact 的 payload。
//
// # 为什么用 JSONB merge 而不是覆盖
//
// 根 fact 的 payload 里还有其它字段（资产标的、as_of 等），
// 覆盖会丢掉它们。
func writeStageGraphVersion(tx *sql.Tx, rootID int64, from, to string) error {
	if rootID == 0 {
		// 没有根 fact —— 无法记版本。
		//
		// # 为什么这不算错误
		//
		// 根 fact 是任务创建时 seed 的；而 MaterializeGraph 也可能被
		// 手工测试数据调用（没有 root fact）。那时「没有地方记版本」
		// 意味着每次调用都会重建 —— 但重建是「新增节点」，不会破坏已有数据。
		//
		// 所以这是「幂等失效但不破坏数据」的降级，可接受。
		return nil
	}

	patch, err := json.Marshal(map[string]any{
		"stage_graph_applied":      to,
		"stage_graph_applied_prev": from, // 空串表示首次
	})
	if err != nil {
		return fmt.Errorf("序列化版本标记: %w", err)
	}
	// # 为什么是 json_patch 而不是 ||（2026-11-08 实测）
	//
	// PG 的 `jsonb || jsonb` 是**深合并**，而 SQLite 的 `||` 是**字符串拼接**。
	// 直接去掉 `::jsonb` 会变成静默语义错误 —— 不报错，但 payload 被拼成非法 JSON：
	//
	//	'{"a":1}' || '{"b":2}'  →  '{"a":1}{"b":2}'     ← 解析不了
	//
	// 而 `json_patch` 的嵌套合并语义与 PG `||` 完全一致：
	//
	//	json_patch('{"x":{"y":1}}', '{"x":{"z":2}}')  →  '{"x":{"y":1,"z":2}}'
	//
	// 这条注释保留原因：下次有人看到 `||` 以为是等价的，会再踩一次。
	if _, err := tx.Exec(`
UPDATE exploration_nodes
   SET payload = json_patch(COALESCE(payload,'{}'), $2),
       content_version = content_version + 1
 WHERE id=$1`, rootID, string(patch)); err != nil {
		return fmt.Errorf("写阶段图版本: %w", err)
	}
	return nil
}

// MaterializeDownstream 在选品完成后补建下游阶段（analysis → decision）。
//
// # 为什么需要（2026-11-05，两件事一起改）
//
// 原来无标的任务一次性建 14 个意图，但其中 13 个都要 instrument_id ——
// 而标的要等 screener 跑完才知道。结果那 13 个节点建出来全是空标的，
// 三个分析师都判「标的未知，无法分析」（实测任务 36198）。
//
// 现在创建任务时只建选品节点（ExpandOpts.ScreenerOnly），
// screener 调 set_task_instrument 选定标的后由本函数补建其余阶段。
//
// # 为什么统一交给 agent.MaterializeGraph（2026-11-08 修正）
//
// 原实现自己逐条 INSERT intent，漏掉了三样东西：
//
//	① payload.role / owner —— 角色 prompt 与工具子集全丢（list_facts 报 unknown tool）；
//	② gates 依赖边 —— 所有阶段同时 open，Frontier 门控完全失效；
//	③ join 节点 —— join_analysts / join_risk 根本不存在，ReconcileJoins 无事可做。
//
// 实测表现：辩论、风控在分析师还没产出时就跑完（「本轮分析师事实 0 份」）。
//
// MaterializeGraph 是唯一经过完整测试的路径：节点、gates 边、join、
// skipped_by_design 在一个事务里一次性落库。Expand 时 InstrumentID>0，
// 图里不含 screener，所以不会重复插选品节点。
//
// # 幂等性
//
// join 节点是「下游阶段已落库」的标志：已存在就直接返回，避免重复选品
// 插出两套阶段图。完整图路径（创建时已指定标的）也会建 join，同样被覆盖。
func MaterializeDownstream(ctx context.Context, pg *db.DB, expID, instID int64, assetClass string) (int, error) {
	if expID <= 0 {
		return 0, fmt.Errorf("exploration id 无效: %d", expID)
	}
	if assetClass == "" {
		assetClass = agent.AssetClassStock
	}
	g, err := agent.Expand(assetClass, agent.ExpandOpts{
		InstrumentID: instID,
		AsOf:         time.Now().UTC(),
	})
	if err != nil {
		return 0, fmt.Errorf("展开下游阶段失败: %w", err)
	}

	// 幂等：join 节点是下游阶段图已落库的标志。
	var joins int
	if err := pg.QueryRow(`SELECT COUNT(*) FROM exploration_nodes
WHERE exploration_id=$1 AND kind='join'`, expID).Scan(&joins); err != nil {
		return 0, fmt.Errorf("检查已有阶段图失败: %w", err)
	}
	if joins > 0 {
		return 0, nil
	}

	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	res, mErr := agent.MaterializeGraph(tx, expID, g)
	if mErr != nil {
		return 0, fmt.Errorf("落库下游阶段图失败: %w", mErr)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return res.NodeCount, nil
}
