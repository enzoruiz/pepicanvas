package media

import "time"

const mebibyte int64 = 1024 * 1024

// Limits defines configurable acceptance and inspection resource ceilings.
// DefaultLimits returns the secure MVP defaults; callers may provide stricter
// positive values for a deployment or operation.
type Limits struct {
	MaxImageBytes          int64
	MaxGIFBytes            int64
	MaxAudioBytes          int64
	MaxVideoBytes          int64
	MaxWidth               int
	MaxHeight              int
	MaxPixels              int64
	MaxGIFDuration         time.Duration
	MaxAudioDuration       time.Duration
	MaxVideoDuration       time.Duration
	MaxGIFFrames           int
	MaxStreams             int
	InspectionTimeout      time.Duration
	MaxCapturedOutputBytes int64
	MaxTasks               int // Linux cgroup processes and threads.
	MaxMemoryBytes         int64
	MaxCPUTime             time.Duration
}

// DefaultLimits returns the provisional secure defaults for the MVP. These
// values are configurable safeguards, not universal definitions of valid media.
func DefaultLimits() Limits {
	return Limits{
		MaxImageBytes:          25 * mebibyte,
		MaxGIFBytes:            25 * mebibyte,
		MaxAudioBytes:          50 * mebibyte,
		MaxVideoBytes:          250 * mebibyte,
		MaxWidth:               3840,
		MaxHeight:              3840,
		MaxPixels:              8_294_400,
		MaxGIFDuration:         120 * time.Second,
		MaxAudioDuration:       600 * time.Second,
		MaxVideoDuration:       600 * time.Second,
		MaxGIFFrames:           3600,
		MaxStreams:             8,
		InspectionTimeout:      30 * time.Second,
		MaxCapturedOutputBytes: mebibyte,
		MaxTasks:               64,
		MaxMemoryBytes:         512 * mebibyte,
		MaxCPUTime:             30 * time.Second,
	}
}

// Validate rejects incomplete, zero, or negative limit configurations.
func (limits Limits) Validate() error {
	if limits.MaxImageBytes <= 0 || limits.MaxGIFBytes <= 0 ||
		limits.MaxAudioBytes <= 0 || limits.MaxVideoBytes <= 0 ||
		limits.MaxWidth <= 0 || limits.MaxHeight <= 0 || limits.MaxPixels <= 0 ||
		limits.MaxGIFDuration <= 0 || limits.MaxAudioDuration <= 0 || limits.MaxVideoDuration <= 0 ||
		limits.MaxGIFFrames <= 0 || limits.MaxStreams <= 0 ||
		limits.InspectionTimeout <= 0 || limits.MaxCapturedOutputBytes <= 0 ||
		limits.MaxTasks <= 0 || limits.MaxMemoryBytes <= 0 || limits.MaxCPUTime <= 0 {
		return newRejection(RejectionInvalidLimits)
	}
	return nil
}

func (limits Limits) maxInputBytes(kind Kind) int64 {
	switch kind {
	case KindImage:
		return limits.MaxImageBytes
	case KindGIF:
		return limits.MaxGIFBytes
	case KindAudio:
		return limits.MaxAudioBytes
	case KindVideo:
		return limits.MaxVideoBytes
	default:
		return 0
	}
}

func (limits Limits) maxDuration(kind Kind) time.Duration {
	switch kind {
	case KindGIF:
		return limits.MaxGIFDuration
	case KindAudio:
		return limits.MaxAudioDuration
	case KindVideo:
		return limits.MaxVideoDuration
	default:
		return 0
	}
}
