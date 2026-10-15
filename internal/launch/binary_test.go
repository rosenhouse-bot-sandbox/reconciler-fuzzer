package launch_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/launch"
)

// forever keeps the shell alive without exec'ing away, so that it can still
// handle signals.
const forever = "while :; do sleep 0.1; done"

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newBinary launches script through /bin/sh, with args as its positional
// parameters.
func newBinary(t *testing.T, grace time.Duration, script string, args ...string) (*launch.Binary, *safeBuffer) {
	t.Helper()
	log := &safeBuffer{}
	binary := launch.NewBinary(launch.Options{
		Path:        "/bin/sh",
		Args:        append([]string{"-c", script, "sh"}, args...),
		Log:         log,
		GracePeriod: grace,
	})
	t.Cleanup(func() { _ = binary.Stop(context.Background()) })
	return binary, log
}

// kubeconfigPath is a stand-in for the kubeconfig the proxy writes. The
// launcher exports and substitutes the path without reading the file.
func kubeconfigPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "kubeconfig")
}

func mustStart(t *testing.T, binary *launch.Binary) string {
	t.Helper()
	kubeconfig := kubeconfigPath(t)
	if err := binary.Start(t.Context(), kubeconfig); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	return kubeconfig
}

func waitForLog(t *testing.T, log *safeBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(log.String(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the target log never held %q; it holds %q.", want, log.String())
}

// waitForLogCount waits for want to appear in the log exactly count times.
func waitForLogCount(t *testing.T, log *safeBuffer, want string, count int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Count(log.String(), want) >= count {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := strings.Count(log.String(), want); got != count {
		t.Fatalf("the target log holds %q %d times, want %d; it holds %q.", want, got, count, log.String())
	}
}

var pidLine = regexp.MustCompile(`pid=(\d+)`)

func pidFromLog(t *testing.T, log *safeBuffer) int {
	t.Helper()
	waitForLog(t, log, "pid=")
	match := pidLine.FindStringSubmatch(log.String())
	pid, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func requireGone(t *testing.T, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("process %d was not reaped; signalling it reported %v.", pid, err)
	}
}

func TestStartSendsOutputToTheLog(t *testing.T) {
	binary, log := newBinary(t, 0, "echo to stdout; echo to stderr >&2; "+forever)

	mustStart(t, binary)

	waitForLog(t, log, "to stdout")
	waitForLog(t, log, "to stderr")
}

func TestStartSubstitutesKubeconfig(t *testing.T) {
	// ${KUBECONFIG} reads the environment; $KUBECONFIG in the argument is
	// substituted before the target runs (DESIGN.md §5.1).
	binary, log := newBinary(t, 0, `echo "arg=$1"; echo "env=${KUBECONFIG}"; `+forever, "--kubeconfig=$KUBECONFIG")

	kubeconfig := mustStart(t, binary)

	waitForLog(t, log, "arg=--kubeconfig="+kubeconfig)
	waitForLog(t, log, "env="+kubeconfig)
}

const runNamespace = "reconciler-fuzzer-run-x"

func newBinaryInNamespace(t *testing.T, script string, env map[string]string, args ...string) (*launch.Binary, *safeBuffer) {
	t.Helper()
	log := &safeBuffer{}
	binary := launch.NewBinary(launch.Options{
		Path:      "/bin/sh",
		Args:      append([]string{"-c", script, "sh"}, args...),
		Namespace: runNamespace,
		Env:       env,
		Log:       log,
	})
	t.Cleanup(func() { _ = binary.Stop(context.Background()) })
	return binary, log
}

func TestStartSubstitutesNamespace(t *testing.T) {
	binary, log := newBinaryInNamespace(t, `echo "arg=$1"; echo "env=${WATCH_NAMESPACE} ${K}"; `+forever,
		map[string]string{"WATCH_NAMESPACE": "$NAMESPACE", "K": "$KUBECONFIG"}, "--namespace=$NAMESPACE")

	kubeconfig := mustStart(t, binary)

	waitForLog(t, log, "arg=--namespace="+runNamespace+"\n")
	waitForLog(t, log, "env="+runNamespace+" "+kubeconfig+"\n")
}

func TestEnvOverridesAnInheritedVariable(t *testing.T) {
	t.Setenv("WATCH_NAMESPACE", "default")
	t.Setenv("RECONCILER_FUZZER_INHERITED", "kept")
	binary, log := newBinaryInNamespace(t, `echo "env=${WATCH_NAMESPACE} ${RECONCILER_FUZZER_INHERITED}"; `+forever,
		map[string]string{"WATCH_NAMESPACE": "$NAMESPACE"})

	mustStart(t, binary)

	waitForLog(t, log, "env="+runNamespace+" kept\n")
}

func TestEnvDoesNotOverrideKubeconfig(t *testing.T) {
	binary, log := newBinaryInNamespace(t, `echo "env=${KUBECONFIG}"; `+forever,
		map[string]string{"KUBECONFIG": "/elsewhere"})

	kubeconfig := mustStart(t, binary)

	waitForLog(t, log, "env="+kubeconfig+"\n")
}

func TestStopTerminatesGracefully(t *testing.T) {
	binary, log := newBinary(t, 5*time.Second, `trap 'echo caught SIGTERM; exit 0' TERM; echo pid=$$; `+forever)
	mustStart(t, binary)
	pid := pidFromLog(t, log)

	if err := binary.Stop(t.Context()); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	if !strings.Contains(log.String(), "caught SIGTERM") {
		t.Errorf("Stop did not send SIGTERM first; the log holds %q.", log.String())
	}
	requireGone(t, pid)
}

func TestStopEscalatesToSIGKILL(t *testing.T) {
	// The reap after SIGKILL waits for the log to close, and Stop gives it the
	// grace period too. So the grace leaves room for load, and the shell execs
	// sleep, which keeps SIGTERM ignored, rather than leave a child holding the
	// log open.
	grace := time.Second
	binary, log := newBinary(t, grace, `trap "" TERM; echo pid=$$; exec sleep 60`)
	mustStart(t, binary)
	pid := pidFromLog(t, log)

	start := time.Now()
	if err := binary.Stop(t.Context()); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed < grace {
		t.Errorf("Stop returned after %v, before the grace period of %v elapsed.", elapsed, grace)
	}
	if elapsed > grace+5*time.Second {
		t.Errorf("Stop took %v, far beyond the grace period of %v.", elapsed, grace)
	}
	requireGone(t, pid)
}

func TestStopEscalatesWhenTheContextExpires(t *testing.T) {
	// The grace period is the default, which outlasts the context.
	binary, log := newBinary(t, 0, `trap "" TERM; echo pid=$$; `+forever)
	mustStart(t, binary)
	pid := pidFromLog(t, log)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := binary.Stop(ctx); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	if elapsed := time.Since(start); elapsed >= launch.DefaultGracePeriod {
		t.Errorf("Stop took %v; it waited out the grace period instead of the context.", elapsed)
	}
	requireGone(t, pid)
}

// TestStopWaitsTheDefaultGracePeriod covers Options.GracePeriod left unset.
func TestStopWaitsTheDefaultGracePeriod(t *testing.T) {
	binary, log := newBinary(t, 0, `trap "" TERM; echo pid=$$; `+forever)
	mustStart(t, binary)
	pid := pidFromLog(t, log)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	stopped := make(chan error, 1)
	go func() { stopped <- binary.Stop(ctx) }()

	select {
	case err := <-stopped:
		t.Fatalf("Stop returned at once, with %v; the default grace period is %v.", err, launch.DefaultGracePeriod)
	case <-time.After(300 * time.Millisecond):
	}
	if err := <-stopped; err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	requireGone(t, pid)
}

func TestStopIsIdempotent(t *testing.T) {
	binary, _ := newBinary(t, 0, forever)
	mustStart(t, binary)

	for i := range 2 {
		if err := binary.Stop(t.Context()); err != nil {
			t.Fatalf("Stop %d failed: %v", i, err)
		}
	}
}

func TestStopAfterTheTargetExits(t *testing.T) {
	binary, log := newBinary(t, 0, "echo done")
	mustStart(t, binary)
	waitForLog(t, log, "done")

	if err := binary.Stop(t.Context()); err != nil {
		t.Errorf("Stop failed after the target exited on its own: %v", err)
	}
}

// TestRestartWaitsForTheOldProcessToBeReaped proves the ordering DESIGN.md
// §5.1 requires: the replacement starts only once the old process and whatever
// it spawned are gone, so that a fixed port or lock file is free. The script
// leaves a child holding the lock for half a second after the kill.
func TestRestartWaitsForTheOldProcessToBeReaped(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "target.lock")
	script := fmt.Sprintf(`
if [ -e %[1]q ]; then echo predecessor=holds-the-lock; else echo predecessor=gone; fi
: > %[1]q
( sleep 0.5; rm -f %[1]q ) &
echo pid=$$
%s`, lock, forever)
	binary, log := newBinary(t, 0, script)
	mustStart(t, binary)
	first := pidFromLog(t, log)

	if err := binary.Restart(t.Context()); err != nil {
		t.Fatalf("Restart failed: %v", err)
	}

	requireGone(t, first)
	waitForLogCount(t, log, "predecessor=gone", 2)
}

// TestRestartCrashesTheTarget pins the Restart of DESIGN.md §5.1: SIGKILL, so
// the target never runs its shutdown path.
func TestRestartCrashesTheTarget(t *testing.T) {
	binary, log := newBinary(t, 0, `trap 'echo shutting down; exit 0' TERM; echo pid=$$; `+forever)
	mustStart(t, binary)
	pidFromLog(t, log)

	if err := binary.Restart(t.Context()); err != nil {
		t.Fatalf("Restart failed: %v", err)
	}

	waitForLogCount(t, log, "pid=", 2)
	if strings.Contains(log.String(), "shutting down") {
		t.Errorf("Restart let the target shut down gracefully; the log holds %q.", log.String())
	}
}

func TestRestartRunsTheTargetAgain(t *testing.T) {
	binary, log := newBinary(t, 0, `echo "running with [${KUBECONFIG}]"; `+forever)
	kubeconfig := mustStart(t, binary)
	waitForLog(t, log, "running with ["+kubeconfig+"]")

	if err := binary.Restart(t.Context()); err != nil {
		t.Fatalf("Restart failed: %v", err)
	}

	waitForLogCount(t, log, "running with ["+kubeconfig+"]", 2)
}

func TestRestartBeforeStart(t *testing.T) {
	binary, _ := newBinary(t, 0, forever)

	if err := binary.Restart(t.Context()); err == nil {
		t.Error("Restart started a target that had never been started.")
	}
}

func TestStartTwice(t *testing.T) {
	binary, _ := newBinary(t, 0, forever)
	mustStart(t, binary)

	if err := binary.Start(t.Context(), kubeconfigPath(t)); err == nil {
		t.Error("Start ran a second process while the first was still running.")
	}
}

func TestStartRejectsAMissingBinary(t *testing.T) {
	binary := launch.NewBinary(launch.Options{Path: filepath.Join(t.TempDir(), "no-such-binary")})

	err := binary.Start(t.Context(), kubeconfigPath(t))
	if err == nil {
		t.Fatal("Start accepted a binary that does not exist.")
	}
	if !strings.Contains(err.Error(), "no-such-binary") {
		t.Errorf("Start reported %q, which does not name the binary.", err)
	}
}

func TestRestartRejectsADoneContext(t *testing.T) {
	binary, log := newBinary(t, 0, "echo started; "+forever)
	mustStart(t, binary)
	waitForLog(t, log, "started")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := binary.Restart(ctx); err == nil {
		t.Error("Restart ran the target again although its context was already done.")
	}
	waitForLogCount(t, log, "started", 1)
}

func TestStartRejectsADoneContext(t *testing.T) {
	binary, log := newBinary(t, 0, "echo started; "+forever)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := binary.Start(ctx, kubeconfigPath(t)); err == nil {
		t.Error("Start ran the target although its context was already done.")
	}
	if log.String() != "" {
		t.Errorf("Start ran the target; its log holds %q.", log.String())
	}
}

// awaitExit waits for the target to stop on its own and returns its status.
func awaitExit(t *testing.T, binary *launch.Binary) launch.Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		status := binary.Status()
		if !status.Running {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatal("the target was still running 10s after it should have exited.")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A target that dies takes the run with it, so the launcher keeps the exit
// status for its caller to report instead of a settle expiry.
func TestStatusAfterANonZeroExit(t *testing.T) {
	binary, _ := newBinary(t, 0, "exit 3")
	mustStart(t, binary)

	status := awaitExit(t, binary)

	var exit *exec.ExitError
	if !errors.As(status.Exit, &exit) || exit.ExitCode() != 3 {
		t.Errorf("Status reported %v, and the target exited 3.", status.Exit)
	}
}

// An exit of zero mid-run stops the target just as surely.
func TestStatusAfterAnExitOfZero(t *testing.T) {
	binary, _ := newBinary(t, 0, "exit 0")
	mustStart(t, binary)

	status := awaitExit(t, binary)

	if !errors.Is(status.Exit, launch.ErrExitedZero) {
		t.Errorf("Status reported %v for a target that exited 0.", status.Exit)
	}
}

func TestStatusWhileTheTargetRuns(t *testing.T) {
	binary, log := newBinary(t, 0, "echo started; "+forever)
	beforeStart := time.Now()
	mustStart(t, binary)
	waitForLog(t, log, "started")

	status := binary.Status()

	if !status.Running || status.Restarting || status.Exit != nil || status.Started.Before(beforeStart) || status.Started.After(time.Now()) {
		t.Errorf("Status reported %+v for a target that is still running since after %v.", status, beforeStart)
	}
}

// Before Start and after Stop, reconciler-fuzzer is running no target and
// nothing stopped on its own.
func TestStatusWithNoTargetRunning(t *testing.T) {
	binary, log := newBinary(t, 0, "echo started; "+forever)

	if status := binary.Status(); status.Running || status.Exit != nil {
		t.Errorf("Status reported %+v before Start.", status)
	}

	mustStart(t, binary)
	waitForLog(t, log, "started")
	if err := binary.Stop(t.Context()); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	if status := binary.Status(); status.Running || status.Exit != nil {
		t.Errorf("Status reported %+v after Stop.", status)
	}
}

func requireOpen(t *testing.T, exited <-chan struct{}, while string) {
	t.Helper()
	select {
	case <-exited:
		t.Fatalf("Exited was closed while %s.", while)
	default:
	}
}

func requireClosed(t *testing.T, exited <-chan struct{}, when string) {
	t.Helper()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatalf("Exited was still open %s.", when)
	}
}

// A settle wait ends where the target stopped, so its caller waits on the
// process rather than polling Status (DESIGN.md §5.5).
func TestExitedClosesWhenTheTargetStops(t *testing.T) {
	quit := filepath.Join(t.TempDir(), "quit")
	binary, log := newBinary(t, 0, `echo started; until [ -e "$1" ]; do sleep 0.05; done; exit 3`, quit)
	mustStart(t, binary)
	waitForLog(t, log, "started")
	exited := binary.Exited()
	requireOpen(t, exited, "the target was running")

	if err := os.WriteFile(quit, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	requireClosed(t, exited, "after the target exited")
}

// Before Start and after Stop reconciler-fuzzer runs no target, and a caller
// has nothing to wait for.
func TestExitedWithNoTargetRunning(t *testing.T) {
	binary, log := newBinary(t, 0, "echo started; "+forever)
	requireClosed(t, binary.Exited(), "before Start")

	mustStart(t, binary)
	waitForLog(t, log, "started")
	requireOpen(t, binary.Exited(), "the target was running")
	if err := binary.Stop(t.Context()); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	requireClosed(t, binary.Exited(), "after Stop")
}

// Restart execs a second process, and a caller waits on that one.
func TestExitedAfterARestart(t *testing.T) {
	binary, log := newBinary(t, 0, "echo started; "+forever)
	mustStart(t, binary)
	waitForLog(t, log, "started")
	killed := binary.Exited()

	if err := binary.Restart(t.Context()); err != nil {
		t.Fatalf("Restart failed: %v", err)
	}

	requireClosed(t, killed, "after the target it belonged to was killed")
	waitForLogCount(t, log, "started", 2)
	requireOpen(t, binary.Exited(), "the replacement was running")
}

// A caller reads why the target stopped as soon as Exited closes, so the exit
// is set before the close.
func TestStatusIsSetWhereExitedCloses(t *testing.T) {
	for range 20 {
		binary, _ := newBinary(t, 0, "exit 3")
		mustStart(t, binary)

		<-binary.Exited()

		if status := binary.Status(); status.Running || status.Exit == nil {
			t.Fatalf("Status reported %+v where Exited closed, want the exit that closed it.", status)
		}
	}
}

// Restart replaces the process a caller is waiting on, while it waits.
func TestExitedWhileTheTargetRestarts(t *testing.T) {
	binary, log := newBinary(t, 0, "echo started; "+forever)
	mustStart(t, binary)
	waitForLog(t, log, "started")
	restarting, read := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(read)
		for {
			select {
			case <-restarting:
				return
			default:
				binary.Exited()
			}
		}
	}()

	err := binary.Restart(t.Context())

	close(restarting)
	<-read
	if err != nil {
		t.Fatalf("Restart failed: %v", err)
	}
	waitForLogCount(t, log, "started", 2)
	requireOpen(t, binary.Exited(), "the replacement was running")
}

// exits records what a supervised target's exits reported, in order.
type exits struct {
	mu       sync.Mutex
	seen     []error
	restarts []time.Time
}

func (e *exits) record(exit error, restart time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen = append(e.seen, exit)
	e.restarts = append(e.restarts, restart)
}

func (e *exits) all() []error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.seen)
}

