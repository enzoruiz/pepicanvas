// Package mediaexec adapts bounded media-tool metadata into the pure media domain.
package mediaexec

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
)

// ErrorCode identifies a fixed, public-safe adapter failure category.
type ErrorCode string

const (
	ErrorInvalidProbe          ErrorCode = "invalid_probe"
	ErrorProbeTooLarge         ErrorCode = "probe_too_large"
	ErrorUnsupportedMedia      ErrorCode = "unsupported_media"
	ErrorFrameMetadataRequired ErrorCode = "frame_metadata_required"
	ErrorConfiguration         ErrorCode = "configuration_invalid"
	ErrorInputTooLarge         ErrorCode = "input_too_large"
	ErrorStagingFailed         ErrorCode = "staging_failed"
	ErrorCommandFailed         ErrorCode = "command_failed"
	ErrorTimeout               ErrorCode = "timeout"
	ErrorOutputOverflow        ErrorCode = "output_overflow"
	ErrorInvalidFrames         ErrorCode = "invalid_frames"
	ErrorValidation            ErrorCode = "validation_failed"
	ErrorCleanupFailed         ErrorCode = "cleanup_failed"
)

var errorMessages = map[ErrorCode]string{
	ErrorInvalidProbe:          "media probe metadata is invalid",
	ErrorProbeTooLarge:         "media probe output exceeds the allowed size",
	ErrorUnsupportedMedia:      "media format is unsupported",
	ErrorFrameMetadataRequired: "GIF frame metadata is required",
	ErrorConfiguration:         "media inspection configuration is invalid",
	ErrorInputTooLarge:         "media input exceeds the staging limit",
	ErrorStagingFailed:         "media input staging failed",
	ErrorCommandFailed:         "media inspection command failed",
	ErrorTimeout:               "media inspection timed out",
	ErrorOutputOverflow:        "media tool output exceeds the allowed size",
	ErrorInvalidFrames:         "GIF frame metadata is invalid",
	ErrorValidation:            "media inspection failed validation",
	ErrorCleanupFailed:         "media inspection cleanup failed",
}

// Error is a bounded adapter error that never includes tool or input details.
type Error struct {
	code ErrorCode
}

func newError(code ErrorCode) *Error {
	return &Error{code: code}
}

// Code returns the stable machine-readable adapter failure category.
func (err *Error) Code() ErrorCode {
	return err.code
}

// Error returns a fixed safe message without raw probe data or private values.
func (err *Error) Error() string {
	return errorMessages[err.code]
}

type container string

const (
	containerJPEG         container = "jpeg"
	containerPNG          container = "png"
	containerWebP         container = "webp"
	containerGIF          container = "gif"
	containerMP3          container = "mp3"
	containerWAV          container = "wav"
	containerFLAC         container = "flac"
	containerOgg          container = "ogg"
	containerISOBaseMedia container = "iso_base_media"
	containerWebM         container = "webm"
)

// normalizedFormat is adapter-internal evidence used by later command slices.
// It is deliberately not part of the domain contract or public error surface.
type normalizedFormat struct {
	container  container
	videoCodec string
	audioCodec string
}

type probeDocument struct {
	Format  probeFormat   `json:"format"`
	Streams []probeStream `json:"streams"`
}

type probeFormat struct {
	Name     string          `json:"format_name"`
	Duration json.RawMessage `json:"duration"`
}

type probeStream struct {
	CodecType string          `json:"codec_type"`
	CodecName string          `json:"codec_name"`
	Width     int             `json:"width"`
	Height    int             `json:"height"`
	Duration  json.RawMessage `json:"duration"`
	Frames    string          `json:"nb_frames"`
}

