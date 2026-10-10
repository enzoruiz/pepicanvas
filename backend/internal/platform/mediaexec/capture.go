package mediaexec

import "sync"

type captureBudget struct {
	mu        sync.Mutex
	remaining int64
	overflow  bool
}

type commandCapture struct {
	budget *captureBudget
	mu     sync.Mutex
	data   []byte
}

type captureWriter struct {
	capture *commandCapture
	stdout  bool
}

func newCaptureBudget(limit int64) *captureBudget {
	return &captureBudget{remaining: limit}
}

func (budget *captureBudget) command() *commandCapture {
	return &commandCapture{budget: budget}
}

func (capture *commandCapture) stdout() *captureWriter {
	return &captureWriter{capture: capture, stdout: true}
}

func (capture *commandCapture) stderr() *captureWriter {
	return &captureWriter{capture: capture}
}

func (writer *captureWriter) Write(data []byte) (int, error) {
	written := len(data)
	writer.capture.budget.mu.Lock()
	allowed := int64(len(data))
	if allowed > writer.capture.budget.remaining {
		allowed = writer.capture.budget.remaining
		writer.capture.budget.overflow = true
	}
	writer.capture.budget.remaining -= allowed
	if writer.stdout && allowed > 0 {
		writer.capture.mu.Lock()
		writer.capture.data = append(writer.capture.data, data[:int(allowed)]...)
		writer.capture.mu.Unlock()
	}
	writer.capture.budget.mu.Unlock()
	return written, nil
}

func (capture *commandCapture) output() []byte {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return append([]byte(nil), capture.data...)
}

func (budget *captureBudget) overflowed() bool {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	return budget.overflow
}
