package check_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

const releasedFirst = "released a CR before deleting its resources"

var (
	// bucketOfA is the bucket in cloud that slot a's owns entry names.
	bucketOfA   = trace.Ref{System: "cloud", Resource: "buckets", Name: "a"}
	widgetModel = trace.ModelCaller("toy", "widgets")
)

// A child is a resource of slot a that gc does not delete with it.
type child struct {
	ref trace.Ref
	// tie sets the rule that ties the child to slot a.
	tie          func(r *tt.Script)
	body         map[string]any
	kind, detail string
}

var children = map[string]child{
	"rule 2 in another namespace": {
		ref:  elsewhereA,
		tie:  func(r *tt.Script) { r.Spec.Owners = ownerLabel },
		body: labeledBy("a-0", "ns/a"),
		kind: "kube/configmaps", detail: "configmaps other/a-0 (rule 2, slot a)",
	},
	"rule 3 in a fake": {
		ref: bucketOfA,
		tie: func(r *tt.Script) {
			r.Spec.Systems = []trace.System{cloudSystem}
			r.Spec.Primaries[0].Owns = []trace.Owns{{System: "cloud", Kind: trace.Kind{Resource: "buckets"}, Name: "self.metadata.name"}}
		},
		body: map[string]any{"id": "a", "state": "READY"},
		kind: "cloud/buckets", detail: "cloud buckets a (rule 3, slot a)",
	},
}

var (
	ruleTwo   = children["rule 2 in another namespace"]
	ruleThree = children["rule 3 in a fake"]
)

// finalizedA is slot a's Widget with the controller's finalizer, and a
// deletionTimestamp when deleting.
func finalizedA(deleting bool) map[string]any { return objectBody("a", deleting, own) }

// finalized has the harness create slot a's Widget, tied to c, and the
// controller give it its finalizer. It returns the Widget's uid.
func finalized(c child) (*tt.Script, string) {
	r := newRun(2)
	c.tie(r)
	r.Begin()
	uid := r.CreateCR("a", widgetA)
	r.Add(write(ctl, "update", widgetA, "f1", own), tt.Version(widgetA, uid, "f1", finalizedA(false)))
	return r, uid
}

// deletingA has the harness create slot a's Widget, the controller give it
// its finalizer and create c, a model add held, and the harness delete the
// Widget in step 1. It returns the Widget's uid.
func deletingA(c child, held ...string) (*tt.Script, string) {
	r, uid := finalized(c)
	finalizers := append([]string{own}, held...)
	if len(held) > 0 {
		r.Add(write(widgetModel, "patch", widgetA, "f2", finalizers...), tt.Version(widgetA, uid, "f2", objectBody("a", false, finalizers...)))
	}
	r.Create(ctl, c.ref, c.body)
	deleteA(r, uid, finalizers...)
	return r, uid
}

// deleteA ends step 0 and has the harness delete slot a's Widget, which
// holds finalizers, in step 1.
func deleteA(r *tt.Script, uid string, finalizers ...string) {
	r.EndFaults().Settle()
	r.Begin()
	r.Add(write(trace.Harness, "delete", widgetA, "d1", finalizers...), tt.Version(widgetA, uid, "d1", objectBody("a", true, finalizers...)))
	r.Act("delete", "a", &widgetA)
}

// releaseA has the controller remove its finalizer from slot a's deleting
// Widget, which deletes it.
func releaseA(r *tt.Script, uid string) {
	r.Add(write(ctl, "update", widgetA, "d1"), tt.Deleted(widgetA, uid, "d2", finalizedA(true)))
}

func teardown(t *testing.T, r *tt.Script) []check.Finding {
	t.Helper()
	return judgeWith(t, r.EndFaults().Settle(), check.Teardown)
}

func TestTeardownFailsAReleaseBeforeTheChildsDelete(t *testing.T) {
	for name, c := range children {
		t.Run(name, func(t *testing.T) {
			r, uid := deletingA(c)
			releaseA(r, uid)
			r.Delete(ctl, c.ref)
			requireFinding(t, teardown(t, r), "Teardown", 1, releasedFirst, c.kind, c.detail)
		})
	}
}

