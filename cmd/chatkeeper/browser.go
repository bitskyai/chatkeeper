package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/config"
	"github.com/bitskyai/chatkeeper/internal/web"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

func openBrowser(raw string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", raw)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", raw)
	default:
		cmd = exec.Command("xdg-open", raw)
	}
	if cmd.Start() == nil {
		go func() { _ = cmd.Wait() }()
	}
}
func runWeb(home, path, mode, bind, hosts string, noBrowser, dev bool, out io.Writer) error {
	host, port, e := net.SplitHostPort(bind)
	if e != nil {
		return errors.New("bind must be host:port")
	}
	mode = web.ConfiguredMode(mode, host)
	slog.Info("server configuration", "mode", mode, "bind", bind, "version", version, "persistent_logs", filepath.Join(home, "logs", "chatkeeper.log"))
	if mode == "desktop" && !config.Loopback(host) {
		return errors.New("desktop requires loopback bind")
	}
	if e = os.MkdirAll(home, 0700); e != nil {
		return errors.New("cannot create application home")
	}
	unlock, e := instanceLock(filepath.Join(home, "instance.lock"))
	if e != nil {
		if mode != "desktop" {
			return errors.New("application home already in use")
		}
		b, e := os.ReadFile(filepath.Join(home, "instance.url"))
		u, parseErr := url.Parse(string(b))
		if e != nil || parseErr != nil || u.Scheme != "http" || !config.Loopback(u.Hostname()) {
			return errors.New("existing instance is still starting; retry shortly")
		}
		if !noBrowser {
			openBrowser(u.String())
		}
		fmt.Fprintln(out, "ChatKeeper is already running; opened its browser UI")
		return nil
	}
	defer unlock()
	listener, e := net.Listen("tcp", bind)
	if e != nil && mode == "desktop" && port == "8787" {
		listener, e = net.Listen("tcp", net.JoinHostPort(host, "0"))
	}
	if e != nil {
		return errors.New("cannot bind application listener")
	}
	defer listener.Close()
	b := make([]byte, 32)
	if _, e = rand.Read(b); e != nil {
		return errors.New("random source unavailable")
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	if _, e = rand.Read(b); e != nil {
		return errors.New("random source unavailable")
	}
	code := base64.RawURLEncoding.EncodeToString(b)
	allowed := []string{}
	if hosts != "" {
		for _, v := range strings.Split(hosts, ",") {
			allowed = append(allowed, strings.TrimSpace(v))
		}
	}
	app, e := web.NewApp(web.Options{Home: home, Config: path, Mode: mode, Host: host, AllowedHosts: allowed, SetupCode: code, LaunchToken: token, AdminPassword: os.Getenv("CHATKEEPER_ADMIN_PASSWORD"), Dev: dev, SecureCookies: os.Getenv("CHATKEEPER_SECURE_COOKIES") == "1"})
	if e != nil {
		return e
	}
	defer app.Close()
	base := "http://" + listener.Addr().String()
	if mode == "desktop" {
		launch := app.LaunchURL(base)
		if e = config.AtomicWrite(filepath.Join(home, "instance.url"), []byte(launch), 0600); e != nil {
			return e
		}
		defer os.Remove(filepath.Join(home, "instance.url"))
		fmt.Fprintln(out, "Open ChatKeeper:", launch)
		if !noBrowser {
			openBrowser(launch)
		}
	} else {
		fmt.Fprintln(out, "ChatKeeper listening on", base)
		if _, e = os.Stat(filepath.Join(home, "state", "admin.hash")); os.IsNotExist(e) {
			if cfg, e := config.Load(path, false); e != nil || len(cfg.Workspaces) == 0 {
				fmt.Fprintln(out, "One-time setup code:", code, "— enter it at /bootstrap")
			}
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if gone := defaultLauncherWatch().start(ctx); gone != nil {
		// When the launcher dies it also takes the read ends of our stdout
		// and stderr pipes; a write there must fail with EPIPE, not kill us
		// with SIGPIPE before the graceful shutdown removes instance.url.
		signal.Ignore(syscall.SIGPIPE)
		go func() {
			select {
			case reason := <-gone:
				fmt.Fprintln(os.Stderr, "chatkeeper: "+reason+"; shutting down")
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	go app.Watch(ctx)
	stopReload := watchReload(ctx, app)
	defer stopReload()
	server := &http.Server{Handler: app, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second}
	errs := make(chan error, 1)
	go func() { errs <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
	case e = <-errs:
		if !errors.Is(e, http.ErrServerClosed) {
			return errors.New("application listener failed")
		}
	}
	cancel()
	shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	return server.Shutdown(shutdown)
}
