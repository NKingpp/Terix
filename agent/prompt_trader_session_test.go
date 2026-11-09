package agent

// trader 的交易时段三态分支测试。
//
// 三态是最容易写错 {{if}}{{else if}}{{else}} 的地方，而三态混同的后果很实在：
// SessionError 非空但被当成「已闭市」，trader 会建议「等下一次开盘」，
// 而实际可能正在盘中 —— 一句「等开盘」就错过了一个交易日。

import (
	"strings"
	"testing"
)

// traderVarsForSession 造一个最小可渲染的 TraderVars。
func traderVarsForSession() TraderVars {
	return TraderVars{
		Instrument:         InstrumentView{Symbol: "SPY", AssetClass: "etf", Market: "us"},
		AsOf:               "2026-10-26",
		ResearchConclusion: "bull_wins",
		LastPrice:          "580.12",
		AccountMode:        "paper",
	}
}

func renderTrader(t *testing.T, v TraderVars) string {
	t.Helper()
	tmpl := BuiltinPromptSeeds()["trader"]
	if tmpl == "" {
		t.Fatal("BuiltinPromptSeeds 缺 trader")
	}
	out := renderSystem("trader", tmpl, v)
	if strings.Contains(out, "{{") {
		idx := strings.Index(out, "{{")
		end := idx + 80
		if end > len(out) {
			end = len(out)
		}
		t.Fatalf("渲染有残留占位符（位置 %d）：%q", idx, out[idx:end])
	}
	return out
}

// TestTraderSessionErrorBranch SessionError 非空 → 必须说「未知」，不能说「已闭市」或「等开盘」。
func TestTraderSessionErrorBranch(t *testing.T) {
	v := traderVarsForSession()
	v.SessionError = "未知 market: xx"
	v.TradingSession = TradingSessionView{} // 全空 —— 模拟日历彻底失败

	out := renderTrader(t, v)

	if !strings.Contains(out, "交易时段未知") {
		t.Error("SessionError 非空时应明确说「交易时段未知」")
	}
	if !strings.Contains(out, "建议人工确认") {
		t.Error("应建议人工确认，而不是让 trader 自己猜")
	}
	if !strings.Contains(out, "你可能正处在盘中") {
		t.Error("必须点明「可能正处在盘中」—— 这是三态混同的核心危害")
	}
	// 不该出现的：把「未知」当「已闭市」
	if strings.Contains(out, "已闭市。下一个开盘") {
		t.Error("SessionError 非空时不该渲染「已闭市」分支 —— 那会让 trader 等开盘而实际可能在盘中")
	}
}

// TestTraderIsOpenBranch 开市中 → 说「在交易时段内」，不说「等开盘」。
func TestTraderIsOpenBranch(t *testing.T) {
	v := traderVarsForSession()
	v.TradingSession = TradingSessionView{
		Open:     "2026-10-26T13:30:00Z",
		Close:    "2026-10-26T20:00:00Z",
		IsOpen:   true,
		Timezone: "America/New_York",
	}

	out := renderTrader(t, v)

	if !strings.Contains(out, "在交易时段内") {
		t.Error("IsOpen=true 时应说「在交易时段内」")
	}
	if !strings.Contains(out, "America/New_York") {
		t.Error("应显示时区（否则 trader 不知道 Open/Close 是哪个时区的）")
	}
	if !strings.Contains(out, "13:30:00Z") {
		t.Error("应显示开盘时间")
	}
	if strings.Contains(out, "下一个开盘是") {
		t.Error("IsOpen=true 时不该出现「下一个开盘」")
	}
}

// TestTraderClosedBranch 闭市 → 说「不在交易时段」+ 给 NextOpen。
func TestTraderClosedBranch(t *testing.T) {
	v := traderVarsForSession()
	v.TradingSession = TradingSessionView{
		IsOpen:   false,
		NextOpen: "2026-10-27T13:30:00Z",
		Timezone: "America/New_York",
	}

	out := renderTrader(t, v)

	if !strings.Contains(out, "不在交易时段") {
		t.Error("IsOpen=false 且无 SessionError 时应说「不在交易时段」")
	}
	if !strings.Contains(out, "2026-10-27T13:30:00Z") {
		t.Error("应给出下一个开盘时间")
	}
	// 注意：反模式清单里也有「交易时段未知」这几个字（那是规则说明），
	// 所以要检查分支正文的独有文案，而不是关键词。
	if strings.Contains(out, "日历查询失败") {
		t.Error("SessionError 为空时不该渲染「日历查询失败」分支")
	}
}

// TestTraderAccountModeBranches paper 与实盘的措辞必须不同。
func TestTraderAccountModeBranches(t *testing.T) {
	base := traderVarsForSession()
	base.TradingSession = TradingSessionView{IsOpen: true, Timezone: "America/New_York"}

	paper := base
	paper.AccountMode = "paper"
	outPaper := renderTrader(t, paper)
	if !strings.Contains(outPaper, "模拟盘") {
		t.Error("AccountMode=paper 时应说「模拟盘」")
	}
	if !strings.Contains(outPaper, "不要说「即将下单」") {
		t.Error("paper 模式应禁止「即将下单」的措辞")
	}

	live := base
	live.AccountMode = "live_whitelist"
	outLive := renderTrader(t, live)
	if strings.Contains(outLive, "本次为**模拟盘**") {
		t.Error("实盘模式不该出现「模拟盘」")
	}
	if !strings.Contains(outLive, "实盘授权模式") {
		t.Error("实盘模式应说明「实盘授权模式」")
	}
	if !strings.Contains(outLive, "不要写「已下单」") {
		t.Error("实盘模式必须禁止「已下单/已成交」—— 输出是意图不是指令")
	}
}
