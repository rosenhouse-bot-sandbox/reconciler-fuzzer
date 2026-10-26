package plan

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateCreateFixture(t *testing.T) {
	secret := &Target{Resource: "secrets", Name: "admin-password"}
	deleted := Action{Do: ChangeFixture, Target: secret}
	again := Action{Do: CreateFixture, Target: secret}
	for name, c := range map[string]struct {
		steps []Step
		want  string
	}{
		"no earlier delete":       {[]Step{{Actions: []Action{again}}}, "steps[0].actions[0]: createFixture of secrets admin-password follows no changeFixture that deleted it"},
		"after a patch":           {[]Step{{Actions: []Action{{Do: ChangeFixture, Target: secret, Patch: json.RawMessage(`{"data":{}}`)}, again}}}, "steps[0].actions[1]: createFixture of secrets admin-password follows no changeFixture"},
		"twice":                   {[]Step{{Actions: []Action{deleted, again}}, {Actions: []Action{again}}}, "steps[1].actions[0]: createFixture of secrets admin-password follows no changeFixture"},
		"another fixture deleted": {[]Step{{Actions: []Action{{Do: ChangeFixture, Target: fixture}, again}}}, "steps[0].actions[1]: createFixture of secrets admin-password follows no changeFixture"},
		"no target":               {[]Step{{Actions: []Action{{Do: CreateFixture}}}}, "createFixture needs a target with a resource and a name"},
		"no name":                 {[]Step{{Actions: []Action{{Do: CreateFixture, Target: cms}}}}, "createFixture needs a target with a resource and a name"},
		"no resource":             {[]Step{{Actions: []Action{{Do: CreateFixture, Target: &Target{Name: "admin-password"}}}}}, "createFixture needs a target with a resource and a name"},
		"a group and namespace":   {[]Step{{Actions: []Action{{Do: CreateFixture, Target: &Target{Group: "storage.thirdparty", Resource: "volumes", Namespace: "ns", Name: "v"}}}}}, "createFixture of volumes.storage.thirdparty ns/v follows no changeFixture"},
		"a fake":                  {[]Step{{Actions: []Action{{Do: CreateFixture, Target: &Target{System: "cloud", Resource: "buckets", Name: "b"}}}}}, "createFixture acts only on kube"},
		"a slot":                  {[]Step{{Actions: []Action{deleted, {Do: CreateFixture, Slot: "a", Target: secret}}}}, "createFixture takes no slot, object or patch"},
		"a patch":                 {[]Step{{Actions: []Action{deleted, {Do: CreateFixture, Target: secret, Patch: json.RawMessage(`{}`)}}}}, "createFixture takes no slot, object or patch"},
		"an object":               {[]Step{{Actions: []Action{deleted, {Do: CreateFixture, Target: secret, Object: json.RawMessage(`{}`)}}}}, "createFixture takes no slot, object or patch"},
		"a propagation":           {[]Step{{Actions: []Action{deleted, {Do: CreateFixture, Target: secret, Propagation: Foreground}}}}, "createFixture takes no propagation"},
	} {
		if err := (&Plan{Steps: c.steps}).Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Validate = %v, want %q.", name, err, c.want)
		}
	}
}

func TestValidateAcceptsCreateFixtureAfterADelete(t *testing.T) {
	secret := &Target{Resource: "secrets", Name: "admin-password"}
	p := &Plan{Steps: []Step{
		{Actions: []Action{{Do: ChangeFixture, Target: secret}}},
		{Actions: []Action{{Do: CreateFixture, Target: &Target{System: "kube", Resource: "secrets", Name: "admin-password"}}, {Do: ChangeFixture, Target: secret}, {Do: CreateFixture, Target: secret}}},
	}}
	if err := p.Validate(); err != nil {
		t.Error(err)
	}
}
