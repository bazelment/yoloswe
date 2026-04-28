package cliapp

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// notifyContext wires a context that:
//   - cancels on the first SIGINT or SIGTERM (graceful shutdown)
//   - calls forceExit on the second signal
//
// The returned cancel must be called to release resources (and to silence
// the signal goroutine) once the caller is done.
//
// forceExit is parameterized so tests can observe the second-signal path
// without the test binary actually exiting.
func notifyContext(parent context.Context, forceExit func()) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// done is closed by the returned cancel func to tell the goroutine to
	// stop watching for signals. We need a separate signal because ctx is
	// cancelled by the goroutine itself on the first signal — using
	// ctx.Done() to detect the "caller is finished" case would race.
	done := make(chan struct{})
	var closeOnce sync.Once
	wrappedCancel := func() {
		closeOnce.Do(func() { close(done) })
		cancel()
	}

	go func() {
		defer signal.Stop(sigCh)
		select {
		case <-done:
			return
		case <-parent.Done():
			return
		case <-sigCh:
			cancel()
		}
		// First signal received. Wait for a second signal to force exit,
		// or for the caller to finish cleanup (close done).
		select {
		case <-done:
			return
		case <-sigCh:
			forceExit()
		}
	}()

	return ctx, wrappedCancel
}
