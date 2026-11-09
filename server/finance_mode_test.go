package server

// 金融任务判定与「无标的」路径的验收测试（2026-11-05）。
//
// # 这组测试守的是什么
//
// 用户要求「不指定标的也让模型自行发挥」。而原实现把
// 「instrument_id 非空」当金融路径的唯一判据，于是无标的的金融任务
// 会掉进渗透路径（渗透 prompt + 不建阶段图 + 跳过 seed）。
// 三条链任何一条漏改，用户都会看到「任务建好了但什么都没发生」。

import (
	"testing"

	"github.com/Autumn-27/artex/agent"
)

func TestTaskIsFinance(t *testing.T) {
	id := int64(42)

	cases := []struct {
		name         string
		instrumentID *int64
		texts        []string
		want         bool
	}{
		{
			name:         "有标的必然是金融任务（不看文本）",
			instrumentID: &id,
			texts:        []string{"帮我看看这个"},
			want:         true,
		},
		{
			name:  "无标的但文本含交易语义",
			texts: []string{"在 OKX 上完成交易操作，账户资产达到 1000 USDT"},
			want:  true,
		},
		{
			name:  "无标的但含加密币种",
			texts: []string{"挑选几个比特币合约做多"},
			want:  true,
		},
		{
			name:  "无标的但含A股",
			texts: []string{"从A股里选出低估值标的"},
			want:  true,
		},
		{
			name:  "无标的但含策略",
			texts: []string{"设计一个对冲策略并回测"},
			want:  true,
		},
		{
			name:  "纯渗透任务不该被判为金融",
			texts: []string{"扫描 example.com 的开放端口并查找漏洞"},
			want:  false,
		},
		{
			name:  "空文本不判为金融",
			texts: nil,
			want:  false,
		},
		{
			name:         "零值标的不算（防御空 id）",
			instrumentID: ptrInt64(0),
			texts:        []string{"扫描端口"},
			want:         false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := taskIsFinance(c.instrumentID, c.texts...); got != c.want {
				t.Errorf("taskIsFinance(%v, %q) = %v, want %v",
					c.instrumentID, c.texts, got, c.want)
			}
		})
	}
}

// TestFinanceNoInstrumentUsesStockTemplate 无标的的金融任务必须建阶段图。
//
// # 为什么这条关键
//
// 阶段图是「有分析师干活」的前提。没有图就没有分析师，
// 模型即使知道该自己选品也没有角色去执行 —— 任务只会空转。
func TestFinanceNoInstrumentUsesStockTemplate(t *testing.T) {
	// 通过 Expand 间接验证：stock 模板必须能展开出多个分析师阶段。
	// 无标的时选它就是因为维度最全（crypto 无财报/无新闻源）。
	g, err := agent.Expand(agent.AssetClassStock, agent.ExpandOpts{})
	if err != nil {
		t.Fatalf("展开 stock 阶段图失败: %v", err)
	}
	if g == nil {
		t.Fatal("stock 阶段图为 nil")
	}
	// 统计分析师节点（Role 以 analyst_ 开头）。
	// 三个维度（market/fundamental/news）是无标的时能自行选品的前提。
	n := 0
	for _, node := range g.Nodes {
		if len(node.Role) > 8 && node.Role[:8] == "analyst_" {
			n++
		}
	}
	if n < 3 {
		t.Errorf("stock 模板分析师数 = %d, 期望 >= 3（无标的时依赖它覆盖多维度）", n)
	}
}

func ptrInt64(v int64) *int64 { return &v }