// NormalizeProbe parses bounded ffprobe JSON and enforces the closed MVP
// container/codec matrix. inputBytes comes from the trusted staging caller.
// Successful non-GIF normalization proves structural coherence, not acceptance:
// the result may exceed configured byte, dimension, duration, or stream limits.
// Callers MUST pass the inspection to media.Validate with their selected limits
// before accepting it. GIF output is intentionally incomplete until a later
// all-frame probe supplies one verified dimension per frame; that case returns
// ErrorFrameMetadataRequired.
func NormalizeProbe(data []byte, inputBytes, maxJSONBytes int64) (media.Inspection, normalizedFormat, error) {
	if inputBytes <= 0 || maxJSONBytes <= 0 || len(data) == 0 {
		return media.Inspection{}, normalizedFormat{}, newError(ErrorInvalidProbe)
	}
	if int64(len(data)) > maxJSONBytes {
		return media.Inspection{}, normalizedFormat{}, newError(ErrorProbeTooLarge)
	}

	document, ok := decodeProbe(data)
	if !ok {
		return media.Inspection{}, normalizedFormat{}, newError(ErrorInvalidProbe)
	}
	if document.Format.Name == "" {
		return media.Inspection{}, normalizedFormat{}, newError(ErrorInvalidProbe)
	}

	formatDuration, formatHasDuration, ok := parseDuration(document.Format.Duration)
	if !ok {
		return media.Inspection{}, normalizedFormat{}, newError(ErrorInvalidProbe)
	}
	for _, stream := range document.Streams {
		if _, _, valid := parseDuration(stream.Duration); !valid {
			return media.Inspection{}, normalizedFormat{}, newError(ErrorInvalidProbe)
		}
	}

	inspection, normalized, code := classify(document, inputBytes, formatDuration, formatHasDuration)
	if code != "" {
		return inspection, normalized, newError(code)
	}
	return inspection, normalized, nil
}

func decodeProbe(data []byte) (probeDocument, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var document probeDocument
	if err := decoder.Decode(&document); err != nil {
		return probeDocument{}, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return probeDocument{}, false
	}
	return document, true
}

func classify(document probeDocument, inputBytes int64, formatDuration time.Duration, formatHasDuration bool) (media.Inspection, normalizedFormat, ErrorCode) {
	videos, audios, supportedTypes := splitStreams(document.Streams)
	if !supportedTypes || len(document.Streams) == 0 {
		return media.Inspection{}, normalizedFormat{}, ErrorUnsupportedMedia
	}

	base := media.Inspection{InputBytes: inputBytes, StreamCount: len(document.Streams)}
	switch document.Format.Name {
	case "jpeg_pipe":
		return classifyImage(base, videos, audios, "mjpeg", containerJPEG, formatHasDuration)
	case "png_pipe":
		return classifyImage(base, videos, audios, "png", containerPNG, formatHasDuration)
	case "webp_pipe":
		return classifyImage(base, videos, audios, "webp", containerWebP, formatHasDuration)
	case "image2":
		return classifyImage2(base, videos, audios, formatHasDuration)
	case "gif":
		return classifyGIF(base, videos, audios, formatDuration, formatHasDuration)
	case "mp3":
		return classifyAudio(base, videos, audios, containerMP3, formatDuration, formatHasDuration, func(codec string) bool { return codec == "mp3" })
	case "wav":
		return classifyAudio(base, videos, audios, containerWAV, formatDuration, formatHasDuration, isApprovedWAVCodec)
	case "flac":
		return classifyAudio(base, videos, audios, containerFLAC, formatDuration, formatHasDuration, func(codec string) bool { return codec == "flac" })
	case "ogg":
		return classifyAudio(base, videos, audios, containerOgg, formatDuration, formatHasDuration, func(codec string) bool { return codec == "vorbis" || codec == "opus" })
	case "mov,mp4,m4a,3gp,3g2,mj2":
		return classifyISOBaseMedia(base, videos, audios, formatDuration, formatHasDuration)
	case "matroska,webm":
		return classifyVideo(base, videos, audios, containerWebM, "vp9", "opus", formatDuration, formatHasDuration)
	default:
		return media.Inspection{}, normalizedFormat{}, ErrorUnsupportedMedia
	}
}

