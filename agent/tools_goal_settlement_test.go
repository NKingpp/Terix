package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

func goalSettlementFixture(t *testing.T) (*db.DB, *ToolSet, int64, int64) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "goals.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	expID, err := d.CreateExploration("目标验收", "验证目标")
	if err != nil {
		t.Fatal(err)
	}
	tools := NewToolSet(d.Exploration(expID), "planner")
	goal, err := tools.ts.AddGoal(map[string]any{"text": "收益达到 10%"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := tools.ts.AddNode(db.KindFact, map[string]any{"text": "分析已完成，声称收益达到 10%"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	return d, tools, goal, evidence
}

func callGoalSettlement(t *testing.T, tool actool.CoreTool, input string) actool.Result {
	t.Helper()
	result, err := tool.Call(context.Background(), json.RawMessage(input), nil)
	if err != nil {
		t.Fatalf("工具调用失败: %v", err)
	}
	return result
}

func TestFinancialGoalSettlementRejectsTextEvidence(t *testing.T) {
	for _, mode := range []string{"asset_class", "instrument_id", "registered", "registered_without_task_id", "finance_mode"} {
		for _, toolName := range []string{"prove_goal", "goal_met"} {
			t.Run(mode+"/"+toolName, func(t *testing.T) {
				d, tools, goal, evidence := goalSettlementFixture(t)
				switch mode {
				case "registered", "registered_without_task_id":
					instID, _, err := d.Instruments().UpsertInstrument(db.InstrumentInput{Market: "crypto", Symbol: "TINSTR-GOAL", AssetClass: "crypto", Name: "验收测试"})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						for _, q := range []string{"DELETE FROM ratings WHERE instrument_id=$1", "DELETE FROM signals WHERE instrument_id=$1", "DELETE FROM orders_draft WHERE instrument_id=$1", "DELETE FROM paper_orders WHERE instrument_id=$1", "DELETE FROM tasks WHERE instrument_id=$1", "DELETE FROM instruments WHERE id=$1"} {
							if _, err := d.Exec(q, instID); err != nil {
								t.Errorf("cleanup %s: %v", q, err)
							}
						}
					})
					if _, err := d.Exec(`INSERT INTO tasks(name,description,goal,exploration_id,status,instrument_id) VALUES('金融验收','金融验收','收益达到 10%',$1,'running',$2)`, tools.ts.ID(), instID); err != nil {
						t.Fatal(err)
					}
					if mode == "registered" {
						var taskID int64
						if err := d.QueryRow(`SELECT id FROM tasks WHERE exploration_id=$1`, tools.ts.ID()).Scan(&taskID); err != nil {
							t.Fatal(err)
						}
						tools.SetTaskID(taskID)
					}
				case "finance_mode":
					tools.financeMode = true
				default:
					payload := map[string]any{"asset_class": "crypto"}
					if mode == "instrument_id" {
						payload = map[string]any{"instrument_id": 1}
					}
					if _, err := tools.ts.AddNode(db.KindFact, payload, 0, "confirmed", "worker", nil); err != nil {
						t.Fatal(err)
					}
				}
				tool, input := tools.proveGoal(), fmt.Sprintf(`{"goal_id":%d,"evidence_id":%d}`, goal, evidence)
				if toolName == "goal_met" {
					tool, input = tools.goalMet(), `{"reason":"分析已完成，收益达到 10%"}`
				}
				result := callGoalSettlement(t, tool, input)
				if !result.IsError || !strings.Contains(result.Flatten(), "缺少结构化业务验收，需人工核验") {
					t.Errorf("文本证据不得完成金融目标: IsError=%v result=%s", result.IsError, result.Flatten())
				}
				if tools.GoalMet || tools.Reason != "" {
					t.Errorf("拒绝后仍报告任务完成: GoalMet=%v Reason=%q", tools.GoalMet, tools.Reason)
				}
				node, err := tools.ts.GetNode(goal)
				if err != nil || node.State != "open" {
					t.Fatalf("拒绝后目标被改动: node=%+v err=%v", node, err)
				}
				var edges int
				if err := d.QueryRow(`SELECT COUNT(*) FROM exploration_edges WHERE rel='proves'`).Scan(&edges); err != nil || edges != 0 {
					t.Fatalf("拒绝后留下证明边: edges=%d err=%v", edges, err)
				}
			})
		}
	}
}

func TestGoalSettlementValidatesInputAndContext(t *testing.T) {
	for _, toolName := range []string{"prove_goal", "goal_met"} {
		for _, failure := range []string{"json", "closed_db", "wrong_task", "missing_task", "missing_exploration", "invalid_input"} {
			t.Run(toolName+"/"+failure, func(t *testing.T) {
				d, tools, goal, evidence := goalSettlementFixture(t)
				input := fmt.Sprintf(`{"goal_id":%d,"evidence_id":%d}`, goal, evidence)
				if toolName == "goal_met" {
					input = `{"reason":"完整证据已核验"}`
				}
				switch failure {
				case "json":
					if toolName == "prove_goal" {
						input = input[:len(input)-1] + `,"reason":42}`
					} else {
						input = `{"reason":42}`
					}
				case "closed_db":
					if err := d.Close(); err != nil {
						t.Fatal(err)
					}
				case "wrong_task":
					task, err := d.CreateTask("另一个任务", "独立目标", nil, 0, 0)
					if err != nil {
						t.Fatal(err)
					}
					tools.SetTaskID(task.ID)
				case "missing_task":
					tools.SetTaskID(99999999)
				case "missing_exploration":
					tools.ts = d.Exploration(99999999)
				case "invalid_input":
					input = `{"goal_id":-1,"evidence_id":-2}`
					if toolName == "goal_met" {
						input = `{"reason":" \n\t "}`
					}
				}
				tool := tools.proveGoal()
				if toolName == "goal_met" {
					tool = tools.goalMet()
				}
				result := callGoalSettlement(t, tool, input)
				if !result.IsError || tools.GoalMet || tools.Reason != "" {
					t.Fatalf("无效请求被接受: result=%s GoalMet=%v Reason=%q", result.Flatten(), tools.GoalMet, tools.Reason)
				}
			})
		}
	}
}

func TestProveGoalRollsBackWriteErrors(t *testing.T) {
	for _, table := range []string{"exploration_edges", "exploration_nodes"} {
		t.Run(table, func(t *testing.T) {
			d, tools, goal, evidence := goalSettlementFixture(t)
			event := "INSERT"
			if table == "exploration_nodes" {
				event = "UPDATE"
			}
			if _, err := d.Exec(fmt.Sprintf(`CREATE TRIGGER fail_goal_write BEFORE %s ON %s BEGIN SELECT RAISE(ABORT,'goal write failed'); END`, event, table)); err != nil {
				t.Fatal(err)
			}
			result := callGoalSettlement(t, tools.proveGoal(), fmt.Sprintf(`{"goal_id":%d,"evidence_id":%d}`, goal, evidence))
			if !result.IsError || !strings.Contains(result.Flatten(), "goal write failed") || tools.GoalMet {
				t.Errorf("写失败仍成功: result=%s GoalMet=%v", result.Flatten(), tools.GoalMet)
			}
			node, err := tools.ts.GetNode(goal)
			if err != nil || node.State != "open" {
				t.Errorf("写失败未回滚目标: node=%+v err=%v", node, err)
			}
			var edges int
			if err := d.QueryRow(`SELECT COUNT(*) FROM exploration_edges WHERE rel='proves'`).Scan(&edges); err != nil || edges != 0 {
				t.Errorf("写失败未回滚证明边: edges=%d err=%v", edges, err)
			}
		})
	}
}

func TestProveGoalCountsAllGoals(t *testing.T) {
	d, tools, oldest, evidence := goalSettlementFixture(t)
	if _, err := d.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<1000)
		INSERT INTO exploration_nodes(exploration_id,kind,payload,priority,state)
		SELECT $1,'goal','{"text":"已核验目标"}',0,'met' FROM n`, tools.ts.ID()); err != nil {
		t.Fatal(err)
	}
	var latest int64
	if err := d.QueryRow(`SELECT MAX(id) FROM exploration_nodes WHERE kind='goal'`).Scan(&latest); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE exploration_nodes SET state='met' WHERE exploration_id=$1 AND kind='goal' AND id<>$2`, tools.ts.ID(), oldest); err != nil {
		t.Fatal(err)
	}
	input := fmt.Sprintf(`{"goal_id":%d,"evidence_id":%d}`, latest, evidence)
	if result := callGoalSettlement(t, tools.proveGoal(), input); result.IsError || tools.GoalMet {
		t.Fatalf("仍有最旧目标未完成时不得自动收官: result=%s GoalMet=%v", result.Flatten(), tools.GoalMet)
	}
	input = fmt.Sprintf(`{"goal_id":%d,"evidence_id":%d}`, oldest, evidence)
	if result := callGoalSettlement(t, tools.proveGoal(), input); result.IsError || !tools.GoalMet || !strings.Contains(tools.Reason, "1001") {
		t.Fatalf("全部目标未完成收官: result=%s GoalMet=%v Reason=%q", result.Flatten(), tools.GoalMet, tools.Reason)
	}
}

func TestNonFinancialGoalMetAcceptsReason(t *testing.T) {
	_, tools, goal, _ := goalSettlementFixture(t)
	if err := tools.ts.SetNodeState(goal, "met"); err != nil {
		t.Fatal(err)
	}
	result := callGoalSettlement(t, tools.goalMet(), `{"reason":"  全部证据已核验  "}`)
	if result.IsError || !tools.GoalMet || strings.TrimSpace(tools.Reason) != "全部证据已核验" {
		t.Fatalf("非金融目标不能正常完成: result=%s GoalMet=%v Reason=%q", result.Flatten(), tools.GoalMet, tools.Reason)
	}
}
