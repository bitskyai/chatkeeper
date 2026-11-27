package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"github.com/bitskyai/chatkeeper/internal/cleanup"
	"github.com/bitskyai/chatkeeper/internal/config"
	"github.com/bitskyai/chatkeeper/internal/search"
	"github.com/bitskyai/chatkeeper/internal/secrets"
	"github.com/bitskyai/chatkeeper/internal/storage"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func cleanupCommand(args []string, out io.Writer) error {
	home := os.Getenv("CHATKEEPER_HOME")
	if home == "" {
		dir, e := os.UserConfigDir()
		if e != nil {
			return e
		}
		home = filepath.Join(dir, "chatkeeper")
	}
	f := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	name := f.String("config", filepath.Join(home, "config.yaml"), "configuration")
	workspace := f.String("workspace", "", "workspace id (required)")
	apply := f.Bool("apply", false, "apply deletions (default preview)")
	if f.Parse(args) != nil || f.NArg() != 0 || *workspace == "" {
		return errors.New("usage: chatkeeper cleanup --workspace X [--apply] [--config PATH]")
	}
	cfg, e := config.Load(*name, false)
	if e != nil {
		return e
	}
	for _, entry := range cfg.Workspaces {
		w := entry.Workspace
		if w.ID != *workspace {
			continue
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if !w.Cleanup.Enabled {
			return json.NewEncoder(out).Encode(cleanup.Report{Workspace: w.ID, Reason: "cleanup disabled"})
		}
		var d pluginapi.CleanupDriver
		if *apply {
			ss := &secrets.Store{Home: home, Desktop: config.Loopback(cfg.Host)}
			token, _, e := ss.Get(w.Cleanup.TokenEnv)
			if e != nil {
				return errors.New("cannot resolve cleanup credential")
			}
			d, e = entry.Plugin.NewCleanupDriver(ctx, w, pluginapi.Env{Credentials: pluginapi.Credentials{"cleanup_token": token}, HTTP: backfillHTTP})
			if e != nil {
				return e
			}
		}
		s, e := storage.Open(w.Storage)
		if e != nil {
			return e
		}
		defer s.Close()
		indexFailed := false
		r := cleanup.Run(ctx, w, s, d, *apply, func(progress cleanup.Report) {
			if !*apply || len(progress.Threads) == 0 {
				return
			}
			// A running server owns the index. Update its shared index when present;
			// otherwise the next reader/configuration builds from the journal overlay.
			if _, err := os.Stat(filepath.Join(home, "state", "index", w.ID+".db")); err != nil {
				return
			}
			source, err := search.FromPlugin(ctx, w.Storage, entry.Plugin)
			if err != nil {
				indexFailed = true
				return
			}
			thread := progress.Threads[len(progress.Threads)-1]
			if search.SyncShared(ctx, home, w.ID, source, pluginapi.Key{Channel: thread.Channel, RootTS: thread.RootTS}) != nil {
				indexFailed = true
			}
		})
		if indexFailed {
			r.Reason = "cleanup finished but search update failed; rebuild index"
		}
		return writeCleanupReport(out, r, *apply)
	}
	return errors.New("workspace not found")
}

func writeCleanupReport(out io.Writer, r cleanup.Report, apply bool) error {
	if e := json.NewEncoder(out).Encode(r); e != nil {
		return e
	}
	if apply {
		if r.Reason != "" {
			return errors.New("cleanup blocked; see report")
		}
		for _, t := range r.Threads {
			if t.Status != "deleted" && t.Status != "cleaned" && t.Status != "deferred" {
				return errors.New("cleanup blocked; see report")
			}
		}
	}
	return nil
}
