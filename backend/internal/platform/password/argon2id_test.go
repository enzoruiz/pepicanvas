package password_test

import (
	"strings"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/password"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

var _ access.PasswordHasher = password.Argon2id{}

func pw(s string) secret.Value { return secret.New(secret.KindPassword, s) }

func TestHashUsesArgon2idPHCFormatAndVerifies(t *testing.T) {
	const raw = "decoy-password-7f3a9c1e"
	h := password.Argon2id{}
	hash, err := h.Hash(pw(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("hash %q does not use the OWASP argon2id parameters", hash)
	}
	if strings.Contains(hash, raw) {
		t.Fatal("hash contains the password")
	}
	if ok, err := h.Verify(hash, pw(raw)); err != nil || !ok {
		t.Fatalf("Verify(correct) = %v, %v", ok, err)
	}
	if ok, err := h.Verify(hash, pw(raw+"x")); err != nil || ok {
		t.Fatalf("Verify(wrong) = %v, %v", ok, err)
	}
	again, _ := h.Hash(pw(raw))
	if again == hash {
		t.Fatal("two hashes of one password must use different salts")
	}
}

func TestVerifyHonorsTheParametersStoredInTheHash(t *testing.T) {
	// Generated with m=8192, t=1, p=1 so future parameter upgrades keep old hashes valid.
	weak := password.Argon2id{Memory: 8192, Iterations: 1, Parallelism: 1}
	hash, err := weak.Hash(pw("legacy password!"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Fatalf("hash %q", hash)
	}
	if ok, err := (password.Argon2id{}).Verify(hash, pw("legacy password!")); err != nil || !ok {
		t.Fatalf("Verify with default parameters = %v, %v", ok, err)
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	for _, hash := range []string{
		"",
		"plain$secret",
		"$argon2i$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaA",
		"$argon2id$v=18$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaA",
		"$argon2id$v=19$m=0,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaA",
		"$argon2id$v=19$m=4194304,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaA",
		"$argon2id$v=19$m=19456,t=2,p=1$!!!$aGFzaA",
		"$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$",
	} {
		if ok, err := (password.Argon2id{}).Verify(hash, pw("whatever password")); err == nil || ok {
			t.Fatalf("Verify(%q) = %v, %v; want an error", hash, ok, err)
		}
	}
}
