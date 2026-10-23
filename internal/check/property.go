package check

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/owner"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Property fails a config property that does not hold, or cannot be
// evaluated, at a settled step's end, or for an always property at a
// version event's time. It gives one finding per property per step.
func Property(ix *trace.Index, s Spec) []Finding {
	if len(s.Properties) == 0 {
		return nil
	}
	v := newView(ix, s)
	p := &properties{view: v, keys: worldKeys(ix, v.w), decoded: map[uint64]map[string]any{}}
	var findings []Finding
	for _, property := range s.Properties {
		eval, err := property.Program.Bind(p.firstWrittenUnder)
		if err != nil {
			findings = append(findings, propertyFinding(property, -1, 0, "could not be evaluated", err.Error()))
			continue
		}
		judged := map[int]bool{}
		for _, at := range p.times(property) {
			if judged[at.step] {
				continue
			}
			p.t, p.origins = at.t, nil
			crs, world := p.world(at.t)
			switch holds, err := eval(crs, world); {
			case err != nil:
				findings = append(findings, propertyFinding(property, at.step, at.t, "could not be evaluated", err.Error()))
			case !holds:
				f := propertyFinding(property, at.step, at.t, "does not hold", property.Expr+" is false")
				f.Details = append(f.Details, p.origins...)
				findings = append(findings, f)
			default:
				continue
			}
			judged[at.step] = true
		}
	}
	return findings
}

func propertyFinding(p PropertySpec, step int, t time.Duration, summary, detail string) Finding {
	return Finding{Check: "Property", Step: step, At: t, Summary: fmt.Sprintf("property %s %s", p.Name, summary), Details: []string{detail}}
}

// properties evaluates properties at t.
type properties struct {
	*view
	t time.Duration
	// keys holds an empty world: each system, and each of its kinds that
	// the run declares or the trace holds.
	keys    map[string]map[string]bool
	decoded map[uint64]map[string]any
	// origins says, for each firstWrittenUnder call of an evaluation, where
	// a value came from when it did not come from the object's instance
	// under its owning CR.
	origins []string
}

type judgeTime struct {
	t    time.Duration
	step int
}

// times are when p is judged: each settled step's end, or for an always
// property each version event's time, in the step whose fault period
// started last by then.
func (p *properties) times(property PropertySpec) []judgeTime {
	var times []judgeTime
	if !property.Always {
		for _, step := range p.steps {
			times = append(times, judgeTime{step.ConfirmEnd, step.Step})
		}
		return times
	}
	for _, e := range p.ix.Events() {
		if e.Version == nil || len(times) > 0 && times[len(times)-1].t == e.At {
			continue
		}
		step := -1
		if latest, found := latestStep(p.ix, e.At); found {
			step = latest.Step
		}
		times = append(times, judgeTime{e.At, step})
	}
	return times
}

// worldKeys holds kube and each fake, and each kind the run declares or
// any instance has.
func worldKeys(ix *trace.Index, w *owner.World) map[string]map[string]bool {
	keys := map[string]map[string]bool{"kube": {}}
	add := func(kind trace.Ref) {
		if keys[kind.System] == nil {
			keys[kind.System] = map[string]bool{}
		}
		keys[kind.System][worldKey(kind)] = true
	}
	if run, ok := ix.Run(); ok {
		for _, system := range run.Systems {
			keys[system.Name] = map[string]bool{}
		}
		for _, kind := range run.Kinds {
			add(kind)
		}
	}
	for _, i := range w.All() {
		add(i.Ref)
	}
	return keys
}

// worldKey is <resource> on core kube, <resource>.<group> on other kube
// groups, and the collection on a fake.
func worldKey(ref trace.Ref) string {
	if ref.Group != "" {
		return ref.Resource + "." + ref.Group
	}
	return ref.Resource
}

// world returns crs, the live CRs' bodies at t, and world, every live
// instance's body by system and key.
func (p *properties) world(t time.Duration) ([]any, map[string]map[string][]any) {
	crs := []any{}
	world := map[string]map[string][]any{}
	for system, kinds := range p.keys {
		world[system] = map[string][]any{}
		for key := range kinds {
			world[system][key] = []any{}
		}
	}
	for _, i := range p.w.Instances(t) {
		body := p.w.Object(i, t)
		if body == nil {
			continue
		}
		key := worldKey(i.Ref)
		world[i.Ref.System][key] = append(world[i.Ref.System][key], body)
		if p.w.Primary(i) {
			crs = append(crs, body)
		}
	}
	return crs, world
}

