package launch_test

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/launch"
)

// forever keeps the shell alive and able to handle signals.
const forever = "while :; do sleep 0.05; done"

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

// heard collects every start and exit.
type heard struct {
	mu       sync.Mutex
	starts   []int
	startsAt []time.Time
	exits    []launch.Exit
}

func (h *heard) start(pid int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.starts = append(h.starts, pid)
	h.startsAt = append(h.startsAt, time.Now())
}

// gap is the time between start i-1 and start i.
func (h *heard) gap(i int) time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.startsAt[i].Sub(h.startsAt[i-1])
}

func (h *heard) exit(e launch.Exit) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.exits = append(h.exits, e)
}

func (h *heard) counts() (starts, exits int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.starts), len(h.exits)
}

func (h *heard) lastExit() launch.Exit {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.exits[len(h.exits)-1]
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("Timed out waiting until %s.", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func newBinary(t *testing.T, script string, configure ...func(*launch.Options)) (*launch.Binary, *heard, *safeBuffer) {
	t.Helper()
	h, log := &heard{}, &safeBuffer{}
	o := launch.Options{
		Path:        "/bin/sh",
		Args:        []string{"-c", script},
		Log:         log,
		GracePeriod: 2 * time.Second,
		OnStart:     h.start,
		OnExit:      h.exit,
	}
	for _, c := range configure {
		c(&o)
	}
	b := launch.New(o)
	t.Cleanup(func() {
		b.Supervise(false)
		b.Stop(context.Background())
	})
	return b, h, log
}

func mustStart(t *testing.T, b *launch.Binary) {
	t.Helper()
	if err := b.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

func TestEnvOverridesTheInheritedEnvironment(t *testing.T) {
	t.Setenv("RF_INHERITED", "inherited")
	t.Setenv("RF_OVERRIDDEN", "inherited")
	b, h, log := newBinary(t, `echo "$RF_INHERITED $RF_OVERRIDDEN $RF_NEW"`, func(o *launch.Options) {
		o.Env = map[string]string{"RF_OVERRIDDEN": "override", "RF_NEW": "new"}
	})
	mustStart(t, b)
	eventually(t, "the script exits", func() bool { _, exits := h.counts(); return exits == 1 })

	if got := log.String(); got != "inherited override new\n" {
		t.Errorf("Logged %q, want %q.", got, "inherited override new\n")
	}
}

func TestExitCarriesTheTailOfBothStreams(t *testing.T) {
	b, h, log := newBinary(t, `head -c 5000 /dev/zero | tr '\0' a; echo; echo out; echo err >&2; exit 2`)
	mustStart(t, b)
	eventually(t, "the script exits", func() bool { _, exits := h.counts(); return exits == 1 })

	exit := h.lastExit()
	if len(exit.Tail) != launch.TailBytes || !strings.HasSuffix(exit.Tail, "aaa\nout\nerr\n") {
		t.Errorf("Tail has %d bytes ending %q, want %d ending with the last output.", len(exit.Tail), exit.Tail[max(0, len(exit.Tail)-20):], launch.TailBytes)
	}
	if !strings.HasSuffix(log.String(), exit.Tail) || len(log.String()) != 5009 {
		t.Errorf("Logged %d bytes, want all 5009 with the tail at the end.", len(log.String()))
	}
	if exit.Status != "exit status 2" || exit.HarnessCaused || exit.PID != h.starts[0] {
		t.Errorf("Exit is %+v, want a self-caused exit status 2 of pid %d.", exit, h.starts[0])
	}
}

func TestASuccessfulExitSaysSo(t *testing.T) {
	b, h, _ := newBinary(t, "exit 0")
	mustStart(t, b)
	eventually(t, "the script exits", func() bool { _, exits := h.counts(); return exits == 1 })

	if got := h.lastExit().Status; got != "exit status 0" {
		t.Errorf("Status is %q, want %q.", got, "exit status 0")
	}
	if b.Running() {
		t.Error("Running is true after the exit.")
	}
}

func TestStartWhileRunningFails(t *testing.T) {
	b, _, _ := newBinary(t, forever)
	mustStart(t, b)

	if err := b.Start(t.Context()); err == nil {
		t.Error("A second Start succeeded.")
	}
}

func TestStopTerminatesAndIsHarnessCaused(t *testing.T) {
	b, h, _ := newBinary(t, "trap 'exit 3' TERM; "+forever)
	mustStart(t, b)
	if !b.Running() {
		t.Fatal("Running is false after Start.")
	}
	time.Sleep(50 * time.Millisecond) // The trap must be set.

	if err := b.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}

	if b.Running() {
		t.Error("Running is true after Stop.")
	}
	if _, exits := h.counts(); exits != 1 {
		t.Fatalf("Heard %d exits by the time Stop returned, want 1.", exits)
	}
	if exit := h.lastExit(); exit.Status != "exit status 3" || !exit.HarnessCaused || !exit.Stopped {
		t.Errorf("Exit is %+v, want exit status 3 from a Stop.", exit)
	}
}

func TestStopEscalatesToSIGKILL(t *testing.T) {
	b, h, _ := newBinary(t, "trap '' TERM; "+forever, func(o *launch.Options) { o.GracePeriod = 200 * time.Millisecond })
	mustStart(t, b)
	time.Sleep(50 * time.Millisecond)
	start := time.Now()

	if err := b.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}

	if elapsed := time.Since(start); elapsed < 200*time.Millisecond || elapsed > time.Second {
		t.Errorf("Stop returned after %v, want just after the 200ms grace period.", elapsed)
	}
	if exit := h.lastExit(); exit.Status != "signal: killed" || !exit.HarnessCaused || !exit.Stopped {
		t.Errorf("Exit is %+v, want a SIGKILL from a Stop.", exit)
	}
}

func TestStopEscalatesWhenTheContextEnds(t *testing.T) {
	b, h, _ := newBinary(t, "trap '' TERM; "+forever, func(o *launch.Options) { o.GracePeriod = time.Hour })
	mustStart(t, b)
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	if err := b.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	if exit := h.lastExit(); exit.Status != "signal: killed" {
		t.Errorf("Exit is %+v, want a SIGKILL.", exit)
	}
}

func TestKillIsHarnessCaused(t *testing.T) {
	b, h, _ := newBinary(t, forever)
	mustStart(t, b)

	if err := b.Kill(t.Context()); err != nil {
		t.Fatal(err)
	}

	if b.Running() {
		t.Error("Running is true after Kill.")
	}
	if exit := h.lastExit(); exit.Status != "signal: killed" || !exit.HarnessCaused || exit.Stopped {
		t.Errorf("Exit is %+v, want a harness-caused SIGKILL that no Stop sent.", exit)
	}
}

func TestKillOutwaitsAGracePeriodShorterThanTheReap(t *testing.T) {
	// The background sleep holds the output pipe, so the reap takes the
	// wait delay of 500ms.
	b, _, _ := newBinary(t, "sleep 1 & "+forever, func(o *launch.Options) { o.GracePeriod = 50 * time.Millisecond })
	mustStart(t, b)
	time.Sleep(50 * time.Millisecond) // The sleep must start.

	if err := b.Kill(t.Context()); err != nil {
		t.Error(err)
	}
}

func TestStopAndKillOfNothingDoNothing(t *testing.T) {
	b, h, _ := newBinary(t, forever)

	if err := b.Stop(t.Context()); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if err := b.Kill(t.Context()); err != nil {
		t.Errorf("Kill: %v", err)
	}
	if starts, exits := h.counts(); starts != 0 || exits != 0 {
		t.Errorf("Heard %d starts and %d exits, want none.", starts, exits)
	}
}

func TestSupervisionRestartsACrash(t *testing.T) {
	b, h, _ := newBinary(t, "sleep 0.05; exit 1")
	mustStart(t, b)
	if err := b.Supervise(true); err != nil {
		t.Fatal(err)
	}

	eventually(t, "the script has started three times", func() bool { starts, _ := h.counts(); return starts >= 3 })
	if exit := h.lastExit(); exit.HarnessCaused {
		t.Errorf("Exit is %+v, want a self-caused one.", exit)
	}
}

func TestSupervisionBacksOffUntilTheHarnessStartsIt(t *testing.T) {
	b, h, _ := newBinary(t, "exit 1")
	b.Supervise(true)
	eventually(t, "the script has started four times", func() bool { starts, _ := h.counts(); return starts >= 4 })
	for i, least := range []time.Duration{launch.InitialBackoff, 2 * launch.InitialBackoff, 4 * launch.InitialBackoff} {
		if gap := h.gap(i + 1); gap < least {
			t.Errorf("Restart %d came %v after the start before it, want at least %v.", i+1, gap, least)
		}
	}
	b.Supervise(false)
	eventually(t, "the script stays down", func() bool { return !b.Running() })
	before, _ := h.counts()
	b.Supervise(true)
	eventually(t, "the script restarts twice more", func() bool { starts, _ := h.counts(); return starts >= before+2 })
	if gap := h.gap(before + 1); gap > 2*launch.InitialBackoff-10*time.Millisecond {
		t.Errorf("The first restart after Supervise came %v after it, want the initial backoff.", gap)
	}
}

func TestSupervisionDoesNotRestartAKill(t *testing.T) {
	b, h, _ := newBinary(t, forever)
	b.Supervise(true)

	if err := b.Kill(t.Context()); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)
	if starts, _ := h.counts(); starts != 1 || b.Running() {
		t.Errorf("Heard %d starts and Running is %v, want one start and no process.", starts, b.Running())
	}
}

