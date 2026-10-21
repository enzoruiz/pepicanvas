package media_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
)

func TestQuotaIsOneBillionDecimalBytes(t *testing.T) {
	if media.QuotaBytes != 1_000_000_000 {
		t.Fatalf("QuotaBytes = %d, want 1000000000", media.QuotaBytes)
	}
}

func TestUsageAdmitsExactlyTheQuotaAndRejectsOneByteMore(t *testing.T) {
	cases := []struct {
		name  string
		usage media.Usage
		bytes int64
		want  bool
	}{
		{"empty canvas takes the whole quota", media.Usage{QuotaBytes: media.QuotaBytes}, media.QuotaBytes, true},
		{"empty canvas rejects one byte over", media.Usage{QuotaBytes: media.QuotaBytes}, media.QuotaBytes + 1, false},
		{"used plus reserved fills it exactly", media.Usage{QuotaBytes: media.QuotaBytes, UsedBytes: 600_000_000, ReservedBytes: 399_999_999}, 1, true},
		{"used plus reserved leaves no byte", media.Usage{QuotaBytes: media.QuotaBytes, UsedBytes: 600_000_000, ReservedBytes: 400_000_000}, 1, false},
		{"reservations count against the quota", media.Usage{QuotaBytes: media.QuotaBytes, ReservedBytes: 999_999_999}, 2, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.usage.Admits(tc.bytes); got != tc.want {
				t.Fatalf("Admits(%d) = %v, want %v", tc.bytes, got, tc.want)
			}
		})
	}
}

func TestUsageAvailableNeverGoesNegative(t *testing.T) {
	usage := media.Usage{QuotaBytes: media.QuotaBytes, UsedBytes: 700_000_000, ReservedBytes: 300_000_000}
	if got := usage.Available(); got != 0 {
		t.Fatalf("Available() = %d, want 0", got)
	}
	usage.ReservedBytes = 100_000_000
	if got := usage.Available(); got != 200_000_000 {
		t.Fatalf("Available() = %d, want 200000000", got)
	}
}

func TestOnlyReadyAssetsAreUsable(t *testing.T) {
	for _, state := range []media.State{media.StatePending, media.StateReady, media.StateQuarantined, media.StateDeleting} {
		if got, want := state.Usable(), state == media.StateReady; got != want {
			t.Fatalf("%s.Usable() = %v, want %v", state, got, want)
		}
		if !state.Valid() {
			t.Fatalf("%s.Valid() = false", state)
		}
	}
	if media.State("gone").Valid() {
		t.Fatal(`State("gone").Valid() = true`)
	}
}

func TestValidateCommandAcceptsWellFormedCommands(t *testing.T) {
	key := media.CanvasKey{TenantID: "tenant-a", CanvasID: "canvas-1"}
	operation := media.Operation{ID: "op-1", CorrelationID: "0123456789abcdef0123456789abcdef"}
	commands := []media.Command{
		media.Reserve{DeclaredBytes: 1},
		media.Reserve{DeclaredBytes: media.QuotaBytes},
		media.Reserve{DeclaredBytes: 10, ReplacesReference: "object-1"},
		media.Complete{AssetID: "asset-1", ReferenceID: "object-1", Kind: media.KindVideo},
		media.Abandon{AssetID: "asset-1", Reason: media.AbandonCancelled},
		media.Abandon{AssetID: "asset-1", Reason: media.AbandonFailed},
		media.Abandon{AssetID: "asset-1", Reason: media.AbandonRejected, Rejection: media.RejectionDimensionsExceeded},
		media.Attach{AssetID: "asset-1", ReferenceID: "object-2"},
		media.Detach{ReferenceID: "object-2"},
	}
	for _, command := range commands {
		if err := media.ValidateCommand(key, operation, command); err != nil {
			t.Fatalf("ValidateCommand(%#v) = %v, want nil", command, err)
		}
	}
}

