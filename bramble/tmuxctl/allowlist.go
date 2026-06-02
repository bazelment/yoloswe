// Package tmuxctl is the tmux control plane for bramble: a single executor that
// owns the full read+write tmux command vocabulary behind a command allowlist.
//
// Reads delegate to the primitives in bramble/session (capture/parse). Writes
// (send-keys, paste-buffer, special keys, window lifecycle) are new and are the
// sanctioned way to drive an interactive agent CLI running in a tmux pane —
// e.g. delivering a follow-up prompt, which the interactive tmux runner itself
// (session.tmuxRunner.RunTurn) deliberately does not do.
//
// The allowlist is defense-in-depth: once a control plane is reachable over the
// network (via the hub), the tmux subcommand is attacker-influenceable, so every
// mutating call routes through one chokepoint that refuses anything not listed.
package tmuxctl

import "fmt"

// allowedOps is the set of tmux subcommands tmuxctl is permitted to run. Mirrors
// tmux-mobile's allowlist: read + send + lifecycle, but never destructive
// server-wide commands (kill-server, kill-session) or arbitrary shell.
var allowedOps = map[string]struct{}{
	"list-sessions":   {},
	"list-windows":    {},
	"list-panes":      {},
	"capture-pane":    {},
	"display-message": {},
	"send-keys":       {},
	"set-buffer":      {},
	"paste-buffer":    {},
	"select-window":   {},
	"new-window":      {},
	"rename-window":   {},
	"kill-window":     {},
}

// checkOp returns an error if op is not in the allowlist.
func checkOp(op string) error {
	if _, ok := allowedOps[op]; !ok {
		return fmt.Errorf("tmuxctl: tmux subcommand %q is not allowed", op)
	}
	return nil
}
