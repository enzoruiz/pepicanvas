package mediaexec

import (
	"bytes"
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
