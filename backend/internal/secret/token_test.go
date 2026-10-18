package secret_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

func TestGenerateProducesPrefixedUniqueTokens(t *testing.T) {
	prefixes := map[secret.Kind]string{
		secret.KindInvitationToken: "pci_",
		secret.KindSessionToken:    "pcs_",
		secret.KindOBSCredential:   "pco_",
	}
	shape := regexp.MustCompile(`^pc[iso]_[A-Za-z0-9_-]{43}$`)
	for kind, prefix := range prefixes {
		seen := map[string]bool{}
		for range 64 {
			v, err := secret.Generate(kind)
			if err != nil {
				t.Fatalf("Generate(%s): %v", kind, err)
			}
			raw := v.Reveal()
			if !strings.HasPrefix(raw, prefix) || !shape.MatchString(raw) {
				t.Fatalf("Generate(%s) = %q, want %s + 43 base64url chars", kind, raw, prefix)
			}
			if v.Kind() != kind {
				t.Fatalf("Generate(%s).Kind() = %s", kind, v.Kind())
			}
			if seen[raw] {
				t.Fatalf("Generate(%s) repeated a token", kind)
			}
			seen[raw] = true
		}
	}
}

func TestGenerateRejectsMediaURLKind(t *testing.T) {
	if _, err := secret.Generate(secret.KindMediaURL); err == nil {
		t.Fatal("media URLs are not generated tokens; Generate must fail")
	}
}

func TestScrubRedactsGeneratedTokensInFreeText(t *testing.T) {
	for _, kind := range []secret.Kind{secret.KindInvitationToken, secret.KindSessionToken, secret.KindOBSCredential} {
		v, err := secret.Generate(kind)
		if err != nil {
			t.Fatal(err)
		}
		raw := v.Reveal()
		in := "lookup failed for " + raw + ": not found (" + raw + ")"
		got := secret.Scrub(in)
		if strings.Contains(got, raw) || strings.Contains(got, raw[4:]) {
			t.Fatalf("Scrub leaked %s: %q", kind, got)
		}
		want := "lookup failed for [REDACTED:" + string(kind) + "]: not found ([REDACTED:" + string(kind) + "])"
		if got != want {
			t.Fatalf("Scrub() = %q, want %q", got, want)
		}
	}
}

func TestScrubStripsURLCredentialsQueryAndFragment(t *testing.T) {
	cases := map[string]string{
		"GET https://bucket.s3.example.com/a/b.png?X-Amz-Signature=decoysig&X-Amz-Credential=decoycred failed": "GET https://bucket.s3.example.com/a/b.png?[REDACTED] failed",
		"dial https://user:decoypass@minio.local:9000/obj":                                                     "dial https://[REDACTED]@minio.local:9000/obj",
		"open wss://app.example/ws#decoyfrag now":                                                              "open wss://app.example/ws#[REDACTED] now",
		"plain https://app.example/editor stays":                                                               "plain https://app.example/editor stays",
		"relative /accept?code=decoyrel is cut":                                                                "relative /accept?[REDACTED] is cut",
	}
	for in, want := range cases {
		got := secret.Scrub(in)
		if got != want {
			t.Errorf("Scrub(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(got, "decoy") {
			t.Errorf("Scrub(%q) leaked a decoy: %q", in, got)
		}
	}
}

func TestScrubLeavesOrdinaryTextUnchanged(t *testing.T) {
	in := "canvas 42 moved object pcx_short to layer 3 at 10:00"
	if got := secret.Scrub(in); got != in {
		t.Fatalf("Scrub changed ordinary text: %q", got)
	}
}