func isApprovedWAVCodec(codec string) bool {
	switch codec {
	case "pcm_u8", "pcm_s16le", "pcm_s24le", "pcm_s32le", "pcm_f32le", "pcm_f64le":
		return true
	default:
		return false
	}
}

func splitStreams(streams []probeStream) ([]probeStream, []probeStream, bool) {
	var videos []probeStream
	var audios []probeStream
	for _, stream := range streams {
		if stream.CodecName == "" {
			return nil, nil, false
		}
		switch stream.CodecType {
		case "video":
			videos = append(videos, stream)
		case "audio":
			audios = append(audios, stream)
		default:
			return nil, nil, false
		}
	}
	return videos, audios, true
}

func classifyImage2(base media.Inspection, videos, audios []probeStream, hasDuration bool) (media.Inspection, normalizedFormat, ErrorCode) {
	if len(videos) != 1 {
		return media.Inspection{}, normalizedFormat{}, ErrorUnsupportedMedia
	}
	switch videos[0].CodecName {
	case "mjpeg":
		return classifyImage(base, videos, audios, "mjpeg", containerJPEG, hasDuration)
	case "png":
		return classifyImage(base, videos, audios, "png", containerPNG, hasDuration)
	case "webp":
		return classifyImage(base, videos, audios, "webp", containerWebP, hasDuration)
	default:
		return media.Inspection{}, normalizedFormat{}, ErrorUnsupportedMedia
	}
}

func classifyImage(base media.Inspection, videos, audios []probeStream, codec string, normalizedContainer container, hasDuration bool) (media.Inspection, normalizedFormat, ErrorCode) {
	if len(videos) != 1 || len(audios) != 0 || videos[0].CodecName != codec {
		return media.Inspection{}, normalizedFormat{}, ErrorUnsupportedMedia
	}
	if hasDuration || len(videos[0].Duration) != 0 || videos[0].Width <= 0 || videos[0].Height <= 0 {
		return media.Inspection{}, normalizedFormat{}, ErrorInvalidProbe
	}
	base.Kind = media.KindImage
	base.Dimensions = media.Dimensions{Width: videos[0].Width, Height: videos[0].Height}
	return base, normalizedFormat{container: normalizedContainer, videoCodec: codec}, ""
}

func classifyGIF(base media.Inspection, videos, audios []probeStream, formatDuration time.Duration, formatHasDuration bool) (media.Inspection, normalizedFormat, ErrorCode) {
	normalized := normalizedFormat{container: containerGIF, videoCodec: "gif"}
	if len(videos) != 1 || len(audios) != 0 || videos[0].CodecName != "gif" {
		return media.Inspection{}, normalizedFormat{}, ErrorUnsupportedMedia
	}
	if videos[0].Width <= 0 || videos[0].Height <= 0 {
		return media.Inspection{}, normalized, ErrorInvalidProbe
	}
	duration, ok := normalizedDuration(formatDuration, formatHasDuration, videos[0].Duration)
	if !ok {
		return media.Inspection{}, normalized, ErrorInvalidProbe
	}
	frames, err := strconv.Atoi(videos[0].Frames)
	if err != nil || frames <= 0 {
		return media.Inspection{}, normalized, ErrorInvalidProbe
	}
	base.Kind = media.KindGIF
	base.Duration = duration
	base.GIFFrameCount = frames
	return base, normalized, ErrorFrameMetadataRequired
}

