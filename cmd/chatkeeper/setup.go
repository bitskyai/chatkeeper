package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bitskyai/chatkeeper/internal/config"
	"github.com/bitskyai/chatkeeper/internal/secrets"
	"github.com/bitskyai/chatkeeper/internal/web"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
)

var setupHTTP = &http.Client{Timeout: 30 * time.Second}

const setupUsage = `Usage: chatkeeper setup PLATFORM [flags]

Runs the browser wizard's steps and checks without a browser, saves the
workspace (config plus secret store) and prints a JSON result. Tokens are read
only from environment variables and are never printed.

Flags:
  --workspace-name NAME   Workspace id to save (required unless --dry-run)
  --storage PATH          Archive folder (default: data root/NAME)
  --dry-run               Validate every step, save nothing
  --replace               Overwrite an existing workspace with the same name
  --skip STEP             Skip an optional step (repeatable)
  --print-manifest        Print the platform app manifest and exit
  --print-agent-prompt    Print the coding-agent setup prompt and exit
  --config PATH           YAML configuration (default: app home/config.yaml)
  --mode desktop|server   Secret store and storage defaults (default: desktop)

`

// setupCommand is the non-interactive setup path for scripts and coding agents.
func setupCommand(home string, args []string, out io.Writer) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprint(out, setupUsage)
		ids := []string{}
		for _, p := range pluginapi.Plugins() {
			if len(p.SetupFlow()) > 0 {
				ids = append(ids, p.Manifest().ID)
			}
		}
		fmt.Fprintf(out, "Platforms: %s\n", strings.Join(ids, ", "))
		if len(args) == 0 {
			return errors.New("setup: platform required")
		}
		return nil
	}
	p, ok := pluginapi.Lookup(args[0])
	if !ok || len(p.SetupFlow()) == 0 {
		return fmt.Errorf("setup: unknown platform %q", args[0])
	}
	o := web.SetupOptions{Platform: args[0], Config: filepath.Join(home, "config.yaml"), Home: home, Values: map[string]string{}, Skip: map[string]bool{}, Getenv: os.Getenv, HTTP: setupHTTP}
	mode := ""
	printManifest, printPrompt := false, false
	rest := args[1:]
	for len(rest) > 0 {
		arg := rest[0]
		rest = rest[1:]
		if !strings.HasPrefix(arg, "--") {
			return fmt.Errorf("setup: unexpected argument %q", arg)
		}
		name, value, inline := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		switch name {
		case "help", "h":
			fmt.Fprint(out, setupUsage+web.SetupHelp(p))
			return nil
		case "dry-run":
			o.DryRun = true
			continue
		case "replace":
			o.Replace = true
			continue
		case "print-manifest":
			printManifest = true
			continue
		case "print-agent-prompt":
			printPrompt = true
			continue
		}
		if !inline {
			if len(rest) == 0 {
				return fmt.Errorf("setup: --%s needs a value", name)
			}
			value, rest = rest[0], rest[1:]
		}
		switch name {
		case "workspace-name":
			o.Workspace = value
		case "storage":
			o.Storage = value
		case "config":
			o.Config = value
		case "mode":
			mode = value
		case "skip":
			o.Skip[value] = true
		default:
			field := ""
			for _, f := range web.SetupFields(p) {
				if f.Flag == name {
					field = f.Name
				}
			}
			for _, f := range web.SetupFields(p) {
				if f.Secret && f.Flag == name+"-env" {
					return fmt.Errorf("setup: --%s is secret and is read only from an environment variable: --%s-env VAR", name, name)
				}
			}
			if field == "" {
				return fmt.Errorf("setup: unknown flag --%s (see chatkeeper setup %s --help)", name, args[0])
			}
			o.Values[field] = value
		}
	}
	if printManifest || printPrompt {
		agent, ok := p.(pluginapi.AgentSetup)
		if !ok {
			return fmt.Errorf("setup: %s has no app manifest or agent prompt", args[0])
		}
		if printManifest {
			fmt.Fprint(out, agent.SetupManifest())
		} else {
			fmt.Fprintln(out, agent.AgentPrompt())
		}
		return nil
	}
	o.Mode = web.ConfiguredMode(mode, "127.0.0.1")
	if o.Mode != "desktop" && o.Mode != "server" {
		return errors.New("setup: --mode must be desktop or server")
	}
	o.Secrets = &secrets.Store{Home: home, Desktop: o.Mode == "desktop"}
	// Same hardening as the web wizard: MkdirAll keeps the mode of an
	// existing directory, so restrict it explicitly.
	if e := os.MkdirAll(home, 0o700); e != nil {
		return errors.New("setup: cannot create application home")
	}
	if e := config.PrivatePath(home); e != nil {
		return errors.New("setup: cannot restrict application home to the current user")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	res := web.RunSetup(ctx, o)
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if e := enc.Encode(res); e != nil {
		return e
	}
	if !res.OK {
		return errors.New("setup failed: " + res.Error)
	}
	return nil
}
