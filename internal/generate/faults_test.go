package generate

import (
	"slices"
	"testing"

	gvkschema "k8s.io/apimachinery/pkg/runtime/schema"
	"pgregory.net/rapid"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
)

func TestDrawsInjectFaultsByDefault(t *testing.T) {
	const seeds = 200
	for _, testCase := range targets {
		t.Run(testCase.path, func(t *testing.T) {
			g := newGenerator(t, loadTarget(t, testCase.path), Options{})
			faults := 0
			for seed := range int64(seeds) {
				sequence, err := g.Draw(seed)
				if err != nil {
					t.Fatalf("Draw(%d) failed: %v.", seed, err)
				}
				for _, op := range sequence.Ops {
					if op.Type == run.OpFault {
						faults++
						break
					}
				}
			}
			if faults < seeds/7 {
				t.Errorf("%d of %d seeds drew a fault, want at least %d.", faults, seeds, seeds/7)
			}
		})
	}
}

func TestADrawnFaultEndsAtASettleInsertedAfterTheOpsItCovers(t *testing.T) {
	for _, testCase := range targets {
		t.Run(testCase.path, func(t *testing.T) {
			g := newGenerator(t, loadTarget(t, testCase.path), Options{})
			rapid.Check(t, func(rt *rapid.T) {
				ops := g.sequence(rt).Ops
				for i, op := range ops {
					if op.Type != run.OpFault {
						continue
					}
					if op.Fault == nil {
						rt.Fatalf("Op %d is a fault with no Fault.", i)
					}
					if op.Fault.Until.Op == nil {
						rt.Fatalf("Op %d is a fault with no until.op.", i)
					}
					end := *op.Fault.Until.Op
					if end <= i || end >= len(ops) {
						rt.Fatalf("Op %d is a fault until op %d, outside %d..%d.", i, end, i+1, len(ops)-1)
					}
					if ops[end].Type != run.OpSettle {
						rt.Fatalf("Op %d is a fault until op %d, a %s, want a settle.", i, end, ops[end].Type)
					}
				}
			})
		})
	}
}

// G7 does not judge a deleteManaged under a fault.
func TestNoDrawnFaultIsActiveDuringADeleteManaged(t *testing.T) {
	for _, testCase := range targets {
		t.Run(testCase.path, func(t *testing.T) {
			g := newGenerator(t, loadTarget(t, testCase.path), Options{})
			rapid.Check(t, func(rt *rapid.T) {
				ops := g.sequence(rt).Ops
				for i, op := range ops {
					if op.Type != run.OpFault {
						continue
					}
					for _, active := range ops[i+1 : *op.Fault.Until.Op] {
						if active.Type == run.OpDeleteManaged {
							rt.Fatalf("The fault of op %d is active during op %d, a deleteManaged.", i, active.Index)
						}
					}
				}
			})
		})
	}
}

func TestADrawnFaultMatchesThePrimaryOrAManagedKind(t *testing.T) {
	for _, testCase := range targets {
		t.Run(testCase.path, func(t *testing.T) {
			loaded := loadTarget(t, testCase.path)
			g := newGenerator(t, loaded, Options{})
			faultables, err := buildFaultables(loaded)
			if err != nil {
				t.Fatalf("buildFaultables failed: %v.", err)
			}
			rapid.Check(t, func(rt *rapid.T) {
				for _, op := range g.sequence(rt).Ops {
					if op.Type != run.OpFault {
						continue
					}
					if !slices.Contains(faultables, op.Fault.Match.Resource) {
						rt.Fatalf("A fault matches the resource %q, and the target allows %v.",
							op.Fault.Match.Resource, faultables)
					}
				}
			})
		})
	}
}

func TestDrawnFaultsReachEveryResourceVerbAndAction(t *testing.T) {
	const seeds = 500
	loaded := loadTarget(t, certManagerTarget)
	g := newGenerator(t, loaded, Options{})
	faultables, err := buildFaultables(loaded)
	if err != nil {
		t.Fatalf("buildFaultables failed: %v.", err)
	}
	resources, verbs, errors, delays := map[string]bool{}, map[string]bool{}, map[int]bool{}, 0
	for seed := range int64(seeds) {
		sequence, err := g.Draw(seed)
		if err != nil {
			t.Fatalf("Draw(%d) failed: %v.", seed, err)
		}
		for _, op := range sequence.Ops {
			if op.Type != run.OpFault {
				continue
			}
			resources[op.Fault.Match.Resource] = true
			verbs[op.Fault.Match.Verb] = true
			if op.Fault.Action.Error != 0 {
				errors[op.Fault.Action.Error] = true
			}
			if op.Fault.Action.Delay != 0 {
				delays++
			}
		}
	}
	for _, resource := range faultables {
		if !resources[resource] {
			t.Errorf("No fault matched the resource %q in %d seeds.", resource, seeds)
		}
	}
	for _, verb := range faultVerbs {
		if !verbs[verb] {
			t.Errorf("No fault matched the verb %q in %d seeds.", verb, seeds)
		}
	}
	for _, code := range faultErrors {
		if !errors[code] {
			t.Errorf("No fault returned the error %d in %d seeds.", code, seeds)
		}
	}
	if delays == 0 {
		t.Errorf("No fault delayed a request in %d seeds.", seeds)
	}
}

