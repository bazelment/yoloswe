// Package ndjson provides utilities for reading and writing newline-delimited JSON.
package ndjson

import (
	"bufio"
	"encoding/json"
	"io"
	"sync"
)

// Reader reads newline-delimited JSON from an io.Reader.
type Reader struct {
	scanner *bufio.Scanner
}

// NewReader creates a new NDJSON reader.
func NewReader(r io.Reader) *Reader {
	scanner := bufio.NewScanner(r)
	// Set a larger buffer for potentially large JSON messages
	const maxTokenSize = 1024 * 1024 // 1MB
	scanner.Buffer(make([]byte, 64*1024), maxTokenSize)
	return &Reader{scanner: scanner}
}

// ReadLine reads the next JSON line as raw bytes.
// Returns io.EOF when there are no more lines.
func (r *Reader) ReadLine() ([]byte, error) {
	if r.scanner.Scan() {
		// Return a copy of the bytes since Scanner reuses the buffer
		line := r.scanner.Bytes()
		result := make([]byte, len(line))
		copy(result, line)
		return result, nil
	}
	if err := r.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

// Writer writes newline-delimited JSON to an io.Writer.
type Writer struct {
	w  io.Writer
	mu sync.Mutex
}

// NewWriter creates a new NDJSON writer.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w}
}

// Write writes a value as a JSON line.
func (w *Writer) Write(v interface{}) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	data, err := json.Marshal(v)
	if err != nil {
		return err
	}

	// Write JSON followed by newline
	if _, err := w.w.Write(data); err != nil {
		return err
	}
	if _, err := w.w.Write([]byte("\n")); err != nil {
		return err
	}

	return nil
}

// WriteRaw writes raw bytes as a line.
func (w *Writer) WriteRaw(data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if _, err := w.w.Write(data); err != nil {
		return err
	}
	if _, err := w.w.Write([]byte("\n")); err != nil {
		return err
	}

	return nil
}