func classifyAudio(base media.Inspection, videos, audios []probeStream, normalizedContainer container, formatDuration time.Duration, formatHasDuration bool, codecAllowed func(string) bool) (media.Inspection, normalizedFormat, ErrorCode) {
	if len(videos) != 0 || len(audios) != 1 || !codecAllowed(audios[0].CodecName) {
		return media.Inspection{}, normalizedFormat{}, ErrorUnsupportedMedia
	}
	if audios[0].Width != 0 || audios[0].Height != 0 {
		return media.Inspection{}, normalizedFormat{}, ErrorInvalidProbe
	}
	duration, ok := normalizedDuration(formatDuration, formatHasDuration, audios[0].Duration)
	if !ok {
		return media.Inspection{}, normalizedFormat{}, ErrorInvalidProbe
	}
	base.Kind = media.KindAudio
	base.Duration = duration
	return base, normalizedFormat{container: normalizedContainer, audioCodec: audios[0].CodecName}, ""
}

func classifyISOBaseMedia(base media.Inspection, videos, audios []probeStream, formatDuration time.Duration, formatHasDuration bool) (media.Inspection, normalizedFormat, ErrorCode) {
	if len(videos) == 0 {
		return classifyAudio(base, videos, audios, containerISOBaseMedia, formatDuration, formatHasDuration, func(codec string) bool { return codec == "aac" })
	}
	return classifyVideo(base, videos, audios, containerISOBaseMedia, "h264", "aac", formatDuration, formatHasDuration)
}

func classifyVideo(base media.Inspection, videos, audios []probeStream, normalizedContainer container, videoCodec, audioCodec string, formatDuration time.Duration, formatHasDuration bool) (media.Inspection, normalizedFormat, ErrorCode) {
	if len(videos) != 1 || len(audios) > 1 || videos[0].CodecName != videoCodec || (len(audios) == 1 && audios[0].CodecName != audioCodec) {
		return media.Inspection{}, normalizedFormat{}, ErrorUnsupportedMedia
	}
	if videos[0].Width <= 0 || videos[0].Height <= 0 {
		return media.Inspection{}, normalizedFormat{}, ErrorInvalidProbe
	}
	if len(audios) == 1 && (audios[0].Width != 0 || audios[0].Height != 0) {
		return media.Inspection{}, normalizedFormat{}, ErrorInvalidProbe
	}
	durationSources := []json.RawMessage{videos[0].Duration}
	if len(audios) == 1 {
		durationSources = append(durationSources, audios[0].Duration)
	}
	duration, ok := normalizedDuration(formatDuration, formatHasDuration, durationSources...)
	if !ok {
		return media.Inspection{}, normalizedFormat{}, ErrorInvalidProbe
	}
	base.Kind = media.KindVideo
	base.Dimensions = media.Dimensions{Width: videos[0].Width, Height: videos[0].Height}
	base.Duration = duration
	normalized := normalizedFormat{container: normalizedContainer, videoCodec: videoCodec}
	if len(audios) == 1 {
		normalized.audioCodec = audioCodec
	}
	return base, normalized, ""
}

func normalizedDuration(formatDuration time.Duration, formatPresent bool, streamRaw ...json.RawMessage) (time.Duration, bool) {
	duration := formatDuration
	durationPresent := formatPresent
	for _, raw := range streamRaw {
		streamDuration, streamPresent, valid := parseDuration(raw)
		if !valid {
			return 0, false
		}
		if !streamPresent {
			continue
		}
		if durationPresent && duration != streamDuration {
			return 0, false
		}
		duration = streamDuration
		durationPresent = true
	}
	return duration, durationPresent
}

func parseDuration(raw json.RawMessage) (time.Duration, bool, bool) {
	if len(raw) == 0 {
		return 0, false, true
	}
	value := string(raw)
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &value); err != nil {
			return 0, true, false
		}
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 || seconds > float64(math.MaxInt64)/float64(time.Second) {
		return 0, true, false
	}
	nanoseconds := seconds * float64(time.Second)
	if math.IsNaN(nanoseconds) || math.IsInf(nanoseconds, 0) || nanoseconds > float64(math.MaxInt64) {
		return 0, true, false
	}
	duration := time.Duration(math.Round(nanoseconds))
	if duration <= 0 {
		return 0, true, false
	}
	return duration, true, true
}
