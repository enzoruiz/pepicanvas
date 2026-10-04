package media

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDefaultLimitsMatchApprovedMVPMatrix(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()
	const mebibyte = int64(1024 * 1024)

	if limits.MaxImageBytes != 25*mebibyte || limits.MaxGIFBytes != 25*mebibyte {
		t.Fatalf("image/GIF byte limits = %d/%d, want %d", limits.MaxImageBytes, limits.MaxGIFBytes, 25*mebibyte)
	}
	if limits.MaxAudioBytes != 50*mebibyte || limits.MaxVideoBytes != 250*mebibyte {
		t.Fatalf("audio/video byte limits = %d/%d, want %d/%d", limits.MaxAudioBytes, limits.MaxVideoBytes, 50*mebibyte, 250*mebibyte)
	}
	if limits.MaxWidth != 3840 || limits.MaxHeight != 3840 || limits.MaxPixels != 8_294_400 {
		t.Fatalf("dimension limits = %dx%d/%d, want 3840x3840/8294400", limits.MaxWidth, limits.MaxHeight, limits.MaxPixels)
	}
	if limits.MaxGIFDuration != 120*time.Second || limits.MaxAudioDuration != 600*time.Second || limits.MaxVideoDuration != 600*time.Second {
		t.Fatalf("duration limits = %s/%s/%s, want 2m/10m/10m", limits.MaxGIFDuration, limits.MaxAudioDuration, limits.MaxVideoDuration)
	}
	if limits.MaxGIFFrames != 3600 || limits.MaxStreams != 8 {
		t.Fatalf("count limits = %d frames/%d streams, want 3600/8", limits.MaxGIFFrames, limits.MaxStreams)
	}
	if limits.InspectionTimeout != 30*time.Second || limits.MaxCapturedOutputBytes != mebibyte {
		t.Fatalf("inspection limits = %s/%d bytes, want 30s/%d bytes", limits.InspectionTimeout, limits.MaxCapturedOutputBytes, mebibyte)
	}
	if limits.MaxProcesses != 8 || limits.MaxMemoryBytes != 512*mebibyte || limits.MaxCPUTime != 30*time.Second {
		t.Fatalf("process limits = %d/%d bytes/%s, want 8/%d bytes/30s", limits.MaxProcesses, limits.MaxMemoryBytes, limits.MaxCPUTime, 512*mebibyte)
	}
	if err := limits.Validate(); err != nil {
		t.Fatalf("DefaultLimits().Validate() error = %v, want nil", err)
	}
}

func TestLimitsValidateRejectsNonPositiveConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*Limits)
	}{
		{name: "image bytes", mutate: func(l *Limits) { l.MaxImageBytes = 0 }},
		{name: "GIF bytes", mutate: func(l *Limits) { l.MaxGIFBytes = -1 }},
		{name: "audio bytes", mutate: func(l *Limits) { l.MaxAudioBytes = 0 }},
		{name: "video bytes", mutate: func(l *Limits) { l.MaxVideoBytes = -1 }},
		{name: "width", mutate: func(l *Limits) { l.MaxWidth = 0 }},
		{name: "height", mutate: func(l *Limits) { l.MaxHeight = -1 }},
		{name: "pixels", mutate: func(l *Limits) { l.MaxPixels = 0 }},
		{name: "GIF duration", mutate: func(l *Limits) { l.MaxGIFDuration = -time.Second }},
		{name: "audio duration", mutate: func(l *Limits) { l.MaxAudioDuration = 0 }},
		{name: "video duration", mutate: func(l *Limits) { l.MaxVideoDuration = -time.Second }},
		{name: "GIF frames", mutate: func(l *Limits) { l.MaxGIFFrames = 0 }},
		{name: "streams", mutate: func(l *Limits) { l.MaxStreams = -1 }},
		{name: "inspection timeout", mutate: func(l *Limits) { l.InspectionTimeout = 0 }},
		{name: "captured output", mutate: func(l *Limits) { l.MaxCapturedOutputBytes = -1 }},
		{name: "processes", mutate: func(l *Limits) { l.MaxProcesses = 0 }},
		{name: "memory", mutate: func(l *Limits) { l.MaxMemoryBytes = -1 }},
		{name: "CPU time", mutate: func(l *Limits) { l.MaxCPUTime = 0 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			limits := DefaultLimits()
			tt.mutate(&limits)
			assertRejection(t, limits.Validate(), RejectionInvalidLimits)
			assertRejection(t, Validate(validInspection(KindImage), limits), RejectionInvalidLimits)
		})
	}
}

