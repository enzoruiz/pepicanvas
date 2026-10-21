package outbox_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/outbox"
)

func TestMutationCodecRoundTripsEveryKind(t *testing.T) {
	transform := canvas.Transform{X: -3, Y: 4, Width: 50, Height: 20, Rotation: 12.345678901234567}
	mutations := []canvas.Mutation{
		canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 3840, Height: 2160}},
		canvas.CreateObject{Kind: canvas.KindVideo, Content: "clip ✓", Transform: transform},
		canvas.SetContent{ObjectID: "obj-1", Content: "new text"},
		canvas.SetTransform{ObjectID: "obj-1", Transform: transform},
		canvas.DeleteObject{ObjectID: "obj-1"},
	}
	for _, mutation := range mutations {
		kind, payload, err := outbox.EncodeMutation(mutation)
		if err != nil {
			t.Fatalf("EncodeMutation(%T): %v", mutation, err)
		}
		decoded, err := outbox.DecodeMutation(kind, payload)
		if err != nil {
			t.Fatalf("DecodeMutation(%s, %s): %v", kind, payload, err)
		}
		if !reflect.DeepEqual(decoded, mutation) {
			t.Fatalf("round trip of %T = %#v, want %#v", mutation, decoded, mutation)
		}
	}
}

func TestMutationCodecNamesEachKind(t *testing.T) {
	cases := map[string]canvas.Mutation{
		"set_dimensions": canvas.SetDimensions{Dimensions: canvas.DefaultDimensions},
		"create_object":  canvas.CreateObject{Kind: canvas.KindText, Content: "a", Transform: canvas.Transform{Width: 1, Height: 1}},
		"set_content":    canvas.SetContent{ObjectID: "o", Content: "a"},
		"set_transform":  canvas.SetTransform{ObjectID: "o", Transform: canvas.Transform{Width: 1, Height: 1}},
		"delete_object":  canvas.DeleteObject{ObjectID: "o"},
	}
	for want, mutation := range cases {
		kind, _, err := outbox.EncodeMutation(mutation)
		if err != nil || kind != want {
			t.Fatalf("EncodeMutation(%T) kind = %q, %v; want %q", mutation, kind, err, want)
		}
	}
}

func TestMutationCodecRejectsMalformedInput(t *testing.T) {
	if _, _, err := outbox.EncodeMutation(nil); !errors.Is(err, outbox.ErrMalformed) {
		t.Fatalf("EncodeMutation(nil) error = %v, want ErrMalformed", err)
	}
	if _, _, err := outbox.EncodeMutation(&canvas.DeleteObject{ObjectID: "o"}); !errors.Is(err, outbox.ErrMalformed) {
		t.Fatalf("EncodeMutation(pointer) error = %v, want ErrMalformed", err)
	}
	cases := map[string]struct {
		kind    string
		payload string
	}{
		"unknown kind":     {"rename_object", `{"object_id":"o"}`},
		"not JSON":         {"delete_object", `{`},
		"unknown field":    {"delete_object", `{"object_id":"o","extra":1}`},
		"invalid mutation": {"set_dimensions", `{"width":1,"height":1}`},
		"trailing data":    {"delete_object", `{"object_id":"o"} {}`},
	}
	for name, c := range cases {
		if _, err := outbox.DecodeMutation(c.kind, []byte(c.payload)); !errors.Is(err, outbox.ErrMalformed) {
			t.Fatalf("%s: DecodeMutation error = %v, want ErrMalformed", name, err)
		}
	}
}
