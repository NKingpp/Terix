package exec

// 持仓漂移检测的测试。
//
// # 这些断言钉住什么
//
// 2026-11-15 那次不一致（本地 -68.95 / 交易所 0）存活了一整天，
// 因为两边都不报错、报表只是「偏小」。漂移检测的价值就是把它变成
// 一个显式结论，所以测试要钉住：
//   - 真的能比出来（别静默通过）
//   - 容差生效（小尾数不误报）
//   - 一边有一边无也算漂移（不是只比交集）

import (
	"context"
	"errors"
	"testing"

	"github.com/Autumn-27/artex/exec/okxrest"
)

type fakePosReader struct {
	rows []okxrest.Position
	err  error
}

func (f *fakePosReader) GetPositions(context.Context, string) ([]okxrest.Position, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

// 本地认为还有净空 68.95，交易所已平仓 —— 这就是 2026-11-15 的实际形态。
func TestDriftDetectsLocalPositionWithNoExchangePosition(t *testing.T) {
	c := &fakePosReader{rows: nil} // 交易所空仓
	local := map[string]string{"NVDA-USDT-SWAP": "-68.95"}

	got, err := DetectPositionDrift(context.Background(), c, local, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("应检出 1 条漂移，实际 %d", len(got))
	}
	d := got[0]
	if d.InstID != "NVDA-USDT-SWAP" || d.LocalNet != "-68.95" || d.ExchangeNet != "0" {
		t.Errorf("漂移内容不符: %+v", d)
	}
	if d.Reason == "" {
		t.Error("应给出可能成因（漏同步）")
	}
}

// 完全一致时不能报漂移 —— 否则每轮同步都在喊狼来了。
func TestDriftQuietWhenConsistent(t *testing.T) {
	c := &fakePosReader{rows: []okxrest.Position{
		{InstID: "NVDA-USDT-SWAP", PosSide: "net", Pos: "0.2"},
	}}
	local := map[string]string{"NVDA-USDT-SWAP": "0.2"}
	got, err := DetectPositionDrift(context.Background(), c, local, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("一致时不该报漂移，实际 %+v", got)
	}
}

// 容差语义：噪声不报、真实差异必报。
//
// # 为什么空容差不是「精确比对」（2026-11-15 首次上线踩到）
//
// 本地净持仓来自 SQL SUM(REAL)，浮点噪声可达 1e-17 量级；
// 交易所是十进制字符串。零容差会把这些噪声全报成漂移。
//
// 所以空容差 = 用 1e-8 的隐式噪声下限（远低于任何可交易手数），
// 而不是「一模一样才算一致」。
func TestDriftTolerance(t *testing.T) {
	c := &fakePosReader{rows: []okxrest.Position{
		{InstID: "NVDA-USDT-SWAP", PosSide: "net", Pos: "0.2"},
	}}

	// 浮点噪声级差异 → 不该报
	for _, local := range []string{"0.20000000000000001", "0.19999999999999998"} {
		if got, _ := DetectPositionDrift(context.Background(), c,
			map[string]string{"NVDA-USDT-SWAP": local}, ""); len(got) != 0 {
			t.Errorf("local=%q 是噪声级差异，不该报，实际 %+v", local, got)
		}
	}

	// 真实差异（0.01 手）→ 必须报
	if got, _ := DetectPositionDrift(context.Background(), c,
		map[string]string{"NVDA-USDT-SWAP": "0.21"}, ""); len(got) != 1 {
		t.Error("0.01 手的真实差异必须报出来")
	}

	// 调用方给更宽的容差时按其口径
	if got, _ := DetectPositionDrift(context.Background(), c,
		map[string]string{"NVDA-USDT-SWAP": "0.21"}, "0.1"); len(got) != 0 {
		t.Error("调用方容差 0.1 时应放过 0.01 的差异")
	}
}

// long_short 模式下 pos 恒正，方向要由 posSide 补 —— 否则空仓会被
// 算成多仓，本地有仓时反而「一致」，漂移检不出来。
func TestDriftLongShortDirection(t *testing.T) {
	c := &fakePosReader{rows: []okxrest.Position{
		{InstID: "X-USDT-SWAP", PosSide: "short", Pos: "3"},
	}}
	// 本地认为净空 3 → 应一致
	local := map[string]string{"X-USDT-SWAP": "-3"}
	got, err := DetectPositionDrift(context.Background(), c, local, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("netshort 3 与交易所 short 3 应一致，实际 %+v", got)
	}

	// 本地认为净多 3 → 应报漂移（差 6）
	local2 := map[string]string{"X-USDT-SWAP": "3"}
	got2, _ := DetectPositionDrift(context.Background(), c, local2, "")
	if len(got2) != 1 {
		t.Fatalf("方向反了应报漂移，实际 %+v", got2)
	}
	if got2[0].Diff != "6" {
		t.Errorf("Diff = %q, want 6（3 - (-3)）", got2[0].Diff)
	}
}

// 交易所有仓、本地无记录 —— 也是漂移（可能是手工下单）。
func TestDriftDetectsExchangeOnlyPosition(t *testing.T) {
	c := &fakePosReader{rows: []okxrest.Position{
		{InstID: "Y-USDT-SWAP", PosSide: "net", Pos: "5"},
	}}
	got, err := DetectPositionDrift(context.Background(), c, map[string]string{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("交易所独有持仓应报漂移，实际 %d", len(got))
	}
	if got[0].Reason == "" {
		t.Error("应提示可能是手工下单")
	}
}

// 读交易所失败必须返回错误，不能当成「无持仓」静默通过。
//
// 静默通过会让漂移检测在最需要它的时刻（网络/权限故障）失效。
func TestDriftPropagatesReadError(t *testing.T) {
	c := &fakePosReader{err: errors.New("boom")}
	if _, err := DetectPositionDrift(context.Background(), c, map[string]string{"Z": "1"}, ""); err == nil {
		t.Fatal("读持仓失败必须返回错误")
	}
}

// 浮点噪声不能产生假阳性漂移。
//
// # 首次上线就踩到（2026-11-15）
//
// SQL SUM 用 REAL 累加，一买一卖「正好抵消」时会算出 -1.4e-17，
// 格式化成 "-0"。而交易所返回 "0"。数值相等、字符串不等，
// 对账按字符串比就报了一条假漂移。
//
// 假阳性比漏报更糟：它会让「漂移」这个信号失去可信度，
// 下次真的不一致时没人当回事。
func TestDriftIgnoresFloatNoise(t *testing.T) {
	c := &fakePosReader{rows: []okxrest.Position{
		{InstID: "N-USDT-SWAP", PosSide: "net", Pos: "0"},
	}}
	for _, local := range []string{"-0", "0.0000000001", "-0.0000000001"} {
		got, err := DetectPositionDrift(context.Background(), c,
			map[string]string{"N-USDT-SWAP": local}, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("local=%q 应视为无漂移，实际 %+v", local, got)
		}
	}
}
