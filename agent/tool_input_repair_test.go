package agent

// 工具入参修复的验收测试（2026-11-05）。
//
// # 事故
//
// 模型一次发多个并行工具调用时，参数被拼成一条字节串：
//
//	get_kline {"symbol":"BTC-USDT"}{"symbol":"ETH-USDT"}
//	→ invalid character '{' after top-level value
//
// 于是退化成空调用 `get_kline {}` → 「缺少 symbol」→ 反复十几轮，
// 全流程卡在选品阶段跑不完（三次复现）。
//
// 修法：工具层先尝试「只取第一个 JSON 对象」，修好了正常执行并告知模型。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

// TestRepairToolInputSplitsConcatenated 拼接的多个对象只取第一个。
func TestRepairToolInputSplitsConcatenated(t *testing.T) {
	raw := `{"symbol":"BTC-USDT","market":"crypto"}{"symbol":"ETH-USDT","market":"crypto"}`
	fixed, repaired := repairToolInput([]byte(raw))
	if !repaired {
		t.Fatal("应识别为可修复（多个对象拼接）")
	}
	var got map[string]any
	if err := json.Unmarshal(fixed, &got); err != nil {
		t.Fatalf("修复结果不是合法 JSON: %v (%s)", err, fixed)
	}
	if got["symbol"] != "BTC-USDT" {
		t.Errorf("应保留第一个对象的 symbol，实际 %v", got["symbol"])
	}
}

// TestRepairToolInputLeavesSingleObjectAlone 单个正常对象不该被改。
func TestRepairToolInputLeavesSingleObjectAlone(t *testing.T) {
	raw := `{"symbol":"BTC-USDT"}`
	fixed, repaired := repairToolInput([]byte(raw))
	if repaired {
		t.Error("单个对象不该被判为需要修复")
	}
	if string(fixed) != raw {
		t.Errorf("单个对象不该被改动: %s", fixed)
	}
}

// TestRepairToolInputRejectsTruncated 被截断的 JSON 不该硬猜。
//
// # 为什么不修
//
// 末尾缺 `}` 时无法可靠推断原意（是漏了括号，还是模型写到一半被截断）。
// 猜错比报错更糟 —— 会让工具拿一个错误的 symbol 去取数，
// 而模型以为成功了。
func TestRepairToolInputRejectsTruncated(t *testing.T) {
	raw := `{"symbol":"BTC-USDT"`
	_, repaired := repairToolInput([]byte(raw))
	if repaired {
		t.Error("截断的 JSON 不该被修复（无法可靠推断原意）")
	}
}

// TestRepairToolInputIgnoresNonObject 非对象入参原样返回。
func TestRepairToolInputIgnoresNonObject(t *testing.T) {
	for _, raw := range []string{`[1,2,3]`, `"str"`, `123`, ``} {
		if _, repaired := repairToolInput([]byte(raw)); repaired {
			t.Errorf("%q 不该被判为可修复", raw)
		}
	}
}

// TestReadToolRepairsAndRuns 端到端：拼接入参也能正常执行。
//
// 这是修复的最终目的 —— 不只「检测到」，而是「照常跑通」。
func TestReadToolRepairsAndRuns(t *testing.T) {
	var gotSymbol string
	tool := readTool("probe", "probe",
		obj(map[string]any{"symbol": str("symbol")}, "symbol"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Symbol string `json:"symbol"`
			}
			_ = json.Unmarshal(in, &a)
			gotSymbol = a.Symbol
			return actool.Text("ok:" + a.Symbol), nil
		})

	raw := `{"symbol":"BTC-USDT"}{"symbol":"ETH-USDT"}`
	res, err := tool.Call(context.Background(), json.RawMessage(raw), nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if gotSymbol != "BTC-USDT" {
		t.Errorf("应执行第一个对象（symbol=BTC-USDT），实际 %q", gotSymbol)
	}
	out := res.Flatten()
	if !strings.Contains(out, "ok:BTC-USDT") {
		t.Errorf("结果应含执行输出，实际 %s", out)
	}
	if !strings.Contains(out, "逐个单独调用") {
		t.Errorf("应告知模型分次调用，否则它下次还这么发。实际: %s", out)
	}
}

// TestReadToolStillValidatesMissingRequired 真漏参数时仍报错。
//
// # 为什么保留校验
//
// 修复只针对「拼接」这一种形态。如果模型真的漏了 required 字段，
// 必须明确报错 —— 否则它会拿着空参数去取数，然后困惑于"没有数据"。
func TestReadToolStillValidatesMissingRequired(t *testing.T) {
	tool := readTool("probe2", "probe2",
		obj(map[string]any{"symbol": str("symbol")}, "symbol"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			t.Error("缺 required 字段时不该执行到 handler")
			return actool.Text("should not run"), nil
		})

	res, err := tool.Call(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	out := res.Flatten()
	if !strings.Contains(out, "symbol") {
		t.Errorf("应报缺失字段名，实际: %s", out)
	}
}

// TestDecoratedToolForwardsAcceptsRawInput 装饰器必须转发 AcceptsRawInput。
//
// # 为什么这条必须存在（2026-11-05 三次事故）
//
// harness 用类型断言发现 RawInput：
//
//	type rawInputTool interface{ AcceptsRawInput() bool }
//
// 而该方法刻意不在 CoreTool 里（加进公开接口会破坏实现方）。
// 所以 struct 嵌入 CoreTool **不会**提升它 —— 装饰后断言失败，
// RawInput 被静默丢弃，harness 继续先做 schema 校验，
// 「多个 JSON 拼接」的入参必被拒，全流程卡死。
//
// 这条测试防的是「有人后又加了一个装饰器却忘了转发」。
func TestDecoratedToolForwardsAcceptsRawInput(t *testing.T) {
	raw := readTool("probe3", "probe3",
		obj(map[string]any{"symbol": str("symbol")}, "symbol"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			return actool.Text("ok"), nil
		})
	// readTool 设了 RawInput=true，原工具应报告 true。
	if r, ok := raw.(interface{ AcceptsRawInput() bool }); !ok || !r.AcceptsRawInput() {
		t.Fatal("readTool 应实现 AcceptsRawInput 并返回 true")
	}

	dec := DecorateTool(raw, "desc", nil)
	r, ok := dec.(interface{ AcceptsRawInput() bool })
	if !ok {
		t.Fatal("装饰后的工具缺 AcceptsRawInput —— harness 的 RawInput 会失效")
	}
	if !r.AcceptsRawInput() {
		t.Error("装饰后应转发原工具的 true")
	}
}
