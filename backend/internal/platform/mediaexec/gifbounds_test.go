package mediaexec

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"testing"
)

func TestGIFFramesFitScreenChecksEveryImageDescriptor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		data  func(*testing.T) []byte
		valid bool
	}{
		{name: "frames cover the screen", data: gifSample(16, 16, 2), valid: true},
		{name: "offset frame inside the screen", data: gifOffsetFrame, valid: true},
		{name: "local color table and comment extension", data: gifWithLocalPaletteAndComment, valid: true},
		{name: "frame wider than the screen", data: gifPatchedFrame(1, frameDescriptor{width: 5000, height: 16})},
		{name: "frame taller than the screen", data: gifPatchedFrame(0, frameDescriptor{width: 16, height: 17})},
		{name: "left offset pushes the frame out", data: gifPatchedFrame(1, frameDescriptor{left: 1, width: 16, height: 16})},
		{name: "top offset pushes the frame out", data: gifPatchedFrame(1, frameDescriptor{top: 8, width: 16, height: 9})},
		{name: "empty frame", data: gifPatchedFrame(0, frameDescriptor{width: 0, height: 16})},
		{name: "not a GIF", data: func(*testing.T) []byte { return []byte("\x89PNG\r\n\x1a\n0000000") }},
		{name: "no frame", data: func(*testing.T) []byte { return append([]byte("GIF89a\x10\x00\x10\x00\x00\x00\x00"), 0x3B) }},
		{name: "unknown block", data: func(t *testing.T) []byte {
			data := gifSample(16, 16, 1)(t)
			return append(data[:len(data)-1], 0x00, 0x3B)
		}},
		{name: "truncated before the trailer", data: func(t *testing.T) []byte {
			data := gifSample(16, 16, 2)(t)
			return data[:len(data)-1]
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := gifFramesFitScreen(bytes.NewReader(tt.data(t)))
			if tt.valid {
				if err != nil {
					t.Fatalf("gifFramesFitScreen() = %v, want nil", err)
				}
				return
			}
			assertAdapterError(t, err, ErrorInvalidFrames)
		})
	}
}

type frameDescriptor struct {
	left, top, width, height int
}

// gifPatchedFrame rewrites the image descriptor of one frame of a 16x16
// two-frame GIF. The encoder writes an 8-byte graphic control extension right
// before each image descriptor.
func gifPatchedFrame(index int, descriptor frameDescriptor) func(*testing.T) []byte {
	return func(t *testing.T) []byte {
		data := gifSample(16, 16, 2)(t)
		seen := 0
		for i := 0; i+18 < len(data); i++ {
			if data[i] != 0x21 || data[i+1] != 0xF9 || data[i+2] != 0x04 || data[i+8] != 0x2C {
				continue
			}
			if seen == index {
				fields := data[i+9:]
				binary.LittleEndian.PutUint16(fields[0:], uint16(descriptor.left))
				binary.LittleEndian.PutUint16(fields[2:], uint16(descriptor.top))
				binary.LittleEndian.PutUint16(fields[4:], uint16(descriptor.width))
				binary.LittleEndian.PutUint16(fields[6:], uint16(descriptor.height))
				return data
			}
			seen++
		}
		t.Fatalf("image descriptor %d not found", index)
		return nil
	}
}

func gifOffsetFrame(t *testing.T) []byte {
	return encodeGIF(t, &gif.GIF{
		Image: []*image.Paletted{
			image.NewPaletted(image.Rect(0, 0, 16, 16), gifPalette),
			image.NewPaletted(image.Rect(4, 6, 16, 16), gifPalette),
		},
		Delay:  []int{10, 10},
		Config: image.Config{ColorModel: gifPalette, Width: 16, Height: 16},
	})
}

func gifWithLocalPaletteAndComment(t *testing.T) []byte {
	local := color.Palette{color.Black, color.White, color.Gray{Y: 128}, color.Gray{Y: 64}}
	data := encodeGIF(t, &gif.GIF{
		Image: []*image.Paletted{
			image.NewPaletted(image.Rect(0, 0, 16, 16), gifPalette),
			image.NewPaletted(image.Rect(0, 0, 16, 16), local),
		},
		Delay:  []int{10, 10},
		Config: image.Config{ColorModel: gifPalette, Width: 16, Height: 16},
	})
	comment := []byte{0x21, 0xFE, 0x03, 'a', 'b', 'c', 0x00}
	return append(append(data[:len(data)-1:len(data)-1], comment...), 0x3B)
}
