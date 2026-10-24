package main

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRejectsAnUnknownBug(t *testing.T) {
	if err := run([]string{"--bug=99"}, nil); err == nil || !strings.Contains(err.Error(), "--bug=99") {
		t.Errorf("run(--bug=99) = %v, want an error naming --bug=99", err)
	}
}

func TestCacheOptionsFollowTheWatchNamespace(t *testing.T) {
	if got := cacheOptions(""); got.DefaultNamespaces != nil {
		t.Errorf("no namespace gave %v", got.DefaultNamespaces)
	}
	got := cacheOptions("ns").DefaultNamespaces
	if _, ok := got["ns"]; !ok || len(got) != 1 {
		t.Errorf("ns gave %v", got)
	}
}

func writeKubeconfig(t *testing.T, server string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	contents := `apiVersion: v1
kind: Config
current-context: test
clusters:
  - name: test
    cluster: {server: ` + server + `}
contexts:
  - name: test
    context: {cluster: test, user: test}
users:
  - name: test
    user: {}
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRestConfigPrefersTheFlagOverTheEnvironment(t *testing.T) {
	t.Setenv("KUBECONFIG", writeKubeconfig(t, "https://environment.example"))

	config, err := restConfig(writeKubeconfig(t, "https://flag.example"))
	if err != nil {
		t.Fatalf("restConfig returned an error: %v", err)
	}
	if config.Host != "https://flag.example" {
		t.Errorf("restConfig used the server %q, want the one from --kubeconfig.", config.Host)
	}
}

func TestRestConfigFallsBackToTheEnvironment(t *testing.T) {
	t.Setenv("KUBECONFIG", writeKubeconfig(t, "https://environment.example"))

	config, err := restConfig("")
	if err != nil {
		t.Fatalf("restConfig returned an error: %v", err)
	}
	if config.Host != "https://environment.example" {
		t.Errorf("restConfig used the server %q, want the one from $KUBECONFIG.", config.Host)
	}
}

func TestRunPrintsTheUsageForHelp(t *testing.T) {
	printed := &strings.Builder{}

	err := run([]string{"--help"}, printed)

	if !errors.Is(err, flag.ErrHelp) {
		t.Errorf("run returned %v for --help, want flag.ErrHelp so that main exits 0.", err)
	}
	for _, flag := range []string{"-bug", "-kubeconfig", "-metrics-bind-address"} {
		if !strings.Contains(printed.String(), flag) {
			t.Errorf("run printed %q for --help, want the usage of %s.", printed.String(), flag)
		}
	}
}