func TestWithoutSupervisionACrashStaysDown(t *testing.T) {
	b, h, _ := newBinary(t, "exit 1")
	b.Supervise(true)
	eventually(t, "the script restarts", func() bool { starts, _ := h.counts(); return starts >= 2 })

	b.Supervise(false)

	eventually(t, "the script stays down", func() bool {
		starts, exits := h.counts()
		return starts == exits && !b.Running()
	})
	before, _ := h.counts()
	time.Sleep(100 * time.Millisecond)
	if after, _ := h.counts(); after != before {
		t.Errorf("Heard %d starts after supervision ended, want none.", after-before)
	}
}

func TestSupervisionStartsADownProcess(t *testing.T) {
	b, h, _ := newBinary(t, forever)

	if err := b.Supervise(true); err != nil {
		t.Fatal(err)
	}

	if starts, _ := h.counts(); starts != 1 || !b.Running() {
		t.Errorf("Heard %d starts and Running is %v, want one running process.", starts, b.Running())
	}
	if err := b.Supervise(true); err != nil {
		t.Fatal(err)
	}
	if starts, _ := h.counts(); starts != 1 {
		t.Errorf("Heard %d starts, want Supervise to leave a running process alone.", starts)
	}
}

func TestCallbacksMayAskWhetherItRuns(t *testing.T) {
	var b *launch.Binary
	var runningAtStart, runningAtExit []bool
	b, _, _ = newBinary(t, forever, func(o *launch.Options) {
		o.OnStart = func(int) { runningAtStart = append(runningAtStart, b.Running()) }
		o.OnExit = func(launch.Exit) { runningAtExit = append(runningAtExit, b.Running()) }
	})
	b.Supervise(true)

	if err := b.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}

	if len(runningAtStart) != 1 || !runningAtStart[0] || len(runningAtExit) != 1 || runningAtExit[0] {
		t.Errorf("Running was %v at start and %v at exit, want [true] and [false].", runningAtStart, runningAtExit)
	}
}

func TestStartFailsForAMissingBinary(t *testing.T) {
	b, h, _ := newBinary(t, "", func(o *launch.Options) { o.Path = "/nonexistent/controller" })

	if err := b.Start(t.Context()); err == nil {
		t.Error("Start succeeded.")
	}
	if starts, _ := h.counts(); starts != 0 || b.Running() {
		t.Error("A failed start counts as running.")
	}
}

func TestOnExitNeverPrecedesOnStart(t *testing.T) {
	var mu sync.Mutex
	var order []string
	note := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, s)
	}
	b, _, _ := newBinary(t, "exit 0", func(o *launch.Options) {
		o.OnStart = func(int) { time.Sleep(50 * time.Millisecond); note("start") }
		o.OnExit = func(launch.Exit) { note("exit") }
	})
	mustStart(t, b)

	eventually(t, "the exit is heard", func() bool { mu.Lock(); defer mu.Unlock(); return len(order) == 2 })
	if order[0] != "start" {
		t.Errorf("Heard %q, want start before exit.", order)
	}
}
