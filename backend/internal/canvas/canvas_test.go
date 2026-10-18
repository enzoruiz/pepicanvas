package canvas_test

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
)

var (
	key       = canvas.Key{TenantID: "tenant-a", CanvasID: "canvas-1"}
	operation = canvas.Operation{ID: "op-1"}
	transform = canvas.Transform{X: 10, Y: 20, Width: 300, Height: 200, Rotation: 45.5}
)

func TestDefaultDimensions(t *testing.T) {
	if canvas.DefaultDimensions != (canvas.Dimensions{Width: 1920, Height: 1080}) {
		t.Fatalf("DefaultDimensions = %+v, want 1920x1080", canvas.DefaultDimensions)
	}
}

func TestValidateAccepts(t *testing.T) {
	for name, mutation := range map[string]canvas.Mutation{
		"minimum dimensions":       canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 320, Height: 180}},
		"maximum dimensions":       canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 3840, Height: 2160}},
		"default dimensions":       canvas.SetDimensions{Dimensions: canvas.DefaultDimensions},
		"text object":              canvas.CreateObject{Kind: canvas.KindText, Content: "line one\nline two", Transform: transform},
		"media object outside":     canvas.CreateObject{Kind: canvas.KindVideo, Content: "asset-9", Transform: canvas.Transform{X: -5000, Y: 9000, Width: 1, Height: 1}},
		"largest content":          canvas.SetContent{ObjectID: "obj-1", Content: strings.Repeat("é", canvas.MaxContentBytes/2)},
		"transform near full turn": canvas.SetTransform{ObjectID: "obj-1", Transform: canvas.Transform{Width: 1, Height: 1, Rotation: math.Nextafter(360, 0)}},
		"delete":                   canvas.DeleteObject{ObjectID: "obj-1"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := canvas.Validate(key, operation, mutation); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestValidateRejects(t *testing.T) {
	valid := canvas.SetDimensions{Dimensions: canvas.DefaultDimensions}
	cases := map[string]struct {
		key       canvas.Key
		operation canvas.Operation
		mutation  canvas.Mutation
	}{
		"missing tenant":        {canvas.Key{CanvasID: "c"}, operation, valid},
		"missing canvas":        {canvas.Key{TenantID: "t"}, operation, valid},
		"spaced canvas id":      {canvas.Key{TenantID: "t", CanvasID: "a b"}, operation, valid},
		"oversized tenant id":   {canvas.Key{TenantID: strings.Repeat("t", 129), CanvasID: "c"}, operation, valid},
		"missing operation":     {key, canvas.Operation{}, valid},
		"malformed correlation": {key, canvas.Operation{ID: "op", CorrelationID: "bad id"}, valid},
		"nil mutation":          {key, operation, nil},
		"width below minimum":   {key, operation, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 319, Height: 180}}},
		"height below minimum":  {key, operation, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 320, Height: 179}}},
		"width above maximum":   {key, operation, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 3841, Height: 2160}}},
		"height above maximum":  {key, operation, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 3840, Height: 2161}}},
		"unknown kind":          {key, operation, canvas.CreateObject{Kind: "shape", Content: "x", Transform: transform}},
		"empty content":         {key, operation, canvas.CreateObject{Kind: canvas.KindText, Transform: transform}},
		"content with NUL":      {key, operation, canvas.SetContent{ObjectID: "obj-1", Content: "a\x00b"}},
		"content not UTF-8":     {key, operation, canvas.SetContent{ObjectID: "obj-1", Content: "\xff"}},
		"content too long":      {key, operation, canvas.SetContent{ObjectID: "obj-1", Content: strings.Repeat("a", canvas.MaxContentBytes+1)}},
		"content missing id":    {key, operation, canvas.SetContent{Content: "x"}},
		"zero width":            {key, operation, canvas.SetTransform{ObjectID: "obj-1", Transform: canvas.Transform{Height: 1}}},
		"zero height":           {key, operation, canvas.SetTransform{ObjectID: "obj-1", Transform: canvas.Transform{Width: 1}}},
		"oversized object":      {key, operation, canvas.SetTransform{ObjectID: "obj-1", Transform: canvas.Transform{Width: canvas.MaxObjectSize + 1, Height: 1}}},
		"far coordinate":        {key, operation, canvas.SetTransform{ObjectID: "obj-1", Transform: canvas.Transform{X: -canvas.MaxCoordinate - 1, Width: 1, Height: 1}}},
		"full turn rotation":    {key, operation, canvas.SetTransform{ObjectID: "obj-1", Transform: canvas.Transform{Width: 1, Height: 1, Rotation: 360}}},
		"negative rotation":     {key, operation, canvas.SetTransform{ObjectID: "obj-1", Transform: canvas.Transform{Width: 1, Height: 1, Rotation: -0.5}}},
		"NaN rotation":          {key, operation, canvas.SetTransform{ObjectID: "obj-1", Transform: canvas.Transform{Width: 1, Height: 1, Rotation: math.NaN()}}},
		"transform missing id":  {key, operation, canvas.SetTransform{Transform: transform}},
		"delete missing id":     {key, operation, canvas.DeleteObject{}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := canvas.Validate(tc.key, tc.operation, tc.mutation); !errors.Is(err, canvas.ErrInvalid) {
				t.Fatalf("Validate error = %v, want ErrInvalid", err)
			}
		})
	}
}
