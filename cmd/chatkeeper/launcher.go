package main

import (
	"context"
	"io"
	"os"
	"strconv"
	"time"
)

// Launcher mode: a desktop launcher (the macOS menu-bar app) that starts
// `chatkeeper run` sets CHATKEEPER_LAUNCHER and keeps the write end of a pipe
// on our stdin. The server must not outlive its launcher, even when the
// launcher crashes or is force-quit and cannot send SIGTERM:
//
//   - stdin EOF: the kernel closes the launcher's write end when it dies;
//   - parent pid: CHATKEEPER_LAUNCHER_PID names the launcher; when our parent
//     pid changes (we were re-parented to launchd/init), it is gone.
//
// Either signal triggers the normal graceful shutdown.
const (
	launcherEnv    = "CHATKEEPER_LAUNCHER"
	launcherPIDEnv = "CHATKEEPER_LAUNCHER_PID"
)

type launcherWatch struct {
	getenv   func(string) string
	stdin    io.Reader
	getppid  func() int
	interval time.Duration
}

func defaultLauncherWatch() launcherWatch {
	return launcherWatch{getenv: os.Getenv, stdin: os.Stdin, getppid: os.Getppid, interval: time.Second}
}

// start returns nil outside launcher mode. Otherwise the returned channel
// receives one reason when the launcher is gone; it stops watching when ctx
// ends (a blocked stdin read is abandoned, which is harmless at exit).
func (w launcherWatch) start(ctx context.Context) <-chan string {
	if w.getenv(launcherEnv) == "" {
		return nil
	}
	gone := make(chan string, 2)
	if w.stdin != nil {
		go func() {
			// The launcher never writes; any read result other than data
			// (EOF or error) means its end of the pipe is closed.
			buf := make([]byte, 512)
			for {
				if _, e := w.stdin.Read(buf); e != nil {
					gone <- "launcher closed stdin"
					return
				}
			}
		}()
	}
	// Poll only when the named launcher really is our parent; otherwise (a
	// wrapper in between) stdin EOF is the signal.
	if want, e := strconv.Atoi(w.getenv(launcherPIDEnv)); e == nil && want > 1 && w.getppid() == want {
		go func() {
			t := time.NewTicker(w.interval)
			defer t.Stop()
			for {
				if w.getppid() != want {
					gone <- "launcher process exited"
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	}
	return gone
}
