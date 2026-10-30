package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
)

// ReadyReference resolves a reference of one canvas to its ready asset, and
// answers ErrNotFound for a missing reference, a reference of another canvas
// or tenant, and a released asset. A replaced reference resolves to the
// replacement.
func TestMediaReadyReferenceResolvesOnlyReadyAssetsOfItsCanvas(t *testing.T) {
	mediaA2 := media.CanvasKey{TenantID: mediaA.TenantID, CanvasID: "canvas-2"}
	store, _ := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA, mediaA2, mediaB})
	ctx := context.Background()
	wantNotFound := func(t *testing.T, key media.CanvasKey, ref string) {
		t.Helper()
		if asset, err := store.ReadyReference(ctx, key, ref); !errors.Is(err, media.ErrNotFound) {
			t.Fatalf("ReadyReference(%+v, %s) = %+v, %v; want ErrNotFound", key, ref, asset, err)
		}
	}
	wantAsset := func(t *testing.T, key media.CanvasKey, ref, assetID string, bytes int64, refs int) {
		t.Helper()
		asset, err := store.ReadyReference(ctx, key, ref)
		if err != nil || asset.ID != assetID || asset.State != media.StateReady || asset.Kind != media.KindImage ||
			asset.DeclaredBytes != bytes || asset.StorageKey == "" || asset.References != refs {
			t.Fatalf("ReadyReference(%+v, %s) = %+v, %v; want ready asset %s of %d bytes with %d references", key, ref, asset, err, assetID, bytes, refs)
		}
		ready, err := store.Ready(ctx, key, assetID)
		if err != nil || ready != asset {
			t.Fatalf("Ready(%s) = %+v, %v; want %+v", assetID, ready, err, asset)
		}
	}

	hero := upload(t, store, mediaA, 100, "hero")
	wantAsset(t, mediaA, "hero", hero, 100, 1)
	run(t, store, mediaA, media.Attach{AssetID: hero, ReferenceID: "copy"})
	wantAsset(t, mediaA, "copy", hero, 100, 2)

	wantNotFound(t, mediaA, "missing")
	wantNotFound(t, mediaA2, "hero")
	wantNotFound(t, mediaB, "hero")
	other := upload(t, store, mediaB, 50, "hero")
	wantAsset(t, mediaB, "hero", other, 50, 1)
	wantAsset(t, mediaA, "hero", hero, 100, 2)

	// A pending asset has no reference until it completes.
	run(t, store, mediaA, media.Reserve{DeclaredBytes: 10})
	wantNotFound(t, mediaA, "pending")

	replacing := run(t, store, mediaA, media.Reserve{DeclaredBytes: 70, ReplacesReference: "copy"})
	run(t, store, mediaA, media.Complete{AssetID: replacing.AssetID, ReferenceID: "copy", Kind: media.KindImage})
	wantAsset(t, mediaA, "copy", replacing.AssetID, 70, 1)

	run(t, store, mediaA, media.Detach{ReferenceID: "hero"})
	wantNotFound(t, mediaA, "hero")
	wantNotReady(t, store, mediaA, hero)

	for _, bad := range []string{"", strings.Repeat("r", media.MaxIdentifierBytes+1)} {
		if _, err := store.ReadyReference(ctx, mediaA, bad); !errors.Is(err, media.ErrInvalid) {
			t.Fatalf("ReadyReference(%q) = %v, want ErrInvalid", bad, err)
		}
	}
	if _, err := store.ReadyReference(ctx, media.CanvasKey{TenantID: "bad tenant", CanvasID: "canvas-1"}, "hero"); !errors.Is(err, media.ErrInvalid) {
		t.Fatalf("ReadyReference with a malformed key = %v, want ErrInvalid", err)
	}
}
