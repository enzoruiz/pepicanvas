package correlation_test

import (
	"context"
	"regexp"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
)

func TestNewIDsAreUniqueLowercaseHex(t *testing.T) {
	shape := regexp.MustCompile(`^[0-9a-f]{32}$`)
	seen := map[string]bool{}
	for range 128 {
		id, err := correlation.New()
		if err != nil {
			t.Fatal(err)
		}
		if !shape.MatchString(id.String()) {
			t.Fatalf("New() = %q, want 32 lowercase hex chars", id)
		}
		if seen[id.String()] {
			t.Fatal("New() repeated an ID")
		}
		seen[id.String()] = true
	}
}

func TestParseAcceptsOnlyCanonicalIDs(t *testing.T) {
	valid := "0123456789abcdef0123456789abcdef"
	id, err := correlation.Parse(valid)
	if err != nil || id.String() != valid {
		t.Fatalf("Parse(%q) = %q, %v", valid, id, err)
	}
	for _, in := range []string{
		"",
		"0123456789ABCDEF0123456789ABCDEF",
		"0123456789abcdef0123456789abcde",
		"0123456789abcdef0123456789abcdef0",
		"0123456789abcdef\n123456789abcdef",
		"pcs_0123456789abcdef0123456789ab",
	} {
		if _, err := correlation.Parse(in); err == nil {
			t.Errorf("Parse(%q) must fail", in)
		}
	}
}

func TestContextRoundTrip(t *testing.T) {
	if _, ok := correlation.FromContext(context.Background()); ok {
		t.Fatal("empty context must carry no ID")
	}
	id, err := correlation.New()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := correlation.FromContext(correlation.WithID(context.Background(), id))
	if !ok || got != id {
		t.Fatalf("FromContext() = %q, %v; want %q", got, ok, id)
	}
}
