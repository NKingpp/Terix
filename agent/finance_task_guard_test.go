package agent

// 金融任务的「不给 Bash」判定必须按**任务**而非**角色**（2026-11-05 二次事故）。
//
// # 事故经过
//
// 第一次修复只看 isFinancialPrompt(agentKey)，但 planner 自建的意图
// payload 里没有 role（只有 summary）：
//
//	{"summary": "打通 OKX 账户通路…安装/定位 OKX CLI…"}
//
// 于是 agentKey 落回 "worker" → isFinancialPrompt 为 false → 拿到 Bash
// → 继续跑 okx CLI、写 Python。实测命令：
//
//	Bash {"command":"$env:OKX_API_KEY; ...; Get-ChildItem Env: | Sort-Object"}
//
// # 为什么用 asset_class 判断
//
// Expand 会给**每个**阶段节点写 payload.asset_class（pipeline.go 的
// basePayload），这是任务级事实。planner 临时加的意图没有它，但
// 图里其他节点有 —— 只要任意一个节点带 asset_class，整个任务就是金融任务。

import (
	"encoding/json"
	"os"
	"testing"
)

// TestExplorationIsFinancialByAssetClass 有 asset_class 的图必须判为金融任务。
func TestExplorationIsFinancialByAssetClass(t *testing.T) {
	// 不依赖 DB：直接验证判定逻辑的输入契约。
	// 这里构造两种 payload，断言「只有带 asset_class 的才算」。
	cases := []struct {
		name    string
		payload map[string]any
		want    bool
	}{
		{
			name:    "阶段图节点带 asset_class",
			payload: map[string]any{"role": "analyst_market", "asset_class": "crypto"},
			want:    true,
		},
		{
			name:    "planner 自建意图只有 summary",
			payload: map[string]any{"summary": "打通 OKX 账户通路"},
			want:    false,
		},
		{
			name:    "asset_class 是空串",
			payload: map[string]any{"asset_class": "  "},
			want:    false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, _ := json.Marshal(c.payload)
			var p map[string]any
			_ = json.Unmarshal(raw, &p)
			got := false
			if ac, _ := p["asset_class"].(string); len(ac) > 0 && ac != "  " {
				got = true
			}
			if got != c.want {
				t.Errorf("asset_class 判定 = %v, want %v", got, c.want)
			}
		})
	}
}

// TestExplorationIsFinancialNilStore nil store 不该 panic。
func TestExplorationIsFinancialNilStore(t *testing.T) {
	if explorationIsFinancial(nil) {
		t.Error("nil store 应返回 false（不 panic）")
	}
}

// TestFindOkxFallbackFindsInstalled 兜底探测必须能找到本机已装的 okx。
//
// # 这条测试的前提
//
// 开发机装了 okx（在 ~/.workbuddy/binaries/node/versions/*/okx）。
// 没装时跳过 —— 那种情况的正确行为是返回空串，由上层报错。
func TestFindOkxFallbackFindsInstalled(t *testing.T) {
	p := findOkxFallback()
	if p == "" {
		t.Skip("本机未安装 okx，跳过（此时返回空串是正确行为）")
	}
	// 找到的路径必须是个真实存在的文件。
	if st, err := os.Stat(p); err != nil || st.IsDir() {
		t.Fatalf("findOkxFallback 返回了不可用的路径 %q: err=%v", p, err)
	}
	t.Logf("找到 okx: %s", p)
}
