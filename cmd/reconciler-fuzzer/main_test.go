package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// asFuzzer has the test binary run as reconciler-fuzzer, over a session that
// runs until it is interrupted, so that a test signals a process other than its
// own.
const asFuzzer = "RECONCILER_FUZZER_TEST_AS_FUZZER"

// closedFile is where that reconciler-fuzzer records that it closed its
// session, and closeTakes how long closing takes.
const (
	closedFile = "RECONCILER_FUZZER_TEST_CLOSED_FILE"
	closeTakes = "RECONCILER_FUZZER_TEST_CLOSE_TAKES"
)

func TestMain(m *testing.M) {
	if os.Getenv(asFuzzer) != "" {
		c := newCLI(os.Stdout, os.Stderr)
		c.newGenerator = countingGenerator(nil)
		c.open = func(options, *target.Target) (session, error) { return untilInterrupted{}, nil }
		os.Exit(c.interruptible(os.Args[1:]))
	}
	os.Exit(m.Run())
}

type untilInterrupted struct{}

func (untilInterrupted) vet(*target.Target) error { return nil }

func (untilInterrupted) prepare(context.Context, *target.Target) error { return nil }

func (untilInterrupted) execute(ctx context.Context, _ *target.Target, _ run.Sequence, dir string, _ run.Checker) (run.Result, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return run.Result{}, err
	}
	<-ctx.Done()
	return run.Result{}, ctx.Err()
}

func (untilInterrupted) close() error {
	takes, _ := time.ParseDuration(os.Getenv(closeTakes))
	time.Sleep(takes)
	return os.WriteFile(os.Getenv(closedFile), nil, 0o644)
}

// fuzzerProcess is the test binary running as reconciler-fuzzer, in a process
// group of its own.
type fuzzerProcess struct {
	cmd            *exec.Cmd
	exited         <-chan struct{}
	closed         string
	out            string
	stdout, stderr *output
	lines          chan string
	// readers are the ends of reconciler-fuzzer's output pipes that the test
	// reads.
	readers []*os.File
	reading sync.WaitGroup
}

// output keeps what reconciler-fuzzer wrote to one stream, and passes each line
// on.
type output struct {
	mu      sync.Mutex
	written strings.Builder
	partial string
	lines   chan<- string
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.written.Write(p)
	o.partial += string(p)
	for {
		line, rest, found := strings.Cut(o.partial, "\n")
		if !found {
			return len(p), nil
		}
		o.lines <- line
		o.partial = rest
	}
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.written.String()
}

// startFuzzer starts reconciler-fuzzer under the shell's prefix, if
// any, which can set how it starts out handling signals.
func startFuzzer(t *testing.T, takes time.Duration, prefix ...string) *fuzzerProcess {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 1000)
	f := &fuzzerProcess{closed: filepath.Join(t.TempDir(), "closed"), out: t.TempDir(),
		stdout: &output{lines: lines}, stderr: &output{lines: lines}, lines: lines}
	args := append(prefix, self, "run", "--target", toyTargetYAML, "--out", f.out, "--runs", "3")
	f.cmd = exec.Command(args[0], args[1:]...)
	f.cmd.Env = append(os.Environ(), asFuzzer+"=1", closedFile+"="+f.closed, closeTakes+"="+takes.String())
	f.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, stderr := f.pipe(t, f.stdout), f.pipe(t, f.stderr)
	f.cmd.Stdout, f.cmd.Stderr = stdout, stderr
	err = f.cmd.Start()
	_, _ = stdout.Close(), stderr.Close()
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = f.cmd.Wait()
		f.reading.Wait()
	}()
	f.exited = exited
	t.Cleanup(func() {
		_ = f.cmd.Process.Kill()
		<-exited
	})
	return f
}

// pipe passes what reconciler-fuzzer writes to one stream on to out.
func (f *fuzzerProcess) pipe(t *testing.T, out *output) *os.File {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	f.readers = append(f.readers, reader)
	f.reading.Go(func() { _, _ = io.Copy(out, reader) })
	return writer
}

// stopReading closes the pipes reconciler-fuzzer writes to, as a Ctrl-C does to
// the tee that reads them.
func (f *fuzzerProcess) stopReading() {
	for _, reader := range f.readers {
		_ = reader.Close()
	}
}

// waitFor waits for reconciler-fuzzer to print a line holding want.
func (f *fuzzerProcess) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(time.Minute)
	for {
		select {
		case line := <-f.lines:
			if strings.Contains(line, want) {
				return
			}
		case <-deadline:
			t.Fatalf("reconciler-fuzzer never printed %q.\nstdout:\n%s\nstderr:\n%s", want, f.stdout, f.stderr)
		}
	}
}