func TestValidateCommandRejectsMalformedInput(t *testing.T) {
	key := media.CanvasKey{TenantID: "tenant-a", CanvasID: "canvas-1"}
	operation := media.Operation{ID: "op-1"}
	long := strings.Repeat("x", media.MaxIdentifierBytes+1)
	cases := []struct {
		name      string
		key       media.CanvasKey
		operation media.Operation
		command   media.Command
	}{
		{"missing tenant", media.CanvasKey{CanvasID: "canvas-1"}, operation, media.Detach{ReferenceID: "o"}},
		{"oversized canvas", media.CanvasKey{TenantID: "t", CanvasID: long}, operation, media.Detach{ReferenceID: "o"}},
		{"missing operation", key, media.Operation{}, media.Detach{ReferenceID: "o"}},
		{"space in operation", key, media.Operation{ID: "op 1"}, media.Detach{ReferenceID: "o"}},
		{"malformed correlation", key, media.Operation{ID: "op", CorrelationID: "a b"}, media.Detach{ReferenceID: "o"}},
		{"nil command", key, operation, nil},
		{"typed nil command", key, operation, (*media.Reserve)(nil)},
		{"pointer command", key, operation, &media.Reserve{DeclaredBytes: 1}},
		{"zero declared bytes", key, operation, media.Reserve{}},
		{"negative declared bytes", key, operation, media.Reserve{DeclaredBytes: -1}},
		{"declared bytes over the quota", key, operation, media.Reserve{DeclaredBytes: media.QuotaBytes + 1}},
		{"malformed replaced reference", key, operation, media.Reserve{DeclaredBytes: 1, ReplacesReference: "a\x00"}},
		{"complete without asset", key, operation, media.Complete{ReferenceID: "o", Kind: media.KindImage}},
		{"complete without reference", key, operation, media.Complete{AssetID: "a", Kind: media.KindImage}},
		{"complete with unknown kind", key, operation, media.Complete{AssetID: "a", ReferenceID: "o", Kind: "svg"}},
		{"abandon without asset", key, operation, media.Abandon{Reason: media.AbandonFailed}},
		{"abandon with unknown reason", key, operation, media.Abandon{AssetID: "a", Reason: "lost"}},
		{"rejected abandon without code", key, operation, media.Abandon{AssetID: "a", Reason: media.AbandonRejected}},
		{"rejected abandon with unknown code", key, operation, media.Abandon{AssetID: "a", Reason: media.AbandonRejected, Rejection: "lost"}},
		{"failed abandon with code", key, operation, media.Abandon{AssetID: "a", Reason: media.AbandonFailed, Rejection: media.RejectionInvalidMetadata}},
		{"cancelled abandon with code", key, operation, media.Abandon{AssetID: "a", Reason: media.AbandonCancelled, Rejection: media.RejectionInvalidMetadata}},
		{"attach without reference", key, operation, media.Attach{AssetID: "a"}},
		{"detach without reference", key, operation, media.Detach{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := media.ValidateCommand(tc.key, tc.operation, tc.command)
			if !errors.Is(err, media.ErrInvalid) {
				t.Fatalf("ValidateCommand() = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestFingerprintSeparatesIntentAndIgnoresCorrelation(t *testing.T) {
	same := []media.Command{
		media.Reserve{DeclaredBytes: 10},
		media.Reserve{DeclaredBytes: 10},
	}
	if media.Fingerprint(same[0]) != media.Fingerprint(same[1]) {
		t.Fatal("equal commands have different fingerprints")
	}
	distinct := []media.Command{
		media.Reserve{DeclaredBytes: 10},
		media.Reserve{DeclaredBytes: 11},
		media.Reserve{DeclaredBytes: 10, ReplacesReference: "o"},
		media.Complete{AssetID: "a", ReferenceID: "o", Kind: media.KindImage},
		media.Complete{AssetID: "a", ReferenceID: "o", Kind: media.KindGIF},
		media.Complete{AssetID: "ao", ReferenceID: "", Kind: media.KindImage},
		media.Abandon{AssetID: "a", Reason: media.AbandonFailed},
		media.Abandon{AssetID: "a", Reason: media.AbandonRejected, Rejection: media.RejectionDimensionsExceeded},
		media.Abandon{AssetID: "a", Reason: media.AbandonRejected, Rejection: media.RejectionInvalidMetadata},
		media.Attach{AssetID: "a", ReferenceID: "o"},
		media.Detach{ReferenceID: "o"},
	}
	seen := map[[32]byte]int{}
	for i, command := range distinct {
		sum := media.Fingerprint(command)
		if j, ok := seen[sum]; ok {
			t.Fatalf("commands %d and %d share a fingerprint", j, i)
		}
		seen[sum] = i
	}
}
