package agent

import (
	"context"
	"iter"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

type plannerDenyHook struct{ calls int }

func (h *plannerDenyHook) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	h.calls++
	return true, "blocked by test policy", nil
}
func (*plannerDenyHook) PostToolUse(context.Context, string, []byte, []byte, bool) {}
func (*plannerDenyHook) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return false, nil, ""
}

func TestPlannerUsesExecutionGuard(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	es := d.Exploration(mustExploration(t, d))
	turn := 0
	provider := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
		turn++
		events := []llm.StreamEvent{{Type: llm.SETextDelta, Text: "done"}, {Type: llm.SEMessageDelta, StopReason: "end_turn"}}
		if turn == 1 {
			events = []llm.StreamEvent{{Type: llm.SEToolUseStart, ToolID: "probe", ToolName: "graph_overview"}, {Type: llm.SEToolInputJSON, Text: `{}`}, {Type: llm.SEMessageDelta, StopReason: "tool_use"}}
		}
		for _, event := range events {
			if !yield(event, nil) {
				return
			}
		}
	}}
	hook := &plannerDenyHook{}
	p := NewPlanner(provider, "test-model", t.TempDir(), nil, 0, 3)
	if _, _, err := p.Plan(context.Background(), es.ID(), nil, es, "test", nil, nil, hook); err != nil {
		t.Fatal(err)
	}
	if hook.calls != 1 {
		t.Fatalf("guard calls=%d, want 1", hook.calls)
	}
}

func TestNonFinancialPlannerKeepsDefaultTools(t *testing.T) {
	tools, _, cleanup := AugmentTools(context.Background(), "planner", actool.DefaultTools())
	defer cleanup()
	for _, tool := range tools {
		if tool.Name() == "Bash" {
			return
		}
	}
	t.Fatal("non-financial planner lost Bash")
}

func plannerPermissionTask(t *testing.T, d *db.DB, bound bool) *db.Task {
	t.Helper()
	task, err := d.CreateTask("planner 权限回归", "验证权限", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := d.Exec(`DELETE FROM tasks WHERE id=$1`, task.ID); err != nil {
			t.Error(err)
		}
		if _, err := d.Exec(`DELETE FROM explorations WHERE id=$1`, task.ExplorationID); err != nil {
			t.Error(err)
		}
	})
	if bound {
		id, err := d.Instruments().InsertInstrument(db.InstrumentInput{Market: "crypto", Symbol: t.Name(), AssetClass: "crypto"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = d.Exec(`UPDATE tasks SET instrument_id=NULL WHERE id=$1`, task.ID)
			_, _ = d.Exec(`DELETE FROM instruments WHERE id=$1`, id)
		})
		if _, err := d.Exec(`UPDATE tasks SET instrument_id=$1 WHERE id=$2`, id, task.ID); err != nil {
			t.Fatal(err)
		}
	}
	return task
}

func TestPlannerTaskAuthority(t *testing.T) {
	for _, name := range []string{"绑定标的空图", "绑定标的缺少类别", "任务不存在", "任务探索不匹配", "运行探索不匹配", "数据库失败", "无探索上下文", "通用任务"} {
		t.Run(name, func(t *testing.T) {
			d := testDB(t)
			t.Cleanup(func() { d.Close() })
			task := plannerPermissionTask(t, d, strings.HasPrefix(name, "绑定标的"))
			es := d.Exploration(task.ExplorationID)
			ri := RunInfo{TaskID: task.ID, ExplorationID: es.ID()}
			switch name {
			case "绑定标的缺少类别":
				if _, err := es.AddNode(db.KindIntent, map[string]any{"summary": "分析"}, 1, "open", "system", nil); err != nil {
					t.Fatal(err)
				}
			case "任务不存在":
				ri.TaskID = -1
			case "任务探索不匹配":
				es = d.Exploration(mustExploration(t, d))
				ri.ExplorationID = es.ID()
			case "运行探索不匹配":
				ri.ExplorationID++
			case "数据库失败":
				conn, err := db.Open(testDBPath(t))
				if err != nil {
					t.Fatal(err)
				}
				es = conn.Exploration(task.ExplorationID)
				if err := conn.Close(); err != nil {
					t.Fatal(err)
				}
			case "无探索上下文":
				es = nil
			}
			ctx := WithRunInfo(WithExplorationStore(context.Background(), es), ri)
			augmentCalled := false
			oldAugment := ToolAugment
			t.Cleanup(func() { ToolAugment = oldAugment })
			ToolAugment = func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
				augmentCalled = true
				return nil, DeferredInfo{}, nil
			}
			tools, _, cleanup := AugmentTools(ctx, "planner", actool.DefaultTools())
			defer cleanup()
			wantBash := name == "通用任务"
			hasBash := false
			for _, tool := range tools {
				hasBash = hasBash || tool.Name() == "Bash"
			}
			if hasBash != wantBash {
				t.Errorf("Bash=%v, want %v", hasBash, wantBash)
			}
			if augmentCalled != wantBash {
				t.Errorf("ToolAugment called=%v, want %v", augmentCalled, wantBash)
			}
		})
	}
}

