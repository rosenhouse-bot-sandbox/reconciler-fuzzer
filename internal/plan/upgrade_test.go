package plan

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadResolvesARestartsPathsAgainstThePlan(t *testing.T) {
	path := write(t, `{"steps": [{"actions": [{"do": "restart", "binary": "bin/v2", "args": ["--version=2"], "crds": ["crds/", "/abs/crds"]}]}]}`)
	p, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	want := Upgrade{Binary: filepath.Join(dir, "bin/v2"), Args: []string{"--version=2"}, CRDs: []string{filepath.Join(dir, "crds"), "/abs/crds"}}
	if got := p.Steps[0].Actions[0].Upgrade; !reflect.DeepEqual(got, want) {
		t.Errorf("Upgrade = %#v, want %#v", got, want)
	}
}

func TestARelativePlanPathResolvesToAbsolutePaths(t *testing.T) {
	path := write(t, `{"steps": [{"actions": [{"do": "restart", "binary": "v2"}]}]}`)
	t.Chdir(filepath.Dir(path))
	p, err := Load(filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := p.Steps[0].Actions[0].Binary, filepath.Join(filepath.Dir(path), "v2"); got != want {
		t.Errorf("Binary = %q, want %q", got, want)
	}
}

func TestOnlyARestartTakesAnUpgrade(t *testing.T) {
	create, widget := Action{Do: Create, Slot: "a"}, json.RawMessage(`{"metadata":{"name":"w"}}`)
	for name, a := range map[string]Action{
		"kill with args":          {Do: Kill, Upgrade: Upgrade{Args: []string{}}},
		"kill with crds":          {Do: Kill, Upgrade: Upgrade{CRDs: []string{}}},
		"stop with a binary":      {Do: Stop, Upgrade: Upgrade{Binary: "/v2"}},
		"create with crds":        {Do: Create, Slot: "b", Object: widget, Upgrade: Upgrade{CRDs: []string{"/crds"}}},
		"update with args":        {Do: Update, Slot: "a", Patch: json.RawMessage(`{}`), Upgrade: Upgrade{Args: []string{"--v2"}}},
		"delete with a binary":    {Do: Delete, Slot: "a", Upgrade: Upgrade{Binary: "/v2"}},
		"damage with crds":        {Do: Damage, Target: cms, Upgrade: Upgrade{CRDs: []string{"/crds"}}},
		"changeFixture with args": {Do: ChangeFixture, Target: fixture, Upgrade: Upgrade{Args: []string{"--v2"}}},
	} {
		t.Run(name, func(t *testing.T) {
			err := (&Plan{Steps: []Step{{Actions: []Action{create, a}}}}).Validate()
			if want := "steps[0].actions[1]: " + string(a.Do) + " takes no binary, args or crds"; err == nil || err.Error() != want {
				t.Errorf("Validate = %v, want %q", err, want)
			}
		})
	}
}

func TestARestartTakesAnUpgrade(t *testing.T) {
	p := &Plan{Steps: []Step{{Actions: []Action{{Do: Restart, Upgrade: Upgrade{Binary: "/v2", Args: []string{}, CRDs: []string{"/crds"}}}}}}}
	if err := p.Validate(); err != nil {
		t.Error(err)
	}
}

// Empty args drop the launch's args, and empty crds still make the restart
// an upgrade, so a saved plan keeps both apart from unset ones.
func TestARestartsEmptyArgsAndCRDsSurviveASave(t *testing.T) {
	for _, body := range []string{`{"do":"restart"}`, `{"do":"restart","args":[]}`, `{"do":"restart","crds":[]}`} {
		var a Action
		if err := json.Unmarshal([]byte(body), &a); err != nil {
			t.Fatal(err)
		}
		if out, err := json.Marshal(a); err != nil || string(out) != body {
			t.Errorf("Marshal(Unmarshal(%s)) = %s, %v", body, out, err)
		}
	}
}