// signal sends sig to reconciler-fuzzer, or to its process group as a
// terminal's Ctrl-C does.
func (f *fuzzerProcess) signal(t *testing.T, sig syscall.Signal, group bool) {
	t.Helper()
	pid := f.cmd.Process.Pid
	if group {
		pid = -pid
	}
	if err := syscall.Kill(pid, sig); err != nil {
		t.Fatal(err)
	}
}

// exit waits for reconciler-fuzzer to exit, and returns how it did.
func (f *fuzzerProcess) exit(t *testing.T, within time.Duration) syscall.WaitStatus {
	t.Helper()
	select {
	case <-f.exited:
	case <-time.After(within):
		t.Fatalf("reconciler-fuzzer was still running %v after the signal.\nstderr:\n%s", within, f.stderr)
	}
	return f.cmd.ProcessState.Sys().(syscall.WaitStatus)
}

func (f *fuzzerProcess) closedItsSession() bool {
	_, err := os.Stat(f.closed)
	return err == nil
}

// A signal interrupts reconciler-fuzzer, which stops what it started and then
// dies of that signal, so that a shell, make or timeout sees what happened.
func TestASignalStopsReconcilerFuzzerAndThenKillsIt(t *testing.T) {
	for _, test := range []struct {
		name   string
		signal syscall.Signal
		group  bool
	}{
		{"SIGTERM", syscall.SIGTERM, false},
		{"a terminal's Ctrl-C", syscall.SIGINT, true},
		{"SIGHUP", syscall.SIGHUP, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if signal.Ignored(test.signal) {
				t.Skipf("The tests run ignoring %v, and so would reconciler-fuzzer.", test.signal)
			}
			f := startFuzzer(t, 0)
			f.waitFor(t, "run 1:")

			f.signal(t, test.signal, test.group)

			status := f.exit(t, 10*time.Second)
			if !status.Signaled() || status.Signal() != test.signal {
				t.Errorf("reconciler-fuzzer ended with %v, want death by %v.", status, test.signal)
			}
			if !f.closedItsSession() {
				t.Error("reconciler-fuzzer never closed its session.")
			}
			stderr := f.stderr.String()
			for _, want := range []string{"reconciler-fuzzer: an interrupt arrived", "run 1: an interrupt stopped the run"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("reconciler-fuzzer printed\n%s\nwhich does not say %q.", stderr, want)
				}
			}
			written := readSummary(t, f.out)
			if written.Outcome != "interrupted" || written.ExitCode != 128+int(test.signal) || written.Runs[0].Outcome != "interrupted" {
				t.Errorf("The summary says %s, exit %d, and run 1 %s, want run 1 interrupted and exit %d.",
					written.Outcome, written.ExitCode, written.Runs[0].Outcome, 128+int(test.signal))
			}
		})
	}
}

// reconciler-fuzzer 2>&1 | tee log: an interrupt kills tee too, and
// reconciler-fuzzer writes on. SIGTERM stands in for a Ctrl-C, which a
// background job starts out ignoring.
func TestAnInterruptOutlivesTheReaderOfItsOutput(t *testing.T) {
	f := startFuzzer(t, 0)
	f.waitFor(t, "run 1:")
	f.stopReading()

	f.signal(t, syscall.SIGTERM, true)

	status := f.exit(t, 10*time.Second)
	if !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Errorf("reconciler-fuzzer ended with %v, want death by SIGTERM.", status)
	}
	if !f.closedItsSession() {
		t.Error("reconciler-fuzzer never closed its session.")
	}
}

func TestASecondSignalKillsReconcilerFuzzerAtOnce(t *testing.T) {
	f := startFuzzer(t, time.Hour)
	f.waitFor(t, "run 1:")
	f.signal(t, syscall.SIGTERM, false)
	f.waitFor(t, "reconciler-fuzzer: an interrupt arrived")

	f.signal(t, syscall.SIGTERM, false)

	status := f.exit(t, 10*time.Second)
	if !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Errorf("reconciler-fuzzer ended with %v, want death by SIGTERM.", status)
	}
	if f.closedItsSession() {
		t.Error("reconciler-fuzzer closed its session, and the second signal came first.")
	}
}

// nohup starts a program ignoring SIGHUP, and a shell starts a background job
// ignoring SIGINT. reconciler-fuzzer leaves those alone.
func TestASignalReconcilerFuzzerWasStartedIgnoringStaysIgnored(t *testing.T) {
	f := startFuzzer(t, 0, "/bin/sh", "-c", `trap "" HUP INT; exec "$@"`, "sh")
	f.waitFor(t, "run 1:")

	f.signal(t, syscall.SIGHUP, false)
	f.signal(t, syscall.SIGINT, false)
	f.signal(t, syscall.SIGTERM, false)

	status := f.exit(t, 10*time.Second)
	if !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Errorf("reconciler-fuzzer ended with %v, want death by SIGTERM: it was started ignoring SIGHUP and SIGINT.", status)
	}
}