type plannerPromptProvider struct {
	captureUsageProvider
	request llm.CompletionRequest
}

func (p *plannerPromptProvider) Stream(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	p.request = req
	return func(yield func(llm.StreamEvent, error) bool) {
		yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
	}
}

func TestPlannerBoundTaskPrompt(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	task := plannerPermissionTask(t, d, true)
	provider := &plannerPromptProvider{}
	p := NewPlanner(provider, "test-model", t.TempDir(), nil, 0, 1)
	p.SetWebSearch(WebSearchOpts{Enabled: true})
	if _, _, err := p.Plan(context.Background(), task.ID, nil, d.Exploration(task.ExplorationID), "验证权限", nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(provider.request.System, "\n"), "【金融规划权限】") {
		t.Error("绑定标的空图缺少金融权限提示")
	}
	allowed := map[string]bool{"TodoWrite": true}
	for _, tool := range NewToolSet(d.Exploration(task.ExplorationID), "planner").PlannerTools() {
		allowed[tool.Name()] = true
	}
	seenTodo := false
	for _, tool := range provider.request.Tools {
		if !allowed[tool.Name] {
			t.Errorf("金融 planner 最终工具集合暴露 %s", tool.Name)
		}
		seenTodo = seenTodo || tool.Name == "TodoWrite"
	}
	if !seenTodo {
		t.Error("金融 planner 丢失 TodoWrite")
	}
}

func TestFinancialPlannerToolsCannotBeReintroduced(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	es := d.Exploration(mustExploration(t, d))
	if _, err := es.AddNode(db.KindIntent, map[string]any{"asset_class": "crypto", "role": "screener"}, 1, "open", "system", nil); err != nil {
		t.Fatal(err)
	}
	oldAugment, oldResolve := ToolAugment, ToolResolve
	defer func() { ToolAugment, ToolResolve = oldAugment, oldResolve }()
	ToolAugment = func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
		return actool.DefaultTools(), DeferredInfo{Deferred: []string{"Bash"}, GlobalNames: []string{"Bash"}}, func() {}
	}
	ToolResolve = func(_ context.Context, _ string, tools []actool.CoreTool) []actool.CoreTool {
		return append(tools, actool.DefaultTools()...)
	}
	base := append(NewToolSet(es, "planner").PlannerTools(), actool.DefaultTools()...)
	tools, def, cleanup := AugmentTools(WithExplorationStore(context.Background(), es), "planner", base)
	defer cleanup()
	allowed := map[string]bool{}
	for _, tool := range NewToolSet(es, "planner").PlannerTools() {
		allowed[tool.Name()] = true
	}
	allowed["TodoWrite"] = true
	for _, tool := range tools {
		if !allowed[tool.Name()] {
			t.Errorf("financial planner exposes %s", tool.Name())
		}
	}
	if len(def.Deferred) != 0 || len(def.GlobalNames) != 0 || def.UnlockSkill != nil {
		t.Fatal("financial planner exposes deferred tools")
	}
}
