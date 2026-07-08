// labdeck — homelab NOC dashboard (M1: probe engine + status board + notifications).
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"labdeck/internal/api"
	"labdeck/internal/config"
	"labdeck/internal/creds"
	"labdeck/internal/engine"
	"labdeck/internal/notify"
	"labdeck/internal/sshgw"
	"labdeck/internal/store"
)

func main() {
	configPath := flag.String("config", "services.yaml", "path to config file")
	dbPath := flag.String("db", "labdeck.db", "path to SQLite database")
	listen := flag.String("listen", "", "listen address (overrides config)")
	retentionDays := flag.Int("retention-days", 30, "history retention in days")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		slog.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	checkService := map[string]string{}
	for _, svc := range cfg.Services {
		for _, ck := range svc.Checks {
			checkService[ck.ID] = svc.ID
		}
	}

	masterKey := creds.DeriveKey(os.Getenv("LABDECK_MASTER_KEY"))
	gw := sshgw.New(cfg, st, masterKey)
	if !gw.Enabled() {
		hasSSH := false
		for _, h := range cfg.Hosts {
			if h.SSH != nil {
				hasSSH = true
				break
			}
		}
		if hasSSH {
			slog.Warn("hosts have ssh configured but LABDECK_MASTER_KEY is not set — SSH gateway disabled")
		}
	}

	eng := engine.New(cfg)
	notifiers := notify.Build(cfg.Notifiers)
	srv := api.New(cfg, eng, st, gw)

	eng.OnRecord = func(r engine.ProbeRecord) {
		if err := st.RecordProbe(checkService[r.CheckID], r); err != nil {
			slog.Error("record probe", "err", err)
		}
	}
	eng.OnTransition = func(t engine.Transition) {
		if err := st.RecordEvent(t); err != nil {
			slog.Error("record event", "err", err)
		}
		notify.Dispatch(notifiers, t)
		srv.Poke()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go eng.Run(ctx)
	go st.RetainLoop(ctx, *retentionDays)
	go srv.Broadcast(ctx.Done())

	httpSrv := &http.Server{Addr: cfg.Listen, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	slog.Info("labdeck listening", "addr", cfg.Listen,
		"services", len(cfg.Services), "notifiers", len(notifiers))
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("http server", "err", err)
		os.Exit(1)
	}
}
