//go:build !unix

package main

import (
	"fmt"
	"log/slog"
)

type teamSignal int

const (
	teamSignalReload teamSignal = iota + 1
	teamSignalRestart
)

func watchTeamSignals(logger *slog.Logger, _ func(teamSignal)) func() {
	logger.Warn("team-mode SIGHUP/SIGUSR2 controls are not supported on this platform")
	return func() {}
}

func execRestart(_ string, _ []string, _ []string) error {
	return fmt.Errorf("exec restart is not supported on this platform")
}
