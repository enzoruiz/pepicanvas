package mediaexec

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
)

type frameDocument struct {
	Frames []frame `json:"frames"`
}

type frame struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

func mergeGIFFrames(inspection media.Inspection, data []byte) (media.Inspection, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var document frameDocument
	if err := decoder.Decode(&document); err != nil {
		return media.Inspection{}, newError(ErrorInvalidFrames)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF || len(document.Frames) != inspection.GIFFrameCount {
		return media.Inspection{}, newError(ErrorInvalidFrames)
	}

	dimensions := make([]media.Dimensions, len(document.Frames))
	for index, frame := range document.Frames {
		if frame.Width <= 0 || frame.Height <= 0 {
			return media.Inspection{}, newError(ErrorInvalidFrames)
		}
		dimensions[index] = media.Dimensions{Width: frame.Width, Height: frame.Height}
	}
	inspection.GIFFrameDimensions = dimensions
	return inspection, nil
}
