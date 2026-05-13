package ndjson

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReaderReadLine(t *testing.T) {
	t.Parallel()

	reader := NewReader(strings.NewReader("first\nsecond\n"))

	first, err := reader.ReadLine()
	if err != nil {
		t.Fatalf("ReadLine() first error = %v", err)
	}
	second, err := reader.ReadLine()
	if err != nil {
		t.Fatalf("ReadLine() second error = %v", err)
	}

	if string(first) != "first" {
		t.Fatalf("first line = %q, want %q", first, "first")
	}
	if string(second) != "second" {
		t.Fatalf("second line = %q, want %q", second, "second")
	}

	first[0] = 'F'
	if string(second) != "second" {
		t.Fatalf("mutating first line changed second line to %q", second)
	}

	_, err = reader.ReadLine()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("ReadLine() final error = %v, want io.EOF", err)
	}
}

func TestReaderAcceptsLargeLine(t *testing.T) {
	t.Parallel()

	want := strings.Repeat("x", 70*1024)
	reader := NewReader(strings.NewReader(want + "\n"))

	got, err := reader.ReadLine()
	if err != nil {
		t.Fatalf("ReadLine() error = %v", err)
	}
	if string(got) != want {
		t.Fatalf("ReadLine() length = %d, want %d", len(got), len(want))
	}
}

func TestWriterWrite(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	writer := NewWriter(&buf)

	err := writer.Write(struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}{
		Name:  "build",
		Count: 2,
	})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	want := "{\"name\":\"build\",\"count\":2}\n"
	if got := buf.String(); got != want {
		t.Fatalf("Write() output = %q, want %q", got, want)
	}
}

func TestWriterWriteRaw(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	writer := NewWriter(&buf)

	if err := writer.WriteRaw([]byte(`{"raw":true}`)); err != nil {
		t.Fatalf("WriteRaw() error = %v", err)
	}

	want := "{\"raw\":true}\n"
	if got := buf.String(); got != want {
		t.Fatalf("WriteRaw() output = %q, want %q", got, want)
	}
}

func TestWriterErrors(t *testing.T) {
	t.Parallel()

	t.Run("marshal", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		err := NewWriter(&buf).Write(func() {})
		if err == nil {
			t.Fatal("Write() error = nil, want marshal error")
		}
		if buf.Len() != 0 {
			t.Fatalf("buffer length after marshal error = %d, want 0", buf.Len())
		}
	})

	t.Run("write", func(t *testing.T) {
		t.Parallel()

		wantErr := errors.New("write failed")
		err := NewWriter(errorWriter{err: wantErr}).WriteRaw([]byte("line"))
		if !errors.Is(err, wantErr) {
			t.Fatalf("WriteRaw() error = %v, want %v", err, wantErr)
		}
	})
}

type errorWriter struct {
	err error
}

func (w errorWriter) Write([]byte) (int, error) {
	return 0, w.err
}
