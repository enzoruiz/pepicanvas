package canvas_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
)

func foldBase() canvas.Canvas {
	return canvas.Canvas{
		Key:        key,
		Dimensions: canvas.DefaultDimensions,
		Revision:   3,
		Objects: []canvas.Object{
			{ID: "obj-1", Kind: canvas.KindText, Content: "one", Transform: transform, Z: 1},
			{ID: "obj-2", Kind: canvas.KindVideo, Content: "asset-2", Transform: transform, Z: 2},
		},
	}
}

func TestFoldAppliesEachMutationKind(t *testing.T) {
	moved := canvas.Transform{X: -40, Y: 70, Width: 64, Height: 48, Rotation: 12.5}
	cases := map[string]struct {
		mutation canvas.Mutation
		result   canvas.Result
		want     func(canvas.Canvas) canvas.Canvas
	}{
		"dimensions keep objects": {
			canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 320, Height: 180}},
			canvas.Result{Revision: 4},
			func(c canvas.Canvas) canvas.Canvas {
				c.Dimensions = canvas.Dimensions{Width: 320, Height: 180}
				return c
			},
		},
		"create goes on top": {
			canvas.CreateObject{Kind: canvas.KindAudio, Content: "asset-3", Transform: moved},
			canvas.Result{Revision: 4, ObjectID: "obj-3", Z: 7},
			func(c canvas.Canvas) canvas.Canvas {
				c.Objects = append(c.Objects, canvas.Object{ID: "obj-3", Kind: canvas.KindAudio, Content: "asset-3", Transform: moved, Z: 7})
				return c
			},
		},
		"content keeps order": {
			canvas.SetContent{ObjectID: "obj-1", Content: "changed"},
			canvas.Result{Revision: 4, ObjectID: "obj-1"},
			func(c canvas.Canvas) canvas.Canvas {
				c.Objects[0].Content = "changed"
				return c
			},
		},
		"transform raises to the top": {
			canvas.SetTransform{ObjectID: "obj-1", Transform: moved},
			canvas.Result{Revision: 4, ObjectID: "obj-1", Z: 9},
			func(c canvas.Canvas) canvas.Canvas {
				raised := c.Objects[0]
				raised.Transform, raised.Z = moved, 9
				c.Objects = []canvas.Object{c.Objects[1], raised}
				return c
			},
		},
		"delete removes the object": {
			canvas.DeleteObject{ObjectID: "obj-2"},
			canvas.Result{Revision: 4, ObjectID: "obj-2"},
			func(c canvas.Canvas) canvas.Canvas {
				c.Objects = c.Objects[:1]
				return c
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			base := foldBase()
			got, err := canvas.Fold(base, tc.mutation, tc.result)
			if err != nil {
				t.Fatalf("Fold: %v", err)
			}
			want := tc.want(foldBase())
			want.Revision = 4
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Fold = %+v, want %+v", got, want)
			}
			if !reflect.DeepEqual(base, foldBase()) {
				t.Fatalf("Fold modified its input: %+v", base)
			}
		})
	}
}

func TestFoldRejectsInconsistentResults(t *testing.T) {
	create := canvas.CreateObject{Kind: canvas.KindText, Content: "x", Transform: transform}
	cases := map[string]struct {
		mutation canvas.Mutation
		result   canvas.Result
	}{
		"revision gap":             {canvas.SetContent{ObjectID: "obj-1", Content: "x"}, canvas.Result{Revision: 5, ObjectID: "obj-1"}},
		"revision repeated":        {canvas.SetContent{ObjectID: "obj-1", Content: "x"}, canvas.Result{Revision: 3, ObjectID: "obj-1"}},
		"missing mutation":         {nil, canvas.Result{Revision: 4}},
		"pointer mutation":         {&canvas.DeleteObject{ObjectID: "obj-1"}, canvas.Result{Revision: 4, ObjectID: "obj-1"}},
		"invalid mutation":         {canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 1, Height: 1}}, canvas.Result{Revision: 4}},
		"unknown object content":   {canvas.SetContent{ObjectID: "obj-9", Content: "x"}, canvas.Result{Revision: 4, ObjectID: "obj-9"}},
		"unknown object transform": {canvas.SetTransform{ObjectID: "obj-9", Transform: transform}, canvas.Result{Revision: 4, ObjectID: "obj-9", Z: 9}},
		"unknown object delete":    {canvas.DeleteObject{ObjectID: "obj-9"}, canvas.Result{Revision: 4, ObjectID: "obj-9"}},
		"create without object ID": {create, canvas.Result{Revision: 4, Z: 9}},
		"create existing object":   {create, canvas.Result{Revision: 4, ObjectID: "obj-1", Z: 9}},
		"create reuses z":          {create, canvas.Result{Revision: 4, ObjectID: "obj-3", Z: 2}},
		"create without z":         {create, canvas.Result{Revision: 4, ObjectID: "obj-3"}},
		"transform reuses z":       {canvas.SetTransform{ObjectID: "obj-1", Transform: transform}, canvas.Result{Revision: 4, ObjectID: "obj-1", Z: 2}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			base := foldBase()
			got, err := canvas.Fold(base, tc.mutation, tc.result)
			if !errors.Is(err, canvas.ErrInvalid) {
				t.Fatalf("Fold error = %v, want ErrInvalid", err)
			}
			if !reflect.DeepEqual(got, canvas.Canvas{}) {
				t.Fatalf("Fold returned state %+v on error", got)
			}
			if !reflect.DeepEqual(base, foldBase()) {
				t.Fatalf("Fold modified its input: %+v", base)
			}
		})
	}
}
