// Command bot is the merlin entrypoint: load config, wire core, start plugins.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"

	"github.com/6586x57890143/merlin/internal/audit"
	"github.com/6586x57890143/merlin/internal/config"
	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordguard"
	"github.com/6586x57890143/merlin/internal/plugins/adminconfig"
	"github.com/6586x57890143/merlin/internal/plugins/ping"
	"github.com/6586x57890143/merlin/internal/plugins/roles"
	"github.com/6586x57890143/merlin/internal/plugins/rotation"
	"github.com/6586x57890143/merlin/internal/scheduler"
	"github.com/6586x57890143/merlin/internal/settings"
	"github.com/6586x57890143/merlin/internal/storage"
)

func main() {
	// Set from config once it's loaded; until then everything logs at Info.
	// A LevelVar rather than a rebuilt handler so the level can move without
	// invalidating the logger already handed to every plugin.
	level := new(slog.LevelVar)
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	// Loaded in dev only; in the container, env comes from Docker secrets /
	// the platform's secret manager, and .env won't exist — that's fine.
	_ = godotenv.Load()

	if err := runGuarded(log, level); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// runGuarded turns a panic outside any of the recovered call sites (plugin
// lifecycle, command dispatch, bus subscribers, scheduler jobs) into a
// logged fatal rather than a bare runtime trace on stderr. Those traces are
// the one failure mode that leaves no evidence in the log aggregator an
// operator actually reads.
func runGuarded(log *slog.Logger, level *slog.LevelVar) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("panic", "value", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return run(log, level)
}

func run(log *slog.Logger, level *slog.LevelVar) error {
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
	level.Set(cfg.Level())

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

	session, err := core.NewSession(cfg.Discord.Token, cfg.GuildMembersIntent)
	if err != nil {
		return err
	}

	bus := core.NewEventBus(log)
	settingsStore := settings.New(db.Pool, bus)
	// A guild whose settings couldn't be re-read after a mutation falls back
	// to fail-closed defaults; this walks those back to real values once the
	// database is reachable again, instead of waiting on the next mutation.
	settingsStore.StartRetry(ctx, log)
	perms := core.NewPermissions(session, settingsStore, cfg.BootstrapAdminUserID)
	commands := core.NewCommandRouter(perms, settingsStore, log)
	sched := scheduler.New(scheduler.NewPostgresJobStateStore(db.Pool), settingsStore, log)
	journal := discordguard.NewPostgresJournal(db.Pool)
	auditWriter := audit.New(db.Pool, session, settingsStore)
	// audit_log and action_journal both grow forever otherwise. Housekeeping,
	// not a Scheduler job: neither table is per guild, and missing a tick just
	// means the next one prunes the same rows.
	auditWriter.StartRetention(ctx, log, journal)

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

	// Every destructive Discord call the plugins make goes through guard, so
	// /config pause (or MERLIN_PAUSE_ALL_WRITES) can stop the bot mutating
	// anything without a redeploy. Bound per guild at each call site because
	// most of those calls are channel-scoped and carry no guild themselves.
	guard := discordguard.New(session, settingsStore, log, cfg.PauseAllWrites).WithJournal(journal)
	if cfg.PauseAllWrites {
		log.Warn("MERLIN_PAUSE_ALL_WRITES is set: every destructive Discord action is refused process-wide")
	}

	rotationPlugin := rotation.New(settingsStore,
		func(guildID string) rotation.DiscordChannelOps { return guard.For(guildID) },
		guard.DryRun,
	)
	rolesPlugin := roles.New(roles.NewPostgresStore(db.Pool), settingsStore,
		func(guildID string) roles.DiscordMemberOps { return guard.For(guildID) },
		guard.DryRun,
	)

	registry := core.NewRegistry(deps, log)
	registry.Register(sched)
	registry.Register(ping.New())
	registry.Register(rotationPlugin)
	registry.Register(rolesPlugin)
	adminconfigPlugin := adminconfig.New(settingsStore, configPath, db, sched)
	registry.Register(adminconfigPlugin)

	if err := registry.InitAll(); err != nil {
		return err
	}
	if err := commands.Finalize(); err != nil {
		return fmt.Errorf("finalize commands: %w", err)
	}
	// This bot registers commands exclusively per-guild (RegisterGuild below,
	// on every GuildCreate) — nothing here should ever leave a global command
	// behind. Clears any that pre-Milestone-4 code left registered (a REST
	// call, doesn't need session.Open() first).
	if err := commands.PurgeGlobalCommands(session, cfg.Discord.AppID); err != nil {
		log.Error("purge global commands", "err", err)
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

		// A failed settings load must not skip the wiring below, and it used
		// to: this handler returned, and because that guild had never been
		// through a mutator it was never queued for retry either, so nothing
		// re-ran for the life of the process.
		//
		// What made that invisible is that slash commands are registered on
		// Discord's side and persist across restarts. A guild in this state
		// still answered /roles jail perfectly, while its roles-sweep job — the
		// only thing that releases jails when they expire and the only
		// intent-free defense against a rejoin evader — had never been
		// registered at all. No command failed, no error repeated, and the
		// symptom was simply that jails quietly became permanent and escapable.
		//
		// So: queue the guild for retry, and go on to do everything that does
		// not actually need settings. The roles sweep needs none of it.
		settingsLoaded := true
		if err := settingsStore.Refresh(guildCtx, gc.ID); err != nil {
			log.Error("refresh settings for guild, continuing on fail-closed defaults",
				"guild", gc.ID, "err", err)
			settingsStore.MarkStale(gc.ID)
			settingsLoaded = false
		}
		commandsRegistered := true
		if err := commands.RegisterGuild(s, cfg.Discord.AppID, gc.ID); err != nil {
			log.Error("register commands for guild", "guild", gc.ID, "err", err)
			commandsRegistered = false
		}
		rolesPlugin.SyncGuild(gc.ID)
		if settingsLoaded {
			// Rotation, unlike the sweep, derives which jobs should exist from
			// settings — reconciling against fail-closed defaults would read as
			// "this guild rotates nothing" and unregister real work. The
			// stale-retry loop publishes EventConfigChanged once the guild is
			// readable again, which is what reconciles it.
			rotationPlugin.SyncGuild(guildCtx, gc.ID)
		}
		if commandsRegistered && settingsLoaded {
			// Only nudge toward /config setup if it was actually registered
			// here — otherwise the nudge would point at a command that
			// doesn't exist yet, and (if the DM itself succeeds) burn the
			// one-time nudge for nothing instead of retrying once
			// registration succeeds on a later restart. Same reasoning for
			// unreadable settings: "unconfigured" can't be told apart from
			// "couldn't be read", and the nudge only fires once.
			adminconfigPlugin.NudgeIfUnconfigured(guildCtx, gc)
		}
	})

	// The bot was removed from a guild (or the guild was deleted). Without
	// this, that guild's rotation and sweep jobs keep ticking against a
	// server the bot can no longer see: every REST call fails, the failure
	// counter climbs, and the eventual "job is wedged" alert tries to post to
	// a status channel in the same unreachable guild.
	//
	// Nothing in Postgres is deleted. Being removed is frequently temporary —
	// a kick and re-invite, a botched permission change — and a guild's mod
	// roles, permission policy, rotation config, and jail snapshots must not
	// be silently destroyed by the bot briefly losing access. GuildCreate
	// puts it all back on rejoin.
	// A member who left to shed their Jailed role gets it back as they walk in,
	// rather than up to a sweep later. Registered only when the intent was
	// actually requested, so the handler's presence always matches reality —
	// without GUILD_MEMBERS Discord never sends this event, and reapplyEvadedJails
	// on the one-minute sweep is the whole protection instead of the backstop.
	if cfg.GuildMembersIntent {
		log.Info("GUILD_MEMBERS intent requested: jails re-apply the moment an evader rejoins")
		session.AddHandler(func(s *discordgo.Session, ma *discordgo.GuildMemberAdd) {
			if ma.Member == nil || ma.User == nil {
				return
			}
			joinCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			rolesPlugin.HandleMemberJoin(joinCtx, ma.GuildID, ma.User.ID)
		})
	} else {
		log.Warn("GUILD_MEMBERS intent disabled by MERLIN_DISABLE_GUILD_MEMBERS_INTENT: " +
			"a jailed member who rejoins keeps full access until the next sweep")
	}

	// A channel disappearing under a rotation config is otherwise only
	// noticed as a job that quietly fails for five runs before alerting.
	session.AddHandler(func(s *discordgo.Session, cd *discordgo.ChannelDelete) {
		if cd.GuildID == "" {
			return
		}
		guildCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		rotationPlugin.HandleChannelDeleted(guildCtx, cd.GuildID, cd.ID)
	})

	session.AddHandler(func(s *discordgo.Session, gd *discordgo.GuildDelete) {
		// Discord sends the same event for "this guild is temporarily
		// unavailable" (an outage on their side) as for "you were removed",
		// distinguished only by this flag. Tearing down on an outage would
		// mean an unrelated Discord incident silently stopped rotations.
		if gd.Unavailable {
			log.Warn("guild unavailable, keeping jobs registered", "guild", gd.ID)
			return
		}
		dropped := sched.UnregisterGuild(gd.ID)
		rotationPlugin.ForgetGuild(gd.ID)
		rolesPlugin.ForgetGuild(gd.ID)
		settingsStore.Forget(gd.ID)
		log.Info("left guild, unregistered its jobs", "guild", gd.ID, "jobs", dropped)
	})

	// Armed before Open, which is where discordgo starts dispatching — see
	// core.WatchReady. Open only means the identify was sent, never that
	// Discord accepted it, and nothing below works without a live gateway.
	awaitReady := core.WatchReady(session)

	if err := session.Open(); err != nil {
		return err
	}
	defer func() {
		if err := session.Close(); err != nil {
			log.Error("closing discord session", "err", err)
		}
	}()

	if err := awaitReady(); err != nil {
		return err
	}

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