func (e *exits) restartsAt() []time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.restarts)
}

// newSupervised is newBinary with a restart backoff, and a record of what
// supervision hears.
func newSupervised(t *testing.T, backoff time.Duration, script string, args ...string) (*launch.Binary, *safeBuffer, *exits) {
	t.Helper()
	log := &safeBuffer{}
	binary := launch.NewBinary(launch.Options{
		Path:    "/bin/sh",
		Args:    append([]string{"-c", script, "sh"}, args...),
		Log:     log,
		Backoff: backoff,
	})
	t.Cleanup(func() { _ = binary.Stop(context.Background()) })
	return binary, log, &exits{}
}

// quitFile makes a script exit once a file appears, so that a test decides when
// the target exits.
func quitFile(t *testing.T) (path, script string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "quit")
	return path, `echo started; until [ -e "$1" ]; do sleep 0.05; done; rm -f "$1"; exit 3`
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A supervised target that exits on its own starts again at once, as a
// kubelet restarts a container, and the caller hears why it stopped.
func TestASupervisedTargetThatExitsStartsAgain(t *testing.T) {
	quit, script := quitFile(t)
	binary, log, heard := newSupervised(t, launch.MaxBackoff, script, quit)
	mustStart(t, binary)
	waitForLog(t, log, "started")
	binary.Supervise(t.Context(), heard.record)
	exited := binary.Exited()
	beforeExit := time.Now()

	touch(t, quit)

	waitForLogCount(t, log, "started", 2)
	var exit *exec.ExitError
	if seen := heard.all(); len(seen) != 1 || !errors.As(seen[0], &exit) || exit.ExitCode() != 3 {
		t.Errorf("The supervisor reported the exits %v, want the one exit status 3.", seen)
	}
	requireOpen(t, exited, "the target was restarted")
	if status := binary.Status(); !status.Running || status.Restarting || status.Exit != nil || status.Started.Before(beforeExit) {
		t.Errorf("Status reported %+v for a target that was restarted after %v.", status, beforeExit)
	}
}

