package main_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

const databaseConfig = "database/config.yaml"

// configs are the toy's configs. B0 passes every plan in plans/ beside each.
var configs = []string{"config.yaml", databaseConfig}

func planPath(config, plan string) string {
	return filepath.Join(filepath.Dir(config), "plans", plan+".json")
}

// plans lists the plans beside config, without .json.
func plans(config string) ([]string, error) {
	paths, err := filepath.Glob(planPath(config, "*"))
	for i, path := range paths {
		paths[i] = strings.TrimSuffix(filepath.Base(path), ".json")
	}
	return paths, err
}

func TestEveryPlanLoads(t *testing.T) {
	for _, config := range configs {
		names, err := plans(config)
		if err != nil || len(names) == 0 {
			t.Fatalf("%s has plans %v, %v.", config, names, err)
		}
		for _, name := range names {
			if _, err := plan.Load(planPath(config, name)); err != nil {
				t.Errorf("%v", err)
			}
		}
	}
}

func TestEveryPlanCreatesOnlyListedKinds(t *testing.T) {
	t.Skip("enabled by the toy-fleet package")
	for _, path := range configs {
		c, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		names, err := plans(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			p, err := plan.Load(planPath(path, name))
			if err != nil {
				t.Fatal(err)
			}
			if err := p.ValidateKinds(c.ListedKinds()); err != nil {
				t.Errorf("%s under %s: %v", name, path, err)
			}
		}
	}
}
