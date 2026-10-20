package trace

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"
	"time"
)

// Recorder appends events to trace.jsonl and keeps them in memory. Any
// goroutine may call Record; one writer orders and writes the events.
type Recorder struct {
	start time.Time

	mu     sync.Mutex
	events []Event
	file   *os.File
	out    *bufio.Writer
	err    error
}

// NewRecorder creates the file at path and starts the clock.
func NewRecorder(path string) (*Recorder, error) {
	file, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &Recorder{start: time.Now(), file: file, out: bufio.NewWriter(file)}, nil
}

// Record assigns e its Seq and At, then appends it.
func (r *Recorder) Record(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.Seq = uint64(len(r.events) + 1)
	e.At = r.Now()
	r.events = append(r.events, e)
	if r.err != nil {
		return
	}
	line, err := json.Marshal(e)
	if err == nil {
		line = append(line, '\n')
		_, err = r.out.Write(line)
	}
	r.err = err
}

// Now is the time since the recorder started.
func (r *Recorder) Now() time.Duration { return time.Since(r.start) }

// Events returns a copy of every event recorded so far, in Seq order.
func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

// EventsSince returns a copy of the events after the first n.
func (r *Recorder) EventsSince(n int) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events[min(n, len(r.events)):])
}

// Close flushes and closes the file. It returns the first write error.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err == nil {
		r.err = r.out.Flush()
	}
	if err := r.file.Close(); r.err == nil {
		r.err = err
	}
	return r.err
}

// Load reads a trace.jsonl file.
func Load(path string) ([]Event, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	in := bufio.NewReader(file)
	var events []Event
	for number := 1; ; number++ {
		line, err := in.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var e Event
			if jsonErr := json.Unmarshal(line, &e); jsonErr != nil {
				return nil, fmt.Errorf("%s line %d: %w", path, number, jsonErr)
			}
			events = append(events, e)
		}
		if err == io.EOF {
			return events, nil
		}
		if err != nil {
			return nil, err
		}
	}
}
