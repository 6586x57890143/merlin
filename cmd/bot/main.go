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

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"

	"github.com/6586x57890143/merlin/internal/audit"
	"github.com/6586x57890143/merlin/internal/config"
	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/plugins/adminconfig"
	"github.com/6586x57890143/merlin/internal/plugins/ping"
	"github.com/6586x57890143/merlin/internal/plugins/rotation"
	"github.com/6586x57890143/merlin/internal/scheduler"
	"github.com/6586x57890143/merlin/internal/settings"
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
		return errors.New("DATABASE_URL not set: Postgres is a hard runtime requirement (scheduler and settings persist state there)")
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
	settingsStore := settings.New(db.Pool, bus)
	perms := core.NewPermissions(session, settingsStore, cfg.BreakGlassAdminUserID)
	commands := core.NewCommandRouter(perms, log)
	sched := scheduler.New(scheduler.NewPostgresJobStateStore(db.Pool), settingsStore, log)
	auditWriter := audit.New(db.Pool, session, settingsStore)

	deps := core.Deps{
		Session:   session,
		Bus:       bus,
		Config:    cfgLoader,
		Perms:     perms,
		Commands:  commands,
		Audit:     auditWriter,
		Logger:    log,
		DB:        db,
		Scheduler: sched,
	}

	rotationPlugin := rotation.New(settingsStore)

	registry := core.NewRegistry(deps, log)
	registry.Register(sched)
	registry.Register(ping.New())
	registry.Register(rotationPlugin)
	registry.Register(adminconfig.New(settingsStore, configPath))

	if err := registry.InitAll(); err != nil {
		return err
	}
	if err := commands.Finalize(); err != nil {
		return fmt.Errorf("finalize commands: %w", err)
	}

	// One dispatcher for every plugin's commands (spec.MD §4a) — replaces
	// each plugin calling session.AddHandler itself. Guild-scoped command
	// registration and settings loading both happen reactively per guild via
	// GuildCreate, not from a static config list: discordgo fires one
	// GuildCreate per guild the bot is in, both during the initial post-Open
	// sync and whenever the bot is added to a new guild later, so this one
	// handler covers "known at startup" and "joined while running" the same
	// way, with no restart required for the latter.
	session.AddHandler(commands.HandleInteraction)
	session.AddHandler(func(s *discordgo.Session, gc *discordgo.GuildCreate) {
		guildCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := settingsStore.Refresh(guildCtx, gc.ID); err != nil {
			log.Error("refresh settings for guild", "guild", gc.ID, "err", err)
			return
		}
		if err := commands.RegisterGuild(s, cfg.Discord.AppID, gc.ID); err != nil {
			log.Error("register commands for guild", "guild", gc.ID, "err", err)
		}
		rotationPlugin.SyncGuild(gc.ID)
	})

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
