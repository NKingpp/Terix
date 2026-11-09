package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestFallbackCompressionBodyKeepsNodeIDs(t *testing.T) {
	p1, _ := json.Marshal(map[string]any{"summary": "AAPL 11-06 收 342.37，双底 338.30 守稳"})
	p2, _ := json.Marshal(map[string]any{"summary": "VIX 25.64 未冷却，R1/R2 入场条件均未触发"})
	byID := map[int64]*db.Node{
		12: {ID: 12, Kind: db.KindFact, Payload: p1},
		13: {ID: 13, Kind: db.KindFact, Payload: p2},
	}
	body := fallbackCompressionBody(block{Members: []int64{12, 13}}, byID)
	if !strings.Contains(body, "自动生成的摘要") {
		t.Error("降级摘要必须显式标注来源，避免被当成模型结论")
	}
	for _, want := range []string{"#12", "#13", "342.37", "VIX"} {
		if !strings.Contains(body, want) {
			t.Errorf("降级摘要缺少 %q：%s", want, body)
		}
	}
}

func TestFallbackCompressionBodyHandlesMissingNodes(t *testing.T) {
	body := fallbackCompressionBody(block{Members: []int64{99}}, map[int64]*db.Node{})
	if !strings.Contains(body, "自动生成的摘要") {
		t.Error("成员节点缺失时也要返回可用的降级摘要")
	}
}
