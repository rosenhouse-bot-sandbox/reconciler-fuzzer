package generate

import (
	"reflect"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
)

func baseline(t *testing.T, g *Generator) run.Sequence {
	t.Helper()
	sequence, err := g.Baseline()
	if err != nil {
		t.Fatalf("Baseline() failed: %v.", err)
	}
	return sequence
}

func TestTheBaselineCreatesTheSampleThenDeletesTheFirstObjectOfEachManagedKind(t *testing.T) {
	for _, testCase := range targets {
		t.Run(testCase.path, func(t *testing.T) {
			loaded := loadTarget(t, testCase.path)
			ops := baseline(t, newGenerator(t, loaded, Options{})).Ops

			if ops[0].Type != run.OpCreate || !reflect.DeepEqual(ops[0].Obj.Object, loaded.Sample.Object) {
				t.Fatalf("The baseline opens with %+v, want the create of the sample.", ops[0])
			}
			var deleted []string
			for _, op := range ops[1:] {
				if op.Type != run.OpDeleteManaged {
					continue
				}
				if *op.Nth != 0 {
					t.Errorf("Op %d deletes the managed object %d, want the first.", op.Index, *op.Nth)
				}
				if op.Index != len(deleted)+1 {
					t.Errorf("Op %d deletes a managed object after an update.", op.Index)
				}
				deleted = append(deleted, op.Kind)
			}
			if !slices.Equal(deleted, testCase.managed) {
				t.Errorf("The baseline deletes managed objects of the kinds %v, want one of each of %v.", deleted, testCase.managed)
			}
		})
	}
}

func TestTheBaselineChangesEachMutableFieldOfTheSettledCR(t *testing.T) {
	for _, testCase := range targets {
		t.Run(testCase.path, func(t *testing.T) {
			loaded := loadTarget(t, testCase.path)
			g := newGenerator(t, loaded, Options{})
			cr := runtime.DeepCopyJSON(loaded.Sample.Object)
			var changed []string
			for _, op := range baseline(t, g).Ops {
				if op.Type != run.OpUpdate {
					continue
				}
				patched := run.MergePatch(runtime.DeepCopyJSON(cr), op.Patch)
				moved := differences(cr, patched, "")
				if len(moved) != 1 {
					t.Fatalf("Op %d changes %v, want one field.", op.Index, moved)
				}
				if refused := g.rules.refusal(patched, cr); refused != nil {
					t.Errorf("The CRD refuses op %d: %v.", op.Index, refused)
				}
				changed = append(changed, moved[0])
				cr = patched
			}
			slices.Sort(changed)
			if want := slices.Sorted(slices.Values(testCase.mutablePaths)); !slices.Equal(changed, want) {
				t.Errorf("The baseline changes %v, want each of %v once.", changed, want)
			}
		})
	}
}

func TestEveryBaselineOpSettles(t *testing.T) {
	for _, testCase := range targets {
		t.Run(testCase.path, func(t *testing.T) {
			loaded := loadTarget(t, testCase.path)
			sequence := baseline(t, newGenerator(t, loaded, Options{}))

			if err := sequence.Validate(); err != nil {
				t.Fatalf("The Runner rejects the baseline: %v.", err)
			}
			if sequence.Target != loaded.Name {
				t.Errorf("The baseline names the target %q, want %q.", sequence.Target, loaded.Name)
			}
			for i, op := range sequence.Ops {
				if op.Index != i || !op.Settles() {
					t.Errorf("Op %d is %+v, want op %d and one that settles.", i, op, i)
				}
			}
		})
	}
}

func TestTheBaselineIsTheSameEveryTime(t *testing.T) {
	loaded := loadTarget(t, certManagerTarget)
	first := baseline(t, newGenerator(t, loaded, Options{}))
	again := baseline(t, newGenerator(t, loaded, Options{}))

	if !reflect.DeepEqual(first, again) {
		t.Errorf("Two baselines differ:\n%+v\n%+v", first, again)
	}
}

func TestTheBaselineLeavesOutAFieldNoDrawChanges(t *testing.T) {
	loaded := loadTarget(t, toyTarget)
	g := newGenerator(t, loaded, Options{})
	count, _, _ := unstructured.NestedInt64(loaded.Sample.Object, "spec", "count")
	draws := 0
	g.fields = []field{countField(count, &draws)}

	for _, op := range baseline(t, g).Ops {
		if op.Type == run.OpUpdate {
			t.Errorf("The baseline updates %v, though every draw holds the sample's count %d.", op.Patch, count)
		}
	}
	if draws != updateDraws {
		t.Errorf("The baseline drew %d counts, want %d.", draws, updateDraws)
	}
}

func TestTheBaselineLeavesOutAnUpdateTheCRDRefuses(t *testing.T) {
	g := newGenerator(t, loadTarget(t, rulesTarget), Options{})
	draws := 0
	// No count reaches minCount's default.
	g.fields = []field{countField(0, &draws)}

	for _, op := range baseline(t, g).Ops {
		if op.Type == run.OpUpdate {
			t.Errorf("The baseline updates %v, which the CRD refuses.", op.Patch)
		}
	}
}

func TestTheBaselineChangesAFieldFromWhatAnEarlierUpdateSet(t *testing.T) {
	g := newGenerator(t, loadTarget(t, toyTarget), Options{})
	draws := 0
	g.fields = []field{countField(5, &draws), countField(5, &draws)}

	var updates int
	for _, op := range baseline(t, g).Ops {
		if op.Type == run.OpUpdate {
			updates++
		}
	}
	if updates != 1 {
		t.Errorf("The baseline sets count to 5 in %d updates, want 1: the second finds it already 5.", updates)
	}
}
