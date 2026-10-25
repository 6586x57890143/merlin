// Command bot is the merlin entrypoint: load config, wire core, start plugins.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"
	"github.com/joho/godotenv"

	"github.com/6586x57890143/merlin/internal/audit"
	"github.com/6586x57890143/merlin/internal/config"
	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordguard"
	"github.com/6586x57890143/merlin/internal/fleet"
	"github.com/6586x57890143/merlin/internal/obs"
	"github.com/6586x57890143/merlin/internal/plugins/adminconfig"
	"github.com/6586x57890143/merlin/internal/plugins/aimod"
	"github.com/6586x57890143/merlin/internal/plugins/contest"
	"github.com/6586x57890143/merlin/internal/plugins/fleetgrant"
	"github.com/6586x57890143/merlin/internal/plugins/help"
	"github.com/6586x57890143/merlin/internal/plugins/perf"
	"github.com/6586x57890143/merlin/internal/plugins/ping"
	"github.com/6586x57890143/merlin/internal/plugins/rapsheet"
	"github.com/6586x57890143/merlin/internal/plugins/roles"
	"github.com/6586x57890143/merlin/internal/plugins/rotation"
	"github.com/6586x57890143/merlin/internal/plugins/statistics"
	"github.com/6586x57890143/merlin/internal/scheduler"
	"github.com/6586x57890143/merlin/internal/scripts"
	"github.com/6586x57890143/merlin/internal/secret"
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

	// The link to cygnet, the fleet's hub, only where it is configured.
	// Every request Discord refuses for want of a permission is reported
	// to her, whichever plugin made it, so she can grant it. Outside that,
	// every REST call is timed by route for /perf, so the two wrap the one
	// client and nothing merlin sends goes unseen by either.
	hub := fleet.Dial(os.Getenv("MERLIN_CYGNET_URL"), os.Getenv("MERLIN_CYGNET_TOKEN"), os.Getenv("MERLIN_ATTEST"), log)
	httpClient := &http.Client{Timeout: 20 * time.Second, Transport: obs.Transport(hub.Transport(nil, log), obs.Default)}
	// Every REST call merlin makes goes through this one client.
	restClient := core.NewRest(cfg.Discord.Token, httpClient)
	// The privileged intents are read from the Developer Portal, not
	// configured: merlin identifies with exactly what is granted, so Discord
	// never refuses her connection over one.
	appID, err := snowflake.Parse(cfg.Discord.AppID)
	if err != nil {
		return fmt.Errorf("DISCORD_APP_ID: %w", err)
	}
	granted, err := core.Probe(restClient)
	if err != nil {
		return err
	}
	log.Info("gateway intents from the Developer Portal", "members", granted.Members, "message_content", granted.MessageContent)
	gw, err := core.NewGateway(cfg.Discord.Token, granted, log)
	if err != nil {
		return err
	}
	if addr := os.Getenv("MERLIN_DEBUG_ADDR"); addr != "" {
		if err := obs.Serve(addr); err != nil {
			log.Error("debug listener", "err", err)
		} else {
			log.Info("debug listener", "addr", addr)
		}
	}

	bus := core.NewEventBus(log)
	settingsStore := settings.New(db.Pool, bus)
	// A guild whose settings couldn't be re-read after a mutation falls back
	// to fail-closed defaults; this walks those back to real values once the
	// database is reachable again, instead of waiting on the next mutation.
	settingsStore.StartRetry(ctx, log)
	perms := core.NewPermissions(gw.Caches, settingsStore, cfg.BootstrapAdminUserID)
	// Loaded and validated before anything can use it, so a catalog that
	// has lost a required placeholder (the rotation notice's retention
	// window, most of all) stops the bot here rather than posting an
	// incomplete notice to a live channel.
	speaker, err := voice.New(log)
	if err != nil {
		return fmt.Errorf("load voice catalog: %w", err)
	}

	// One dispatcher for every interaction, answered through restClient.
	commands := core.NewCommandRouter(perms, settingsStore, log).WithVoice(speaker).WithRest(restClient)
	sched := scheduler.New(scheduler.NewPostgresJobStateStore(db.Pool), settingsStore, log)
	journal := discordguard.NewPostgresJournal(db.Pool)
	auditWriter := audit.New(db.Pool, restClient, settingsStore)
	// audit_log, action_journal and aimod's stored evidence all grow forever
	// otherwise. Housekeeping, not a Scheduler job: none of these tables is
	// per guild, and missing a tick just means the next one prunes the same
	// rows. aimodStore ignores the cutoff it is handed, because its window is
	// per guild and admin-settable; see aimod.PruneEvidence.
	aimodStore := aimod.NewPostgresStore(db.Pool)
	auditWriter.StartRetention(ctx, log, journal, aimodStore)

	botID, err := core.SelfID(cfg.Discord.Token)
	if err != nil {
		return err
	}
	deps := core.Deps{
		Rest:      restClient,
		BotID:     botID,
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
	guard := discordguard.New(restClient, settingsStore, log, cfg.PauseAllWrites).WithJournal(journal)
	if cfg.PauseAllWrites {
		log.Warn("MERLIN_PAUSE_ALL_WRITES is set: every destructive Discord action is refused process-wide")
	}

	rotationPlugin := rotation.New(settingsStore,
		func(guildID string) rotation.DiscordChannelOps { return guard.For(guildID) },
		guard.DryRun,
		speaker,
	)
	rolesPlugin := roles.New(roles.NewPostgresStore(db.Pool), settingsStore, scripts.NewPostgresStore(db.Pool),
		func(guildID string) roles.DiscordMemberOps { return guard.For(guildID) },
		guard.DryRun,
		speaker,
		// Reads the gateway cache's voice state. Enrichment only:
		// roles.disconnectFromVoice still force-disconnects unconditionally
		// even when this reports nothing, so a cold or incomplete cache never
		// costs a missed kick, only a less specific log line.
		func(guildID, userID string) (string, bool) {
			g, err1 := snowflake.Parse(guildID)
			u, err2 := snowflake.Parse(userID)
			if err1 != nil || err2 != nil {
				return "", false
			}
			vs, ok := gw.Caches.VoiceState(g, u)
			if !ok || vs.ChannelID == nil {
				return "", false
			}
			return vs.ChannelID.String(), true
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
		func(guildID string) aimod.DiscordOps { return aimod.GuardOps(guard.For(guildID)) },
		cfg.SecretKey,
		speaker,
		granted.MessageContent,
	)
	if err != nil {
		return fmt.Errorf("start ai moderation: %w", err)
	}
	// The sanction ladder prefers this bot's own jail over Discord's timeout.
	// Wired here rather than imported, so aimod depends on the behaviour and
	// not on the package: *roles.Plugin satisfies aimod.Jailer structurally.
	aimodPlugin.WithJailer(rolesPlugin)
	// The supporter role follows cygnet's ledger (cygnet.supporters), wired
	// like WithJailer so roles never imports the fleet link.
	if hub != nil {
		rolesPlugin.WithSupporters(roles.SupportersFunc(func(ctx context.Context) ([]roles.Backer, error) {
			got, err := hub.Supporters(ctx)
			out := make([]roles.Backer, 0, len(got))
			for _, b := range got {
				out = append(out, roles.Backer{UserID: b.User, Until: time.UnixMilli(b.Until)})
			}
			return out, err
		}))
	}
	// /config plugins set aimod false has to stop the scanning too, not just
	// the commands: HandleMessage below is the one plugin entry point the
	// CommandRouter's own gate check never sees. See aimod.PluginGate.
	aimodPlugin.WithGate(settingsStore)
	// The tip jar's rails. Empty maps leave every rail on its compiled-in
	// endpoint and token contract, which is the normal case: an operator only
	// sets these to move off a public RPC or to point a rail somewhere else.
	aimodPlugin.WithFundingChains(cfg.FundingRPCURLs, cfg.FundingContracts)

	// Contests. The Worker URL may be empty, in which case the gallery and
	// voting simply do not exist on this deployment and the Discord half
	// still runs; /contest status says so rather than failing quietly. The
	// sealer is built from the same MERLIN_SECRET_KEY aimod uses, and a nil
	// one means /contest prize refuses to store a prize code rather than
	// storing one in the clear.
	contestSealer, err := secret.New(cfg.SecretKey)
	if err != nil {
		return fmt.Errorf("read MERLIN_SECRET_KEY: %w", err)
	}
	contestPlugin := contest.New(
		contest.NewPostgresStore(db.Pool),
		func(guildID string) contest.DiscordOps { return guard.For(guildID) },
		speaker,
		contestSealer,
		cfg.ContestWorkerURL, cfg.ContestWorkerToken, cfg.ContestLinkKey,
	)

	// Rapsheets. The ledger every moderation action lands in, whoever or
	// whatever decided it. Reads its mod roles off settings for the case-file
	// forum's overwrites and nothing else from it; everything guild-scoped it
	// owns lives in its own tables.
	rapsheetPlugin := rapsheet.New(
		rapsheet.NewPostgresStore(db.Pool),
		func(guildID string) rapsheet.DiscordOps { return guard.For(guildID) },
		settingsStore,
		speaker,
	)
	// /config plugins set rapsheet false has to stop the ledger growing too,
	// not just the commands: bus events and gateway handlers are the entry
	// points the CommandRouter's own gate check never sees.
	rapsheetPlugin.WithGate(settingsStore)
	// The ladder's jail band goes through roles, the same seam aimod's
	// sanction uses; and aimod's own ladder now counts priors from the
	// rapsheet, so a mod's /roles jail last week is not invisible to the
	// escalation deciding this week's sentence. Both structural, neither
	// package imports the other.
	rapsheetPlugin.WithJailer(rolesPlugin)
	aimodPlugin.WithHistory(rapsheetPlugin)
	// Summaries, the weekly consistency review and alt second opinions run
	// on aimod's model, key and budget through one Complete call; a guild
	// with no key gets the plain versions.
	rapsheetPlugin.WithReviewer(aimodPlugin)

	registry := core.NewRegistry(deps, log)
	registry.Register(sched)
	registry.Register(ping.New(speaker))
	registry.Register(rotationPlugin)
	registry.Register(rolesPlugin)
	adminconfigPlugin := adminconfig.New(settingsStore, configPath, db, sched)
	settingsStore.Recovery(adminconfigPlugin.Name())
	registry.Register(aimodPlugin)
	registry.Register(contestPlugin)
	registry.Register(rapsheetPlugin)
	// Channel names come off the gateway cache, the only piece of it this
	// plugin reads; the counting itself is wired below.
	statisticsPlugin := statistics.New(statistics.NewPostgresStore(db.Pool), settingsStore, func(guildID, channelID snowflake.ID) string {
		if ch, ok := gw.Caches.Channel(channelID); ok && ch.GuildID() == guildID {
			return ch.Name()
		}
		return ""
	})
	registry.Register(statisticsPlugin)
	// The server's own message volume feeds aimod's cost projection, so a
	// guild pricing a model stack before it has spent anything is quoted on
	// what it actually does rather than a compiled-in guess. Structural,
	// like WithJailer: aimod never imports statistics.
	aimodPlugin.WithTraffic(statisticsPlugin)
	registry.Register(adminconfigPlugin)
	// Another bird on the link may ask merlin for a role an admin allowed
	// it (/fleet); fleetgrant decides, the link only carries the ask.
	fleetPlugin := fleetgrant.New(fleetgrant.NewPostgresStore(db.Pool), settingsStore,
		func(guildID string) fleetgrant.MemberOps { return guard.For(guildID) })
	registry.Register(fleetPlugin)
	hub.Serve(fleetgrant.Verb, fleetPlugin.Answer)
	// Where cygnet is a member, an attested bird's ask needs no listing.
	fleetPlugin.Vouch(hub.Here)
	registry.Register(perf.New(obs.Default))
	registry.Register(help.New(registry.Plugins, settingsStore))

	if err := registry.InitAll(); err != nil {
		return err
	}
	if err := commands.Finalize(); err != nil {
		return fmt.Errorf("finalize commands: %w", err)
	}
	// What merlin asks servers for: what the plugins in use need now,
	// written to the app's install settings, which Discord's own invite
	// link and cygnet both read. Recomputed a little after anything that
	// could change it, since guilds arrive in a burst at boot; cygnet's
	// manifest waits for the first.
	permissioned := []core.Permissioned{rotationPlugin, rolesPlugin, aimodPlugin, contestPlugin, rapsheetPlugin, statisticsPlugin, adminconfigPlugin}
	install := newInstaller(installSettle, func() {
		var guilds []string
		for g := range gw.Caches.Guilds() {
			guilds = append(guilds, g.ID.String())
		}
		perms := core.Install(guilds, permissioned, settingsStore)
		hub.Describe(cfg.Discord.AppID, uint64(core.GatewayIntents(granted)), uint64(perms), commands.Tiers())
		app, err := restClient.GetCurrentApplication()
		if err != nil {
			log.Warn("reading the install settings", "err", err)
			return
		}
		if wrote, err := core.SyncInstall(restClient, app, perms); err != nil {
			log.Warn("updating the install settings", "err", err)
		} else if wrote {
			log.Info("install settings updated", "permissions", perms)
		}
	})
	bus.Subscribe(core.EventConfigChanged, "install", func(context.Context, core.Event) { install.poke() })
	// On ctx, so a shutdown says goaway to the hub before the gateway closes.
	go hub.Run(ctx)
	// This bot registers commands exclusively per-guild (RegisterGuild below,
	// on every GuildCreate), so nothing here should ever leave a global command
	// behind. Clears any that pre-Milestone-4 code left registered (a REST
	// call, doesn't need session.Open() first).
	if err := commands.PurgeGlobalCommands(appID); err != nil {
		log.Error("purge global commands", "err", err)
	}
	// Plugin icons as application emoji, in the background: one list call
	// when nothing changed, and until it lands every icon goes as text or an
	// attachment, so nothing waits on it.
	go func() {
		if err := core.SyncEmoji(restClient, appID); err != nil {
			log.Warn("sync plugin icons", "err", err)
		}
	}()

	// One dispatcher for every plugin's commands (spec.MD §4a), replacing
	// each plugin listening to the gateway itself. Guild-scoped command
	// registration and settings loading both happen reactively per guild via
	// GuildCreate, not from a static config list: Discord sends one
	// GUILD_CREATE per guild the bot is in, both as the connection comes up
	// and whenever the bot is added to a new guild later, so this one
	// handler covers "known at startup" and "joined while running" the same
	// way, with no restart required for the latter.
	var typed []bot.EventListener
	guildUp := func(g discord.GatewayGuild) {
		gid := g.ID.String()
		defer install.poke()
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
		if err := settingsStore.Refresh(guildCtx, gid); err != nil {
			log.Error("refresh settings for guild, continuing on fail-closed defaults",
				"guild", gid, "err", err)
			settingsStore.MarkStale(gid)
			settingsLoaded = false
		}
		commandsRegistered := true
		if err := commands.RegisterGuild(appID, g.ID); err != nil {
			log.Error("register commands for guild", "guild", gid, "err", err)
			commandsRegistered = false
		}
		rolesPlugin.SyncGuild(gid)
		// Reads aimod's own store rather than internal/settings, so unlike
		// rotation below it does not wait on settingsLoaded: a guild whose
		// settings refresh failed still gets its weekly calibration review.
		aimodPlugin.SyncGuild(guildCtx, gid)
		// Reads its own tables too, so like aimod it does not wait on
		// settingsLoaded: a guild whose settings refresh failed still gets
		// its running contest ticked on to the next phase.
		contestPlugin.SyncGuild(guildCtx, gid)
		rapsheetPlugin.SyncGuild(guildCtx, gid)
		statisticsPlugin.SyncGuild(guildCtx, gid)
		// Whoever is in voice as the guild arrives starts their clock now;
		// GUILD_VOICE_STATES is always requested, so this list is complete.
		statisticsPlugin.SyncVoice(gid, slices.Collect(gw.Caches.VoiceStates(g.ID)))
		if settingsLoaded {
			// Rotation, unlike the sweep, derives which jobs should exist from
			// settings, and reconciling against fail-closed defaults would read as
			// "this guild rotates nothing" and unregister real work. The
			// stale-retry loop publishes EventConfigChanged once the guild is
			// readable again, which is what reconciles it.
			rotationPlugin.SyncGuild(guildCtx, gid)
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
			adminconfigPlugin.NudgeIfUnconfigured(guildCtx, g)
		}
	}
	// Ready is a guild the connection came up with, Join one merlin was
	// added to, Available one back from an outage: Discord sends each as a
	// GUILD_CREATE, and each needs the same wiring.
	typed = append(typed,
		bot.NewListenerFunc(func(e *events.GuildReady) { guildUp(e.Guild) }),
		bot.NewListenerFunc(func(e *events.GuildJoin) { guildUp(e.Guild) }),
		bot.NewListenerFunc(func(e *events.GuildAvailable) { guildUp(e.Guild) }),
	)

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
	if granted.Members {
		log.Info("GUILD_MEMBERS granted: jails re-apply the moment an evader rejoins")
		typed = append(typed, bot.NewListenerFunc(func(e *events.GuildMemberJoin) {
			if e.Member.User.ID == 0 {
				return
			}
			guildID := e.GuildID.String()
			statisticsPlugin.HandleMemberJoin(guildID)
			joinCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			rolesPlugin.HandleMemberJoin(joinCtx, guildID, e.Member.User.ID.String())
			// Compared with the members on record for a possible alt; a hint
			// for a mod, never a link. Takes the whole member: the avatar
			// hash, the names and the join time are the signals.
			rapsheetPlugin.HandleMemberJoin(joinCtx, guildID, e.Member)
		}))
		// Counted, and nothing else: see the note on GuildRoleDelete below for
		// why roles deliberately has no handler here.
		typed = append(typed, bot.NewListenerFunc(func(e *events.GuildMemberLeave) {
			mr := struct{ GuildID string }{e.GuildID.String()}
			statisticsPlugin.HandleMemberLeave(mr.GuildID)
		}))

		// A guild's Onboarding or Membership Screening flow grants a member
		// its configured roles the moment they complete it, arriving here as
		// GUILD_MEMBER_UPDATE, entirely outside anything this bot does. For a
		// member jailed on join, that regrant lands after HandleMemberJoin's
		// strip already ran, so without this a jailed member walks straight
		// back in with whatever onboarding hands out, some of which can carry
		// real server access. reapplyEvadedJails' sweep is the backstop for
		// this too (bounded to one minute), so it still closes even with the
		// intent off, just not instantly.
		typed = append(typed, bot.NewListenerFunc(func(e *events.GuildMemberUpdate) {
			updateCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			rolesPlugin.HandleMemberUpdate(updateCtx, e.GuildID.String(), e.Member.User.ID.String(), core.IDStrings(e.Member.RoleIDs))
		}))
	} else {
		log.Warn("GUILD_MEMBERS not granted in the Developer Portal: " +
			"a jailed member who rejoins keeps full access until the next sweep, and roles regranted by " +
			"a guild's Onboarding/Membership Screening flow after a jail aren't stripped again until then either")
	}

	// Every message is counted (author, channel, hour; never text), which
	// GUILD_MESSAGES delivers without any privileged intent. A map write
	// under a lock.
	onMessage := []func(discord.Message){statisticsPlugin.HandleMessage}
	var onEdit []func(discord.Message)
	// Voice time is booked from channel changes (member, channel, hour;
	// never a session log). Same map write under the same lock. The state
	// before is the cache's, which is how a move is told from a mute.
	typed = append(typed,
		bot.NewListenerFunc(func(e *events.GuildVoiceStateUpdate) {
			statisticsPlugin.HandleVoiceState(e.VoiceState, e.OldVoiceState, e.Member)
		}),
	)

	// Message scanning. Registered only when the intent was actually
	// requested, so the handler's presence always matches reality: without
	// MESSAGE_CONTENT Discord delivers no content to read, and a handler that
	// ran anyway would scan a stream of empty strings and report a healthy,
	// entirely useless filter.
	if granted.MessageContent {
		log.Info("MESSAGE_CONTENT granted: AI moderation can read messages")
		// Returns immediately; the paid work happens on aimod's own batch
		// timer, so a busy server's messages never pile up goroutines
		// waiting on a model.
		onMessage = append(onMessage, aimodPlugin.HandleMessage)
		// Edits, too, and this is not polish. Without it the whole plugin has
		// a one-step bypass: post something innocuous, wait for it to be
		// cleared, then edit it into whatever you like. Nothing looked again.
		onEdit = append(onEdit, aimodPlugin.HandleMessageEdit)
	} else {
		log.Info("MESSAGE_CONTENT not granted: AI moderation will scan nothing. " +
			"Enable Message Content Intent in the Developer Portal to use it.")
	}

	// A channel disappearing under a rotation config is otherwise only
	// noticed as a job that quietly fails for five runs before alerting.
	typed = append(typed, bot.NewListenerFunc(func(e *events.GuildChannelDelete) {
		cd := struct{ GuildID, ID string }{e.GuildID.String(), e.ChannelID.String()}
		if cd.GuildID == "" {
			return
		}
		guildCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		rotationPlugin.HandleChannelDeleted(guildCtx, cd.GuildID, cd.ID)
		// Drops the cached webhook for that channel, which would otherwise
		// keep failing every rewrite until the process restarted.
		aimodPlugin.HandleChannelDeleted(cd.ID)
	}))

	// A new forum post in a contest's forum is an entry. This handler is
	// latency, not correctness: the contest tick re-derives the whole entry
	// list from the forum every minute anyway, so this failing costs a
	// member up to a minute of not seeing their post counted, and never
	// costs them the entry.
	//
	// ThreadCreate carries the thread's id, name and owner, none of which
	// are message-content fields, so it arrives with no privileged intent.
	// The plugin then reads that one starter message over REST. contest
	// deliberately does not join aimod on the gateway firehose.
	typed = append(typed, bot.NewListenerFunc(func(e *events.ThreadCreate) {
		contestPlugin.HandleThreadCreate(e.Thread)
	}))
	// Bans, kicks and timeouts done through the Discord client, not through
	// merlin, so the rapsheet does not lie by omission. Arrives under the
	// always-on GUILD_MODERATION intent, but only if the bot holds View
	// Audit Log in the guild; /rapsheet status says when it does not.
	typed = append(typed, bot.NewListenerFunc(func(ev *events.GuildAuditLogEntryCreate) {
		entryCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		rapsheetPlugin.HandleAuditLogEntry(entryCtx, ev.GuildID.String(), ev.AuditLogEntry)
	}))

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
	typed = append(typed, bot.NewListenerFunc(func(e *events.RoleDelete) {
		rd := struct{ GuildID, RoleID string }{e.GuildID.String(), e.RoleID.String()}
		guildCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		rolesPlugin.HandleRoleDeleted(guildCtx, rd.GuildID, rd.RoleID)

		removed, err := settingsStore.PruneDeletedRole(guildCtx, rd.GuildID, rd.RoleID)
		if err != nil {
			log.Error("prune deleted role from settings", "guild", rd.GuildID, "role", rd.RoleID, "err", err)
			return
		}
		if len(removed) == 0 {
			return
		}
		log.Info("pruned deleted role from settings", "guild", rd.GuildID, "role", rd.RoleID, "from", removed)
		if err := auditWriter.Record(guildCtx, rd.GuildID, botID.String(), "config.role_pruned",
			fmt.Sprintf("role %s referenced by: %s", rd.RoleID, strings.Join(removed, ", ")),
			"role deleted in Discord, references removed"); err != nil {
			// Same log-and-continue policy every other audit call site uses:
			// the prune already happened and is durable, so a failed audit
			// post must not be reported as a failed prune.
			log.Error("audit role prune", "guild", rd.GuildID, "err", err)
		}
	}))

	// An edited role interests the eternal-role script, which answers an
	// edit with a fresh copy of the original (latency only: the roles sweep
	// re-checks every minute regardless), and the join-role script, which
	// reports an edit to a role every new member is given.
	typed = append(typed, bot.NewListenerFunc(func(e *events.RoleUpdate) {
		guildCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		rolesPlugin.HandleRoleUpdated(guildCtx, e.GuildID.String(), e.Role)
	}))

	// Discord sends the same event for "this guild is temporarily
	// unavailable" (an outage on their side) as for "you were removed";
	// disgo tells them apart. Tearing down on an outage would mean an
	// unrelated Discord incident silently stopped rotations.
	typed = append(typed, bot.NewListenerFunc(func(e *events.GuildUnavailable) {
		log.Warn("guild unavailable, keeping jobs registered", "guild", e.GuildID)
	}))
	typed = append(typed, bot.NewListenerFunc(func(e *events.GuildLeave) {
		defer install.poke()
		gd := struct{ ID string }{e.GuildID.String()}
		dropped := sched.UnregisterGuild(gd.ID)
		rotationPlugin.ForgetGuild(gd.ID)
		rolesPlugin.ForgetGuild(gd.ID)
		aimodPlugin.ForgetGuild(gd.ID)
		contestPlugin.ForgetGuild(gd.ID)
		rapsheetPlugin.ForgetGuild(gd.ID)
		statisticsPlugin.ForgetGuild(gd.ID)
		settingsStore.Forget(gd.ID)
		log.Info("left guild, unregistered its jobs", "guild", gd.ID, "jobs", dropped)
	}))

	typed = append(typed,
		bot.NewListenerFunc(func(e *events.MessageCreate) {
			for _, f := range onMessage {
				f(e.Message)
			}
		}),
		bot.NewListenerFunc(func(e *events.MessageUpdate) {
			for _, f := range onEdit {
				f(e.Message)
			}
		}),
	)
	gw.EventManager.AddEventListeners(append(typed,
		bot.NewListenerFunc(commands.OnInteraction),
	)...)
	if err := core.OpenGateway(ctx, gw); err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		gw.Close(closeCtx)
	}()

	if err := registry.StartAll(ctx); err != nil {
		return err
	}

	bus.Publish(ctx, core.Event{Type: core.EventReady})
	log.Info("merlin is running")

	// A portal toggle switched since boot restarts merlin onto it: what she
	// identified with is fixed for the connection's life, and the restart
	// policy brings her back.
	changed := make(chan struct{})
	go watchIntents(ctx, restClient, granted, intentsEvery, changed, log)
	var exit error
	select {
	case <-ctx.Done():
	case <-changed:
		exit = errIntentsChanged
	}
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	registry.ShutdownAll(shutdownCtx)

	return exit
}