// Every restart after the first waits, so a target that keeps exiting does not
// spin. The caller hears when each restart comes.
func TestASupervisedTargetWaitsBeforeItsSecondRestart(t *testing.T) {
	binary, log, heard := newSupervised(t, launch.MaxBackoff, "echo started; sleep 0.1; exit 3")
	mustStart(t, binary)
	waitForLog(t, log, "started")
	supervised := time.Now()
	binary.Supervise(t.Context(), heard.record)

	waitForLogCount(t, log, "started", 2)
	deadline := time.Now().Add(10 * time.Second)
	for len(heard.all()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	heardBoth := time.Now()
	time.Sleep(300 * time.Millisecond)

	if got := strings.Count(log.String(), "started"); got != 2 {
		t.Errorf("The target started %d times, want 2: the second restart waits minutes.", got)
	}
	if status := binary.Status(); !status.Running || !status.Restarting {
		t.Errorf("Status reported %+v for a target that is waiting to restart.", status)
	}
	restarts := heard.restartsAt()
	if len(restarts) != 2 {
		t.Fatalf("The supervisor reported %d restarts, want 2.", len(restarts))
	}
	if first := restarts[0]; first.Before(supervised) || first.After(heardBoth) {
		t.Errorf("The first restart is at %v, want it at once, between %v and %v.", first, supervised, heardBoth)
	}
	if second := restarts[1]; second.Before(supervised.Add(launch.MaxBackoff)) || second.After(heardBoth.Add(launch.MaxBackoff)) {
		t.Errorf("The second restart is at %v, want it %v after the exit.", second, launch.MaxBackoff)
	}
}

// Stop and Restart end the process themselves, so neither is an exit.
func TestAStopOrRestartIsNoExit(t *testing.T) {
	binary, log, heard := newSupervised(t, launch.MaxBackoff, "echo started; "+forever)
	mustStart(t, binary)
	waitForLog(t, log, "started")
	binary.Supervise(t.Context(), heard.record)

	if err := binary.Restart(t.Context()); err != nil {
		t.Fatalf("Restart failed: %v", err)
	}
	waitForLogCount(t, log, "started", 2)
	if err := binary.Stop(t.Context()); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	if seen := heard.all(); len(seen) != 0 {
		t.Errorf("The supervisor reported the exits %v, and reconciler-fuzzer ended every process itself.", seen)
	}
	waitForLogCount(t, log, "started", 2)
	if status := binary.Status(); status.Running {
		t.Errorf("Status reported %+v after Stop.", status)
	}
	requireClosed(t, binary.Exited(), "after Stop")
}

// Stop during the backoff leaves nothing to restart.
func TestStopCancelsARestartThatIsWaiting(t *testing.T) {
	binary, log, heard := newSupervised(t, time.Second, "echo started; exit 3")
	mustStart(t, binary)
	binary.Supervise(t.Context(), heard.record)
	for deadline := time.Now().Add(10 * time.Second); len(heard.all()) < 2; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("The target never exited a second time.")
		}
	}

	if err := binary.Stop(t.Context()); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)

	if got := strings.Count(log.String(), "started"); got != 2 {
		t.Errorf("The target started %d times, want 2: Stop came before the second restart.", got)
	}
}