func TestFaultsLeaveEverySeedsOtherOpsAlone(t *testing.T) {
	const seeds = 100
	for _, testCase := range targets {
		t.Run(testCase.path, func(t *testing.T) {
			loaded := loadTarget(t, testCase.path)
			loaded.Generate.NoFaults = true
			clean := newGenerator(t, loaded, Options{})
			loaded.Generate.NoFaults = false
			dirty := newGenerator(t, loaded, Options{})
			for seed := range int64(seeds) {
				without, err := clean.Draw(seed)
				if err != nil {
					t.Fatalf("Draw(%d) without faults failed: %v.", seed, err)
				}
				with, err := dirty.Draw(seed)
				if err != nil {
					t.Fatalf("Draw(%d) with faults failed: %v.", seed, err)
				}
				stripped := stripFault(with.Ops)
				if len(stripped) != len(without.Ops) {
					t.Fatalf("Seed %d: %d ops without faults, %d stripped; faults changed the count.",
						seed, len(without.Ops), len(stripped))
				}
				for i := range stripped {
					if stripped[i].Type != without.Ops[i].Type {
						t.Fatalf("Seed %d op %d: %s stripped, %s without; faults changed the type.",
							seed, i, stripped[i].Type, without.Ops[i].Type)
					}
				}
			}
		})
	}
}

// stripFault removes the fault op and its inserted settle from a sequence.
func stripFault(ops []run.Op) []run.Op {
	faultAt := -1
	settleAt := -1
	for i, op := range ops {
		if op.Type == run.OpFault {
			faultAt = i
			if op.Fault != nil && op.Fault.Until.Op != nil {
				settleAt = *op.Fault.Until.Op
			}
			break
		}
	}
	if faultAt < 0 {
		return ops
	}
	var stripped []run.Op
	for i, op := range ops {
		if i == faultAt || i == settleAt {
			continue
		}
		stripped = append(stripped, op)
	}
	return stripped
}

func TestATargetWithFaultsOffDrawsNoFault(t *testing.T) {
	loaded := loadTarget(t, toyTarget)
	loaded.Generate.NoFaults = true
	g := newGenerator(t, loaded, Options{})
	rapid.Check(t, func(rt *rapid.T) {
		for _, op := range g.sequence(rt).Ops {
			if op.Type == run.OpFault {
				rt.Fatal("A target with NoFaults drew a fault.")
			}
		}
	})
}

func TestAManagedKindWithNoKnownPluralIsNotFaulted(t *testing.T) {
	loaded := loadTarget(t, toyTarget)
	loaded.Manages = append(loaded.Manages, gvkschema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Unknown"})
	faultables, err := buildFaultables(loaded)
	if err != nil {
		t.Fatalf("buildFaultables failed: %v.", err)
	}
	for _, plural := range faultables {
		if plural == "" {
			t.Error("An empty string is faultable.")
		}
	}
	if slices.Contains(faultables, "unknowns") {
		t.Error("An unknown kind whose plural no CRD or built-in table names is faultable.")
	}
}

func TestPluralsOfTheExamples(t *testing.T) {
	for _, testCase := range []struct {
		path    string
		plurals []string
	}{
		{toyTarget, []string{"widgets", "configmaps"}},
		{certManagerTarget, []string{"certificates", "secrets", "certificaterequests"}},
		{externalSecretsTarget, []string{"externalsecrets", "secrets"}},
	} {
		t.Run(testCase.path, func(t *testing.T) {
			loaded := loadTarget(t, testCase.path)
			got, err := buildFaultables(loaded)
			if err != nil {
				t.Fatalf("buildFaultables failed: %v.", err)
			}
			if !slices.Equal(got, testCase.plurals) {
				t.Errorf("buildFaultables returned %v, want %v.", got, testCase.plurals)
			}
		})
	}
}
