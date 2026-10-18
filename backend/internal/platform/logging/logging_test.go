package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/logging"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

type decoys struct {
	invitation, session, obs secret.Value
	mediaURL                 string
	plain                    string
}

func newDecoys(t *testing.T) decoys {
	t.Helper()
	gen := func(k secret.Kind) secret.Value {
		v, err := secret.Generate(k)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	return decoys{
		invitation: gen(secret.KindInvitationToken),
		session:    gen(secret.KindSessionToken),
		obs:        gen(secret.KindOBSCredential),
		mediaURL:   "https://bucket.s3.example.com/m/1.png?X-Amz-Signature=decoymediasig",
		plain:      "decoyplainpassword",
	}
}

func (d decoys) raws() []string {
	return []string{d.invitation.Reveal(), d.session.Reveal(), d.obs.Reveal(), "decoymediasig", d.plain}
}

type leakyStruct struct {
	Name  string
	Token string
}

func emitEverything(ctx context.Context, logger *slog.Logger, d decoys) {
	logger.InfoContext(ctx, "accept invitation "+d.invitation.Reveal())
	logger.InfoContext(ctx, "session", "token", d.session, "raw", d.session.Reveal())
	logger.WarnContext(ctx, "fetch failed", "url", d.mediaURL, "err", fmt.Errorf("get %s: %w", d.mediaURL, errors.New("timeout")))
	logger.ErrorContext(ctx, "obs", slog.Group("obs", "credential", d.obs.Reveal(), "password", d.plain))
	logger.InfoContext(ctx, "headers", "Authorization", "Bearer "+d.plain, "Cookie", "sid="+d.plain, "Set-Cookie", "sid="+d.plain)
	logger.InfoContext(ctx, "struct", "value", leakyStruct{Name: "x", Token: d.obs.Reveal()})
	logger.With("invite_token", d.plain).WithGroup("g").InfoContext(ctx, "with attrs", "secret", d.plain)
}

func TestHandlerRedactsDecoysInJSONAndText(t *testing.T) {
	d := newDecoys(t)
	id, err := correlation.New()
	if err != nil {
		t.Fatal(err)
	}
	ctx := correlation.WithID(context.Background(), id)

	for name, mk := range map[string]func(*bytes.Buffer) slog.Handler{
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
	} {
		var out bytes.Buffer
		emitEverything(ctx, slog.New(logging.NewHandler(mk(&out))), d)
		logs := out.String()
		for _, raw := range d.raws() {
			if strings.Contains(logs, raw) {
				t.Errorf("%s handler leaked %q in:\n%s", name, raw, logs)
			}
		}
		if got := strings.Count(logs, id.String()); got != 7 {
			t.Errorf("%s handler: correlation ID appears %d times, want 7 (one per record)", name, got)
		}
	}
}

func TestHandlerKeepsOrdinaryAttributesAndAddsCorrelation(t *testing.T) {
	var out bytes.Buffer
	id, _ := correlation.New()
	ctx := correlation.WithID(context.Background(), id)
	slog.New(logging.NewHandler(slog.NewJSONHandler(&out, nil))).InfoContext(ctx, "moved", "canvas", 42, "layer", "front")

	var rec map[string]any
	if err := json.Unmarshal(out.Bytes(), &rec); err != nil {
		t.Fatalf("invalid JSON %q: %v", out.String(), err)
	}
	if rec["msg"] != "moved" || rec["canvas"] != float64(42) || rec["layer"] != "front" {
		t.Fatalf("ordinary attributes changed: %v", rec)
	}
	if rec["correlation_id"] != id.String() {
		t.Fatalf("correlation_id = %v, want %s", rec["correlation_id"], id)
	}
}

func TestHandlerOmitsCorrelationWithoutContextID(t *testing.T) {
	var out bytes.Buffer
	slog.New(logging.NewHandler(slog.NewJSONHandler(&out, nil))).Info("boot")
	if strings.Contains(out.String(), "correlation_id") {
		t.Fatalf("unexpected correlation_id: %s", out.String())
	}
}

func TestHandlerRespectsInnerLevel(t *testing.T) {
	var out bytes.Buffer
	inner := slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelWarn})
	logger := slog.New(logging.NewHandler(inner))
	logger.Info("hidden")
	if out.Len() != 0 {
		t.Fatalf("info record emitted below inner level: %s", out.String())
	}
}

func TestHandlerRedactsSensitiveGroupsAndEncodedURLs(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(logging.NewHandler(slog.NewJSONHandler(&out, nil)))
	logger.Info("m",
		slog.Group("credentials", slog.String("value", "decoygroupvalue")),
		"next", url.QueryEscape("https://b.invalid/a.png?X-Amz-Signature=decoyencodedsig"),
	)
	for _, leak := range []string{"decoygroupvalue", "decoyencodedsig"} {
		if strings.Contains(out.String(), leak) {
			t.Errorf("leaked %q: %s", leak, out.String())
		}
	}
	if !strings.Contains(out.String(), `"credentials":"[REDACTED]"`) {
		t.Errorf("sensitive group not redacted as a whole: %s", out.String())
	}
}
