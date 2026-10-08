package mediaexec

import (
	"errors"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
)

func TestMergeGIFFramesRequiresExactlyOnePositiveDimensionPerDeclaredFrame(t *testing.T) {
	t.Parallel()

	base := media.Inspection{Kind: media.KindGIF, GIFFrameCount: 2}
	tests := []struct {
		name string
		json string
		want []media.Dimensions
	}{
		{name: "all frames", json: `{"frames":[{"width":10,"height":20},{"width":30,"height":40}]}`, want: []media.Dimensions{{Width: 10, Height: 20}, {Width: 30, Height: 40}}},
		{name: "count mismatch", json: `{"frames":[{"width":10,"height":20}]}`},
		{name: "zero width", json: `{"frames":[{"width":0,"height":20},{"width":30,"height":40}]}`},
		{name: "missing height", json: `{"frames":[{"width":10},{"width":30,"height":40}]}`},
		{name: "malformed JSON", json: `{"frames":`},
		{name: "trailing JSON", json: `{"frames":[]} {}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := mergeGIFFrames(base, []byte(tt.json))
			if tt.want == nil {
				assertAdapterError(t, err, ErrorInvalidFrames)
				return
			}
			if err != nil || len(got.GIFFrameDimensions) != len(tt.want) {
				t.Fatalf("mergeGIFFrames() = %#v, %v, want %#v, nil", got, err, tt.want)
			}
			for index := range tt.want {
				if got.GIFFrameDimensions[index] != tt.want[index] {
					t.Fatalf("frame %d = %#v, want %#v", index, got.GIFFrameDimensions[index], tt.want[index])
				}
			}
		})
	}

	var adapterError *Error
	if _, err := mergeGIFFrames(base, []byte(`{"frames":[]}`)); !errors.As(err, &adapterError) {
		t.Fatalf("error type = %T, want *Error", err)
	}
}
