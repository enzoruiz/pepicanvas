package canvas_test

import (
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
)

func TestFitKeepsNaturalSizeOrScalesDownCentered(t *testing.T) {
	hd := canvas.Dimensions{Width: 1920, Height: 1080}
	cases := []struct {
		name          string
		area          canvas.Dimensions
		width, height int
		want          canvas.Transform
	}{
		{"small image keeps its size", hd, 16, 16, canvas.Transform{X: 952, Y: 532, Width: 16, Height: 16}},
		{"exact area keeps its size", hd, 1920, 1080, canvas.Transform{Width: 1920, Height: 1080}},
		{"largest image scales to the height", hd, 4096, 4096, canvas.Transform{X: 420, Width: 1080, Height: 1080}},
		{"wide image scales to the width", hd, 4096, 1024, canvas.Transform{Y: 300, Width: 1920, Height: 480}},
		{"one dimension over scales both", hd, 2000, 100, canvas.Transform{Y: 492, Width: 1920, Height: 96}},
		{"vertical sliver keeps one pixel", hd, 1, 4096, canvas.Transform{X: 959, Width: 1, Height: 1080}},
		{"horizontal sliver keeps one pixel", hd, 4096, 1, canvas.Transform{Y: 539, Width: 1920, Height: 1}},
		{"odd remainder centers down", canvas.Dimensions{Width: 321, Height: 181}, 10, 10, canvas.Transform{X: 155, Y: 85, Width: 10, Height: 10}},
		{"aspect ratio rounds to the nearest pixel", canvas.Dimensions{Width: 320, Height: 180}, 1000, 999, canvas.Transform{X: 70, Width: 180, Height: 180}},
		{"largest canvas", canvas.Dimensions{Width: canvas.MaxWidth, Height: canvas.MaxHeight}, 4096, 4096, canvas.Transform{X: 840, Width: 2160, Height: 2160}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := canvas.Fit(tc.area, tc.width, tc.height)
			if got != tc.want {
				t.Fatalf("Fit(%+v, %d, %d) = %+v, want %+v", tc.area, tc.width, tc.height, got, tc.want)
			}
			if err := canvas.Validate(key, operation, canvas.CreateObject{Kind: canvas.KindImage, Content: "ref-1", Transform: got}); err != nil {
				t.Fatalf("fitted transform is not a valid object: %v", err)
			}
			if got.X < 0 || got.Y < 0 || got.X+got.Width > tc.area.Width || got.Y+got.Height > tc.area.Height {
				t.Fatalf("Fit = %+v lies outside the area %+v", got, tc.area)
			}
		})
	}
}
