package owner_test

import (
	"reflect"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/owner"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

func bucket(id string) trace.Ref { return trace.Ref{System: "cloud", Resource: "buckets", Name: id} }

func bucketBody(id, state string, tags map[string]any) map[string]any {
	return map[string]any{"id": id, "state": state, "tags": tags}
}

var cloud = trace.System{Name: "cloud", API: "crud", Role: "self.tags.role", Gone: "self.state == 'DELETING'"}

func compileSystems(t *testing.T, owners map[string]string, owns []trace.Owns, systems ...trace.System) owner.Rules {
	t.Helper()
	rules, err := owner.Compile(owners, owns, systems)
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func TestCompileRefusesBadSystemCEL(t *testing.T) {
	for _, s := range []trace.System{{Name: "cloud", Role: "1 + 2"}, {Name: "cloud", Gone: "'x'"}} {
		if _, err := owner.Compile(nil, nil, []trace.System{s}); err == nil {
			t.Errorf("%+v compiles", s)
		}
	}
}

func TestAFakeInstanceEndsAtItsFirstGoneVersion(t *testing.T) {
	b := bucket("buckets-1")
	tl := twoWidgets()
	tl.At(3*s, tt.Version(b, "cloud/buckets/buckets-1", "1", bucketBody("buckets-1", "CREATING", nil))).
		At(4*s, tt.Version(b, "cloud/buckets/buckets-1", "2", bucketBody("buckets-1", "DELETING", nil))).
		At(5*s, tt.Version(b, "cloud/buckets/buckets-1", "3", bucketBody("buckets-1", "READY", nil))).
		At(6*s, tt.Deleted(b, "cloud/buckets/buckets-1", "4", bucketBody("buckets-1", "DELETING", nil)))
	w := owner.New(tl.Index(), compileSystems(t, nil, nil, cloud))
	i := instance(t, w, "cloud/buckets/buckets-1")
	if i.Gone != 4*s {
		t.Errorf("gone at %v, want 4s", i.Gone)
	}
	if body := string(w.Body(i, 5*s)); body != `{"id":"buckets-1","state":"DELETING","tags":null}` {
		t.Errorf("body after gone = %s", body)
	}
	if other := instance(t, owner.New(tl.Index(), owner.Rules{}), "cloud/buckets/buckets-1"); other.Gone != 6*s {
		t.Errorf("without gone CEL: gone at %v, want 6s", other.Gone)
	}
}

func TestRoleIsTheSystemsRoleCELOverTheFirstBody(t *testing.T) {
	tl := twoWidgets()
	tl.At(3*s, tt.Version(bucket("b1"), "u1", "1", bucketBody("b1", "CREATING", map[string]any{"role": "data"}))).
		At(4*s, tt.Version(bucket("b1"), "u1", "2", bucketBody("b1", "READY", map[string]any{"role": "logs"}))).
		At(5*s, tt.Version(bucket("b2"), "u2", "3", bucketBody("b2", "READY", nil))).
		At(6*s, tt.Version(trace.Ref{System: "other", Resource: "buckets", Name: "b3"}, "u3", "4", bucketBody("b3", "READY", nil)))
	w := owner.New(tl.Index(), compileSystems(t, nil, nil, cloud))
	for uid, want := range map[string]string{"u1": "data", "u2": "", "u3": "", "ua": "a"} {
		if got := instance(t, w, uid).Role; got != want {
			t.Errorf("%s role = %q, want %q", uid, got, want)
		}
	}
}

func TestRule2And3OnAFake(t *testing.T) {
	tl := twoWidgets()
	tl.At(3*s, tt.Version(bucket("b1"), "u1", "1", bucketBody("b1", "READY", map[string]any{"cr": "ub"}))).
		At(4*s, tt.Version(bucket("b2"), "u2", "2", bucketBody("b2", "READY", nil))).
		At(4*s, tt.Version(widgetA, "ua", "3", map[string]any{"metadata": map[string]any{"name": "a"}, "status": map[string]any{"bucket": "b2"}}))
	rules := compileSystems(t, map[string]string{"cloud": "self.tags.cr"},
		[]trace.Owns{{System: "cloud", Kind: trace.Kind{Resource: "buckets"}, Name: "self.status.bucket"}}, cloud)
	wantOwner(t, ownerBy(t, tl, rules, "u1", 5*s), 2, "ub")
	wantOwner(t, ownerBy(t, tl, rules, "u2", 5*s), 3, "ua")
	kubeOnly := compileSystems(t, nil, []trace.Owns{{Kind: trace.Kind{Resource: "buckets"}, Name: "self.status.bucket"}})
	wantOwner(t, ownerBy(t, tl, kubeOnly, "u2", 5*s), 4, "ua", "ub")
}

func TestSelectOnAFakeWithNoResourceMatchesEveryCollection(t *testing.T) {
	tl := twoWidgets()
	tl.At(3*s, tt.Create(ctl, bucket("b1"), "u1", "1"), tt.Version(bucket("b1"), "u1", "1", bucketBody("b1", "READY", nil))).
		At(4*s, tt.Create(ctl, trace.Ref{System: "cloud", Resource: "disks", Name: "d1"}, "u2", "2"),
			tt.Version(trace.Ref{System: "cloud", Resource: "disks", Name: "d1"}, "u2", "2", bucketBody("d1", "READY", nil)))
	w := owner.New(tl.Index(), owner.Rules{})
	for _, c := range []struct {
		target plan.Target
		want   string
	}{
		{plan.Target{System: "cloud"}, "b1"},
		{plan.Target{System: "cloud", Nth: 2}, "d1"},
		{plan.Target{System: "cloud", Resource: "disks"}, "d1"},
		{plan.Target{System: "other"}, ""},
	} {
		ref, found := w.Select(ctl, c.target, "", 5*s)
		if found != (c.want != "") || ref.Name != c.want {
			t.Errorf("Select(%+v) = %+v, %v; want %q", c.target, ref, found, c.want)
		}
	}
}

func TestSourcesAndSystemsOfAFake(t *testing.T) {
	c := &config.Config{
		Systems: []config.System{{Name: "kube"}, {Name: "cloud", Process: &config.Process{}, API: "crud", Owner: "self.tags.cr",
			Role: "self.tags.role", Ready: "self.state == 'READY'", Gone: "self.state == 'DELETING'",
			Dependency: &config.Dependency{Required: true}, Poll: s}},
		Primary: config.Primary{Owns: []config.Owns{{System: "cloud", Collection: "buckets", Name: "self.status.bucket"}}},
	}
	owners, owns, err := owner.Sources(c, resolveTo("", false, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(owners, map[string]string{"cloud": "self.tags.cr"}) {
		t.Errorf("owners = %v", owners)
	}
	if want := []trace.Owns{{System: "cloud", Kind: trace.Kind{Resource: "buckets"}, Name: "self.status.bucket"}}; !reflect.DeepEqual(owns, want) {
		t.Errorf("owns = %+v", owns)
	}
	want := []trace.System{{Name: "cloud", API: "crud", Role: "self.tags.role", Ready: "self.state == 'READY'", Gone: "self.state == 'DELETING'",
		Ignore: []string{"version"}, Poll: s, Dependency: &trace.Dependency{Required: true}}}
	if got := owner.Systems(c); !reflect.DeepEqual(got, want) {
		t.Errorf("systems = %+v", got)
	}
}