func TestTeardownPassesADeleteBeforeTheRelease(t *testing.T) {
	for name, c := range children {
		t.Run(name, func(t *testing.T) {
			r, uid := deletingA(c)
			r.Delete(ctl, c.ref)
			releaseA(r, uid)
			requireNone(t, teardown(t, r))
		})
	}
}

func TestJudgeRunsTeardown(t *testing.T) {
	r, uid := deletingA(ruleTwo)
	releaseA(r, uid)
	ix := r.EndFaults().Settle().Index()
	spec, err := check.SpecFor(nil, ix)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(check.Judge(ix, spec).Findings, func(f check.Finding) bool { return f.Check == "Teardown" }) {
		t.Error("Judge gives no Teardown finding.")
	}
}

// A fake's gone CEL may never accept what its delete left.
func TestTeardownPassesADeleteAnsweredBeforeTheRelease(t *testing.T) {
	for name, status := range map[string]int{"accepted": 200, "of a child already gone": 404} {
		t.Run(name, func(t *testing.T) {
			r, uid := deletingA(ruleThree)
			r.Call(ctl, "delete", ruleThree.ref, status)
			releaseA(r, uid)
			requireNone(t, teardown(t, r))
		})
	}
}

// The observer may record a deletion after the release that followed it.
func TestTeardownPassesAKubeDeleteWhoseEffectTheTraceShowsAfterTheRelease(t *testing.T) {
	for name, effect := range map[string]func(r *tt.Script, child string){
		"gone": func(r *tt.Script, child string) {
			r.Add(tt.Deleted(ruleTwo.ref, child, "c2", heldChild(false)))
		},
		"deleting": func(r *tt.Script, child string) {
			r.Add(tt.Version(ruleTwo.ref, child, "c2", heldChild(true)))
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := ruleTwo
			c.body = heldChild(false)
			r, uid := deletingA(c)
			r.Call(ctl, "delete", c.ref, 200)
			releaseA(r, uid)
			effect(r, r.UID(c.ref))
			requireNone(t, teardown(t, r))
		})
	}
}

// A dry run, or a deletecollection whose selector the trace lacks, may
// leave the child alone.
func TestTeardownFailsAReleaseAfterAKubeDeleteThatLeftTheChildAlone(t *testing.T) {
	collection := ruleTwo.ref
	collection.Name = ""
	for verb, ref := range map[string]trace.Ref{"delete": ruleTwo.ref, "deletecollection": collection} {
		t.Run(verb, func(t *testing.T) {
			r, uid := deletingA(ruleTwo)
			r.Call(ctl, verb, ref, 200)
			releaseA(r, uid)
			requireFinding(t, teardown(t, r), "Teardown", 1, releasedFirst, ruleTwo.kind, ruleTwo.detail)
		})
	}
}

func TestTeardownFailsAReleaseAfterAFailedDelete(t *testing.T) {
	r, uid := deletingA(ruleThree)
	r.Call(ctl, "delete", ruleThree.ref, 500)
	releaseA(r, uid)
	r.Delete(ctl, ruleThree.ref)
	requireFinding(t, teardown(t, r), "Teardown", 1, releasedFirst, ruleThree.kind, ruleThree.detail)
}

// S3's DeleteBucketTagging is a delete of the bucket's tagging subresource.
func TestTeardownFailsAReleaseAfterOnlyASubresourceDelete(t *testing.T) {
	r, uid := deletingA(ruleThree)
	tagging := tt.Call(ctl, "delete", ruleThree.ref, 204, "")
	tagging.Call.Subresource = "tagging"
	r.Add(tagging)
	releaseA(r, uid)
	r.Delete(ctl, ruleThree.ref)
	requireFinding(t, teardown(t, r), "Teardown", 1, releasedFirst, ruleThree.kind, ruleThree.detail)
}

