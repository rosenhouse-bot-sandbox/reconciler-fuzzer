package launch_test

import (
	"strings"
	"testing"
)

func TestSetCommandChangesLaterStartsAndRestarts(t *testing.T) {
	t.Skip("enabled by the restart package")
	b, h, log := newBinary(t, "echo one; "+forever)
	mustStart(t, b)
	eventually(t, "the first command runs", func() bool { return log.String() == "one\n" })
	b.SetCommand("", []string{"-c", "echo two; sleep 0.05; exit 1"})
	if err := b.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	mustStart(t, b)
	if err := b.Supervise(true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a restart runs the new command", func() bool { return strings.Count(log.String(), "two\n") >= 2 })
	b.Supervise(false)
	if got := log.String(); strings.Count(got, "one\n") != 1 {
		t.Errorf("Logged %q, want one only from the first start.", got)
	}
	if starts, _ := h.counts(); starts < 3 {
		t.Errorf("Heard %d starts, want at least 3.", starts)
	}
}

func TestSetCommandKeepsWhatItIsNotGiven(t *testing.T) {
	t.Skip("enabled by the restart package")
	b, h, log := newBinary(t, "echo sh")
	for i, args := range [][]string{nil, nil, {}} {
		path := "/bin/echo"
		if i > 0 {
			path = ""
		}
		b.SetCommand(path, args)
		mustStart(t, b)
		eventually(t, "the command exits", func() bool { _, exits := h.counts(); return exits == i+1 })
	}
	if got, want := log.String(), "-c echo sh\n-c echo sh\n\n"; got != want {
		t.Errorf("Logged %q, want %q.", got, want)
	}
}
