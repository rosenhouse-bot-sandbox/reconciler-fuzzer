package run

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
)

func TestExpandLaunch(t *testing.T) {
	l := config.Launch{
		Binary: "/bin/toy",
		Args:   []string{"--namespace=${systems.kube.namespace}", "--bug=0"},
		Env:    map[string]string{"KUBECONFIG": "${systems.kube.kubeconfig}"},
	}
	vars := map[string]string{"namespace": "rf-1", "kubeconfig": "/out/kubeconfig"}
	o, err := expandLaunch(l, []string{"--bug=3"}, vars)
	if err != nil {
		t.Fatal(err)
	}
	if o.Path != "/bin/toy" || !reflect.DeepEqual(o.Args, []string{"--namespace=rf-1", "--bug=0", "--bug=3"}) ||
		!reflect.DeepEqual(o.Env, map[string]string{"KUBECONFIG": "/out/kubeconfig"}) {
		t.Errorf("expandLaunch = %+v", o)
	}
	if _, err := expandLaunch(l, []string{"${systems.kube.url}"}, vars); err == nil || !strings.Contains(err.Error(), "systems.kube.url") {
		t.Errorf("expandLaunch = %v, want an unknown key error", err)
	}
	l.Args = nil
	l.Env["X"] = "${nope}"
	if _, err := expandLaunch(l, nil, vars); err == nil || !strings.Contains(err.Error(), "X") {
		t.Errorf("expandLaunch = %v, want an env error naming X", err)
	}
}
