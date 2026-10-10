package mediaexec

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
)

const maxProbeBytes = int64(1 << 20)

func TestNormalizeProbeAcceptsApprovedMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		format     string
		streams    string
		kind       media.Kind
		container  container
		videoCodec string
		audioCodec string
		duration   time.Duration
	}{
		{name: "JPEG", format: "jpeg_pipe", streams: video("mjpeg", 800, 600, ""), kind: media.KindImage, container: containerJPEG, videoCodec: "mjpeg"},
		{name: "PNG", format: "image2", streams: video("png", 800, 600, ""), kind: media.KindImage, container: containerPNG, videoCodec: "png"},
		{name: "WebP", format: "webp_pipe", streams: video("webp", 800, 600, ""), kind: media.KindImage, container: containerWebP, videoCodec: "webp"},
		{name: "MP3", format: "mp3", streams: audio("mp3", "2.5"), kind: media.KindAudio, container: containerMP3, audioCodec: "mp3", duration: 2500 * time.Millisecond},
		{name: "WAV PCM unsigned 8-bit", format: "wav", streams: audio("pcm_u8", "2.5"), kind: media.KindAudio, container: containerWAV, audioCodec: "pcm_u8", duration: 2500 * time.Millisecond},
		{name: "WAV PCM signed 16-bit little-endian", format: "wav", streams: audio("pcm_s16le", "2.5"), kind: media.KindAudio, container: containerWAV, audioCodec: "pcm_s16le", duration: 2500 * time.Millisecond},
		{name: "WAV PCM signed 24-bit little-endian", format: "wav", streams: audio("pcm_s24le", "2.5"), kind: media.KindAudio, container: containerWAV, audioCodec: "pcm_s24le", duration: 2500 * time.Millisecond},
		{name: "WAV PCM signed 32-bit little-endian", format: "wav", streams: audio("pcm_s32le", "2.5"), kind: media.KindAudio, container: containerWAV, audioCodec: "pcm_s32le", duration: 2500 * time.Millisecond},
		{name: "WAV PCM float 32-bit little-endian", format: "wav", streams: audio("pcm_f32le", "2.5"), kind: media.KindAudio, container: containerWAV, audioCodec: "pcm_f32le", duration: 2500 * time.Millisecond},
		{name: "WAV PCM float 64-bit little-endian", format: "wav", streams: audio("pcm_f64le", "2.5"), kind: media.KindAudio, container: containerWAV, audioCodec: "pcm_f64le", duration: 2500 * time.Millisecond},
		{name: "FLAC", format: "flac", streams: audio("flac", "2.5"), kind: media.KindAudio, container: containerFLAC, audioCodec: "flac", duration: 2500 * time.Millisecond},
		{name: "Ogg Vorbis", format: "ogg", streams: audio("vorbis", "2.5"), kind: media.KindAudio, container: containerOgg, audioCodec: "vorbis", duration: 2500 * time.Millisecond},
		{name: "Ogg Opus", format: "ogg", streams: audio("opus", "2.5"), kind: media.KindAudio, container: containerOgg, audioCodec: "opus", duration: 2500 * time.Millisecond},
		{name: "M4A AAC", format: "mov,mp4,m4a,3gp,3g2,mj2", streams: audio("aac", "2.5"), kind: media.KindAudio, container: containerISOBaseMedia, audioCodec: "aac", duration: 2500 * time.Millisecond},
		{name: "MP4 H.264", format: "mov,mp4,m4a,3gp,3g2,mj2", streams: video("h264", 1920, 1080, "2.5"), kind: media.KindVideo, container: containerISOBaseMedia, videoCodec: "h264", duration: 2500 * time.Millisecond},
		{name: "MP4 H.264 AAC", format: "mov,mp4,m4a,3gp,3g2,mj2", streams: video("h264", 1920, 1080, "2.5") + "," + audio("aac", "2.5"), kind: media.KindVideo, container: containerISOBaseMedia, videoCodec: "h264", audioCodec: "aac", duration: 2500 * time.Millisecond},
		{name: "WebM VP9", format: "matroska,webm", streams: video("vp9", 1920, 1080, "2.5"), kind: media.KindVideo, container: containerWebM, videoCodec: "vp9", duration: 2500 * time.Millisecond},
		{name: "WebM VP9 Opus", format: "matroska,webm", streams: video("vp9", 1920, 1080, "2.5") + "," + audio("opus", "2.5"), kind: media.KindVideo, container: containerWebM, videoCodec: "vp9", audioCodec: "opus", duration: 2500 * time.Millisecond},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			probeDuration := "2.5"
			if tt.kind == media.KindImage {
				probeDuration = ""
			}
			inspection, normalized, err := NormalizeProbe(probe(tt.format, tt.streams, probeDuration), 1234, maxProbeBytes)
			if err != nil {
				t.Fatalf("NormalizeProbe() error = %v, want nil", err)
			}
			if inspection.Kind != tt.kind || inspection.InputBytes != 1234 || inspection.StreamCount != strings.Count(tt.streams, "codec_type") {
				t.Fatalf("inspection identity = %#v, want kind %q, 1234 bytes, matching stream count", inspection, tt.kind)
			}
			if inspection.Duration != tt.duration {
				t.Fatalf("Duration = %s, want %s", inspection.Duration, tt.duration)
			}
			if tt.kind == media.KindImage && inspection.Dimensions != (media.Dimensions{Width: 800, Height: 600}) {
				t.Fatalf("Dimensions = %#v, want 800x600", inspection.Dimensions)
			}
			if tt.kind == media.KindVideo && inspection.Dimensions != (media.Dimensions{Width: 1920, Height: 1080}) {
				t.Fatalf("Dimensions = %#v, want 1920x1080", inspection.Dimensions)
			}
			if normalized.container != tt.container || normalized.videoCodec != tt.videoCodec || normalized.audioCodec != tt.audioCodec {
				t.Fatalf("normalized format = %#v, want %q/%q/%q", normalized, tt.container, tt.videoCodec, tt.audioCodec)
			}
			if err := media.Validate(inspection, media.DefaultLimits()); err != nil {
				t.Fatalf("media.Validate() error = %v, want nil", err)
			}
		})
	}
}

