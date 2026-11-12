package server

import (
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

func TestIntentOutcomeRequiresRoleSpecificPersistentOutput(t *testing.T) {
	d := testOpenDB(t)
	expID, err := d.CreateExploration("outcome", "outcome test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := d.Exec(`DELETE FROM explorations WHERE id=$1`, expID); err != nil {
			t.Errorf("cleanup exploration: %v", err)
		}
	})
	es := d.Exploration(expID)
	instID := upsertTestInstrument(t, d, db.InstrumentInput{Market: "crypto", Symbol: "OUTCOME", AssetClass: "crypto"})
	if _, err := d.Exec(`INSERT INTO tasks(name,description,goal,exploration_id,status,instrument_id) VALUES('outcome','outcome','test',$1,'running',$2)`, expID, instID); err != nil {
		t.Fatal(err)
	}
	var taskID int64
	if err := d.QueryRow(`SELECT id FROM tasks WHERE exploration_id=$1`, expID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	intent := func(role string) int64 {
		t.Helper()
		id, err := es.AddNode(db.KindIntent, map[string]any{"role": role}, 1, "running", "worker", nil)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	analystID := intent(agent.RoleAnalystMarket)
	if ok, reason, err := intentHasRequiredOutput(es, analystID, agent.RoleAnalystMarket, nil, 1); err != nil || ok || reason == "" {
		t.Fatalf("analyst without fact: ok=%v reason=%q err=%v", ok, reason, err)
	}
	factID, err := es.AddNode(db.KindFact, map[string]any{"role": agent.RoleAnalystMarket}, 1, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := es.Link(analystID, db.RelYields, factID); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := intentHasRequiredOutput(es, analystID, agent.RoleAnalystMarket, nil, 1); err != nil || !ok {
		t.Fatalf("analyst with yielded fact: ok=%v err=%v", ok, err)
	}

	traderID := intent(agent.RoleTrader)
	if ok, reason, err := intentHasRequiredOutput(es, traderID, agent.RoleTrader, nil, 1); err != nil || ok || reason == "" {
		t.Fatalf("trader without signal: ok=%v reason=%q err=%v", ok, reason, err)
	}
	if _, err := d.Signals().InsertSignal(db.SignalInput{TaskID: &taskID, InstrumentID: &instID, NodeID: &traderID, SignalType: "technical", Direction: "long"}); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := intentHasRequiredOutput(es, traderID, agent.RoleTrader, &instID, taskID); err != nil || !ok {
		t.Fatalf("trader with matching signal: ok=%v err=%v", ok, err)
	}
}