// A deletecollection's object is its collection.
func TestTeardownPassesADeleteOfTheChildsCollectionBeforeTheRelease(t *testing.T) {
	r, uid := deletingA(ruleTwo)
	child := r.UID(ruleTwo.ref)
	collection := ruleTwo.ref
	collection.Name = ""
	r.Call(ctl, "deletecollection", collection, 200)
	releaseA(r, uid)
	r.Add(tt.Deleted(ruleTwo.ref, child, "x", ruleTwo.body))
	requireNone(t, teardown(t, r))
}

// heldChild is slot a's rule 2 child with a finalizer that holds it.
func heldChild(deleting bool) map[string]any {
	body := objectBody("a-0", deleting, protect)
	body["metadata"].(map[string]any)["labels"] = map[string]any{"owner": "ns/a"}
	return body
}

// The controller need not wait until the child is gone. Here an actor
// without a listener deletes the child.
func TestTeardownJudgesTheReleaseByTheChildsDeletionTimestamp(t *testing.T) {
	for _, row := range []struct {
		name    string
		release int
		failed  bool
	}{{"before it", 0, true}, {"after it", 1, false}} {
		t.Run(row.name, func(t *testing.T) {
			c := ruleTwo
			c.body = heldChild(false)
			r, uid := deletingA(c)
			child := r.UID(c.ref)
			for n := range 2 {
				if n == row.release {
					releaseA(r, uid)
				} else {
					r.Add(tt.Version(c.ref, child, "c2", heldChild(true)))
				}
			}
			r.Add(tt.Deleted(c.ref, child, "c3", heldChild(true)))
			if findings := teardown(t, r); row.failed {
				requireFinding(t, findings, "Teardown", 1, releasedFirst, c.kind, c.detail)
			} else {
				requireNone(t, findings)
			}
		})
	}
}

// A fake's gone CEL may accept a resource that no recorded delete reached.
// A resource made after the release was no resource of the CR then.
func TestTeardownJudgesOnlyResourcesThatLiveAtTheRelease(t *testing.T) {
	gone := map[string]any{"id": "a", "state": "DELETING"}
	for name, build := range map[string]func() *tt.Script{
		"one gone before it": func() *tt.Script {
			r, uid := deletingA(ruleThree)
			r.Add(tt.Version(ruleThree.ref, r.UID(ruleThree.ref), "x", gone))
			releaseA(r, uid)
			return r
		},
		"one made after it": func() *tt.Script {
			r, uid := finalized(ruleThree)
			deleteA(r, uid, own)
			releaseA(r, uid)
			r.Create(ctl, ruleThree.ref, ruleThree.body)
			return r
		},
	} {
		t.Run(name, func(t *testing.T) { requireNone(t, teardown(t, build())) })
	}
}

// A delete of an earlier instance of the child's name does not count.
func TestTeardownFailsAReleaseAfterOnlyAnEarlierChildWasDeleted(t *testing.T) {
	r, uid := deletingA(ruleTwo)
	r.Delete(ctl, ruleTwo.ref)
	r.Create(ctl, ruleTwo.ref, ruleTwo.body)
	releaseA(r, uid)
	r.Delete(ctl, ruleTwo.ref)
	requireFinding(t, teardown(t, r), "Teardown", 1, releasedFirst, ruleTwo.kind, ruleTwo.detail)
}

// A delete sent before the child appeared may have hit an earlier instance
// of its name, though its answer came after.
func TestTeardownFailsAReleaseAfterOnlyADeleteSentBeforeTheChildAppeared(t *testing.T) {
	r, uid := deletingA(ruleTwo)
	sentAt := r.Now
	r.Add(tt.Deleted(ruleTwo.ref, r.UID(ruleTwo.ref), "c2", ruleTwo.body))
	r.Create(ctl, ruleTwo.ref, ruleTwo.body)
	r.Add(sent(tt.Call(ctl, "delete", ruleTwo.ref, 200, ""), r.Now-sentAt))
	releaseA(r, uid)
	r.Delete(ctl, ruleTwo.ref)
	requireFinding(t, teardown(t, r), "Teardown", 1, releasedFirst, ruleTwo.kind, ruleTwo.detail)
}