func TestNormalizeProbeDefersConfiguredLimitAcceptanceToDomainValidation(t *testing.T) {
	t.Parallel()

	limits := media.DefaultLimits()
	tests := []struct {
		name       string
		input      []byte
		inputBytes int64
		code       media.RejectionCode
	}{
		{
			name:       "image input exceeds default byte limit",
			input:      probe("png_pipe", video("png", 800, 600, ""), ""),
			inputBytes: limits.MaxImageBytes + 1,
			code:       media.RejectionInputTooLarge,
		},
		{
			name:       "image dimensions exceed default limit",
			input:      probe("png_pipe", video("png", limits.MaxWidth+1, 1, ""), ""),
			inputBytes: 1,
			code:       media.RejectionDimensionsExceeded,
		},
		{
			name:       "audio duration exceeds default limit",
			input:      probe("mp3", audio("mp3", "601"), "601"),
			inputBytes: 1,
			code:       media.RejectionDurationExceeded,
		},
		{
			name:       "video duration exceeds default limit",
			input:      probe("matroska,webm", video("vp9", 1920, 1080, "601"), "601"),
			inputBytes: 1,
			code:       media.RejectionDurationExceeded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			inspection, _, err := NormalizeProbe(tt.input, tt.inputBytes, maxProbeBytes)
			if err != nil {
				t.Fatalf("NormalizeProbe() error = %v, want nil", err)
			}

			err = media.Validate(inspection, limits)
			var rejection *media.Rejection
			if !errors.As(err, &rejection) || rejection.Code() != tt.code {
				t.Fatalf("media.Validate() error = %v, want rejection %q", err, tt.code)
			}
		})
	}
}

func TestNormalizeProbeMakesGIFFrameMetadataRequirementExplicit(t *testing.T) {
	t.Parallel()

	inspection, normalized, err := NormalizeProbe(
		probe("gif", `{"codec_type":"video","codec_name":"gif","width":320,"height":200,"duration":"1.5","nb_frames":"12"}`, "1.5"),
		456, maxProbeBytes,
	)
	assertAdapterError(t, err, ErrorFrameMetadataRequired)
	if normalized.container != containerGIF || normalized.videoCodec != "gif" {
		t.Fatalf("normalized format = %#v, want GIF/gif", normalized)
	}
	if inspection.Kind != media.KindGIF || inspection.GIFFrameCount != 12 || inspection.Duration != 1500*time.Millisecond {
		t.Fatalf("Inspection = %#v, want incomplete 12-frame GIF metadata", inspection)
	}
	if len(inspection.GIFFrameDimensions) != 0 {
		t.Fatalf("GIFFrameDimensions = %#v, want no unverified frame dimensions", inspection.GIFFrameDimensions)
	}
	if err := media.Validate(inspection, media.DefaultLimits()); err == nil {
		t.Fatal("media.Validate() error = nil, want incomplete GIF rejection")
	}
}

