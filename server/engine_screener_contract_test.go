package server

// 选品员必须有可核验的产出契约。
//
// # 为什么（2026-11-14 任务 30 卡死）
//
// screener 是金融角色但不在 intentHasRequiredOutput 的白名单里，
// 于是掉进 `case agent.IsFinancialRole(role)` 兜底 → 永远返回
// 「金融角色 screener 尚无可核验的产出契约」→ 意图 blocked。
//
// 实测任务 30 的两个 screener 意图（2767 / 2783）都被这一条判死，
// 而第二次判死时它其实已经绑好标的、写了 fact —— blocked 的理由
// 与「有没有产出」无关，纯粹是这个 switch 没写 screener 分支。

import (
	"testing"

	"github.com/Autumn-27/artex/agent"
)

func TestScreenerHasOutputContract(t *testing.T) {
	e := newTradeE2E(t)
	ri := e.runInfo(t, nil) // 不绑标的
	store := e.pg.Exploration(ri.ExplorationID)

	// 未绑标的 → 不达标（这是选品员真的没干活）
	ok, reason, err := intentHasRequiredOutput(store, ri.IntentID, agent.RoleScreener, nil, ri.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("未绑定标的时不该判达标")
	}
	if reason == "" || reason == "金融角色 screener 尚无可核验的产出契约" {
		t.Errorf("拒绝理由应指向「未绑定标的」，实际: %q", reason)
	}

	// 绑定后 → 达标（绑标的就是选品员的全部产出）
	ok, reason, err = intentHasRequiredOutput(store, ri.IntentID, agent.RoleScreener, &e.instID, ri.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("已绑定标的应判达标，实际: %s", reason)
	}
}