// The controller may see a child, and delete it, before the observer
// records it.
func TestTeardownPassesADeleteOfAChildThatTheObserverRecordedLate(t *testing.T) {
	r, uid := finalized(ruleTwo)
	r.Add(tt.Create(ctl, ruleTwo.ref, "c", "c1"), tt.Seen(ctl, ruleTwo.ref, "c", "c1"))
	deleteA(r, uid, own)
	r.Call(ctl, "delete", ruleTwo.ref, 200)
	r.Add(tt.Version(ruleTwo.ref, "c", "c1", ruleTwo.body))
	releaseA(r, uid)
	r.Add(tt.Deleted(ruleTwo.ref, "c", "c2", ruleTwo.body))
	requireNone(t, teardown(t, r))
}

// Like Clean, Teardown judges only what the controller created.
func TestTeardownPassesAChildThatTheHarnessCreated(t *testing.T) {
	r, uid := finalized(ruleTwo)
	r.Create(trace.Harness, ruleTwo.ref, ruleTwo.body)
	deleteA(r, uid, own)
	releaseA(r, uid)
	requireNone(t, teardown(t, r))
}

// gc deletes a child once no owner that its ownerReferences name, directly
// or through other owners, remains. Each child here also carries the label
// that rule 2 would read.
func TestTeardownPassesAChildWhoseOwnerReferencesNameTheCR(t *testing.T) {
	r, uid := finalized(ruleTwo)
	direct, through := tt.Ref("configmaps", "ns", "a-0"), tt.Ref("secrets", "ns", "a-1")
	viaCR := r.Create(ctl, direct, tt.OwnedBy(labeledBy("a-0", "ns/a"), uid, true))
	r.Create(ctl, through, tt.OwnedBy(labeledBy("a-1", "ns/a"), viaCR, true))
	deleteA(r, uid, own)
	releaseA(r, uid)
	collect(r, direct)
	collect(r, through)
	requireNone(t, teardown(t, r))
}

// gc deletes a child whose ownerReferences lead, through owners without the
// label, to another child of the CR, once that child is gone. The label
// alone would make the last child rule 2.
func TestTeardownPassesAChildOwnedThroughAnotherChild(t *testing.T) {
	for _, links := range []int{1, 2} {
		t.Run(fmt.Sprint(links, " links"), func(t *testing.T) {
			r, uid := finalized(ruleTwo)
			ownerUID := r.Create(ctl, ruleTwo.ref, ruleTwo.body)
			var collected []trace.Ref
			for n := 1; n < links; n++ {
				middle := tt.Ref("secrets", "other", fmt.Sprint("a-", n))
				ownerUID = r.Create(ctl, middle, tt.OwnedBy(tt.Object(middle.Name), ownerUID, true))
				collected = append(collected, middle)
			}
			last := tt.Ref("secrets", "other", "a-last")
			r.Create(ctl, last, tt.OwnedBy(labeledBy(last.Name, "ns/a"), ownerUID, true))
			deleteA(r, uid, own)
			r.Delete(ctl, ruleTwo.ref)
			releaseA(r, uid)
			for _, ref := range append(collected, last) {
				collect(r, ref)
			}
			requireNone(t, teardown(t, r))
		})
	}
}

// Only a write that removes the controller's last finalizer from a deleting
// CR releases it.
func TestTeardownPassesWritesThatDoNotRelease(t *testing.T) {
	for name, keep := range map[string][]string{
		"a write that keeps the controller's finalizer":        {own, protect},
		"a write that removes only another caller's finalizer": {own},
	} {
		t.Run(name, func(t *testing.T) {
			r, uid := deletingA(ruleTwo, protect)
			r.Add(write(ctl, "update", widgetA, "d2", keep...), tt.Version(widgetA, uid, "d2", objectBody("a", true, keep...)))
			requireNone(t, teardown(t, r))
		})
	}
}

