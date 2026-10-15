//go:build envtest && linux

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// An interrupt leaves nothing behind: not the control plane and not the target,
// which outlive a reconciler-fuzzer that dies at once, and not envtest's
// scratch directories. The toy's waits are long, so a run is under way when the
// signal comes, and a teardown that waited them out would take half a minute.
func TestAnInterruptLeavesNothingRunning(t *testing.T) {
	dir := t.TempDir()
	fuzzer, toy := filepath.Join(dir, "reconciler-fuzzer"), filepath.Join(dir, "toy-widget")
	build(t, "./cmd/reconciler-fuzzer", fuzzer)
	build(t, "./targets/toy-widget", toy)
	targetFile := slowToy(t, dir, toy)
	for _, test := range []struct {
		name   string
		signal syscall.Signal
		group  bool
	}{
		{"SIGTERM", syscall.SIGTERM, false},
		{"a terminal's Ctrl-C", syscall.SIGINT, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			scratch, out := t.TempDir(), t.TempDir()
			cmd := exec.Command(fuzzer, "run", "--target", targetFile, "--runs", "3", "--seed", "1", "--out", out)
			cmd.Env = append(os.Environ(), "TMPDIR="+scratch)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan struct{})
			go func() {
				defer close(exited)
				_ = cmd.Wait()
			}()
			t.Cleanup(func() {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				<-exited
			})
			children := waitForChildren(t, cmd.Process.Pid, exited, "etcd", "kube-apiserver", "toy-widget")
			t.Cleanup(func() {
				for _, child := range children {
					_ = syscall.Kill(child, syscall.SIGKILL)
				}
			})

			pid := cmd.Process.Pid
			if test.group {
				pid = -pid
			}
			if err := syscall.Kill(pid, test.signal); err != nil {
				t.Fatal(err)
			}

			select {
			case <-exited:
			case <-time.After(7 * time.Second):
				t.Fatalf("reconciler-fuzzer was still running 7s after %v, and CI waits no longer.", test.signal)
			}
			if status := cmd.ProcessState.Sys().(syscall.WaitStatus); !status.Signaled() || status.Signal() != test.signal {
				t.Errorf("reconciler-fuzzer ended with %v, want death by %v.\n%s", status, test.signal, &stderr)
			}
			for name, child := range children {
				if err := syscall.Kill(child, 0); !errors.Is(err, syscall.ESRCH) {
					t.Errorf("%s (pid %d) outlived reconciler-fuzzer.", name, child)
				}
			}
			if left, _ := filepath.Glob(filepath.Join(scratch, "k8s_test_framework_*")); len(left) > 0 {
				t.Errorf("envtest left %v behind.", left)
			}
			runs, _ := filepath.Glob(filepath.Join(out, "*", "run-1"))
			if len(runs) != 1 || !strings.Contains(stderr.String(), "run 1: an interrupt stopped the run") ||
				!strings.Contains(stderr.String(), runs[0]) {
				t.Errorf("reconciler-fuzzer printed\n%s\nwant it to name the run the interrupt stopped, in %v.", &stderr, runs)
			}
		})
	}
}

func build(t *testing.T, pkg, binary string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", binary, pkg)
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Building %s failed: %v\n%s", pkg, err, out)
	}
}

// slowToy writes a copy of the toy's target.yaml whose paths are absolute and
// whose waits are long.
func slowToy(t *testing.T, dir, binary string) string {
	t.Helper()
	toyDir, err := filepath.Abs(filepath.Dir(toyTargetYAML))
	if err != nil {
		t.Fatal(err)
	}
	declared, err := os.ReadFile(toyTargetYAML)
	if err != nil {
		t.Fatal(err)
	}
	slow := strings.NewReplacer(
		"  - crds/", "  - "+filepath.Join(toyDir, "crds"),
		"sample: widget.yaml", "sample: "+filepath.Join(toyDir, "widget.yaml"),
		"  - config.yaml", "  - "+filepath.Join(toyDir, "config.yaml"),
		"  - rbac/role.yaml", "  - "+filepath.Join(toyDir, "rbac", "role.yaml"),
		"binary: bin/toy-widget", "binary: "+binary,
		"settle: 5s", "settle: 60s",
		"stable: 2s", "stable: 30s",
		"delete: 10s", "delete: 60s",
	).Replace(string(declared))
	path := filepath.Join(dir, "target.yaml")
	if err := os.WriteFile(path, []byte(slow), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := target.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Timeouts.Stable != 30*time.Second || loaded.Launch.Binary != binary {
		t.Fatalf("The copy of target.yaml declares %+v and runs %s, want waits of 30s and %s.", loaded.Timeouts, loaded.Launch.Binary, binary)
	}
	return path
}

// waitForChildren waits for reconciler-fuzzer to have started a process of each
// name, and returns their pids by name.
func waitForChildren(t *testing.T, parent int, exited <-chan struct{}, names ...string) map[string]int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		children := childrenOf(parent, names)
		if len(children) == len(names) {
			return children
		}
		select {
		case <-exited:
			t.Fatalf("reconciler-fuzzer exited before it started %v.", names)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("reconciler-fuzzer started %v, want %v.", children, names)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// childrenOf reads the processes of those names whose parent is parent from
// /proc.
func childrenOf(parent int, names []string) map[string]int {
	children := map[string]int{}
	stats, _ := filepath.Glob("/proc/[0-9]*/stat")
	for _, stat := range stats {
		read, err := os.ReadFile(stat)
		if err != nil {
			continue
		}
		// pid (comm) state ppid ...; comm may hold spaces and parentheses.
		lparen, rparen := bytes.IndexByte(read, '('), bytes.LastIndexByte(read, ')')
		if lparen < 0 || rparen < lparen {
			continue
		}
		fields := strings.Fields(string(read[rparen+1:]))
		if len(fields) < 2 || fields[1] != strconv.Itoa(parent) {
			continue
		}
		name := string(read[lparen+1 : rparen])
		pid, _ := strconv.Atoi(strings.TrimSpace(string(read[:lparen])))
		if slices.Contains(names, name) {
			children[name] = pid
		}
	}
	return children
}
