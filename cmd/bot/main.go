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
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"

	"github.com/6586x57890143/merlin/internal/audit"
	"github.com/6586x57890143/merlin/internal/config"
	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordguard"
	"github.com/6586x57890143/merlin/internal/plugins/adminconfig"
	"github.com/6586x57890143/merlin/internal/plugins/aimod"
	"github.com/6586x57890143/merlin/internal/plugins/ping"
	"github.com/6586x57890143/merlin/internal/plugins/roles"
	"github.com/6586x57890143/merlin/internal/plugins/rotation"
	"github.com/6586x57890143/merlin/internal/scheduler"
	"github.com/6586x57890143/merlin/internal/settings"
	"github.com/6586x57890143/merlin/internal/storage"
	"github.com/6586x57890143/merlin/internal/voice"
)

func main() {
	// Set from config once it's loaded; until then everything logs at Info.
	// A LevelVar rather than a rebuilt handler so the level can move without
	// invalidating the logger already handed to every plugin.
	level := new(slog.LevelVar)
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	// Loaded in dev only; in the container, env comes from Docker secrets /
	// the platform's secret manager, and .env won't exist, which is fine.
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

	// Registered before Watch starts, so a SIGHUP that lands immediately
	// still moves the level. Without this hook the level was applied exactly
	// once, at startup, and reloading the config parsed a new LogLevel into
	// a field nothing read again: turning up verbosity to look at a live
	// problem meant restarting the process and losing the problem.
	cfgLoader.OnReload(func(c *config.GlobalConfig) {
		level.Set(c.Level())
		log.Info("log level applied", "level", c.LogLevel)
	})
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

	session, err := core.NewSession(cfg.Discord.Token, core.Intents{
		Members:        cfg.GuildMembersIntent,
		MessageContent: cfg.MessageContentIntent,
	})
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
	// Loaded and validated before anything can use it, so a catalog that
	// has lost a required placeholder (the rotation notice's retention
	// window, most of all) stops the bot here rather than posting an
	// incomplete notice to a live channel.
	speaker, err := voice.New(log)
	if err != nil {
		return fmt.Errorf("load voice catalog: %w", err)
	}

	commands := core.NewCommandRouter(perms, settingsStore, log).WithVoice(speaker)
	sched := scheduler.New(scheduler.NewPostgresJobStateStore(db.Pool), settingsStore, log)
	journal := discordguard.NewPostgresJournal(db.Pool)
	auditWriter := audit.New(db.Pool, session, settingsStore)
	// audit_log, action_journal and aimod's stored evidence all grow forever
	// otherwise. Housekeeping, not a Scheduler job: none of these tables is
	// per guild, and missing a tick just means the next one prunes the same
	// rows. aimodStore ignores the cutoff it is handed, because its window is
	// per guild and admin-settable; see aimod.PruneEvidence.
	aimodStore := aimod.NewPostgresStore(db.Pool)
	auditWriter.StartRetention(ctx, log, journal, aimodStore)

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
		speaker,
	)
	rolesPlugin := roles.New(roles.NewPostgresStore(db.Pool), settingsStore,
		func(guildID string) roles.DiscordMemberOps { return guard.For(guildID) },
		guard.DryRun,
		speaker,
		// Reads discordgo's gateway-cached voice state directly (session.State
		// is a field, not embedded, so *discordgo.Session can't structurally
		// satisfy an interface method for this the way it does for every REST
		// call in DiscordMemberOps). Enrichment only: roles.disconnectFromVoice
		// still force-disconnects unconditionally even when this reports
		// nothing, so a cold or incomplete cache never costs a missed kick,
		// only a less specific log line.
		func(guildID, userID string) (string, bool) {
			vs, err := session.State.VoiceState(guildID, userID)
			if err != nil {
				return "", false
			}
			return vs.ChannelID, true
		},
	)

	// AI moderation. Constructed even when MESSAGE_CONTENT is off: the
	// plugin then registers its commands, scans nothing, and /aimod status
	// says exactly why, which is a far better failure than a command that is
	// simply not there. The policy catalogue is validated in New, so a
	// malformed policy file stops the bot here rather than mid-classification
	// on a live server, the same contract voice.New has.
	aimodPlugin, err := aimod.New(
		// Wrapped so guild config is not re-read from Postgres for every
		// message on every server. The Pruner above keeps the unwrapped
		// store: it only ever writes.
		aimod.NewCachingStore(aimodStore),
		aimod.NewClient(),
		func(guildID string) aimod.DiscordOps { return guard.For(guildID) },
		cfg.SecretKey,
		speaker,
		cfg.MessageContentIntent,
	)
	if err != nil {
		return fmt.Errorf("start ai moderation: %w", err)
	}
	// The sanction ladder prefers this bot's own jail over Discord's timeout.
	// Wired here rather than imported, so aimod depends on the behaviour and
	// not on the package: *roles.Plugin satisfies aimod.Jailer structurally.
	aimodPlugin.WithJailer(rolesPlugin)
	// /config plugins set aimod false has to stop the scanning too, not just
	// the commands: HandleMessage below is the one plugin entry point the
	// CommandRouter's own gate check never sees. See aimod.PluginGate.
	aimodPlugin.WithGate(settingsStore)
	// The tip jar's chain. Both empty leaves the Base defaults, which match
	// the network OpenRouter's own checkout settles USDC on, so a donation is
	// the same token on the same chain the credits are bought with.
	aimodPlugin.WithFundingChain(cfg.ETHRPCURL, cfg.USDCContract)

	registry := core.NewRegistry(deps, log)
	registry.Register(sched)
	registry.Register(ping.New(speaker))
	registry.Register(rotationPlugin)
	registry.Register(rolesPlugin)
	adminconfigPlugin := adminconfig.New(settingsStore, configPath, db, sched)
	registry.Register(aimodPlugin)
	registry.Register(adminconfigPlugin)

	if err := registry.InitAll(); err != nil {
		return err
	}
	if err := commands.Finalize(); err != nil {
		return fmt.Errorf("finalize commands: %w", err)
	}
	// This bot registers commands exclusively per-guild (RegisterGuild below,
	// on every GuildCreate), so nothing here should ever leave a global command
	// behind. Clears any that pre-Milestone-4 code left registered (a REST
	// call, doesn't need session.Open() first).
	if err := commands.PurgeGlobalCommands(session, cfg.Discord.AppID); err != nil {
		log.Error("purge global commands", "err", err)
	}

	// One dispatcher for every plugin's commands (spec.MD §4a), replacing
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
		// still answered /roles jail perfectly, while its roles-sweep job (the
		// only thing that releases jails when they expire and the only
		// intent-free defense against a rejoin evader) had never been
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
		// Reads aimod's own store rather than internal/settings, so unlike
		// rotation below it does not wait on settingsLoaded: a guild whose
		// settings refresh failed still gets its weekly calibration review.
		aimodPlugin.SyncGuild(guildCtx, gc.ID)
		if settingsLoaded {
			// Rotation, unlike the sweep, derives which jobs should exist from
			// settings, and reconciling against fail-closed defaults would read as
			// "this guild rotates nothing" and unregister real work. The
			// stale-retry loop publishes EventConfigChanged once the guild is
			// readable again, which is what reconciles it.
			rotationPlugin.SyncGuild(guildCtx, gc.ID)
		}
		if cfg.OnboardingDM && commandsRegistered && settingsLoaded {
			// Off unless MERLIN_ENABLE_ONBOARDING_DM says otherwise; see
			// config.GlobalConfig.OnboardingDM for why this one is opt-in
			// while the GUILD_MEMBERS intent is opt-out.
			//
			// Only nudge toward /config setup if it was actually registered
			// here. Otherwise the nudge would point at a command that
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
	// Nothing in Postgres is deleted. Being removed is frequently temporary
	// (a kick and re-invite, a botched permission change), and a guild's mod
	// roles, permission policy, rotation config, and jail snapshots must not
	// be silently destroyed by the bot briefly losing access. GuildCreate
	// puts it all back on rejoin.
	// A member who left to shed their Jailed role gets it back as they walk in,
	// rather than up to a sweep later. Registered only when the intent was
	// actually requested, so the handler's presence always matches reality:
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

		// A guild's Onboarding or Membership Screening flow grants a member
		// its configured roles the moment they complete it, arriving here as
		// GUILD_MEMBER_UPDATE, entirely outside anything this bot does. For a
		// member jailed on join, that regrant lands after HandleMemberJoin's
		// strip already ran, so without this a jailed member walks straight
		// back in with whatever onboarding hands out, some of which can carry
		// real server access. reapplyEvadedJails' sweep is the backstop for
		// this too (bounded to one minute), so it still closes even with the
		// intent off, just not instantly.
		session.AddHandler(func(s *discordgo.Session, mu *discordgo.GuildMemberUpdate) {
			if mu.Member == nil || mu.User == nil {
				return
			}
			updateCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			rolesPlugin.HandleMemberUpdate(updateCtx, mu.GuildID, mu.User.ID, mu.Roles)
		})
	} else {
		log.Warn("GUILD_MEMBERS intent disabled by MERLIN_DISABLE_GUILD_MEMBERS_INTENT: " +
			"a jailed member who rejoins keeps full access until the next sweep, and roles regranted by " +
			"a guild's Onboarding/Membership Screening flow after a jail aren't stripped again until then either")
	}

	// Message scanning. Registered only when the intent was actually
	// requested, so the handler's presence always matches reality: without
	// MESSAGE_CONTENT Discord delivers no content to read, and a handler that
	// ran anyway would scan a stream of empty strings and report a healthy,
	// entirely useless filter.
	if cfg.MessageContentIntent {
		log.Info("MESSAGE_CONTENT intent requested: AI moderation can read messages")
		session.AddHandler(func(s *discordgo.Session, mc *discordgo.MessageCreate) {
			// Returns immediately; the paid work happens on aimod's own batch
			// timer. discordgo dispatches events serially per shard, so
			// blocking here would stall every other handler behind it.
			aimodPlugin.HandleMessage(mc.Message)
		})
		// Edits, too, and this is not polish. Without it the whole plugin has
		// a one-step bypass: post something innocuous, wait for it to be
		// cleared, then edit it into whatever you like. Nothing looked again.
		session.AddHandler(func(s *discordgo.Session, mu *discordgo.MessageUpdate) {
			aimodPlugin.HandleMessageEdit(mu.Message)
		})
	} else {
		log.Info("MESSAGE_CONTENT intent not requested: AI moderation will scan nothing. " +
			"Set MERLIN_ENABLE_MESSAGE_CONTENT_INTENT=1 and enable Message Content Intent in the Developer Portal.")
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
		// Drops the cached webhook for that channel, which would otherwise
		// keep failing every rewrite until the process restarted.
		aimodPlugin.HandleChannelDeleted(cd.ID)
	})

	// A deleted role is invisible to this bot otherwise, and it leaves two
	// distinct traces: entries in the guild's settings that name a role
	// nobody can see any more, and, if it was the jail marker, a cached ID
	// that every later jail in that guild fails against.
	//
	// There is deliberately no GuildMemberRemove counterpart. A jailed
	// member who leaves must keep their role_jails row: that row is the only
	// copy of the roles they held before being jailed, and it is what
	// re-applies the marker if they come back inside their window. Dropping
	// or flagging it on leave is precisely the evasion bug that was fixed
	// once already, and evasion_test.go asserts the row survives.
	session.AddHandler(func(s *discordgo.Session, rd *discordgo.GuildRoleDelete) {
		rolesPlugin.HandleRoleDeleted(rd.GuildID, rd.RoleID)

		guildCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		removed, err := settingsStore.PruneDeletedRole(guildCtx, rd.GuildID, rd.RoleID)
		if err != nil {
			log.Error("prune deleted role from settings", "guild", rd.GuildID, "role", rd.RoleID, "err", err)
			return
		}
		if len(removed) == 0 {
			return
		}
		log.Info("pruned deleted role from settings", "guild", rd.GuildID, "role", rd.RoleID, "from", removed)
		if err := auditWriter.Record(guildCtx, rd.GuildID, session.State.User.ID, "config.role_pruned",
			fmt.Sprintf("role %s referenced by: %s", rd.RoleID, strings.Join(removed, ", ")),
			"role deleted in Discord, references removed"); err != nil {
			// Same log-and-continue policy every other audit call site uses:
			// the prune already happened and is durable, so a failed audit
			// post must not be reported as a failed prune.
			log.Error("audit role prune", "guild", rd.GuildID, "err", err)
		}
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
		aimodPlugin.ForgetGuild(gd.ID)
		settingsStore.Forget(gd.ID)
		log.Info("left guild, unregistered its jobs", "guild", gd.ID, "jobs", dropped)
	})

	// Armed before Open, which is where discordgo starts dispatching. See
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
