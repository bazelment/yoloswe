package klogfmt

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSeverityChars(t *testing.T) {
	tests := []struct {
		level slog.Level
		want  byte
	}{
		{slog.LevelDebug - 4, 'D'},
		{slog.LevelDebug, 'D'},
		{slog.LevelInfo, 'I'},
		{slog.LevelWarn, 'W'},
		{slog.LevelError, 'E'},
		{slog.LevelError + 4, 'E'},
	}
	for _, tt := range tests {
		got := severityChar(tt.level)
		if got != tt.want {
			t.Errorf("severityChar(%d) = %c, want %c", tt.level, got, tt.want)
		}
	}
}

func TestFormat(t *testing.T) {
	var buf bytes.Buffer
	h := New(&buf, WithLevel(slog.LevelDebug))
	// Override PID for deterministic output.
	h.pid = 12345

	logger := slog.New(h)

	ts := time.Date(2024, 4, 4, 12, 34, 56, 789012000, time.UTC)
	r := slog.NewRecord(ts, slog.LevelInfo, "order placed", 0)
	r.AddAttrs(slog.String("order_id", "abc-123"), slog.Int("latency_ms", 42))

	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatal(err)
	}

	got := buf.String()
	// Without PC, source is ???:0.
	want := "I0404 12:34:56.789012   12345 ???:0] order placed order_id=abc-123 latency_ms=42\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	// Test via logger (has PC info).
	buf.Reset()
	logger.Info("hello", "key", "value")
	line := buf.String()
	// Should start with I, have source file info.
	if !strings.HasPrefix(line, "I") {
		t.Errorf("expected I prefix, got: %s", line)
	}
	if !strings.Contains(line, "handler_test.go:") {
		t.Errorf("expected handler_test.go source, got: %s", line)
	}
	if !strings.Contains(line, "hello key=value") {
		t.Errorf("expected message and attrs, got: %s", line)
	}
}

func TestQuoting(t *testing.T) {
	var buf bytes.Buffer
	h := New(&buf, WithLevel(slog.LevelDebug))
	h.pid = 1

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
	r.AddAttrs(
		slog.String("simple", "abc"),
		slog.String("spaces", "hello world"),
		slog.String("quotes", `say "hi"`),
	)
	h.Handle(context.Background(), r)

	got := buf.String()
	if !strings.Contains(got, "simple=abc") {
		t.Errorf("simple value should not be quoted: %s", got)
	}
	if !strings.Contains(got, `spaces="hello world"`) {
		t.Errorf("value with spaces should be quoted: %s", got)
	}
	if !strings.Contains(got, `quotes="say \"hi\""`) {
		t.Errorf("value with quotes should be escaped: %s", got)
	}
}

func TestWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	h := New(&buf, WithLevel(slog.LevelDebug))
	h.pid = 1

	h2 := h.WithAttrs([]slog.Attr{slog.String("req_id", "r1")})

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "test", 0)
	r.AddAttrs(slog.String("key", "val"))
	h2.Handle(context.Background(), r)

	got := buf.String()
	if !strings.Contains(got, "req_id=r1 key=val") {
		t.Errorf("expected pre-attached attrs before record attrs: %s", got)
	}
}

func TestWithGroup(t *testing.T) {
	var buf bytes.Buffer
	h := New(&buf, WithLevel(slog.LevelDebug))
	h.pid = 1

	h2 := h.WithGroup("http")

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "request", 0)
	r.AddAttrs(slog.String("method", "GET"))
	h2.Handle(context.Background(), r)

	got := buf.String()
	if !strings.Contains(got, "http.method=GET") {
		t.Errorf("expected group prefix: %s", got)
	}
}

func TestEnabled(t *testing.T) {
	h := New(&bytes.Buffer{}, WithLevel(slog.LevelWarn))
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Info should not be enabled when level is Warn")
	}
	if !h.Enabled(context.Background(), slog.LevelWarn) {
		t.Error("Warn should be enabled when level is Warn")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("Error should be enabled when level is Warn")
	}
}

func TestTimestampFormat(t *testing.T) {
	var buf bytes.Buffer
	h := New(&buf, WithLevel(slog.LevelDebug))
	h.pid = 1

	ts := time.Date(2024, 12, 25, 1, 2, 3, 456789000, time.UTC)
	r := slog.NewRecord(ts, slog.LevelInfo, "msg", 0)
	h.Handle(context.Background(), r)

	got := buf.String()
	// Expected: I1225 01:02:03.456789
	re := regexp.MustCompile(`^I1225 01:02:03\.456789`)
	if !re.MatchString(got) {
		t.Errorf("timestamp format mismatch: %s", got)
	}
}

func TestConcurrentWrites(t *testing.T) {
	var buf bytes.Buffer
	h := New(&buf, WithLevel(slog.LevelDebug))
	logger := slog.New(h)

	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			logger.Info("concurrent", "n", n)
		}(i)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 100 {
		t.Errorf("expected 100 lines, got %d", len(lines))
	}
}

func TestInit(t *testing.T) {
	// Just verify Init doesn't panic.
	Init(WithLevel(slog.LevelWarn))
	// Restore default after test.
	defer slog.SetDefault(slog.Default())
}
