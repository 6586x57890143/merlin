//go:build !windows

package config

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// Watch reloads the config whenever the process receives SIGHUP, logging
// and keeping the previous config on any reload failure so a bad edit never
// takes the bot down mid-run.
func (l *Loader) Watch(ctx context.Context) {
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	go func() {
		defer signal.Stop(sighup)
		for {
			select {
			case <-ctx.Done():
				return
			case <-sighup:
				if err := l.reload(); err != nil {
					l.log.Error("config reload failed, keeping previous config", "err", err)
					continue
				}
				l.log.Info("config reloaded via SIGHUP")
			}
		}
	}()
}
