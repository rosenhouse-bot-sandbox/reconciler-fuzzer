package model

import (
	"encoding/json"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
)

func databaseModel() config.Model {
	m := gadgetModel()
	m.Conditions = []config.Condition{{Type: "Ready"}, {Type: "Synced", Reason: "ReconcileSuccess", Message: "synced", StuckReason: "ReconcileError"}}
	m.Status = map[string]any{"observedGeneration": "${self.metadata.generation}", "endpoint": map[string]any{"host": "${self.metadata.name + '.db'}", "port": 5432}}
	m.StuckStatus = map[string]any{"observedGeneration": "${self.metadata.generation}"}
	return m
}

// writtenConditions returns the type, status, reason, message and
// observedGeneration of each condition a write sets, as JSON.
func writtenConditions(t *testing.T, w write) string {
	t.Helper()
	status, _ := w.patch["status"].(map[string]any)
	conditions, _ := status["conditions"].([]any)
	var brief []any
	for _, c := range conditions {
		c := c.(map[string]any)
		brief = append(brief, []any{c["type"], c["status"], c["reason"], c["message"], c["observedGeneration"]})
	}
	raw, _ := json.Marshal(brief)
	return string(raw)
}

// echo is g as the API server shows it after w.
func echo(g gadget, w write) gadget {
	g.rv, g.status = w.rv, w.patch["status"].(map[string]any)
	return g
}

func TestAModelWritesItsConditionsAndRenderedStatus(t *testing.T) {
	s, w, _ := testStageOf(t, databaseModel())
	g := gadget{name: "g", uid: "u", rv: "1", generation: 2}
	s.hear(g.version())
	ready := w.await(t, 1)[0]
	if got, want := writtenConditions(t, ready), `[["Ready","True","Modelled","modelled ready",2],["Synced","True","ReconcileSuccess","synced",2]]`; got != want {
		t.Errorf("Ready wrote conditions %s, want %s.", got, want)
	}
	if got, want := literalFields(ready), `{"endpoint":{"host":"g.db","port":5432},"observedGeneration":2}`; got != want {
		t.Errorf("Ready wrote %s, want %s.", got, want)
	}
	s.hear(echo(g, ready).version())
	w.still(t, 1, 2*after)

	end, err := s.Fault(stuck(""))
	if err != nil {
		t.Fatal(err)
	}
	stuck := w.await(t, 2)[1]
	if got, want := writtenConditions(t, stuck), `[["Ready","False","Stuck","a stuck fault holds it",2],["Synced","False","ReconcileError","a stuck fault holds it",2]]`; got != want {
		t.Errorf("Stuck wrote conditions %s, want %s.", got, want)
	}
	if got, want := literalFields(stuck), `{"endpoint":null,"observedGeneration":2}`; got != want {
		t.Errorf("Stuck wrote %s, want %s.", got, want)
	}
	s.hear(echo(g, stuck).version())
	w.still(t, 2, 2*after)
	end()
	w.await(t, 3)
}

func TestAModelRewritesAStatusThatLacksOneOfItsConditions(t *testing.T) {
	s, w, _ := testStageOf(t, databaseModel())
	s.hear(gadget{name: "g", uid: "u", rv: "1", generation: 1, ready: readyFor(1)}.version())
	if got := writtenConditions(t, w.await(t, 1)[0]); got == "" {
		t.Errorf("Wrote no conditions.")
	}
}

func TestAModelRewritesAConditionOfAnOldGeneration(t *testing.T) {
	s, w, _ := testStageOf(t, databaseModel())
	g := gadget{name: "g", uid: "u", rv: "1", generation: 1}
	s.hear(g.version())
	ready := w.await(t, 1)[0]
	stale := echo(g, ready)
	stale.status["conditions"].([]any)[1].(map[string]any)["observedGeneration"] = 0.0
	s.hear(stale.version())
	w.await(t, 2)
}

func TestAStuckFailureHoldsOnlyItsCondition(t *testing.T) {
	s, w, _ := testStageOf(t, databaseModel())
	g := gadget{name: "g", uid: "u", rv: "1", generation: 1}
	s.hear(g.version())
	s.hear(echo(g, w.await(t, 1)[0]).version())
	f := stuck("")
	f.Failure = "Synced"
	if !s.Takes(f) {
		t.Fatal("The stage refuses stuck(Synced).")
	}
	if _, err := s.Fault(f); err != nil {
		t.Fatal(err)
	}
	synced := w.await(t, 2)[1]
	if got, want := writtenConditions(t, synced), `[["Ready","True","Modelled","modelled ready",1],["Synced","False","ReconcileError","a stuck fault holds it",1]]`; got != want {
		t.Errorf("stuck(Synced) wrote conditions %s, want %s.", got, want)
	}
	s.hear(echo(g, synced).version())
	w.still(t, 2, 2*after)
}
