//go:build unix

package main

import (
	"os"
	"syscall"
)

// openSpanFile opens path for reading without blocking, so a FIFO or device
// reaches the regular-file check instead of waiting for a writer.
func openSpanFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