// The caller reads what supervision heard once the target is stopped, so Stop
// waits for an exit that is being heard.
func TestStopReturnsOnceTheExitIsHeard(t *testing.T) {
	quit, script := quitFile(t)
	binary, log, heard := newSupervised(t, launch.MaxBackoff, script, quit)
	mustStart(t, binary)
	waitForLog(t, log, "started")
	binary.Supervise(t.Context(), func(exit error, restart time.Time) {
		time.Sleep(300 * time.Millisecond)
		heard.record(exit, restart)
	})
	touch(t, quit)
	for deadline := time.Now().Add(10 * time.Second); fileExists(quit); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("The target never took the quit file.")
		}
	}
	time.Sleep(50 * time.Millisecond)

	if err := binary.Stop(t.Context()); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	if seen := heard.all(); len(seen) != 1 {
		t.Errorf("Stop returned with the exits %v heard, want the one the target made.", seen)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// An interrupt ends supervision, so a target that exits after it stays down.
func TestSupervisionEndsWithItsContext(t *testing.T) {
	quit, script := quitFile(t)
	binary, log, heard := newSupervised(t, launch.MaxBackoff, script, quit)
	mustStart(t, binary)
	waitForLog(t, log, "started")
	ctx, cancel := context.WithCancel(t.Context())
	binary.Supervise(ctx, heard.record)

	cancel()
	touch(t, quit)

	requireClosed(t, binary.Exited(), "after the target exited unsupervised")
	if got := strings.Count(log.String(), "started"); got != 1 {
		t.Errorf("The target started %d times, want once.", got)
	}
	if seen := heard.all(); len(seen) != 0 {
		t.Errorf("The supervisor reported the exits %v, and supervision had ended.", seen)
	}
	var exit *exec.ExitError
	if status := binary.Status(); status.Running || !errors.As(status.Exit, &exit) || exit.ExitCode() != 3 {
		t.Errorf("Status reported %+v, want a target that stopped with exit status 3.", status)
	}
}

func TestARestartThatIsWaitingEndsWithSupervision(t *testing.T) {
	binary, log, heard := newSupervised(t, 200*time.Millisecond, "echo started; exit 3")
	mustStart(t, binary)
	ctx, cancel := context.WithCancel(t.Context())
	binary.Supervise(ctx, heard.record)
	for deadline := time.Now().Add(10 * time.Second); len(heard.all()) < 2; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("The target never exited a second time.")
		}
	}

	cancel()

	requireClosed(t, binary.Exited(), "once the restart that was waiting came due")
	if got := strings.Count(log.String(), "started"); got != 2 {
		t.Errorf("The target started %d times, want 2: supervision ended before the second restart.", got)
	}
}

// A target that cannot start again is gone for good, and says why.
func TestASupervisedTargetThatCannotStartAgainStops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deletes-itself")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho started\nrm \"$0\"\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	log := &safeBuffer{}
	binary := launch.NewBinary(launch.Options{Path: path, Log: log})
	t.Cleanup(func() { _ = binary.Stop(context.Background()) })
	heard := &exits{}
	mustStart(t, binary)
	binary.Supervise(t.Context(), heard.record)

	requireClosed(t, binary.Exited(), "after the restart failed")
	status := binary.Status()
	if status.Running || status.Exit == nil {
		t.Fatalf("Status reported %+v for a target that could not start again.", status)
	}
	for _, want := range []string{"exit status 3", "deletes-itself"} {
		if !strings.Contains(status.Exit.Error(), want) {
			t.Errorf("Status reported %q, which does not mention %q.", status.Exit, want)
		}
	}
}
