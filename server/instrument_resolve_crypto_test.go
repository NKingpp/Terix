package server

// 「任务文本里的标的被正确识别」的回归测试。
//
// # 这组测试守的是什么（2026-11-11）
//
// 任务 goal 写着「分析 BTC-USDT 加密永续的多空方向」，但 tasks.instrument_id
// 恒为 NULL —— 于是走了「无标金融任务」→ screener 重选 → 选了 NVDA（美股）。
// 用户明确指定的标的白纸黑字写在目标里，系统却当没看见。
//
// 根因是 verifyInstrumentSymbol 给 FetchQuote 传了空 Channel：
// OkxClient 第一行 `if ch != ChannelOKX` 就拒绝，crypto 标的**必然**验证失败。
// 而 AkshareClient 只拒绝 ChannelOKX、空串会保留传入 market ——
// 所以美股/A股一直侥幸能过，只有 crypto 全灭。这个不对称正是它没被发现的原因。

import (
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// TestChannelForVerifyMarket 通道映射必须与市场一一对应。
//
// 空串是本 bug 的直接成因，所以这里把「不允许空」写成显式断言。
func TestChannelForVerifyMarket(t *testing.T) {
	cases := map[string]agent.Channel{
		"crypto": agent.ChannelOKX,
		"us":     agent.ChannelAkshareUS,
		"cn":     agent.ChannelAkshareCN,
		"hk":     agent.ChannelAkshareHK,
		// 未知市场兜底美股通道：inferMarket 本来就把未知形态兜成 us，
		// 这里对齐，避免出现「market=xx 但通道为空」的第二种失败形态。
		"":        agent.ChannelAkshareUS,
		"future?": agent.ChannelAkshareUS,
	}
	for market, want := range cases {
		if got := channelForVerifyMarket(market); got != want {
			t.Errorf("channelForVerifyMarket(%q) = %q, want %q", market, got, want)
		}
	}
	// 关键：任何 market 都不许返回空 Channel
	for _, m := range []string{"crypto", "us", "cn", "hk", "", "unknown"} {
		if channelForVerifyMarket(m) == "" {
			t.Errorf("channelForVerifyMarket(%q) 返回空 Channel —— 这正是 2026-11-11 的 crypto 全灭成因", m)
		}
	}
}

// TestAssetClassForMarket crypto 必须映射到 crypto，其余兜底 stock。
//
// 原来这里写死 stock，于是 BTC-USDT 被登记成 market=crypto + asset_class=stock
// 的自相矛盾行 —— 建出 stock 阶段图（多出无数据源的 analyst_fundamental，
// 少了加密情绪分析师）。
func TestAssetClassForMarket(t *testing.T) {
	if got := assetClassForMarket("crypto"); got != db.AssetClassCrypto {
		t.Errorf("crypto 应映射 %q，实际 %q", db.AssetClassCrypto, got)
	}
	for _, m := range []string{"us", "cn", "hk", ""} {
		if got := assetClassForMarket(m); got != db.AssetClassStock {
			t.Errorf("%q 应兜底 %q，实际 %q", m, db.AssetClassStock, got)
		}
	}
	// 与 set_instrument_tool 的规则必须一致（同一语义两条路径不能两种结果）
	// set_instrument_tool.go 里 market=="crypto" → AssetClassCrypto
	if assetClassForMarket("crypto") != agent.AssetClassCrypto {
		t.Error("assetClassForMarket 与 set_instrument_tool 的 crypto 规则不一致")
	}
}

// TestAssetClassHintForTask 无标任务的市场推断。
//
// 这是「screener 被 asset_class=stock 误导去选美股」的修复点。
func TestAssetClassHintForTask(t *testing.T) {
	cases := []struct {
		name  string
		texts []string
		want  string
	}{
		{
			name:  "任务15原样：加密永续 + BTC-USDT",
			texts: []string{"验证planner是否会给意图指定role", "分析 BTC-USDT 加密永续的多空方向，给出交易建议"},
			want:  agent.AssetClassCrypto,
		},
		{
			name:  "只写永续",
			texts: []string{"找个永续合约做多"},
			want:  agent.AssetClassCrypto,
		},
		{
			name:  "只写 USDT",
			texts: []string{"USDT 还能追吗"},
			want:  agent.AssetClassCrypto,
		},
		{
			name:  "美股任务",
			texts: []string{"分析 AAPL 的财报与估值"},
			want:  agent.AssetClassStock,
		},
		{
			name:  "A股任务",
			texts: []string{"600519 现在的市盈率合理吗"},
			want:  agent.AssetClassStock,
		},
		{
			name:  "空文本兜底 stock",
			texts: nil,
			want:  agent.AssetClassStock,
		},
		{
			name:  "「资金」不该被当成加密（币 的过宽匹配）",
			texts: []string{"帮我规划资金分配"},
			want:  agent.AssetClassStock,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := assetClassHintForTask(c.texts...); got != c.want {
				t.Errorf("assetClassHintForTask(%v) = %q, want %q", c.texts, got, c.want)
			}
		})
	}
}

