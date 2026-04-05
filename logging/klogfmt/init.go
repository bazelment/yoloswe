package klogfmt

import (
	"log/slog"
	"os"
)

type options struct {
	level slog.Leveler
}

// Option configures a Handler.
type Option func(*options)

// WithLevel sets the minimum log level.
func WithLevel(l slog.Leveler) Option {
	return func(o *options) { o.level = l }
}

// Init sets slog.SetDefault with a klogfmt handler writing to stderr.
func Init(opts ...Option) {
	slog.SetDefault(slog.New(New(os.Stderr, opts...)))
}
