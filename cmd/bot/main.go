// Command bot is the merlin entrypoint: load config, wire core, start plugins.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/6586x57890143/merlin/internal/config"
	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/plugins/ping"
	"github.com/6586x57890143/merlin/internal/scheduler"
	"github.com/6586x57890143/merlin/internal/storage"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// Loaded in dev only; in the container, env comes from Docker secrets /
	// the platform's secret manager, and .env won't exist — that's fine.
	_ = godotenv.Load()

	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	configPath := os.Getenv("MERLIN_CONFIG_PATH")
	if configPath == "" {
		configPath = "config.yaml"
	}

	cfgLoader, err := config.NewLoader(configPath, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfgLoader.Watch(ctx)

	cfg := cfgLoader.Global()

	if cfg.Database.DSN == "" {
		return errors.New("DATABASE_URL not set: Postgres is a hard runtime requirement (scheduler persists job state there)")
	}
	db, err := storage.Connect(ctx, cfg.Database.DSN)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := storage.Migrate(ctx, db.Pool); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	session, err := core.NewSession(cfg.Discord.Token)
	if err != nil {
		return err
	}

	bus := core.NewEventBus(log)
	perms := core.NewPermissions(session, cfgLoader)
	sched := scheduler.New(scheduler.NewPostgresJobStateStore(db.Pool), log)

	deps := core.Deps{
		Session:   session,
		Bus:       bus,
		Config:    cfgLoader,
		Perms:     perms,
		Logger:    log,
		DB:        db,
		Scheduler: sched,
	}

	registry := core.NewRegistry(deps, log)
	registry.Register(sched)
	registry.Register(ping.New())

	if err := registry.InitAll(); err != nil {
		return err
	}

	if err := session.Open(); err != nil {
		return err
	}
	defer func() {
		if err := session.Close(); err != nil {
			log.Error("closing discord session", "err", err)
		}
	}()

	if err := registry.StartAll(ctx); err != nil {
		return err
	}

	bus.Publish(ctx, core.Event{Type: core.EventReady})
	log.Info("merlin is running")

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	registry.ShutdownAll(shutdownCtx)

	return nil
}
