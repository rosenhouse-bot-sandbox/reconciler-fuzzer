// Package launch runs the controller as a local process.
package launch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"sync"
	"syscall"
	"time"
)

// InitialBackoff is the delay before supervision restarts a process that
// exited on its own. The delay doubles with each such restart, up to
// MaxBackoff, and resets when the harness starts the process.
const (
	InitialBackoff = 100 * time.Millisecond
	MaxBackoff     = 5 * time.Second
)

// TailBytes is how much of the controller's output an Exit carries.
const TailBytes = 4096

// waitDelay bounds how long a reap waits for a descendant that holds the
// output pipe open.
const waitDelay = 500 * time.Millisecond

// reapTimeout bounds how long a kill waits for the reap.
const reapTimeout = 5 * time.Second

// Options configure a Binary. Args and Env arrive already expanded.
type Options struct {
	Path string
	Args []string
	// Env overrides variables the controller inherits.
	Env map[string]string
	// Log receives stdout and stderr through one pipe.
	Log io.Writer
	// GracePeriod is how long Stop waits after SIGTERM before SIGKILL.
	GracePeriod time.Duration
	// OnStart and OnExit hear every start and exit, including restarts.
	OnStart func(pid int)
	OnExit  func(Exit)
}

// Exit describes one end of the process.
type Exit struct {
	PID int
	// Status is the exit status or signal, such as "exit status 2".
	Status string
	// HarnessCaused says Stop or Kill ended the process.
	HarnessCaused bool
	// GracePeriodPassed says the process outlived the grace period, and the
	// SIGKILL that Stop then sent ended it.
	GracePeriodPassed bool
	// Tail is the last TailBytes of the process's output.
	Tail string
}

// Binary runs one controller process at a time.
type Binary struct {
	o Options
	// ops serializes Start, Stop, Kill and Supervise. The reaper never takes
	// it, so those calls may wait for a reap while holding it.
	ops sync.Mutex

	mu        sync.Mutex
	running   *process
	supervise bool
	backoff   time.Duration
}

type process struct {
	cmd  *exec.Cmd
	tail *ring
	// harness is set before Stop or Kill signals the process.
	harness bool
	// gracePeriodPassed is set before Stop sends SIGKILL once the grace period
	// passes.
	gracePeriodPassed bool
	// announced closes once OnStart has run, so OnExit never comes first.
	announced chan struct{}
	// done closes once OnExit has run.
	done chan struct{}
}

func New(o Options) *Binary {
	if o.Log == nil {
		o.Log = io.Discard
	}
	return &Binary{o: o}
}

// Start execs the binary. The context bounds the call, not the process.
func (b *Binary) Start(ctx context.Context) error {
	b.ops.Lock()
	defer b.ops.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	if b.running != nil {
		b.mu.Unlock()
		return fmt.Errorf("%s is already running", b.o.Path)
	}
	b.backoff = 0
	p, err := b.start()
	b.mu.Unlock()
	if err != nil {
		return err
	}
	b.announce(p)
	return nil
}

// start execs a process. The caller holds b.mu and must announce it.
func (b *Binary) start() (*process, error) {
	cmd := exec.Command(b.o.Path, b.o.Args...)
	env := os.Environ()
	for _, name := range slices.Sorted(maps.Keys(b.o.Env)) {
		env = append(env, name+"="+b.o.Env[name])
	}
	cmd.Env = env
	tail := &ring{}
	// One writer for both streams gives the process a single pipe.
	out := io.MultiWriter(b.o.Log, tail)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = waitDelay
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", b.o.Path, err)
	}
	p := &process{cmd: cmd, tail: tail, announced: make(chan struct{}), done: make(chan struct{})}
	b.running = p
	go b.reap(p)
	return p, nil
}

func (b *Binary) announce(p *process) {
	if b.o.OnStart != nil {
		b.o.OnStart(p.cmd.Process.Pid)
	}
	close(p.announced)
}

