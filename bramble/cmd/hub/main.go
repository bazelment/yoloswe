// Command hub is the bramble cloud hub: it relays between a user's browser and
// connected bramble agent machines, serving an authenticated web UI to view and
// drive tmux agent sessions remotely. The hub holds no tmux logic — agents
// execute tmux behind the tmuxctl allowlist; the hub authenticates and forwards.
//
// Usage:
//
//	BRAMBLE_HUB_SECRET=<browser-secret> BRAMBLE_HUB_AGENT_TOKEN=<agent-token> \
//	  bramble-hub --addr :8787
//
// Agents connect to ws(s)://<host>/agent; users open http(s)://<host>/ and log
// in with the browser secret. Run behind TLS / a private network (Tailscale).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/bramble/hub"
)

func main() {
	var addr string
	root := &cobra.Command{
		Use:   "bramble-hub",
		Short: "Relay hub + web UI for remote bramble tmux sessions",
		RunE: func(_ *cobra.Command, _ []string) error {
			secret := os.Getenv("BRAMBLE_HUB_SECRET")
			if secret == "" {
				return errors.New("BRAMBLE_HUB_SECRET must be set (browser access secret)")
			}
			agentToken := os.Getenv("BRAMBLE_HUB_AGENT_TOKEN")

			h := hub.NewHub(agentToken, hub.NewAuthenticator(secret))
			srv := &http.Server{
				Addr:              addr,
				Handler:           h.Handler(),
				ReadHeaderTimeout: 10 * time.Second,
			}

			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(shutCtx)
			}()

			slog.Info("bramble hub listening", "addr", addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}
	root.Flags().StringVar(&addr, "addr", ":8787", "HTTP listen address")
	if err := root.Execute(); err != nil {
		slog.Error("hub exited", "err", err)
		os.Exit(1)
	}
}
