package config_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
)

const vault = "{name: vault, api: vault, token: root, access: read, process: {command: [vault, server, -dev]}}"

func TestEmulatorSystems(t *testing.T) {
	c, err := config.Load(writeConfig(t, minimal(t)+"systems: ["+vault+", {name: aws, api: s3, process: {command: [moto]}}, "+fake+"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	vault, aws, cloud := c.Systems[0], c.Systems[1], c.Systems[2]
	if vault.API != "vault" || vault.Token != "root" || !vault.ReadOnly || aws.API != "s3" || aws.Token != "" || aws.ReadOnly {
		t.Errorf("systems = %+v", c.Systems)
	}
	if vault.Map().ModelFaults || aws.Map().ModelFaults || !cloud.Map().ModelFaults {
		t.Errorf("ModelFaults = %v, %v, %v; want only crud's", vault.Map().ModelFaults, aws.Map().ModelFaults, cloud.Map().ModelFaults)
	}
}

func TestEmulatorSystemsRefuseMistakes(t *testing.T) {
	for systems, want := range map[string]string{
		"[{name: s3, api: rest, process: {command: [x]}}]": `systems[0].api "rest": want crud, vault or s3`,
		"[{name: kube, token: x}]":                         "systems[0]: kube takes only name and owner",
		"[{name: v, api: vault, process: {command: [x]}}]": "systems[0].token is required with api vault",
		"[" + with("access: write") + "]":                  `systems[0].access "write": want read, or none`,
		"[{name: kube, access: read}]":                     "systems[0]: kube takes only name and owner",
		"[" + with("fixtures: [missing.yaml]") + "]":       "systems[0].fixtures",
	} {
		_, err := config.Load(writeConfig(t, minimal(t)+"systems: "+systems+"\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want one containing %q", systems, err, want)
		}
	}
}

func TestATokenIsALaunchVar(t *testing.T) {
	launch := func(value, system string) string {
		return strings.Replace(minimal(t), "launch:\n", "launch:\n  env: {X: '"+value+"'}\n", 1) + "systems: [" + system + "]\n"
	}
	if _, err := config.Load(writeConfig(t, launch("${systems.vault.token}", vault))); err != nil {
		t.Error(err)
	}
	if _, err := config.Load(writeConfig(t, launch("${systems.cloud.token}", fake))); err == nil || !strings.Contains(err.Error(), "system cloud has no token") {
		t.Errorf("err = %v, want one saying cloud has no token", err)
	}
}

func TestSystemFixtures(t *testing.T) {
	path := writeConfig(t, minimal(t)+"systems: ["+with("fixtures: [secrets.yaml]")+"]\nvalues: {fixtures: {secrets.yaml: [token]}}\n")
	dir := filepath.Dir(path)
	if err := os.WriteFile(filepath.Join(dir, "secrets.yaml"), []byte("- {resource: secret, name: app/db, body: {token: s3cr3t}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "secrets.yaml")
	if got := c.Systems[0].Fixtures; !reflect.DeepEqual(got, []string{file}) {
		t.Errorf("fixtures = %v", got)
	}
	if got := c.FixtureValues[file]; !reflect.DeepEqual(got, []string{"token"}) {
		t.Errorf("values.fixtures = %v", c.FixtureValues)
	}
	fixtures, err := config.ReadSystemFixtures(file)
	want := []config.SystemFixture{{Resource: "secret", Name: "app/db", Body: map[string]any{"token": "s3cr3t"}}}
	if err != nil || !reflect.DeepEqual(fixtures, want) {
		t.Errorf("ReadSystemFixtures = %+v, %v; want %+v", fixtures, err, want)
	}
}

func TestReadSystemFixturesRefusesMistakes(t *testing.T) {
	for content, want := range map[string]string{
		"- {name: a, body: {}}\n":                  "[0].resource is required",
		"- {resource: secret, body: {}}\n":         "[0].name is required",
		"- {resource: secret, name: a, bdy: {}}\n": "bdy",
	} {
		path := filepath.Join(t.TempDir(), "f.yaml")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := config.ReadSystemFixtures(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want one containing %q", content, err, want)
		}
	}
}

func TestPeriodic(t *testing.T) {
	primary := func(periodic string) string {
		return strings.Replace(minimal(t), `  ready: "true"`, `  ready: "true"`+"\n  periodic: "+periodic, 1)
	}
	c, err := config.Load(writeConfig(t, primary("{every: 2s, paths: [status.refreshTime]}")))
	if err != nil {
		t.Fatal(err)
	}
	if want := (&config.Periodic{Every: 2 * time.Second, Paths: []string{"status.refreshTime"}}); !reflect.DeepEqual(c.Primaries[0].Periodic, want) {
		t.Errorf("periodic = %+v", c.Primaries[0].Periodic)
	}
	for periodic, want := range map[string]string{
		"{paths: [status.x]}":            "primary.periodic.every",
		"{every: 0s, paths: [status.x]}": "primary.periodic.every",
		"{every: 1s}":                    "primary.periodic.paths",
	} {
		if _, err := config.Load(writeConfig(t, primary(periodic))); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want one containing %q", periodic, err, want)
		}
	}
}

func TestProperties(t *testing.T) {
	c, err := config.Load(writeConfig(t, minimal(t)+"properties:\n- {name: a, expr: 'true'}\n- {name: b, expr: 'true', when: always}\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []config.Property{{Name: "a", Expr: "true", When: config.StepEnd}, {Name: "b", Expr: "true", When: config.Always}}
	if !reflect.DeepEqual(c.Properties, want) {
		t.Errorf("properties = %+v, want %+v", c.Properties, want)
	}
	for properties, want := range map[string]string{
		"[{expr: 'true'}]": "properties[0].name is required",
		"[{name: a}]":      "properties[0].expr is required",
		"[{name: a, expr: 'true', when: sometimes}]":         `properties[0].when "sometimes"`,
		"[{name: a, expr: 'true'}, {name: a, expr: 'true'}]": "properties[1].name a: another property has this name",
		"[{name: a, expr: 'crs.all('}]":                      "properties[0].expr",
		"[{name: a, expr: 'size(crs)'}]":                     "properties[0].expr: expression yields int, want bool",
		"[{name: a, expr: 'self.ready'}]":                    "undeclared reference to 'self'",
		"[{name: a, expr: 'firstWrittenUnder(crs, 1)'}]":     "found no matching overload for 'firstWrittenUnder'",
	} {
		if _, err := config.Load(writeConfig(t, minimal(t)+"properties: "+properties+"\n")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want one containing %q", properties, err, want)
		}
	}
}

func TestCompileProperty(t *testing.T) {
	p, err := config.CompileProperty(`crs.all(c, world.kube.secrets.all(s, firstWrittenUnder(s, ["data", "tls.key"]) == c.metadata.uid)) && base64.decode("eA==") == b"x" && "a,b".split(",").size() == 2`)
	if err != nil {
		t.Fatal(err)
	}
	cr := map[string]any{"metadata": map[string]any{"uid": "u1"}}
	secret := map[string]any{"metadata": map[string]any{"name": "a-tls"}, "data": map[string]any{"tls.key": "K"}}
	var calls []string
	owner := "u1"
	eval, err := p.Bind(func(object map[string]any, path []string) (string, error) {
		calls = append(calls, fmt.Sprint(object["data"], path))
		if owner == "" {
			return "", errors.New("no instance")
		}
		return owner, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	world := map[string]map[string][]any{"kube": {"secrets": {secret}}}
	if holds, err := eval([]any{cr}, world); err != nil || !holds {
		t.Errorf("eval = %v, %v; want true", holds, err)
	}
	if want := []string{"map[tls.key:K] [data tls.key]"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("firstWrittenUnder calls = %q, want %q", calls, want)
	}
	owner = "u2"
	if holds, err := eval([]any{cr}, world); err != nil || holds {
		t.Errorf("eval with another owner = %v, %v; want false", holds, err)
	}
	owner = ""
	if _, err := eval([]any{cr}, world); err == nil || !strings.Contains(err.Error(), "no instance") {
		t.Errorf("eval with a failing firstWrittenUnder: err = %v", err)
	}
	if _, err := eval([]any{cr}, map[string]map[string][]any{}); err == nil {
		t.Error("eval without world.kube succeeded")
	}
}
