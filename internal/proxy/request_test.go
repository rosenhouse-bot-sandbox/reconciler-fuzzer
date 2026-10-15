package proxy_test

import (
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
)

func TestRequestForbidden(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		fault  string
		want   bool
	}{
		{"403 with no fault", 403, "", true},
		{"403 with a delay fault", 403, "delay(500ms)", true},
		{"403 with an error 403 fault", 403, "error(403)", false},
		{"401 with no fault", 401, "", false},
		{"404 with no fault", 404, "", false},
		{"200 with no fault", 200, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := proxy.Request{Status: tc.status, Fault: tc.fault}
			if got := r.Forbidden(); got != tc.want {
				t.Errorf("Forbidden() = %v, want %v", got, tc.want)
			}
		})
	}
}
