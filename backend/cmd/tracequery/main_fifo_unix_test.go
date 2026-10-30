//go:build unix

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
)

func TestQueryRefusesAFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spans.jsonl")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	a, _ := correlation.New()
	type result struct {
		code           int
		stdout, stderr string
	}
	done := make(chan result, 1)
	go func() {
		code, stdout, stderr := runQuery("-file", path, a.String())
		done <- result{code, stdout, stderr}
	}()
	select {
	case got := <-done:
		want := "tracequery: " + path + ": not a regular file\n"
		if got.code != 2 || got.stdout != "" || got.stderr != want {
			t.Fatalf("exit = %d, stdout = %q, stderr = %q; want 2, empty, %q", got.code, got.stdout, got.stderr, want)
		}
	case <-time.After(5 * time.Second):
		// Release the blocked open so the goroutine can end.
		if w, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil {
			_ = w.Close()
		}
		<-done
		t.Fatal("tracequery blocked opening a FIFO")
	}
}