func TestValidateAcceptsExactBoundariesAndRejectsOneBeyond(t *testing.T) {
	t.Parallel()

	type boundaryCase struct {
		name       string
		inspection Inspection
		limits     Limits
		wantCode   RejectionCode
	}

	limits := DefaultLimits()
	gifFramesAtLimit := dimensions(limits.MaxGIFFrames, 1, 1)
	tests := []boundaryCase{
		{name: "image bytes exact", inspection: withBytes(validInspection(KindImage), limits.MaxImageBytes), limits: limits},
		{name: "image bytes one beyond", inspection: withBytes(validInspection(KindImage), limits.MaxImageBytes+1), limits: limits, wantCode: RejectionInputTooLarge},
		{name: "GIF bytes exact", inspection: withBytes(validInspection(KindGIF), limits.MaxGIFBytes), limits: limits},
		{name: "GIF bytes one beyond", inspection: withBytes(validInspection(KindGIF), limits.MaxGIFBytes+1), limits: limits, wantCode: RejectionInputTooLarge},
		{name: "audio bytes exact", inspection: withBytes(validInspection(KindAudio), limits.MaxAudioBytes), limits: limits},
		{name: "audio bytes one beyond", inspection: withBytes(validInspection(KindAudio), limits.MaxAudioBytes+1), limits: limits, wantCode: RejectionInputTooLarge},
		{name: "video bytes exact", inspection: withBytes(validInspection(KindVideo), limits.MaxVideoBytes), limits: limits},
		{name: "video bytes one beyond", inspection: withBytes(validInspection(KindVideo), limits.MaxVideoBytes+1), limits: limits, wantCode: RejectionInputTooLarge},
		{name: "horizontal dimensions exact", inspection: withDimensions(validInspection(KindImage), 3840, 2160), limits: limits},
		{name: "vertical dimensions exact", inspection: withDimensions(validInspection(KindVideo), 2160, 3840), limits: limits},
		{name: "width one beyond", inspection: withDimensions(validInspection(KindImage), limits.MaxWidth+1, 1), limits: limits, wantCode: RejectionDimensionsExceeded},
		{name: "GIF height one beyond", inspection: withGIFDimensions(validInspection(KindGIF), 1, limits.MaxHeight+1), limits: limits, wantCode: RejectionDimensionsExceeded},
		{name: "pixels exact", inspection: withDimensions(validInspection(KindVideo), 1, 11), limits: withMaxPixels(limits, 11)},
		{name: "pixels one beyond", inspection: withDimensions(validInspection(KindVideo), 3, 4), limits: withMaxPixels(limits, 11), wantCode: RejectionDimensionsExceeded},
		{name: "GIF duration exact", inspection: withDuration(validInspection(KindGIF), limits.MaxGIFDuration), limits: limits},
		{name: "GIF duration one beyond", inspection: withDuration(validInspection(KindGIF), limits.MaxGIFDuration+time.Nanosecond), limits: limits, wantCode: RejectionDurationExceeded},
		{name: "audio duration exact", inspection: withDuration(validInspection(KindAudio), limits.MaxAudioDuration), limits: limits},
		{name: "audio duration one beyond", inspection: withDuration(validInspection(KindAudio), limits.MaxAudioDuration+time.Nanosecond), limits: limits, wantCode: RejectionDurationExceeded},
		{name: "video duration exact", inspection: withDuration(validInspection(KindVideo), limits.MaxVideoDuration), limits: limits},
		{name: "video duration one beyond", inspection: withDuration(validInspection(KindVideo), limits.MaxVideoDuration+time.Nanosecond), limits: limits, wantCode: RejectionDurationExceeded},
		{name: "GIF frames exact", inspection: withGIFFrames(validInspection(KindGIF), gifFramesAtLimit), limits: limits},
		{name: "GIF frames one beyond", inspection: withGIFFrames(validInspection(KindGIF), dimensions(limits.MaxGIFFrames+1, 1, 1)), limits: limits, wantCode: RejectionFrameCountExceeded},
	}

	for _, kind := range []Kind{KindImage, KindGIF, KindAudio, KindVideo} {
		tests = append(tests,
			boundaryCase{name: string(kind) + " streams exact", inspection: withStreams(validInspection(kind), limits.MaxStreams), limits: limits},
			boundaryCase{name: string(kind) + " streams one beyond", inspection: withStreams(validInspection(kind), limits.MaxStreams+1), limits: limits, wantCode: RejectionStreamCountExceeded},
		)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := Validate(tt.inspection, tt.limits)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			assertRejection(t, err, tt.wantCode)
		})
	}
}