// Finalizers judges a controller that strips another caller's finalizer
// from a CR that never held its own.
func TestTeardownPassesAStripOfACRWithoutTheControllersFinalizer(t *testing.T) {
	r := newRun(2)
	ruleTwo.tie(r)
	r.Begin()
	uid := r.CreateCR("a", widgetA)
	r.Add(write(widgetModel, "patch", widgetA, "f1", protect), tt.Version(widgetA, uid, "f1", objectBody("a", false, protect)))
	r.Create(ctl, ruleTwo.ref, ruleTwo.body)
	deleteA(r, uid, protect)
	r.Add(write(ctl, "update", widgetA, "d1"), tt.Deleted(widgetA, uid, "d2", objectBody("a", true, protect)))
	requireNone(t, teardown(t, r))
}

// A model's finalizer keeps the CR after the controller removes its own.
func TestTeardownFailsAReleaseThatLeavesAnotherCallersFinalizer(t *testing.T) {
	r, uid := deletingA(ruleTwo, protect)
	r.Add(write(ctl, "update", widgetA, "d2", protect), tt.Version(widgetA, uid, "d2", objectBody("a", true, protect)))
	requireFinding(t, teardown(t, r), "Teardown", 1, releasedFirst, ruleTwo.kind, ruleTwo.detail)
}

// sampled has the harness create slot a's Widget with finalizers, as a
// sample may carry them, and the controller create ruleTwo's child. It
// returns the Widget's uid.
func sampled(finalizers ...string) (*tt.Script, string) {
	r := newRun(2)
	ruleTwo.tie(r)
	r.Begin()
	uid := r.Create(trace.Harness, widgetA, objectBody("a", false, finalizers...))
	r.Act("create", "a", &widgetA)
	r.Create(ctl, ruleTwo.ref, ruleTwo.body)
	return r, uid
}

// A sample may carry the controller's own finalizer.
func TestTeardownFailsAReleaseOfAFinalizerThatTheSampleCarried(t *testing.T) {
	r, uid := sampled(own)
	deleteA(r, uid, own)
	releaseA(r, uid)
	requireFinding(t, teardown(t, r), "Teardown", 1, releasedFirst, ruleTwo.kind, ruleTwo.detail)
}

// Once the controller adds a finalizer, one that the harness added is not
// its own.
func TestTeardownFailsAReleaseThatLeavesTheSamplesFinalizer(t *testing.T) {
	r, uid := sampled(protect)
	r.Add(write(ctl, "update", widgetA, "f1", protect, own), tt.Version(widgetA, uid, "f1", objectBody("a", false, protect, own)))
	deleteA(r, uid, protect, own)
	r.Add(write(ctl, "update", widgetA, "d2", protect), tt.Version(widgetA, uid, "d2", objectBody("a", true, protect)))
	requireFinding(t, teardown(t, r), "Teardown", 1, releasedFirst, ruleTwo.kind, ruleTwo.detail)
}

// A write that removes the controller's finalizer from a live CR does not
// release it: the CR's delete removes it at once, which Clean judges.
func TestTeardownPassesAFinalizerRemovedBeforeTheDelete(t *testing.T) {
	r, uid := finalized(ruleTwo)
	r.Create(ctl, ruleTwo.ref, ruleTwo.body)
	r.Add(write(ctl, "update", widgetA, "f2"), tt.Version(widgetA, uid, "f2", objectBody("a", false)))
	r.EndFaults().Settle()
	r.Begin().DeleteCR("a", widgetA)
	requireNone(t, teardown(t, r))
}

// A delay fault can hold a write past the end of the step that sent it.
func TestTeardownGivesAReleaseTheStepThatSentIt(t *testing.T) {
	r, uid := deletingA(ruleTwo)
	r.EndFaults().Settle()
	sentAt := r.Now
	r.Tick(tt.Tick).Begin()
	r.Add(sent(write(ctl, "update", widgetA, "d1"), r.Now-sentAt), tt.Deleted(widgetA, uid, "d2", finalizedA(true)))
	requireFinding(t, teardown(t, r), "Teardown", 1, releasedFirst, ruleTwo.kind, ruleTwo.detail)
}