// firstWrittenUnder finds the earliest version by t of the object's ref,
// of any instance, with the object's value at path. It returns that
// version's owning CR by rules 1 to 3, or "".
func (p *properties) firstWrittenUnder(object map[string]any, path []string) (string, error) {
	metadata, _ := object["metadata"].(map[string]any)
	uid, _ := metadata["uid"].(string)
	i, found := p.w.Instance(uid)
	if uid == "" || !found {
		return "", fmt.Errorf("no instance has the object's metadata.uid %q", uid)
	}
	value, found := valueAt(object, path)
	if !found {
		return "", nil
	}
	want, _ := json.Marshal(value)
	var first trace.Event
	var firstOf owner.Instance
	for _, j := range p.w.All() {
		if j.Ref != i.Ref {
			continue
		}
		for _, e := range p.w.Versions(j) {
			if e.At > p.t || first.Version != nil && e.Seq >= first.Seq {
				break
			}
			if got, found := valueAt(p.decode(e), path); found {
				if encoded, _ := json.Marshal(got); string(encoded) == string(want) {
					first, firstOf = e, j
					break
				}
			}
		}
	}
	if first.Version == nil {
		return "", nil
	}
	cr, _ := p.w.Owner(firstOf, first.At).Named()
	now, _ := p.w.Owner(i, p.t).Named()
	where := fmt.Sprintf("%s: its %s was first written at %v", describe(i.Ref), strings.Join(path, "."), first.At)
	var origin string
	switch {
	case firstOf.UID != i.UID:
		origin = fmt.Sprintf("%s, in instance %s, under CR %q, before this instance %s", where, firstOf.UID, cr, i.UID)
	case cr != now:
		origin = fmt.Sprintf("%s, in this instance, under CR %q, which it now names %q", where, cr, now)
	}
	if origin != "" && !slices.Contains(p.origins, origin) {
		p.origins = append(p.origins, origin)
	}
	return cr, nil
}

func (p *properties) decode(e trace.Event) map[string]any {
	if body, done := p.decoded[e.Seq]; done {
		return body
	}
	var body map[string]any
	_ = json.Unmarshal(e.Version.Body, &body) // An undecodable body holds no value.
	p.decoded[e.Seq] = body
	return body
}

// valueAt is the value under the keys of path.
func valueAt(object map[string]any, path []string) (any, bool) {
	var value any = object
	for _, key := range path {
		m, isMap := value.(map[string]any)
		if !isMap {
			return nil, false
		}
		if value, isMap = m[key]; !isMap {
			return nil, false
		}
	}
	return value, true
}

// periodicWrite reports whether e is a write of a CR that changed only
// Periodic.Paths, at least Every/2 after the previous such write of that
// CR. Clause 7 does not count it as a changed write.
func (s Spec) periodicWrite(ix *trace.Index, e trace.Event) bool {
	if s.Periodic == nil || !s.onlyPeriodic(ix, e) {
		return false
	}
	ref := e.Call.Object()
	for _, prev := range slices.Backward(ix.Calls(e.Call.Caller, -1, e.At)) {
		if prev.Seq < e.Seq && prev.Call.Object() == ref && s.onlyPeriodic(ix, prev) {
			return e.At-prev.At >= s.Periodic.Every/2
		}
	}
	return true
}

// onlyPeriodic reports whether e wrote a CR version that differs from the
// one before only under Periodic.Paths.
func (s Spec) onlyPeriodic(ix *trace.Index, e trace.Event) bool {
	c := e.Call
	ref := c.Object()
	if ref.System != "kube" || ref.Group != s.PrimaryGroup || ref.Resource != s.PrimaryResource || !trace.Mutating(c.Verb) || !succeeded(*c) {
		return false
	}
	is, found := ix.VersionOf(ref, c.ResultRV)
	was, foundWas := ix.Previous(ref, c.ResultRV)
	if c.ResultRV == "" || !found || !foundWas {
		return false
	}
	var a, b map[string]any
	if json.Unmarshal(was.Body, &a) != nil || json.Unmarshal(is.Body, &b) != nil {
		return false
	}
	for _, side := range []map[string]any{a, b} {
		if metadata, _ := side["metadata"].(map[string]any); metadata != nil {
			delete(metadata, "resourceVersion")
			delete(metadata, "managedFields")
			delete(metadata, "generation")
		}
	}
	paths := leafDiff("", a, b, nil)
	return len(paths) > 0 && !slices.ContainsFunc(paths, func(path string) bool {
		return !slices.ContainsFunc(s.Periodic.Paths, func(periodic string) bool { return under(path, periodic) })
	})
}

// leafDiff appends the dotted paths at which a and b differ, descending
// into maps both sides hold, and into lists of one length as path[*].
func leafDiff(prefix string, a, b any, paths []string) []string {
	if la, aList := a.([]any); aList {
		if lb, bList := b.([]any); bList && len(la) == len(lb) {
			for i := range la {
				paths = leafDiff(prefix+"[*]", la[i], lb[i], paths)
			}
			return slices.Compact(paths)
		}
	}
	ma, aMap := a.(map[string]any)
	mb, bMap := b.(map[string]any)
	if !aMap || !bMap {
		x, _ := json.Marshal(a)
		y, _ := json.Marshal(b)
		if string(x) != string(y) {
			paths = append(paths, prefix)
		}
		return paths
	}
	for _, key := range slices.Sorted(maps.Keys(merge(ma, mb))) {
		child := key
		if prefix != "" {
			child = prefix + "." + key
		}
		paths = leafDiff(child, ma[key], mb[key], paths)
	}
	return paths
}

// under reports whether path is periodic or lies under it.
func under(path, periodic string) bool {
	return path == periodic || strings.HasPrefix(path, periodic+".") || strings.HasPrefix(path, periodic+"[")
}