// reap waits for p and, after the backoff, restarts it when supervision is
// on and the harness did not end it.
func (b *Binary) reap(p *process) {
	err := p.cmd.Wait()
	status := fmt.Sprint(err)
	var killed bool
	if s := p.cmd.ProcessState; s != nil {
		status = s.String()
		ws, _ := s.Sys().(syscall.WaitStatus)
		killed = ws.Signaled() && ws.Signal() == syscall.SIGKILL
	}

	b.mu.Lock()
	exit := Exit{PID: p.cmd.Process.Pid, Status: status, HarnessCaused: p.harness, GracePeriodPassed: p.gracePeriodPassed && killed, Tail: p.tail.String()}
	var delay time.Duration
	if b.running == p {
		b.running = nil
		if b.supervise && !p.harness {
			b.backoff = min(max(2*b.backoff, InitialBackoff), MaxBackoff)
			delay = b.backoff
		}
	}
	b.mu.Unlock()

	<-p.announced
	if b.o.OnExit != nil {
		b.o.OnExit(exit)
	}
	close(p.done)
	if delay > 0 {
		time.AfterFunc(delay, b.restart)
	}
}

// restart starts a process if supervision is still on and none runs.
func (b *Binary) restart() {
	b.mu.Lock()
	if !b.supervise || b.running != nil {
		b.mu.Unlock()
		return
	}
	p, err := b.start()
	b.mu.Unlock()
	if err != nil {
		fmt.Fprintf(b.o.Log, "reconciler-fuzzer: restarting the controller: %v\n", err)
		return
	}
	b.announce(p)
}

// claim marks the running process as ended by the harness and returns it,
// or nil when none runs.
func (b *Binary) claim() *process {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running != nil {
		b.running.harness = true
	}
	return b.running
}

// Stop sends SIGTERM, then SIGKILL after the grace period, and waits until
// the process is reaped. It does nothing when no process runs.
func (b *Binary) Stop(ctx context.Context) error {
	b.ops.Lock()
	defer b.ops.Unlock()
	p := b.claim()
	if p == nil {
		return nil
	}
	if err := signal(p, syscall.SIGTERM); err != nil {
		return err
	}
	grace := time.NewTimer(b.o.GracePeriod)
	defer grace.Stop()
	select {
	case <-p.done:
		return nil
	case <-grace.C:
		b.mu.Lock()
		p.gracePeriodPassed = true
		b.mu.Unlock()
	case <-ctx.Done():
	}
	return b.kill(p)
}

// Kill sends SIGKILL and waits until the process is reaped.
func (b *Binary) Kill(ctx context.Context) error {
	b.ops.Lock()
	defer b.ops.Unlock()
	p := b.claim()
	if p == nil {
		return nil
	}
	return b.kill(p)
}

func (b *Binary) kill(p *process) error {
	if err := signal(p, syscall.SIGKILL); err != nil {
		return err
	}
	reap := time.NewTimer(reapTimeout)
	defer reap.Stop()
	select {
	case <-p.done:
		return nil
	case <-reap.C:
		return fmt.Errorf("%s did not exit within %v of SIGKILL", b.o.Path, reapTimeout)
	}
}

func signal(p *process, sig syscall.Signal) error {
	err := p.cmd.Process.Signal(sig)
	if err == nil || errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return fmt.Errorf("sending %s to %s: %w", sig, p.cmd.Path, err)
}

// Running reports whether a process runs now.
func (b *Binary) Running() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running != nil
}

// Supervise sets whether an exit the harness did not cause starts the
// process again after the backoff. It is off until called. Turning it on
// starts a process that is down at once.
func (b *Binary) Supervise(on bool) error {
	b.ops.Lock()
	defer b.ops.Unlock()
	b.mu.Lock()
	b.supervise = on
	if !on || b.running != nil {
		b.mu.Unlock()
		return nil
	}
	b.backoff = 0
	p, err := b.start()
	b.mu.Unlock()
	if err != nil {
		return err
	}
	b.announce(p)
	return nil
}

// ring keeps the last TailBytes written to it.
type ring struct {
	mu  sync.Mutex
	buf []byte
}

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if over := len(r.buf) - TailBytes; over > 0 {
		r.buf = append(r.buf[:0], r.buf[over:]...)
	}
	return len(p), nil
}

func (r *ring) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}
