//go:build !windows

package main

import (
	"context"
	"github.com/bitskyai/chatkeeper/internal/web"
	"os"
	"os/signal"
	"syscall"
)

func watchReload(ctx context.Context, a *web.App) func() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-signals:
				_ = a.Manager.Reload()
			}
		}
	}()
	return func() { signal.Stop(signals) }
}
