package agent

import (
	"context"
	"strings"
	"testing"
)

// wait_for 是金融任务唯一的「合法空转」出口：目标只能人工验收时，
// planner 必须能声明等待，而不是每轮被唤醒重判一遍。
func TestWaitForToolParksPlanner(t *testing.T) {
	ts := newEmitTestSet(t)
	var gotReason string
	var gotMinutes int
	ts.waitFor = func(reason string, minutes int) (string, error) {
		gotReason, gotMinutes = reason, minutes
		return "2026-11-20 12:30", nil
	}

	out, isErr := callWithCtx(t, ts.waitForTool(), context.Background(),
		map[string]any{"reason": "等待操作员验收 6 个目标", "minutes": 120})
	if isErr {
		t.Fatalf("合法调用不该报错: %q", out)
	}
	if gotReason != "等待操作员验收 6 个目标" || gotMinutes != 120 {
		t.Fatalf("回调收到的参数不对: reason=%q minutes=%d", gotReason, gotMinutes)
	}
	if !strings.Contains(out, "2026-11-20 12:30") {
		t.Fatalf("返回值应回显到点时间: %q", out)
	}
}

func TestWaitForToolRejectsBadInput(t *testing.T) {
	ts := newEmitTestSet(t)
	called := false
	ts.waitFor = func(string, int) (string, error) { called = true; return "", nil }

	cases := []map[string]any{
		{"minutes": 30},                  // 缺 reason
		{"reason": "  ", "minutes": 30},  // reason 空白
		{"reason": "等验收"},                // 缺 minutes
		{"reason": "等验收", "minutes": 0},  // minutes 非正
		{"reason": "等验收", "minutes": -5}, // 负数
	}
	for _, in := range cases {
		if out, isErr := callWithCtx(t, ts.waitForTool(), context.Background(), in); !isErr {
			t.Fatalf("非法规格 %v 应报错，得到 %q", in, out)
		}
	}
	if called {
		t.Fatal("参数非法时不该落到引擎回调")
	}
}

// 未装配引擎回调时明确报不可用（而不是静默成功 —— 静默成功会让 planner
// 以为自己挂起了，实际还在每 10 分钟被唤醒）。
func TestWaitForToolUnavailableWithoutWiring(t *testing.T) {
	ts := newEmitTestSet(t)
	out, isErr := callWithCtx(t, ts.waitForTool(), context.Background(),
		map[string]any{"reason": "等验收", "minutes": 60})
	if !isErr || !strings.Contains(out, "不可用") {
		t.Fatalf("未装配时应报不可用，isErr=%v out=%q", isErr, out)
	}
}
