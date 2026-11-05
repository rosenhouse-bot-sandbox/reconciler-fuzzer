package config_test

import (
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
)

func TestKnown(t *testing.T) {
	c, err := config.Load(writeConfig(t, minimal(t)+"known:\n- {check: Property, summary: property a does not hold, see: '#63'}\n- {check: Converge, summary: did not settle (clause 1), details: 'Synced[A-Z]', see: bugs.md}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Known) != 2 {
		t.Fatalf("known = %+v, want 2 entries", c.Known)
	}
	if k := c.Known[0]; k.Check != "Property" || k.Summary != "property a does not hold" || k.Details != nil || k.See != "#63" {
		t.Errorf("known[0] = %+v", k)
	}
	if k := c.Known[1]; k.Details == nil || !k.Details.MatchString("SyncedError") || k.Details.MatchString("Synced error") {
		t.Errorf("known[1].details = %v, want Synced[A-Z]", k.Details)
	}
	for known, want := range map[string]string{
		"[{summary: s, see: x}]":                               "known[0].check is required",
		"[{check: Restore, see: x}]":                           "known[0].summary is required",
		"[{check: Restore, summary: s}]":                       "known[0].see is required",
		"[{check: Restore, summary: s, see: x, details: '('}]": "known[0].details",
	} {
		if _, err := config.Load(writeConfig(t, minimal(t)+"known: "+known+"\n")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want one containing %q", known, err, want)
		}
	}
}
