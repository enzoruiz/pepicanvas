//go:build !unix

package main

import "os"

// openSpanFile opens path for reading.
func openSpanFile(path string) (*os.File, error) {
	return os.Open(path)
}
