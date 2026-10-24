package outbox

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// KindMediaUsage is the stored kind of a media usage record. Such a record
// has no canvas revision: it reports the media accounting of its canvas
// after a committed media command or reservation expiry.
const KindMediaUsage = "media_usage"

// MediaUsageObjectID is the object ID of usage commands. Object IDs are 32
// hex characters, so it never names an object.
const MediaUsageObjectID = "media_usage"

// MediaUsage is the aggregate media accounting of a canvas right after a
// committed change. Version increases by one with every change of the
// canvas usage, so a consumer drops a stale or duplicated usage. It never
// names an asset, a reference, or the size of one file.
type MediaUsage struct {
	Version       int64 `json:"version"`
	QuotaBytes    int64 `json:"quota_bytes"`
	UsedBytes     int64 `json:"used_bytes"`
	ReservedBytes int64 `json:"reserved_bytes"`
}

func (u MediaUsage) validate() error {
	if u.Version < 1 || u.QuotaBytes < 0 || u.UsedBytes < 0 || u.ReservedBytes < 0 || u.UsedBytes > u.QuotaBytes-u.ReservedBytes {
		return fmt.Errorf("%w: invalid media usage %+v", ErrMalformed, u)
	}
	return nil
}

// EncodeMediaUsage returns the stored JSON payload of a media usage.
func EncodeMediaUsage(usage MediaUsage) ([]byte, error) {
	if err := usage.validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(usage)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return payload, nil
}

// DecodeMediaUsage rebuilds a media usage from its stored payload, which
// must hold exactly the usage fields.
func DecodeMediaUsage(payload []byte) (MediaUsage, error) {
	var usage MediaUsage
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&usage); err != nil {
		return MediaUsage{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if decoder.More() {
		return MediaUsage{}, fmt.Errorf("%w: trailing data", ErrMalformed)
	}
	if err := usage.validate(); err != nil {
		return MediaUsage{}, err
	}
	return usage, nil
}
