package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func waitReason(t *testing.T, gone <-chan string, want string) {
	t.Helper()
	select {
	case got := <-gone:
		if got != want {
			t.Fatalf("reason %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("launcher loss %q not detected", want)
	}
}

func TestLauncherWatchInactiveWithoutLauncherEnv(t *testing.T) {
	w := launcherWatch{getenv: envOf(nil), stdin: strings.NewReader(""), getppid: func() int { return 1 }, interval: time.Millisecond}
	if w.start(context.Background()) != nil {
		t.Fatal("watch active outside launcher mode")
	}
}

func TestLauncherWatchStdinEOF(t *testing.T) {
	r, wr := io.Pipe()
	w := launcherWatch{getenv: envOf(map[string]string{launcherEnv: "menubar"}), stdin: r, getppid: os.Getppid, interval: time.Millisecond}
	gone := w.start(context.Background())
	select {
	case reason := <-gone:
		t.Fatal("early", reason)
	case <-time.After(50 * time.Millisecond):
	}
	_ = wr.Close()
	waitReason(t, gone, "launcher closed stdin")
}

func TestLauncherWatchParentPID(t *testing.T) {
	var ppid atomic.Int64
	ppid.Store(4242)
	r, _ := io.Pipe() // never closed: only the pid poll can fire
	w := launcherWatch{getenv: envOf(map[string]string{launcherEnv: "menubar", launcherPIDEnv: "4242"}), stdin: r, getppid: func() int { return int(ppid.Load()) }, interval: time.Millisecond}
	gone := w.start(context.Background())
	select {
	case reason := <-gone:
		t.Fatal("early", reason)
	case <-time.After(50 * time.Millisecond):
	}
	ppid.Store(1) // re-parented to launchd/init: the launcher died
	waitReason(t, gone, "launcher process exited")
}

func TestLauncherWatchIgnoresPIDThatIsNotParent(t *testing.T) {
	r, _ := io.Pipe()
	w := launcherWatch{getenv: envOf(map[string]string{launcherEnv: "menubar", launcherPIDEnv: "4242"}), stdin: r, getppid: func() int { return 7 }, interval: time.Millisecond}
	select {
	case reason := <-w.start(context.Background()):
		t.Fatal("pid watch fired for a non-parent launcher:", reason)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestLauncherHelperProcess(t *testing.T) {
	if os.Getenv("CHATKEEPER_LAUNCHER_HELPER") != "1" {
		return
	}
	if e := execute([]string{"run", "--mode", "desktop", "--no-browser", "--bind", "127.0.0.1:0"}, os.Stdout); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	os.Exit(0)
}

// A real `chatkeeper run` in launcher mode shuts down cleanly (instance.url
// removed, exit 0) when the launcher's end of its stdin pipe closes — what
// the kernel does when the menu-bar app crashes or is force-quit.
func TestLauncherModeServerExitsWhenLauncherGoes(t *testing.T) {
	home := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLauncherHelperProcess$")
	cmd.Env = append(os.Environ(), "CHATKEEPER_LAUNCHER_HELPER=1", "CHATKEEPER_HOME="+home, launcherEnv+"=test", launcherPIDEnv+"=", "SLACK_BOT_TOKEN=", "SLACK_APP_TOKEN=")
	stdin, e := cmd.StdinPipe()
	if e != nil {
		t.Fatal(e)
	}
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { _ = cmd.Process.Kill() }()
	started := make(chan bool, 1)
	go func() {
		s := bufio.NewScanner(stdout)
		for s.Scan() {
			if strings.HasPrefix(s.Text(), "Open ChatKeeper: ") {
				started <- true
			}
		}
		started <- false
	}()
	select {
	case ok := <-started:
		if !ok {
			t.Fatal("server exited before printing its launch URL")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("server did not start")
	}
	if _, e = os.Stat(filepath.Join(home, "instance.url")); e != nil {
		t.Fatal("instance.url missing while running:", e)
	}
	_ = stdin.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case e = <-done:
		if e != nil {
			t.Fatal("unclean exit:", e)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("server outlived its launcher")
	}
	if _, e = os.Stat(filepath.Join(home, "instance.url")); !os.IsNotExist(e) {
		t.Fatal("instance.url left behind:", e)
	}
}
