//go:build unix

package main

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

type teamSignal int

const (
	teamSignalReload teamSignal = iota + 1
	teamSignalRestart
)

func watchTeamSignals(logger *slog.Logger, handle func(teamSignal)) func() {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGHUP, syscall.SIGUSR2)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case sig := <-ch:
				switch sig {
				case syscall.SIGHUP:
					logger.Info("received SIGHUP, reloading config")
					handle(teamSignalReload)
				case syscall.SIGUSR2:
					logger.Info("received SIGUSR2, exec restarting")
					handle(teamSignalRestart)
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}

func execRestart(selfPath string, argv []string, env []string) error {
	return syscall.Exec(selfPath, argv, env)
}
