package mediaexec

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestCaptureSharesOneBudgetAcrossStdoutAndStderrAndKeepsDraining(t *testing.T) {
	t.Parallel()

	budget := newCaptureBudget(5)
	first := budget.command()
	if n, err := first.stdout().Write([]byte("abc")); n != 3 || err != nil {
		t.Fatalf("stdout Write() = %d, %v, want 3, nil", n, err)
	}
	if n, err := first.stderr().Write([]byte("defg")); n != 4 || err != nil {
		t.Fatalf("stderr Write() = %d, %v, want 4, nil", n, err)
	}
	second := budget.command()
	if n, err := second.stdout().Write(bytes.Repeat([]byte("x"), 32)); n != 32 || err != nil {
		t.Fatalf("overflow Write() = %d, %v, want 32, nil", n, err)
	}
	if !budget.overflowed() {
		t.Fatal("overflowed() = false, want true")
	}
	if got := string(first.output()); got != "abc" {
		t.Fatalf("first stdout = %q, want %q", got, "abc")
	}
}

func TestCaptureConcurrentStdoutAndStderrShareOneBudget(t *testing.T) {
	t.Parallel()

	budget := newCaptureBudget(64)
	capture := budget.command()
	start := make(chan struct{})
	overflowStart := make(chan struct{})
	var initial sync.WaitGroup
	var writers sync.WaitGroup
	initial.Add(2)
	writers.Add(2)
	write := func(writer interface{ Write([]byte) (int, error) }, value byte) {
		defer writers.Done()
		<-start
		if n, err := writer.Write(bytes.Repeat([]byte{value}, 32)); n != 32 || err != nil {
			t.Errorf("initial concurrent Write() = %d, %v, want 32, nil", n, err)
		}
		initial.Done()
		<-overflowStart
		if n, err := writer.Write([]byte{value}); n != 1 || err != nil {
			t.Errorf("overflow concurrent Write() = %d, %v, want 1, nil", n, err)
		}
	}
	go write(capture.stdout(), 'o')
	go write(capture.stderr(), 'e')
	close(start)
	initial.Wait()
	close(overflowStart)
	writers.Wait()

	if !budget.overflowed() {
		t.Fatal("overflowed() = false, want true after concurrent writes")
	}
	if got := string(capture.output()); got != strings.Repeat("o", 32) {
		t.Fatalf("captured stdout = %q, want 32 stdout bytes", got)
	}
}
