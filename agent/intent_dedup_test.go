package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func intentNode(id int64, summary string) *db.Node {
	p, _ := json.Marshal(map[string]any{"summary": summary})
	return &db.Node{ID: id, Kind: db.KindIntent, Payload: p}
}

func stageNode(id int64) *db.Node {
	p, _ := json.Marshal(map[string]any{"stage": "analysis", "role": "analyst_market"})
	return &db.Node{ID: id, Kind: db.KindIntent, Payload: p}
}

func TestIntentDedupRejectsExactRepeat(t *testing.T) {
	nodes := []*db.Node{intentNode(10, "持续跟踪 AAPL 与 VIX 入场条件"), stageNode(9)}
	err := checkIntentDuplicate(nodes, "持续跟踪 AAPL 与 VIX 入场条件")
	if err == nil {
		t.Fatal("最近派过的同文方向应被拒绝")
	}
	if !strings.Contains(err.Error(), "重复意图") {
		t.Errorf("错误文案应说明重复，实际 %v", err)
	}
}

func TestIntentDedupIgnoresWhitespaceVariants(t *testing.T) {
	nodes := []*db.Node{intentNode(10, "持续跟踪 AAPL 与 VIX 入场条件")}
	err := checkIntentDuplicate(nodes, "持续跟踪  AAPL  与 VIX\n入场条件")
	if err == nil {
		t.Fatal("只改空白/换行不算新方向")
	}
}

func TestIntentDedupRejectsSeriesStreak(t *testing.T) {
	nodes := []*db.Node{
		intentNode(15, "续跟踪：待11-09(周一)收盘数据后核 fact60 入场条件，应用 VIX 冷却门槛"),
		intentNode(14, "续跟踪：待11-09(下一交易日)收盘数据后核 fact60 入场条件，应用 VIX 冷却门槛"),
		intentNode(13, "续跟踪：待11-09 收盘数据后核 fact60 入场条件，应用 VIX 冷却门槛"),
		stageNode(12),
	}
	err := checkIntentDuplicate(nodes, "续跟踪：待11-09 收盘后续核 fact60 入场条件，应用 VIX 冷却门槛")
	if err == nil {
		t.Fatal("连续 3 条同系列意图后应触发重复上限")
	}
	if !strings.Contains(err.Error(), "重复上限") {
		t.Errorf("错误文案应说明重复上限，实际 %v", err)
	}
}

func TestIntentDedupAllowsNewDirection(t *testing.T) {
	nodes := []*db.Node{
		intentNode(15, "续跟踪：待下一交易日收盘核 R1/R2 入场条件"),
		intentNode(14, "续跟踪：待下一交易日收盘核 R1/R2 入场条件（补充）"),
	}
	if err := checkIntentDuplicate(nodes, "改用基本面重估：核对 AAPL 最近两季毛利率与估值分位"); err != nil {
		t.Fatalf("本质不同的新方向不该被拒: %v", err)
	}
}

func TestIntentDedupSkipsStageNodesAndEmptySummary(t *testing.T) {
	nodes := []*db.Node{stageNode(9), stageNode(8)}
	if err := checkIntentDuplicate(nodes, "任何新方向"); err != nil {
		t.Fatalf("阶段图预建节点没有 summary，不该影响去重: %v", err)
	}
	if err := checkIntentDuplicate(nodes, "   "); err != nil {
		t.Fatalf("空 summary 由 addOneIntent 自己校验，这里不该报错: %v", err)
	}
}