func TestNormalizeProbeRejectsDisallowedAndContradictoryStreams(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		format  string
		streams string
	}{
		{name: "unknown container", format: "avi", streams: video("h264", 1, 1, "1")},
		{name: "unknown codec", format: "mp3", streams: audio("ac3", "1")},
		{name: "JPEG codec mismatch", format: "jpeg_pipe", streams: video("png", 1, 1, "")},
		{name: "WAV non-PCM", format: "wav", streams: audio("aac", "1")},
		{name: "WAV fake PCM prefix codec", format: "wav", streams: audio("pcm_not_a_codec", "1")},
		{name: "M4A wrong codec", format: "mov,mp4,m4a,3gp,3g2,mj2", streams: audio("opus", "1")},
		{name: "MP4 wrong video codec", format: "mov,mp4,m4a,3gp,3g2,mj2", streams: video("vp9", 1, 1, "1")},
		{name: "MP4 wrong audio codec", format: "mov,mp4,m4a,3gp,3g2,mj2", streams: video("h264", 1, 1, "1") + "," + audio("opus", "")},
		{name: "WebM wrong video codec", format: "matroska,webm", streams: video("h264", 1, 1, "1")},
		{name: "WebM wrong audio codec", format: "matroska,webm", streams: video("vp9", 1, 1, "1") + "," + audio("aac", "")},
		{name: "extra subtitle stream", format: "mov,mp4,m4a,3gp,3g2,mj2", streams: video("h264", 1, 1, "1") + `,{"codec_type":"subtitle","codec_name":"mov_text"}`},
		{name: "extra data stream", format: "matroska,webm", streams: video("vp9", 1, 1, "1") + `,{"codec_type":"data","codec_name":"bin_data"}`},
		{name: "extra attachment stream", format: "matroska,webm", streams: video("vp9", 1, 1, "1") + `,{"codec_type":"attachment","codec_name":"ttf"}`},
		{name: "arbitrary unknown stream type", format: "matroska,webm", streams: video("vp9", 1, 1, "1") + `,{"codec_type":"mystery","codec_name":"unknown"}`},
		{name: "multiple video streams", format: "mov,mp4,m4a,3gp,3g2,mj2", streams: video("h264", 1, 1, "1") + "," + video("h264", 1, 1, "1")},
		{name: "multiple audio streams", format: "mp3", streams: audio("mp3", "1") + "," + audio("mp3", "1")},
		{name: "mixed audio and video in audio container", format: "mp3", streams: audio("mp3", "1") + "," + video("mjpeg", 1, 1, "")},
		{name: "missing streams", format: "mp3", streams: ""},
		{name: "missing codec type", format: "mp3", streams: `{"codec_name":"mp3","duration":"1"}`},
		{name: "missing codec name", format: "mp3", streams: `{"codec_type":"audio","duration":"1"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := NormalizeProbe(probe(tt.format, tt.streams, "1"), 1, maxProbeBytes)
			assertAdapterError(t, err, ErrorUnsupportedMedia)
		})
	}
}

func TestNormalizeProbeRejectsMalformedOrMissingMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input []byte
		size  int64
		max   int64
		code  ErrorCode
	}{
		{name: "empty JSON", input: nil, size: 1, max: maxProbeBytes, code: ErrorInvalidProbe},
		{name: "malformed JSON", input: []byte(`{"streams":`), size: 1, max: maxProbeBytes, code: ErrorInvalidProbe},
		{name: "trailing JSON", input: append(probe("mp3", audio("mp3", "1"), "1"), []byte(` {}`)...), size: 1, max: maxProbeBytes, code: ErrorInvalidProbe},
		{name: "missing format", input: []byte(`{"streams":[]}`), size: 1, max: maxProbeBytes, code: ErrorInvalidProbe},
		{name: "missing format name", input: []byte(`{"format":{"duration":"1"},"streams":[]}`), size: 1, max: maxProbeBytes, code: ErrorInvalidProbe},
		{name: "extension-like tag cannot replace format", input: []byte(`{"format":{"tags":{"filename":"private.mp3"}},"streams":[{"codec_type":"audio","codec_name":"mp3","duration":"1"}]}`), size: 1, max: maxProbeBytes, code: ErrorInvalidProbe},
		{name: "zero input size", input: probe("mp3", audio("mp3", "1"), "1"), size: 0, max: maxProbeBytes, code: ErrorInvalidProbe},
		{name: "negative input size", input: probe("mp3", audio("mp3", "1"), "1"), size: -1, max: maxProbeBytes, code: ErrorInvalidProbe},
		{name: "zero JSON bound", input: probe("mp3", audio("mp3", "1"), "1"), size: 1, max: 0, code: ErrorInvalidProbe},
		{name: "JSON exceeds bound", input: probe("mp3", audio("mp3", "1"), "1"), size: 1, max: 1, code: ErrorProbeTooLarge},
		{name: "missing width", input: probe("png_pipe", video("png", 0, 1, ""), ""), size: 1, max: maxProbeBytes, code: ErrorInvalidProbe},
		{name: "negative height", input: probe("png_pipe", video("png", 1, -1, ""), ""), size: 1, max: maxProbeBytes, code: ErrorInvalidProbe},
		{name: "missing duration", input: probe("mp3", audio("mp3", ""), ""), size: 1, max: maxProbeBytes, code: ErrorInvalidProbe},
		{name: "image stream has duration", input: probe("png_pipe", video("png", 1, 1, "1"), ""), size: 1, max: maxProbeBytes, code: ErrorInvalidProbe},
		{name: "audio stream has dimensions", input: probe("mp3", `{"codec_type":"audio","codec_name":"mp3","width":1,"height":1,"duration":"1"}`, "1"), size: 1, max: maxProbeBytes, code: ErrorInvalidProbe},
		{name: "missing GIF frame count", input: probe("gif", video("gif", 1, 1, "1"), "1"), size: 1, max: maxProbeBytes, code: ErrorInvalidProbe},
		{name: "zero GIF frame count", input: probe("gif", `{"codec_type":"video","codec_name":"gif","width":1,"height":1,"duration":"1","nb_frames":"0"}`, "1"), size: 1, max: maxProbeBytes, code: ErrorInvalidProbe},
		{name: "malformed GIF frame count", input: probe("gif", `{"codec_type":"video","codec_name":"gif","width":1,"height":1,"duration":"1","nb_frames":"many"}`, "1"), size: 1, max: maxProbeBytes, code: ErrorInvalidProbe},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := NormalizeProbe(tt.input, tt.size, tt.max)
			assertAdapterError(t, err, tt.code)
		})
	}
}

func TestNormalizeProbeParsesDurationFieldsRobustly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		formatDuration string
		streamDuration string
		want           time.Duration
		code           ErrorCode
	}{
		{name: "quoted decimal", formatDuration: `"1.000000001"`, streamDuration: `"1.000000001"`, want: time.Second + time.Nanosecond},
		{name: "JSON number", formatDuration: `2.25`, streamDuration: `2.25`, want: 2250 * time.Millisecond},
		{name: "format only", formatDuration: `"3"`, streamDuration: ``, want: 3 * time.Second},
		{name: "stream only", formatDuration: ``, streamDuration: `"4"`, want: 4 * time.Second},
		{name: "conflicting values", formatDuration: `"1"`, streamDuration: `"2"`, code: ErrorInvalidProbe},
		{name: "NaN", formatDuration: `"NaN"`, streamDuration: ``, code: ErrorInvalidProbe},
		{name: "positive infinity", formatDuration: `"Inf"`, streamDuration: ``, code: ErrorInvalidProbe},
		{name: "negative infinity", formatDuration: `"-Inf"`, streamDuration: ``, code: ErrorInvalidProbe},
		{name: "negative", formatDuration: `"-1"`, streamDuration: ``, code: ErrorInvalidProbe},
		{name: "zero", formatDuration: `"0"`, streamDuration: ``, code: ErrorInvalidProbe},
		{name: "positive sub-nanosecond rounds to zero", formatDuration: `0.0000000004`, streamDuration: ``, code: ErrorInvalidProbe},
		{name: "malformed", formatDuration: `"one"`, streamDuration: ``, code: ErrorInvalidProbe},
		{name: "overflow", formatDuration: `"999999999999999999999"`, streamDuration: ``, code: ErrorInvalidProbe},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			input := probeRawDuration("mp3", audioRawDuration("mp3", tt.streamDuration), tt.formatDuration)
			inspection, _, err := NormalizeProbe(input, 1, maxProbeBytes)
			if tt.code != "" {
				assertAdapterError(t, err, tt.code)
				return
			}
			if err != nil {
				t.Fatalf("NormalizeProbe() error = %v, want nil", err)
			}
			if inspection.Duration != tt.want {
				t.Fatalf("Duration = %s, want %s", inspection.Duration, tt.want)
			}
		})
	}
}

func TestNormalizeProbeRejectsContradictoryOptionalAudioDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		format  string
		streams string
	}{
		{
			name:    "MP4 AAC duration contradicts format and video",
			format:  "mov,mp4,m4a,3gp,3g2,mj2",
			streams: video("h264", 1920, 1080, "2.5") + "," + audio("aac", "3"),
		},
		{
			name:    "WebM Opus duration contradicts format and video",
			format:  "matroska,webm",
			streams: video("vp9", 1920, 1080, "2.5") + "," + audio("opus", "3"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := NormalizeProbe(probe(tt.format, tt.streams, "2.5"), 1234, maxProbeBytes)
			assertAdapterError(t, err, ErrorInvalidProbe)
		})
	}
}

func TestAdapterErrorsAreTypedBoundedAndSafe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code    ErrorCode
		message string
	}{
		{code: ErrorInvalidProbe, message: "media probe metadata is invalid"},
		{code: ErrorProbeTooLarge, message: "media probe output exceeds the allowed size"},
		{code: ErrorUnsupportedMedia, message: "media format is unsupported"},
		{code: ErrorFrameMetadataRequired, message: "GIF frame metadata is required"},
		{code: ErrorConfiguration, message: "media inspection configuration is invalid"},
		{code: ErrorInputTooLarge, message: "media input exceeds the staging limit"},
		{code: ErrorStagingFailed, message: "media input staging failed"},
		{code: ErrorCommandFailed, message: "media inspection command failed"},
		{code: ErrorTimeout, message: "media inspection timed out"},
		{code: ErrorOutputOverflow, message: "media tool output exceeds the allowed size"},
		{code: ErrorInvalidFrames, message: "GIF frame metadata is invalid"},
		{code: ErrorValidation, message: "media inspection failed validation"},
		{code: ErrorCleanupFailed, message: "media inspection cleanup failed"},
	}
	unsafe := []string{"/private/input.mov", "stderr", "raw JSON", "h264", "secret"}

	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			t.Parallel()
			err := newError(tt.code)
			if err.Code() != tt.code || err.Error() != tt.message || len(err.Error()) > 64 {
				t.Fatalf("adapter error = %q/%q, want %q/%q bounded to 64 bytes", err.Code(), err.Error(), tt.code, tt.message)
			}
			for _, value := range unsafe {
				if strings.Contains(err.Error(), value) {
					t.Fatalf("Error() = %q, must not contain %q", err.Error(), value)
				}
			}
		})
	}
}

func probe(format, streams, duration string) []byte {
	quotedDuration := ""
	if duration != "" {
		quotedDuration = fmt.Sprintf(`,"duration":%q`, duration)
	}
	return []byte(fmt.Sprintf(`{"format":{"format_name":%q%s},"streams":[%s]}`, format, quotedDuration, streams))
}

func probeRawDuration(format, streams, duration string) []byte {
	field := ""
	if duration != "" {
		field = `,"duration":` + duration
	}
	return []byte(fmt.Sprintf(`{"format":{"format_name":%q%s},"streams":[%s]}`, format, field, streams))
}

func video(codec string, width, height int, duration string) string {
	field := ""
	if duration != "" {
		field = fmt.Sprintf(`,"duration":%q`, duration)
	}
	return fmt.Sprintf(`{"codec_type":"video","codec_name":%q,"width":%d,"height":%d%s}`, codec, width, height, field)
}

func audio(codec, duration string) string {
	field := ""
	if duration != "" {
		field = fmt.Sprintf(`,"duration":%q`, duration)
	}
	return fmt.Sprintf(`{"codec_type":"audio","codec_name":%q%s}`, codec, field)
}

func audioRawDuration(codec, duration string) string {
	field := ""
	if duration != "" {
		field = `,"duration":` + duration
	}
	return fmt.Sprintf(`{"codec_type":"audio","codec_name":%q%s}`, codec, field)
}

func assertAdapterError(t *testing.T, err error, code ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want %q", code)
	}
	var adapterError *Error
	if !errors.As(err, &adapterError) {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if adapterError.Code() != code {
		t.Fatalf("error code = %q, want %q", adapterError.Code(), code)
	}
}
