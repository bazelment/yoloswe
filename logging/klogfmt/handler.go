// Package klogfmt provides a slog.Handler that formats output in klog style:
//
//	I0404 12:34:56.789012   12345 handler.go:42] order placed order_id="abc" latency_ms=200
//
// Format: {severity_char}{MMDD} {HH:MM:SS.microseconds} {PID} {file}:{line}] {message} {key=value}...
package klogfmt

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Handler formats slog output to match klog's format.
type Handler struct {
	w      io.Writer
	mu     *sync.Mutex
	level  slog.Leveler
	attrs  []slog.Attr
	groups []string
	pid    int
}

// New creates a klogfmt Handler writing to the given writer.
func New(w io.Writer, opts ...Option) *Handler {
	o := options{level: slog.LevelInfo}
	for _, opt := range opts {
		opt(&o)
	}
	return &Handler{
		w:     w,
		mu:    &sync.Mutex{},
		level: o.level,
		pid:   os.Getpid(),
	}
}

func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	buf := make([]byte, 0, 256)

	// Severity letter.
	buf = append(buf, severityChar(r.Level))

	// Timestamp: MMDD HH:MM:SS.microseconds
	t := r.Time
	if t.IsZero() {
		t = time.Now()
	}
	buf = fmt.Appendf(buf, "%02d%02d %02d:%02d:%02d.%06d",
		t.Month(), t.Day(),
		t.Hour(), t.Minute(), t.Second(), t.Nanosecond()/1000)

	// PID, right-justified in 7 chars (matches klog).
	buf = fmt.Appendf(buf, " %7d", h.pid)

	// Source file:line.
	if r.PC != 0 {
		fs := runtime.CallersFrames([]uintptr{r.PC})
		f, _ := fs.Next()
		buf = fmt.Appendf(buf, " %s:%d]", filepath.Base(f.File), f.Line)
	} else {
		buf = append(buf, " ???:0]"...)
	}

	// Message.
	buf = fmt.Appendf(buf, " %s", r.Message)

	// Pre-attached attrs (from WithAttrs).
	for _, a := range h.attrs {
		buf = appendAttr(buf, h.groups, a)
	}

	// Per-record attrs.
	r.Attrs(func(a slog.Attr) bool {
		buf = appendAttr(buf, h.groups, a)
		return true
	})

	buf = append(buf, '\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.w.Write(buf)
	return err
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{
		w:      h.w,
		mu:     h.mu,
		level:  h.level,
		attrs:  append(cloneSlice(h.attrs), attrs...),
		groups: h.groups,
		pid:    h.pid,
	}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{
		w:      h.w,
		mu:     h.mu,
		level:  h.level,
		attrs:  cloneSlice(h.attrs),
		groups: append(cloneSlice(h.groups), name),
		pid:    h.pid,
	}
}

// severityChar maps slog levels to klog's single-char prefix.
func severityChar(l slog.Level) byte {
	switch {
	case l >= slog.LevelError:
		return 'E'
	case l >= slog.LevelWarn:
		return 'W'
	case l >= slog.LevelInfo:
		return 'I'
	default:
		return 'D'
	}
}

// appendAttr formats a single key=value pair, quoting if needed.
func appendAttr(buf []byte, groups []string, a slog.Attr) []byte {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return buf
	}

	buf = append(buf, ' ')

	// Prefix with group names: group.subgroup.key
	for _, g := range groups {
		buf = append(buf, g...)
		buf = append(buf, '.')
	}

	buf = append(buf, a.Key...)
	buf = append(buf, '=')

	val := a.Value.String()

	needsQuote := false
	for _, c := range val {
		if c == ' ' || c == '"' || c == '\\' {
			needsQuote = true
			break
		}
	}
	if needsQuote {
		buf = fmt.Appendf(buf, "%q", val)
	} else {
		buf = append(buf, val...)
	}

	return buf
}

func cloneSlice[S ~[]E, E any](s S) S {
	if s == nil {
		return nil
	}
	c := make(S, len(s))
	copy(c, s)
	return c
}