func TestValidateRejectsMalformedContradictoryAndMismatchedMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input Inspection
		code  RejectionCode
	}{
		{name: "unknown kind", input: Inspection{Kind: Kind("document"), InputBytes: 1, StreamCount: 1}, code: RejectionUnsupportedKind},
		{name: "missing input size", input: withBytes(validInspection(KindImage), 0), code: RejectionInvalidMetadata},
		{name: "negative input size", input: withBytes(validInspection(KindImage), -1), code: RejectionInvalidMetadata},
		{name: "missing streams", input: withStreams(validInspection(KindAudio), 0), code: RejectionInvalidMetadata},
		{name: "missing image dimensions", input: withDimensions(validInspection(KindImage), 0, 0), code: RejectionInvalidMetadata},
		{name: "partial video dimensions", input: withDimensions(validInspection(KindVideo), 1920, 0), code: RejectionInvalidMetadata},
		{name: "image has duration", input: withDuration(validInspection(KindImage), time.Second), code: RejectionInvalidMetadata},
		{name: "image has GIF frames", input: withGIFFrames(validInspection(KindImage), dimensions(1, 1, 1)), code: RejectionInvalidMetadata},
		{name: "audio has dimensions", input: withDimensions(validInspection(KindAudio), 1, 1), code: RejectionInvalidMetadata},
		{name: "audio has GIF frame count", input: withGIFFrames(validInspection(KindAudio), dimensions(1, 1, 1)), code: RejectionInvalidMetadata},
		{name: "video missing duration", input: withDuration(validInspection(KindVideo), 0), code: RejectionInvalidMetadata},
		{name: "video has GIF frames", input: withGIFFrames(validInspection(KindVideo), dimensions(1, 1, 1)), code: RejectionInvalidMetadata},
		{name: "GIF missing duration", input: withDuration(validInspection(KindGIF), 0), code: RejectionInvalidMetadata},
		{name: "GIF missing frame count", input: Inspection{Kind: KindGIF, InputBytes: 1, StreamCount: 1, Duration: time.Second}, code: RejectionInvalidMetadata},
		{name: "GIF frame count contradicts dimensions", input: Inspection{Kind: KindGIF, InputBytes: 1, StreamCount: 1, Duration: time.Second, GIFFrameCount: 2, GIFFrameDimensions: dimensions(1, 1, 1)}, code: RejectionInvalidMetadata},
		{name: "GIF frame lacks width", input: withGIFDimensions(validInspection(KindGIF), 0, 1), code: RejectionInvalidMetadata},
		{name: "GIF carries non-frame dimensions", input: withDimensions(validInspection(KindGIF), 1, 1), code: RejectionInvalidMetadata},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertRejection(t, Validate(tt.input, DefaultLimits()), tt.code)
		})
	}
}

