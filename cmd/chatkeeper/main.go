// Command chatkeeper runs read-only capture and archive verification.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/config"
	"github.com/bitskyai/chatkeeper/internal/core"
	"github.com/bitskyai/chatkeeper/internal/logging"
	archiveMCP "github.com/bitskyai/chatkeeper/internal/mcp"
	_ "github.com/bitskyai/chatkeeper/internal/plugins/all"
	"github.com/bitskyai/chatkeeper/internal/search"
	"github.com/bitskyai/chatkeeper/internal/secrets"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/internal/web"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var backfillHTTP = &http.Client{Timeout: 60 * time.Second}

var version = "dev"
var commit = "unknown"
var date = "unknown"

func main() {
	if e := execute(os.Args[1:], os.Stdout); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func execute(args []string, out io.Writer) error {
	if len(args) > 0 && args[0] == "help" {
		printUsage(out)
		return nil
	}
	command := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}
	if command == "setup" {
		return setupCommand(appHome(), args, out)
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		printUsage(out)
		return nil
	}
	if command == "logs" {
		return logsCommand(args, out)
	}
	if command == "cleanup" {
		return cleanupCommand(args, out)
	}

	if command == "healthcheck" {
		return healthcheck(args)
	}
	if command == "version" {
		if len(args) != 0 {
			return errors.New("invalid command arguments")
		}
		fmt.Fprintf(out, "chatkeeper %s (%s, %s)\nplugins: %d\n", version, commit, date, len(pluginapi.Plugins()))
		for _, p := range pluginapi.Plugins() {
			fmt.Fprintln(out, p.Manifest().ID)
		}
		return nil
	}
	switch command {
	case "run", "check-config", "backfill", "verify", "mcp":
	default:
		return errors.New("usage: chatkeeper run|check-config|backfill|verify|setup|cleanup|mcp|version|healthcheck")
	}
	home := appHome()
	if home == "" {
		return errors.New("cannot locate application home")
	}
	if command == "run" || command == "backfill" {
		level := slog.LevelInfo
		if value := os.Getenv("CHATKEEPER_LOG_LEVEL"); value != "" {
			if e := level.UnmarshalText([]byte(value)); e != nil {
				return errors.New("invalid CHATKEEPER_LOG_LEVEL")
			}
		}
		sink, e := logging.Open(home, logging.MaxSize, logging.Backups, os.Stderr, level)
		if e != nil {
			return fmt.Errorf("open persistent logs: %w", e)
		}
		defer sink.Close()
		previous := slog.Default()
		slog.SetDefault(pluginapi.RedactingLogger(sink.Logger, nil))
		defer slog.SetDefault(previous)
		slog.Info("startup", "version", version, "command", command, "home", home)
		defer slog.Info("command finished", "command", command)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	name := flags.String("config", filepath.Join(home, "config.yaml"), "YAML config path")
	mode := flags.String("mode", "", "desktop or server")
	bind := flags.String("bind", "", "listener host:port")
	hosts := flags.String("allowed-hosts", os.Getenv("CHATKEEPER_ALLOWED_HOSTS"), "comma-separated Host allowlist")
	noBrowser := flags.Bool("no-browser", false, "do not open browser")
	dev := flags.Bool("dev", false, "serve frontend files from disk")
	offline := flags.Bool("offline", false, "validate without credentials")
	err := flags.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		printUsage(out)
		return nil
	}
	if err != nil || flags.NArg() != 0 {
		return errors.New("invalid command arguments")
	}
	if *offline && command != "check-config" && command != "verify" {
		return errors.New("offline is supported by check-config and verify")
	}
	if command == "run" {
		address := *bind
		if address == "" {
			address = "127.0.0.1:8787"
			if c, e := config.Load(*name, false); e == nil {
				address = c.Address()
			}
		}
		return runWeb(home, *name, *mode, address, *hosts, *noBrowser, *dev, out)
	}
	if command == "mcp" {
		cfg, e := config.Load(*name, false)
		if e != nil {
			return e
		}
		service := search.NewReadOnly(home)
		defer service.Close()
		sources := map[string]search.Source{}
		for _, entry := range cfg.Workspaces {
			source, e := search.FromPlugin(context.Background(), entry.Workspace.Storage, entry.Plugin)
			if e != nil {
				return e
			}
			sources[entry.Workspace.ID] = source
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if e := service.Configure(ctx, sources); e != nil {
			return e
		}
		server := archiveMCP.New(service)
		store := &secrets.Store{Home: home, Desktop: web.ConfiguredMode(*mode, "127.0.0.1") == "desktop"}
		archiveMCP.AddSetup(server, archiveMCP.SetupTools{
			Status: func(context.Context) (any, error) { return web.SetupStatus(*name, store) },
			Validate: func(ctx context.Context, workspace string) (any, error) {
				return web.ValidateWorkspace(ctx, *name, store, workspace, setupHTTP)
			},
		})
		return server.Run(ctx, &sdk.StdioTransport{})
	}

	secretStore := &secrets.Store{Home: home, Desktop: web.ConfiguredMode(*mode, "127.0.0.1") == "desktop"}
	var secretError error
	cfg, e := config.LoadWithResolver(*name, command != "verify" && !*offline, func(k string) string {
		v, _, err := secretStore.Get(k)
		if err != nil {
			secretError = err
		}
		return v
	})
	if secretError != nil {
		return errors.New("cannot resolve credentials")
	}
	if e != nil {
		return e
	}
	if command == "check-config" {
		fmt.Fprintf(out, "configuration valid: %d workspaces\n", len(cfg.Workspaces))
		return nil
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	sources := map[string]search.Source{}
	if command == "backfill" {
		index := search.New(home)
		defer index.Close()

		for _, entry := range cfg.Workspaces {
			source, e := search.FromPlugin(context.Background(), entry.Workspace.Storage, entry.Plugin)
			if e != nil {
				return e
			}
			sources[entry.Workspace.ID] = source
		}
		for _, entry := range cfg.Workspaces {
			st, e := storage.Open(entry.Workspace.Storage)
			if e != nil {
				return errors.New("cannot open archive")
			}
			st.Close()
		}
		if e := index.Configure(ctx, sources); e != nil && !errors.Is(e, search.ErrLocked) {
			return e
		}
		index.Close()

	}

	slog.Info("capture configuration", "workspaces", len(cfg.Workspaces), "version", version)
	count := 0
	for _, entry := range cfg.Workspaces {
		store, e := storage.Open(entry.Workspace.Storage)
		if e != nil {
			return errors.New("cannot open archive")
		}
		defer store.Close()
		hooks, e := core.Call(ctx, 10*time.Second, func(context.Context) (pluginapi.ContentHooks, error) { return entry.Plugin.Content(), nil })
		if e != nil {
			return e
		}
		if command == "verify" {
			keys, e := store.Keys(ctx)
			if e != nil {
				return errors.New("archive verification failed")
			}
			for _, key := range keys {
				v, e := store.Read(ctx, key)
				if e != nil || v == nil {
					return errors.New("archive verification failed")
				}
				if core.Guard(ctx, 30*time.Second, func(c context.Context) error { return hooks.Validate(c, *v) }) != nil {
					return errors.New("archive content verification failed")
				}
				count++
			}
			if len(store.Failures()) > 0 {
				return errors.New("archive catalog verification failed")
			}
			continue
		}
		runtime := core.New(entry.Workspace, entry.Plugin, pluginapi.Env{Credentials: entry.Credentials, HTTP: backfillHTTP}, store)
		if command == "backfill" {
			runtime.OnArchive = func(k pluginapi.Key) {
				e := search.SyncShared(ctx, home, entry.Workspace.ID, sources[entry.Workspace.ID], k)
				if e != nil {
					fmt.Fprintln(os.Stderr, "search update failed; restart rebuilds index")
				}
			}
		}

		if command == "backfill" {
			session, e := core.Call(ctx, 30*time.Second, func(c context.Context) (pluginapi.Session, error) {
				return entry.Plugin.NewSession(c, entry.Workspace, runtime.Env)
			})
			if e != nil {
				return e
			}
			_, e = core.Call(ctx, 30*time.Second, session.VerifyIdentity)
			if e != nil {
				return e
			}
			n, scanErr := runtime.Backfill(ctx, session)
			stopErr := core.Guard(ctx, 10*time.Second, session.Stop)
			if scanErr != nil {
				return errors.New("backfill incomplete")
			}
			if stopErr != nil {
				return stopErr
			}
			count += n
		}
	}
	if command == "verify" {
		fmt.Fprintf(out, "verified %d snapshots\n", count)
		return nil
	}
	if command == "backfill" {
		fmt.Fprintf(out, "archived %d changed snapshots\n", count)
		return nil
	}
	return nil
}

// appHome is CHATKEEPER_HOME or the per-user config folder ("" if unknown).
func appHome() string {
	if home := os.Getenv("CHATKEEPER_HOME"); home != "" {
		return home
	}
	dir, e := os.UserConfigDir()
	if e != nil {
		return ""
	}
	return filepath.Join(dir, "chatkeeper")
}

func printUsage(out io.Writer) {
	fmt.Fprint(out, `Usage: chatkeeper [command] [flags]

Commands:
  run           Start capture and browser UI (default)
  check-config  Validate configuration (--offline skips credentials)
  backfill      Capture matching threads once
  verify        Verify archive contents without source calls
  setup         Non-interactive setup for scripts and coding agents
                (setup PLATFORM --help; --print-manifest; --dry-run)
  cleanup       Preview cleanup (--workspace X; --apply enables source deletion)
  mcp           Serve read-only MCP over stdio
  healthcheck   GET /healthz; exit 0 when healthy, 1 on failure
  logs          Tail persistent logs (--follow; --filter TEXT)
  version       Print build version and compiled plugins
  help          Show this help

Flags:
  --config PATH          YAML configuration (default: app home/config.yaml)
  --mode desktop|server  Select local browser or server mode
  --bind HOST:PORT       Listener (default: 127.0.0.1:8787)
  --allowed-hosts HOSTS  Comma-separated server Host allowlist
  --no-browser           Print launch URL without opening browser
  --offline              Skip credentials (check-config and verify only)
  --dev                  Serve frontend from disk
  --url URL              Health endpoint (healthcheck; default: http://127.0.0.1:8787/healthz)
  --timeout DURATION     Probe timeout (healthcheck; default: 3s)
  -h, --help             Show this help

Environment: CHATKEEPER_HOME, CHATKEEPER_MODE, CHATKEEPER_ADMIN_PASSWORD,
CHATKEEPER_ADMIN_PASSWORD_HASH, CHATKEEPER_ALLOWED_HOSTS, CHATKEEPER_API_TOKEN,
CHATKEEPER_SECURE_COOKIES.
`)
}

// healthcheck works in the distroless image without a shell or curl.
func healthcheck(args []string) error {
	flags := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	endpoint := flags.String("url", "http://127.0.0.1:8787/healthz", "health endpoint")
	timeout := flags.Duration("timeout", 3*time.Second, "probe timeout")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *timeout <= 0 {
		return errors.New("invalid healthcheck arguments")
	}
	client := &http.Client{
		Timeout:       *timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(*endpoint)
	if err != nil {
		return errors.New("healthcheck request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("healthcheck failed: HTTP %d", resp.StatusCode)
	}
	return nil
}
