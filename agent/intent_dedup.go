package agent

// 意图去重与连续重复上限（2026-11-08）。
//
// # 背景
//
// 目标依赖未来数据（如「等下一交易日收盘后触发入场」）时，planner 的硬底线
// 会逼它每轮至少派一条意图，于是「续跟踪」被换着措辞反复派发：每条只写回
// 一条 fact、没有任何新信息，却每轮烧一次 LLM。
//
// 这里在 add_intent 落库前做硬约束，不依赖模型自觉：
//   - 最近几条里存在内容完全相同的意图 → 直接拒绝（去重）；
//   - 最近连续 N 条与它共享足够长的前缀（同系列）→ 拒绝（上限）。
//
// 判定只用纯函数，便于单测覆盖；读 DB 失败时不阻断派发（fail-open，因为
// 「少派一条意图」比「误拒一条新方向」代价小得多，等待降频由 server 层兜底）。

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
)

const (
	intentDedupLookback = 60 // 最多回看多少条历史意图（最新在前）
	intentSeriesPrefix  = 10 // 共同前缀达到多少字算同一「方向系列」
	maxIntentRepeat     = 3  // 同一方向系列连续出现的上限
	intentExactLookback = 3  // 精确去重只查最近几条
)

// normalizeIntentSummary 去掉所有空白，让「换个换行/空格」不算新方向。
func normalizeIntentSummary(s string) string {
	return strings.Join(strings.Fields(s), "")
}

// commonPrefixRunes 返回两个规范化摘要的公共前缀长度（按字计）。
// 「续跟踪：待11-09(周一)…」与「续跟踪：待11-09(下一交易日)…」前缀足够长，
// 会被判为同系列；「改用基本面重估…」则不会。
func commonPrefixRunes(a, b string) int {
	ar, br := []rune(a), []rune(b)
	n := 0
	for n < len(ar) && n < len(br) && ar[n] == br[n] {
		n++
	}
	return n
}

func intentSummaryOf(n *db.Node) string {
	if n == nil || len(n.Payload) == 0 {
		return ""
	}
	var p struct {
		Summary string `json:"summary"`
	}
	if json.Unmarshal(n.Payload, &p) != nil {
		return ""
	}
	return p.Summary
}

func shortIntentSummary(s string, max int) string {
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// checkIntentDuplicate 检查一条新意图是否属于重复派发。
// nodes 必须按 id 倒序（最新在前），与 db.ExplorationStore.ListByKind 一致。
func checkIntentDuplicate(nodes []*db.Node, summary string) error {
	key := normalizeIntentSummary(summary)
	if key == "" {
		return nil
	}
	streak := 0
	for i, n := range nodes {
		s := intentSummaryOf(n)
		if s == "" {
			continue // 阶段图预建的意图用 stage/role，没有 summary，跳过
		}
		prev := normalizeIntentSummary(s)
		if i < intentExactLookback && prev == key {
			return fmt.Errorf(
				"重复意图：最近已派过内容相同的方向「%s」，本轮不再建节点；若只是等新数据，请直接结束本轮（系统会降频唤醒复查）",
				shortIntentSummary(summary, 60))
		}
		if commonPrefixRunes(key, prev) >= intentSeriesPrefix {
			streak++
		} else {
			break // 只算最近一段连续的同系列意图
		}
	}
	if streak >= maxIntentRepeat {
		return fmt.Errorf(
			"连续 %d 条意图都在跟踪同一方向且无新增信息，已触发重复上限（%d）；本轮不再建节点，请结束本轮，等新数据到达后系统会再唤醒",
			streak, maxIntentRepeat)
	}
	return nil
}

// rejectDuplicateIntent 由 addOneIntent 调用：读最近意图并做去重判定。
func (t *ToolSet) rejectDuplicateIntent(summary string) error {
	if t.ts == nil {
		return nil
	}
	nodes, err := t.ts.ListByKind(db.KindIntent, intentDedupLookback)
	if err != nil {
		return nil // 读失败不阻断派发
	}
	return checkIntentDuplicate(nodes, summary)
}
