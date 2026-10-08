package media

import (
	"context"
	"io"
)

// Inspector accepts an untrusted media byte stream and returns only a fully
// validated, completely decoded inspection.
type Inspector interface {
	Inspect(context.Context, io.Reader) (Inspection, error)
}
