package secret_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

const decoy = "decoy-7f3a9c1e-should-never-appear"

func kinds() []secret.Kind {
	return []secret.Kind{
		secret.KindInvitationToken,
		secret.KindSessionToken,
		secret.KindMediaURL,
		secret.KindOBSCredential,
		secret.KindPassword,
		secret.KindCSRFToken,
	}
}

type holder struct {
	token  secret.Value
	Public secret.Value
}

func TestValueRevealReturnsRawAndKind(t *testing.T) {
	for _, kind := range kinds() {
		v := secret.New(kind, decoy)
		if got := v.Reveal(); got != decoy {
			t.Fatalf("Reveal() = %q, want %q", got, decoy)
		}
		if got := v.Kind(); got != kind {
			t.Fatalf("Kind() = %q, want %q", got, kind)
		}
		if v.IsZero() {
			t.Fatalf("IsZero() = true for a populated %s", kind)
		}
	}
	if !(secret.Value{}).IsZero() {
		t.Fatal("zero Value must report IsZero")
	}
	if got := (secret.Value{}).Reveal(); got != "" {
		t.Fatalf("zero Value Reveal() = %q, want empty", got)
	}
}

func TestValueRedactsEveryFormattingPath(t *testing.T) {
	for _, kind := range kinds() {
		v := secret.New(kind, decoy)
		want := "[REDACTED:" + string(kind) + "]"
		h := holder{token: v, Public: v}

		jsonBytes, err := json.Marshal(h)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		text, err := v.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText: %v", err)
		}
		var logs bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&logs, nil))
		logger.Info("issued", "token", v, "holder", h)
		var textLogs bytes.Buffer
		slog.New(slog.NewTextHandler(&textLogs, nil)).Info("issued", "token", v)

		outputs := map[string]string{
			"String":    v.String(),
			"GoString":  v.GoString(),
			"%v":        fmt.Sprintf("%v", v),
			"%+v":       fmt.Sprintf("%+v", v),
			"%#v":       fmt.Sprintf("%#v", v),
			"%s":        fmt.Sprintf("%s", v),
			"%q":        fmt.Sprintf("%q", v),
			"%x":        fmt.Sprintf("%x", v),
			"%X":        fmt.Sprintf("%X", v),
			"%d":        fmt.Sprintf("%d", v),
			"pointer":   fmt.Sprintf("%v", &v),
			"struct %v": fmt.Sprintf("%v", h),
			"struct +v": fmt.Sprintf("%+v", h),
			"struct #v": fmt.Sprintf("%#v", h),
			"slice":     fmt.Sprintf("%v", []secret.Value{v}),
			"map":       fmt.Sprintf("%v", map[string]secret.Value{"k": v}),
			"error":     fmt.Errorf("lookup %v failed", v).Error(),
			"wrapped":   fmt.Errorf("outer: %w", errors.New(v.String())).Error(),
			"json":      string(jsonBytes),
			"text":      string(text),
			"slog json": logs.String(),
			"slog text": textLogs.String(),
		}
		for name, out := range outputs {
			if strings.Contains(out, decoy) {
				t.Errorf("%s/%s leaked the raw secret: %s", kind, name, out)
			}
		}
		for _, name := range []string{"String", "%v", "%s", "json", "text", "slog json", "slog text"} {
			if !strings.Contains(outputs[name], want) {
				t.Errorf("%s/%s = %q, want marker %q", kind, name, outputs[name], want)
			}
		}
	}
}

func TestValueRejectsUnmarshalSoSecretsNeverEnterFromSerializedForm(t *testing.T) {
	var v secret.Value
	if err := json.Unmarshal([]byte(`"`+decoy+`"`), &v); err == nil {
		t.Fatal("json.Unmarshal into secret.Value must fail")
	}
	if !v.IsZero() {
		t.Fatal("failed unmarshal must leave the Value zero")
	}
}

func TestNewPanicsOnUnknownKind(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New with an unknown kind must panic")
		}
	}()
	secret.New(secret.Kind("not_a_kind"), decoy)
}
