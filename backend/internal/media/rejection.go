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
	RejectionTypeMismatch        RejectionCode = "type_mismatch"
	RejectionDecodeFailed        RejectionCode = "decode_failed"
	RejectionResourcesExceeded   RejectionCode = "resources_exceeded"
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
	RejectionTypeMismatch:        "media type does not match its content",
	RejectionDecodeFailed:        "media could not be fully decoded",
	RejectionResourcesExceeded:   "media inspection exceeded its resource limits",
}

// Rejection is the bounded public error returned by the acceptance contract.
type Rejection struct {
	code RejectionCode
}

func newRejection(code RejectionCode) *Rejection {
	return &Rejection{code: code}
}

// Reject returns the rejection for one of the RejectionCode constants of this
// package, for inspection adapters that classify content-caused failures. An
// unknown code is a programming error and panics, so no rejection can carry
// an arbitrary string or an empty public message.
func Reject(code RejectionCode) *Rejection {
	if _, known := rejectionMessages[code]; !known {
		panic("media: unknown rejection code")
	}
	return newRejection(code)
}

// Code returns the stable machine-readable rejection category.
func (rejection *Rejection) Code() RejectionCode {
	return rejection.code
}

// Error returns only the fixed public-safe message for the rejection category.
func (rejection *Rejection) Error() string {
	return rejectionMessages[rejection.code]
}
