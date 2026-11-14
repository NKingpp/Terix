package exec

import "testing"

// 开仓语义判定 —— 全部交易正确性的地基。
//
// # 最容易错的一条：long_short 下反向开仓是 open 不是 reduce
//
// 判成 reduce 的后果：工具不挂保护（reduce 不动 OCO）、回报「平仓成功」，
// 而实际开了一个**裸的新仓**。用户看到的是「已平仓」，账上是新敞口。
func TestClassifyEntry(t *testing.T) {
	cases := []struct {
		name     string
		posMode  string
		side     string
		qty      string
		heldSame string
		heldOpp  string
		want     string
		wantPost string
	}{
		// ---- net 模式 ----
		{"net 无仓开多", "net", "buy", "1", "", "", EffectOpen, "1"},
		{"net 无仓开空", "net", "sell", "1", "", "", EffectOpen, "-1"},
		{"net 同向加仓", "net", "buy", "0.5", "1", "", EffectScaleIn, "1.5"},
		{"net 同向加空", "net", "sell", "0.5", "1", "", EffectScaleIn, "-1.5"},
		{"net 反向量小于持仓 → 减仓", "net", "buy", "0.3", "", "1", EffectReduce, "-0.7"},
		{"net 反向量等于持仓 → 减仓平光", "net", "buy", "1", "", "1", EffectReduce, "0"},
		{"net 反向量超过持仓 → 反手", "net", "buy", "1.5", "", "1", EffectReverse, "0.5"},
		{"net 反手到空", "net", "sell", "2", "", "1", EffectReverse, "-1"},

		// ---- long_short 模式 ----
		{"ls 无仓开多", "long_short", "buy", "1", "", "", EffectOpen, "1"},
		{"ls 同向加仓", "long_short", "buy", "1", "2", "", EffectScaleIn, "3"},
		// ⚠️ 这两条是分水岭：双向模式下反向是另一条独立仓，纯开新仓。
		{"ls 反向开仓是 open 不是 reduce", "long_short", "buy", "0.5", "", "10", EffectOpen, "0.5"},
		{"ls 反向也不反手", "long_short", "buy", "100", "", "10", EffectOpen, "100"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := ClassifyEntry(c.posMode, c.side, c.qty, c.heldSame, c.heldOpp)
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if p.Effect != c.want {
				t.Errorf("Effect = %q, want %q (最容易错的是 long_short 反向必须 open)",
					p.Effect, c.want)
			}
			if p.PostNet != c.wantPost {
				t.Errorf("PostNet = %q, want %q", p.PostNet, c.wantPost)
			}
		})
	}
}

func TestClassifyEntryRejectsBadInput(t *testing.T) {
	for _, c := range []struct{ side, qty string }{
		{"hold", "1"}, {"", "1"},
		{"buy", "0"}, {"buy", "-1"}, {"buy", "abc"}, {"buy", ""},
	} {
		if _, err := ClassifyEntry("net", c.side, c.qty, "", ""); err == nil {
			t.Errorf("side=%q qty=%q 应报错", c.side, c.qty)
		}
	}
}

// PreNet 必须与 Position.Pos 的符号口径一致，否则回执与 get_positions 对不上。
func TestClassifyEntryPreNet(t *testing.T) {
	// 净多 1，再加多 0.5
	p, _ := ClassifyEntry("net", "buy", "0.5", "1", "")
	if p.PreNet != "1" {
		t.Errorf("PreNet = %q, want 1", p.PreNet)
	}
	// 净空 1（同向栏传 1 表示 heldSame 为空，反向栏 1 表示有空仓）
	p, _ = ClassifyEntry("net", "buy", "0.3", "", "1")
	if p.PreNet != "-1" {
		t.Errorf("空仓时 PreNet = %q, want -1", p.PreNet)
	}
}