func TestTeardownReportsAStepsReleasesOfAKindInOneFinding(t *testing.T) {
	r := newRun(2)
	ruleTwo.tie(r)
	r.Begin()
	slots, uids := []string{"a", "b"}, map[string]string{}
	widget := func(slot string) trace.Ref { return tt.Ref("toy/widgets", "ns", slot) }
	for _, slot := range slots {
		uids[slot] = r.CreateCR(slot, widget(slot))
		r.Add(write(ctl, "update", widget(slot), "f"+slot, own), tt.Version(widget(slot), uids[slot], "f"+slot, objectBody(slot, false, own)))
		r.Create(ctl, tt.Ref("configmaps", "other", slot+"-0"), labeledBy(slot+"-0", "ns/"+slot))
	}
	r.EndFaults().Settle().Begin()
	for _, slot := range slots {
		cr := widget(slot)
		r.Add(write(trace.Harness, "delete", cr, "d"+slot, own), tt.Version(cr, uids[slot], "d"+slot, objectBody(slot, true, own)))
		r.Act("delete", slot, &cr)
	}
	first := r.Now
	for _, slot := range slots {
		r.Add(write(ctl, "update", widget(slot), "d"+slot), tt.Deleted(widget(slot), uids[slot], "r"+slot, objectBody(slot, true, own)))
	}
	findings := teardown(t, r)
	requireFinding(t, findings, "Teardown", 1, releasedFirst, "kube/configmaps", "configmaps other/a-0 (rule 2, slot a)", "configmaps other/b-0 (rule 2, slot b)")
	if findings[0].At != first {
		t.Errorf("at = %v, want the first release at %v.", findings[0].At, first)
	}
}

// gc resolves no ownerReference of a cluster-scoped object to a namespaced
// owner.
func TestTeardownJudgesAClusterScopedChildOfTheCR(t *testing.T) {
	r, uid := finalized(ruleTwo)
	class := tt.Ref("storage.k8s.io/storageclasses", "", "a-class")
	r.Create(ctl, class, tt.OwnedBy(tt.Object(class.Name), uid, true))
	deleteA(r, uid, own)
	releaseA(r, uid)
	requireFinding(t, teardown(t, r), "Teardown", 1, releasedFirst, "kube/storage.k8s.io/storageclasses", "storageclasses.storage.k8s.io a-class (rule 1, slot a)")
}

// gc deletes neither instance of an ownerReferences cycle.
func TestTeardownFailsAReleaseWhileAChildsOwnerReferencesFormACycle(t *testing.T) {
	r, uid := finalized(ruleTwo)
	other := tt.Ref("secrets", "other", "a-1")
	child := r.Create(ctl, ruleTwo.ref, labeledBy("a-0", "ns/a"))
	r.Update(ctl, ruleTwo.ref, tt.OwnedBy(labeledBy("a-0", "ns/a"), r.Create(ctl, other, tt.OwnedBy(tt.Object(other.Name), child, true)), true))
	deleteA(r, uid, own)
	releaseA(r, uid)
	requireFinding(t, teardown(t, r), "Teardown", 1, releasedFirst, ruleTwo.kind, ruleTwo.detail)
}

// gc deletes no resource of a fake, whatever its body says.
func TestTeardownJudgesAFakesResourceWhoseBodyHasOwnerReferences(t *testing.T) {
	r, uid := finalized(ruleThree)
	bucket := ruleThree.ref
	bucket.Namespace = "ns"
	r.Create(ctl, bucket, tt.OwnedBy(map[string]any{"id": "a", "state": "READY"}, uid, true))
	deleteA(r, uid, own)
	releaseA(r, uid)
	requireFinding(t, teardown(t, r), "Teardown", 1, releasedFirst, ruleThree.kind, "cloud buckets a (rule 1, slot a)")
}

