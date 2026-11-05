package config_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

func TestNotRestored(t *testing.T) {
	c, err := config.Load(writeConfig(t, minimal(t)+"notRestored: [v1/Secret, toy/v1/Cluster]\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []trace.Kind{{Version: "v1", Kind: "Secret", Resource: "secrets"}, {Group: "toy", Version: "v1", Kind: "Cluster", Resource: "clusters"}}
	if !reflect.DeepEqual(c.NotRestored, want) {
		t.Errorf("notRestored = %+v, want %+v", c.NotRestored, want)
	}
	for kinds, want := range map[string]string{
		"[Secret]":         "notRestored[0] \"Secret\"",
		"[toy/v1/Nothing]": "notRestored[0] toy/v1/Nothing",
		"[v1/ConfigMap]":   "notRestored[0] v1/ConfigMap: Restore never requires a listed kind back",
	} {
		if _, err := config.Load(writeConfig(t, minimal(t)+"notRestored: "+kinds+"\n")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want one containing %q", kinds, err, want)
		}
	}
}
