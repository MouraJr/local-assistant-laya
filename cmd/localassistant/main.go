// Command localassistant is a local, offline-capable reminder and timer that
// understands plain language.
//
// One process holds the SQLite file, the scheduler, and the HTTP server. There
// is no external service, no Node runtime, and no required API key.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/geral/local-assistant-laya/config"
	"github.com/geral/local-assistant-laya/internal/notify"
	"github.com/geral/local-assistant-laya/internal/parse"
	"github.com/geral/local-assistant-laya/internal/scheduler"
	"github.com/geral/local-assistant-laya/internal/store"
	"github.com/geral/local-assistant-laya/internal/web"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	// .env is a convenience, never a requirement.
	_ = godotenv.Load()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version":
			fmt.Println("local-assistant dev (M1)")
			return nil
		case "help", "-h", "--help":
			usage()
			return nil
		}
	}

	cfg := config.Load()

	if dir := filepath.Dir(cfg.DBPath); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create data dir %s: %w", dir, err)
		}
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()
	log.Info("store ready", "path", cfg.DBPath)

	hub := web.NewHub(log)
	defer hub.Close()

	// The desktop notifier is one link in a chain. Web Push joins it in M4
	// without the scheduler knowing anything changed.
	chain := notify.NewChain(
		notify.NewHost(notify.Options{Sound: cfg.SoundOn, Toast: cfg.ToastOn, Logger: log}),
		web.BroadcastNotifier(hub),
	)

	sched := scheduler.New(st, chain, scheduler.Options{
		Tick:   cfg.Tick,
		Logger: log,
	})

	srv := web.New(web.Options{
		Store:            st,
		Hub:              hub,
		ConfirmThreshold: cfg.ConfirmThreshold,
		Logger:           log,
		Tier:             parse.TierRules,
	})
	srv.SetScheduler(sched)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		if err := sched.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("scheduler stopped", "error", err)
		}
	}()

	httpSrv := &http.Server{
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: /events is a long-lived stream and would be killed
		// by one. Read paths stay bounded by ReadHeaderTimeout.
		IdleTimeout: 120 * time.Second,
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Addr, err)
	}

	log.Info("local assistant listening",
		"addr", "http://"+ln.Addr().String(),
		"parser", parse.TierRules+" (deterministic, offline)",
		"confirm_threshold", cfg.ConfirmThreshold,
		"tick", cfg.Tick.String(),
		"db", cfg.DBPath,
	)
	warnAboutOrigin(ln.Addr().String())

	serveErr := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "error", err)
	}
	select {
	case <-schedDone:
	case <-time.After(2 * time.Second):
	}
	return nil
}

// warnAboutOrigin is the R5 mitigation from the PRD: Web Push needs a secure
// context, so a plain-HTTP LAN address means push will silently not register.
// Catch it at startup instead of letting the user discover it on their phone.
func warnAboutOrigin(addr string) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() {
		return
	}
	if ip.To4() != nil && ip.IsPrivate() {
		slog.Warn("listening on a private address over plain HTTP",
			"addr", addr,
			"issue", "a service worker cannot register here, so Web Push will not work",
			"fix", "serve it through Tailscale: tailscale serve --bg "+addr,
		)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `local-assistant - local plain-language reminders and timers

Usage:
  localassistant          serve the web UI
  localassistant version  print the version
  localassistant help     show this message

Environment:
  LOCAL_ASSISTANT_ADDR                  listen address (default 127.0.0.1:8080)
  LOCAL_ASSISTANT_DB                    SQLite path (default <config dir>/localassistant.db)
  LOCAL_ASSISTANT_TICK_SECONDS          scheduler re-check interval (default 15)
  LOCAL_ASSISTANT_CONFIRM_THRESHOLD     confidence needed to skip confirmation (default 0.85)
  LOCAL_ASSISTANT_SOUND                 host alert sound (default true)
  LOCAL_ASSISTANT_TOAST                 host desktop toast (default true)
  LOCAL_ASSISTANT_HOME                  override the config directory

Examples:
  local-assistant.exe
  tailscale serve --bg 127.0.0.1:8080
`)
}
