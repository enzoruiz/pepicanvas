package media

import "time"

// Kind identifies a supported content-derived media category.
type Kind string

const (
	KindImage Kind = "image"
	KindGIF   Kind = "gif"
	KindAudio Kind = "audio"
	KindVideo Kind = "video"
)

// Valid reports whether kind belongs to the closed media vocabulary.
func (kind Kind) Valid() bool {
	switch kind {
	case KindImage, KindGIF, KindAudio, KindVideo:
		return true
	default:
		return false
	}
}

// Dimensions contains decoded pixel dimensions for one visual surface.
type Dimensions struct {
	Width  int
	Height int
}

// Inspection is normalized, content-derived metadata produced by an inspection
// adapter. GIFFrameDimensions must contain one entry for every decoded GIF frame.
type Inspection struct {
	Kind               Kind
	InputBytes         int64
	StreamCount        int
	Dimensions         Dimensions
	Duration           time.Duration
	GIFFrameCount      int
	GIFFrameDimensions []Dimensions
}

// Validate applies the domain acceptance contract to normalized metadata. It
// performs no file access, subprocess execution, persistence, quota, or playback.
func Validate(inspection Inspection, limits Limits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	if !inspection.Kind.Valid() {
		return newRejection(RejectionUnsupportedKind)
	}
	if !validMetadata(inspection) {
		return newRejection(RejectionInvalidMetadata)
	}
	if inspection.InputBytes > limits.maxInputBytes(inspection.Kind) {
		return newRejection(RejectionInputTooLarge)
	}
	if inspection.StreamCount > limits.MaxStreams {
		return newRejection(RejectionStreamCountExceeded)
	}
	if inspection.Duration > limits.maxDuration(inspection.Kind) {
		return newRejection(RejectionDurationExceeded)
	}

	switch inspection.Kind {
	case KindImage:
		return validateDimensions(inspection.Dimensions, limits)
	case KindGIF:
		if inspection.GIFFrameCount > limits.MaxGIFFrames {
			return newRejection(RejectionFrameCountExceeded)
		}
		for _, frame := range inspection.GIFFrameDimensions {
			if err := validateDimensions(frame, limits); err != nil {
				return err
			}
		}
	case KindAudio:
		return nil
	case KindVideo:
		return validateDimensions(inspection.Dimensions, limits)
	}

	return nil
}

func validMetadata(inspection Inspection) bool {
	if inspection.InputBytes <= 0 || inspection.StreamCount <= 0 {
		return false
	}

	switch inspection.Kind {
	case KindImage:
		return inspection.Dimensions.valid() && inspection.Duration == 0 && !inspection.hasGIFProperties()
	case KindGIF:
		return inspection.Dimensions.absent() && inspection.Duration > 0 &&
			inspection.GIFFrameCount > 0 && inspection.GIFFrameCount == len(inspection.GIFFrameDimensions) &&
			allDimensionsValid(inspection.GIFFrameDimensions)
	case KindAudio:
		return inspection.Dimensions.absent() && inspection.Duration > 0 && !inspection.hasGIFProperties()
	case KindVideo:
		return inspection.Dimensions.valid() && inspection.Duration > 0 && !inspection.hasGIFProperties()
	default:
		return false
	}
}

func validateDimensions(dimensions Dimensions, limits Limits) error {
	if dimensions.Width > limits.MaxWidth || dimensions.Height > limits.MaxHeight ||
		int64(dimensions.Width) > limits.MaxPixels/int64(dimensions.Height) {
		return newRejection(RejectionDimensionsExceeded)
	}
	return nil
}

func (inspection Inspection) hasGIFProperties() bool {
	return inspection.GIFFrameCount != 0 || len(inspection.GIFFrameDimensions) != 0
}

func (dimensions Dimensions) valid() bool {
	return dimensions.Width > 0 && dimensions.Height > 0
}

func (dimensions Dimensions) absent() bool {
	return dimensions.Width == 0 && dimensions.Height == 0
}

func allDimensionsValid(all []Dimensions) bool {
	for _, dimensions := range all {
		if !dimensions.valid() {
			return false
		}
	}
	return true
}