func TestRejectionsExposeOnlyBoundedSafeStrings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code    RejectionCode
		message string
	}{
		{code: RejectionInvalidLimits, message: "media limits are invalid"},
		{code: RejectionUnsupportedKind, message: "media kind is unsupported"},
		{code: RejectionInvalidMetadata, message: "media metadata is invalid"},
		{code: RejectionInputTooLarge, message: "media input exceeds the allowed size"},
		{code: RejectionDimensionsExceeded, message: "media dimensions exceed the allowed limit"},
		{code: RejectionDurationExceeded, message: "media duration exceeds the allowed limit"},
		{code: RejectionFrameCountExceeded, message: "media frame count exceeds the allowed limit"},
		{code: RejectionStreamCountExceeded, message: "media stream count exceeds the allowed limit"},
	}
	unsafeValues := []string{"/private/uploads/customer.mp4", "stderr", "raw_metadata", "h264", "customer secret"}

	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			t.Parallel()

			err := newRejection(tt.code)
			if err.Code() != tt.code {
				t.Fatalf("Code() = %q, want %q", err.Code(), tt.code)
			}
			if err.Error() != tt.message {
				t.Fatalf("Error() = %q, want %q", err.Error(), tt.message)
			}
			if len(err.Error()) > 64 {
				t.Fatalf("Error() length = %d, want <= 64", len(err.Error()))
			}
			for _, unsafeValue := range unsafeValues {
				if strings.Contains(err.Error(), unsafeValue) {
					t.Fatalf("Error() = %q, must not contain %q", err.Error(), unsafeValue)
				}
			}
		})
	}
}

func validInspection(kind Kind) Inspection {
	inspection := Inspection{Kind: kind, InputBytes: 1, StreamCount: 1}
	switch kind {
	case KindImage:
		inspection.Dimensions = Dimensions{Width: 1, Height: 1}
	case KindGIF:
		inspection.Duration = time.Second
		inspection.GIFFrameCount = 1
		inspection.GIFFrameDimensions = dimensions(1, 1, 1)
	case KindAudio:
		inspection.Duration = time.Second
	case KindVideo:
		inspection.Dimensions = Dimensions{Width: 1, Height: 1}
		inspection.Duration = time.Second
	}
	return inspection
}

func withBytes(inspection Inspection, inputBytes int64) Inspection {
	inspection.InputBytes = inputBytes
	return inspection
}

func withStreams(inspection Inspection, streams int) Inspection {
	inspection.StreamCount = streams
	return inspection
}

func withDimensions(inspection Inspection, width, height int) Inspection {
	inspection.Dimensions = Dimensions{Width: width, Height: height}
	return inspection
}

func withGIFDimensions(inspection Inspection, width, height int) Inspection {
	inspection.GIFFrameDimensions[0] = Dimensions{Width: width, Height: height}
	return inspection
}

func withDuration(inspection Inspection, duration time.Duration) Inspection {
	inspection.Duration = duration
	return inspection
}

func withGIFFrames(inspection Inspection, frames []Dimensions) Inspection {
	inspection.GIFFrameCount = len(frames)
	inspection.GIFFrameDimensions = frames
	return inspection
}

func dimensions(count, width, height int) []Dimensions {
	frames := make([]Dimensions, count)
	for index := range frames {
		frames[index] = Dimensions{Width: width, Height: height}
	}
	return frames
}

func withMaxPixels(limits Limits, maxPixels int64) Limits {
	limits.MaxPixels = maxPixels
	return limits
}

func assertRejection(t *testing.T, err error, wantCode RejectionCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want rejection %q", wantCode)
	}
	var rejection *Rejection
	if !errors.As(err, &rejection) {
		t.Fatalf("error type = %T, want *Rejection", err)
	}
	if rejection.Code() != wantCode {
		t.Fatalf("rejection code = %q, want %q", rejection.Code(), wantCode)
	}
}