// TestStageGraphHintedUsesCrypto 无标 + crypto 提示 → 建出 crypto 阶段图。
//
// 验证 hint 真的传到了 Expand：crypto 图含 analyst_sentiment_crypto，
// stock 图含 analyst_fundamental，两者互斥，是最干净的判别特征。
func TestStageGraphHintedUsesCrypto(t *testing.T) {
	s, _, _ := newAuthServer(t)
	expID := createTestExploration(t, s.m.pg)

	tx, err := s.m.pg.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := s.stageGraphMaterializerHinted(nil, agent.AssetClassCrypto)(tx, expID); err != nil {
		tx.Rollback()
		t.Fatalf("materialize: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	_, total := countStageNodes(t, s.m.pg, expID)
	if total != 1 {
		t.Errorf("无标任务应只建 1 个 screener 节点，实际 %d", total)
	}

	var assetClass string
	if err := s.m.pg.QueryRow(
		`SELECT COALESCE(json_extract(payload,'$.asset_class'),'') FROM exploration_nodes
		  WHERE exploration_id=$1 AND kind='intent'`, expID).Scan(&assetClass); err != nil {
		t.Fatalf("查 asset_class: %v", err)
	}
	if assetClass != agent.AssetClassCrypto {
		t.Errorf("screener 节点应带 asset_class=crypto（否则模型会以为要做美股），实际 %q", assetClass)
	}
}

// TestStageGraphInstrumentOverridesHint 有标的时 hint 被忽略。
//
// 标的自己的 asset_class 才是权威：用户选了 crypto 标的、
// 但文本里恰好提到「美股」（如「对标美股科技股估值」），不该被改模板。
func TestStageGraphInstrumentOverridesHint(t *testing.T) {
	s, _, _ := newAuthServer(t)
	instID := upsertTestInstrument(t, s.m.pg, db.InstrumentInput{
		Market: "us", Symbol: "TINSTR-HINT", AssetClass: "stock",
	})
	inst, _ := s.m.pg.Instruments().GetInstrumentByID(instID)
	expID := createTestExploration(t, s.m.pg)

	tx, err := s.m.pg.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// 故意传一个错误的 hint
	if err := s.stageGraphMaterializerHinted(inst, agent.AssetClassCrypto)(tx, expID); err != nil {
		tx.Rollback()
		t.Fatalf("materialize: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var got string
	if err := s.m.pg.QueryRow(
		`SELECT COALESCE(json_extract(payload,'$.asset_class'),'') FROM exploration_nodes
		  WHERE exploration_id=$1 AND kind='intent' LIMIT 1`, expID).Scan(&got); err != nil {
		t.Fatalf("查 asset_class: %v", err)
	}
	if got != agent.AssetClassStock {
		t.Errorf("有标的时应以标的为准（stock），实际 %q —— hint 不该覆盖标的自己的 asset_class", got)
	}
}
