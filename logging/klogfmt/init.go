package klogfmt

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

type options struct {
	level  slog.Leveler
	writer io.Writer // extra writer for multi-writer setup
}

// Option configures a Handler.
type Option func(*options)

// WithLevel sets the minimum log level.
func WithLevel(l slog.Leveler) Option {
	return func(o *options) { o.level = l }
}

// WithWriter adds an extra writer that receives log output alongside stderr.
func WithWriter(w io.Writer) Option {
	return func(o *options) { o.writer = w }
}

// Init sets slog.SetDefault with a klogfmt handler writing to stderr
// (and any extra writer added via WithWriter).
func Init(opts ...Option) {
	o := options{}
	for _, opt := range opts {
		opt(&o)
	}
	var w io.Writer = os.Stderr
	if o.writer != nil {
		w = io.MultiWriter(os.Stderr, o.writer)
	}
	var handlerOpts []Option
	if o.level != nil {
		handlerOpts = append(handlerOpts, WithLevel(o.level))
	}
	slog.SetDefault(slog.New(New(w, handlerOpts...)))
}

// InitWithLogFile sets up dual-write: stderr + a log file at logPath.
// It creates parent directories as needed. Returns a closer for the log file.
func InitWithLogFile(logPath string, opts ...Option) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	Init(append(opts, WithWriter(f))...)
	return f.Close, nil
}
