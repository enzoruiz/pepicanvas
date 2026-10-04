package media

// RejectionCode is a closed, public-safe reason category. Codes and messages
// never include input paths, tool output, codecs, raw metadata, or media content.
type RejectionCode string

const (
	RejectionInvalidLimits       RejectionCode = "invalid_limits"
	RejectionUnsupportedKind     RejectionCode = "unsupported_kind"
	RejectionInvalidMetadata     RejectionCode = "invalid_metadata"
	RejectionInputTooLarge       RejectionCode = "input_too_large"
	RejectionDimensionsExceeded  RejectionCode = "dimensions_exceeded"
	RejectionDurationExceeded    RejectionCode = "duration_exceeded"
	RejectionFrameCountExceeded  RejectionCode = "frame_count_exceeded"
	RejectionStreamCountExceeded RejectionCode = "stream_count_exceeded"
)

var rejectionMessages = map[RejectionCode]string{
	RejectionInvalidLimits:       "media limits are invalid",
	RejectionUnsupportedKind:     "media kind is unsupported",
	RejectionInvalidMetadata:     "media metadata is invalid",
	RejectionInputTooLarge:       "media input exceeds the allowed size",
	RejectionDimensionsExceeded:  "media dimensions exceed the allowed limit",
	RejectionDurationExceeded:    "media duration exceeds the allowed limit",
	RejectionFrameCountExceeded:  "media frame count exceeds the allowed limit",
	RejectionStreamCountExceeded: "media stream count exceeds the allowed limit",
}

// Rejection is the bounded public error returned by the acceptance contract.
type Rejection struct {
	code RejectionCode
}

func newRejection(code RejectionCode) *Rejection {
	return &Rejection{code: code}
}

// Code returns the stable machine-readable rejection category.
func (rejection *Rejection) Code() RejectionCode {
	return rejection.code
}

// Error returns only the fixed public-safe message for the rejection category.
func (rejection *Rejection) Error() string {
	return rejectionMessages[rejection.code]
}