// A release judges only the released CR's resources.
func TestTeardownPassesResourcesOfOtherOwners(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"slot b's":  labeledBy("b-0", "ns/b"),
		"the set's": tt.Object("b-0"),
	} {
		t.Run(name, func(t *testing.T) {
			r, uid := finalized(ruleTwo)
			r.CreateCR("b", widgetB)
			r.Create(ctl, tt.Ref("configmaps", "other", "b-0"), body)
			deleteA(r, uid, own)
			releaseA(r, uid)
			requireNone(t, teardown(t, r))
		})
	}
}

// A controller keeps what its CR's deletion policy retains.
func TestTeardownPassesWhatRetainKeeps(t *testing.T) {
	for retain, want := range map[string]int{`self.metadata.name == "a"`: 0, `self.metadata.name == "b"`: 1, `self.policy == "Retain"`: 1} {
		t.Run(retain, func(t *testing.T) {
			r, uid := deletingA(ruleTwo)
			r.Spec.Primaries[0].Retain = retain
			releaseA(r, uid)
			if got := len(teardown(t, r)); got != want {
				t.Errorf("Found %d findings, want %d.", got, want)
			}
		})
	}
}

// The namespace controller deletes what a deleting namespace holds. Each
// namespace here is deleting by the trace's end.
func TestTeardownCountsAChildInADeletingNamespaceAsBeingDeleted(t *testing.T) {
	namespace := tt.Ref("namespaces", "", "other")
	for name, row := range map[string]struct {
		deleteNamespace func(r *tt.Script)
		failed          bool
	}{
		"with a deletionTimestamp": {func(r *tt.Script) {
			r.Add(tt.Version(namespace, r.UID(namespace), "n2", tt.Deleting(tt.Object("other"))))
		}, false},
		"after an answered delete": {func(r *tt.Script) { r.Call(ctl, "delete", namespace, 200) }, false},
		"while it is live":         {func(r *tt.Script) {}, true},
	} {
		t.Run(name, func(t *testing.T) {
			r, uid := finalized(ruleTwo)
			r.Create(trace.Harness, namespace, tt.Object("other"))
			r.Create(ctl, ruleTwo.ref, ruleTwo.body)
			deleteA(r, uid, own)
			row.deleteNamespace(r)
			releaseA(r, uid)
			r.Add(tt.Version(namespace, r.UID(namespace), "n3", tt.Deleting(tt.Object("other"))))
			if findings := teardown(t, r); row.failed {
				requireFinding(t, findings, "Teardown", 1, releasedFirst, ruleTwo.kind, ruleTwo.detail)
			} else {
				requireNone(t, findings)
			}
		})
	}
}

// A kube namespace holds no fake's resource.
func TestTeardownJudgesAFakesResourceInANamespaceThatKubeDeletes(t *testing.T) {
	namespace, bucket := tt.Ref("namespaces", "", "other"), trace.Ref{System: "cloud", Resource: "buckets", Namespace: "other", Name: "b1"}
	r := newRun(2)
	r.Spec.Systems, r.Spec.Owners = []trace.System{cloudSystem}, tagOwner
	r.Begin()
	uid := r.CreateCR("a", widgetA)
	r.Add(write(ctl, "update", widgetA, "f1", own), tt.Version(widgetA, uid, "f1", finalizedA(false)))
	r.Create(trace.Harness, namespace, tt.Object("other"))
	r.Create(ctl, bucket, bucketBody("READY", uid))
	deleteA(r, uid, own)
	r.Call(ctl, "delete", namespace, 200)
	releaseA(r, uid)
	r.Add(tt.Version(namespace, r.UID(namespace), "n2", tt.Deleting(tt.Object("other"))))
	requireFinding(t, teardown(t, r), "Teardown", 1, releasedFirst, "cloud/buckets", "cloud buckets b1 (rule 2, slot a)")
}
