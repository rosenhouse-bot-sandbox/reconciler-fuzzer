package main

import (
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func build(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "crudfake")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return bin
}

func config(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("collections: {b: {pending: P, ready: R, deleting: D}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestServesOnTheAddrEnv(t *testing.T) {
	bin := build(t)
	addr := freeAddr(t)
	cmd := exec.Command(bin, "--config", config(t))
	cmd.Env = append(os.Environ(), "RECONCILER_FUZZER_ADDR="+addr)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/b")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("got %d", resp.StatusCode)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestExitsTwoOnAnError(t *testing.T) {
	bin := build(t)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	for name, args := range map[string][]string{
		"no config":   {"--listen", freeAddr(t)},
		"listen busy": {"--config", config(t), "--listen", busy.Addr().String()},
	} {
		err := exec.Command(bin, args...).Run()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 {
			t.Errorf("%s: got %v, want exit 2", name, err)
		}
	}
}
