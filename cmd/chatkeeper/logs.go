package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/logging"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func applicationHome() (string, error) {
	if home := os.Getenv("CHATKEEPER_HOME"); home != "" {
		return home, nil
	}
	dir, e := os.UserConfigDir()
	return filepath.Join(dir, "chatkeeper"), e
}
func logsCommand(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("logs", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	follow := flags.Bool("follow", false, "follow active log, including rotations")
	filter := flags.String("filter", "", "case-insensitive text filter")
	if e := flags.Parse(args); e != nil {
		return e
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("invalid logs arguments")
	}
	home, e := applicationHome()
	if e != nil {
		return e
	}
	lines, e := logging.Tail(home, *filter, 200)
	if e != nil {
		return e
	}
	for _, line := range lines {
		fmt.Fprintln(out, line)
	}
	if !*follow {
		return nil
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// Reopen on each tick so size rotation never leaves the follower on an old inode.
	previous := lines
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			lines, e = logging.Tail(home, *filter, 2000)
			if e != nil {
				return e
			}
			overlap := 0
			for n := min(len(previous), len(lines)); n > 0; n-- {
				same := true
				for i := 0; i < n; i++ {
					if previous[len(previous)-n+i] != lines[i] {
						same = false
						break
					}
				}
				if same {
					overlap = n
					break
				}
			}
			for _, line := range lines[overlap:] {
				if _, e = fmt.Fprintln(out, line); e != nil {
					return e
				}
			}
			previous = lines
		}
	}
}
