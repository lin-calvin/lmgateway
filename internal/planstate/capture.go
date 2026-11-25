package planstate

import (
	"bytes"
	"io"
	"strings"
	"sync"
)

// Capture holds the latest snapshot observed on a stream. It is safe for the
// SSE reader goroutine and the usage collector to access concurrently.
type Capture struct {
	mu   sync.Mutex
	snap *Snapshot
}

func NewCapture() *Capture { return &Capture{} }

func (c *Capture) Set(s Snapshot) {
	if c == nil {
		return
	}
	c.mu.Lock()
	copy := s
	c.snap = &copy
	c.mu.Unlock()
}

func (c *Capture) Snapshot() *Snapshot {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snap == nil {
		return nil
	}
	copy := *c.snap
	return &copy
}

// CaptureSSE wraps an SSE body and records `codex.rate_limits` events. It
// passes bytes through unchanged; only complete event frames are inspected.
func CaptureSSE(body io.ReadCloser, capture *Capture) io.ReadCloser {
	if body == nil || capture == nil {
		return body
	}
	return &sseCaptureReader{r: body, capture: capture}
}

type sseCaptureReader struct {
	r       io.ReadCloser
	capture *Capture
	buf     []byte
	name    string
	data    []byte
}

func (r *sseCaptureReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.feed(p[:n])
	}
	return n, err
}

func (r *sseCaptureReader) Close() error { return r.r.Close() }

func (r *sseCaptureReader) feed(b []byte) {
	r.buf = append(r.buf, b...)
	for {
		idx := bytes.IndexByte(r.buf, '\n')
		if idx < 0 {
			return
		}
		line := strings.TrimSuffix(string(r.buf[:idx]), "\r")
		r.buf = r.buf[idx+1:]
		if line == "" {
			r.emit()
			continue
		}
		switch {
		case strings.HasPrefix(line, "event:"):
			r.name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			chunk := strings.TrimPrefix(line, "data:")
			chunk = strings.TrimPrefix(chunk, " ")
			if len(r.data) > 0 {
				r.data = append(r.data, '\n')
			}
			r.data = append(r.data, chunk...)
		}
	}
}

func (r *sseCaptureReader) emit() {
	if len(r.data) > 0 {
		if snap, ok := ParseCodexEvent(r.data); ok {
			r.capture.Set(*snap)
		}
	}
	r.name = ""
	r.data = nil
}
