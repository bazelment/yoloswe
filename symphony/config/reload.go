package config

import (
	"log/slog"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// Reloader watches WORKFLOW.md for changes and maintains the last-known-good config.
// Returns immutable *ServiceConfig snapshots. Spec Section 6.2.
type Reloader struct {
	current *ServiceConfig
	watcher *fsnotify.Watcher
	done    chan struct{}
	logger  *slog.Logger
	path    string
	mu      sync.RWMutex
}

// NewReloader creates a new config reloader for the given workflow path.
// It performs the initial load and starts watching for changes.
func NewReloader(path string, logger *slog.Logger) (*Reloader, error) {
	cfg, err := loadConfig(path)
	if err != nil {
		return nil, err
	}

	r := &Reloader{
		path:    path,
		current: cfg,
		done:    make(chan struct{}),
		logger:  logger,
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	r.watcher = watcher

	if err := watcher.Add(path); err != nil {
		watcher.Close()
		return nil, err
	}

	go r.watchLoop()
	return r, nil
}

// Config returns the current immutable config snapshot.
func (r *Reloader) Config() *ServiceConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.current
}

// Close stops watching and releases resources.
func (r *Reloader) Close() error {
	close(r.done)
	return r.watcher.Close()
}

func (r *Reloader) watchLoop() {
	for {
		select {
		case event, ok := <-r.watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				r.reload()
			}
		case err, ok := <-r.watcher.Errors:
			if !ok {
				return
			}
			r.logger.Error("workflow watcher error", "error", err)
		case <-r.done:
			return
		}
	}
}

func (r *Reloader) reload() {
	cfg, err := loadConfig(r.path)
	if err != nil {
		// Invalid reload: keep last-known-good, log error. Spec Section 6.2.
		r.logger.Error("workflow reload failed, keeping last-known-good config", "error", err)
		return
	}

	r.mu.Lock()
	r.current = cfg
	r.mu.Unlock()

	r.logger.Info("workflow config reloaded", "path", r.path)
}

func loadConfig(path string) (*ServiceConfig, error) {
	wf, err := LoadWorkflow(path)
	if err != nil {
		return nil, err
	}
	return NewServiceConfig(wf), nil
}
