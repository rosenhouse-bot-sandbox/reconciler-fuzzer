package main

import (
	"strings"
	"testing"
)

func TestRunRejectsBadFlagsAndAMissingEndpoint(t *testing.T) {
	t.Setenv("CLOUD_ENDPOINT", "")
	for args, want := range map[string]string{
		"--bug=29":  "--bug=29",
		"--bug=36":  "--bug=36",
		"--poll=0s": "--poll=0s",
		"--bug=35":  "CLOUD_ENDPOINT",
	} {
		if err := run(strings.Fields(args), nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("run(%s) = %v, want an error naming %s", args, err, want)
		}
	}
}

func TestCacheOptionsFollowTheWatchNamespace(t *testing.T) {
	if got := cacheOptions(""); got.DefaultNamespaces != nil {
		t.Errorf("no namespace gave %v", got.DefaultNamespaces)
	}
	if got := cacheOptions("ns"); len(got.DefaultNamespaces) != 1 {
		t.Errorf("ns gave %v", got.DefaultNamespaces)
	}
}
