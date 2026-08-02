// Package rotation implements spec.MD §6's "Refresh" channel rotation:
// periodically renaming a channel into a hidden archive (visible only to
// mods, or mods plus a configured whitelist) and replacing it with a clean
// one, reducing the window of retained history a bad-faith actor can trawl
// through for a retroactive mass-report campaign, while preserving a
// moderation trail and staying transparent to members about the retention
// policy.
//
// Configuration is entirely DB-backed (internal/settings) and mutated via
// this plugin's own /rotation configure commands (spec.MD §4a) — never
// config.yaml. Because a guild's set of rotating channels can change at
// runtime, Scheduler job registration isn't a one-time Init-time loop: it's
// reconciled against current settings every time a guild becomes known
// (SyncGuild, called by cmd/bot/main.go on GuildCreate) and every time
// settings change (subscribed via core.EventConfigChanged).
package rotation

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/scheduler"
	"github.com/6586x57890143/merlin/internal/settings"
)

// sweepInterval is how often each guild's archive-deletion sweep runs.
// Retention is denominated in days, so hourly slop against a day-scale
// window is a non-issue — retention_days is documented as a minimum, not
// an exact-to-the-second promise.
const sweepInterval = time.Hour

// SettingsProvider is the narrow slice of internal/settings.Store this
// plugin depends on, so unit tests can use an in-memory fake instead of a
// live Postgres — mirrors the DiscordChannelOps/ArchiveStore seams already
// used in this package.
type SettingsProvider interface {
	ModRoleIDs(guildID string) []string
	RotationChannels(guildID string) []settings.RotationChannel
	RotationChannel(guildID, channelID string) (settings.RotationChannel, bool)
	RotationChannelByID(guildID string, id int64) (settings.RotationChannel, bool)
	UpsertRotationChannel(ctx context.Context, rc settings.RotationChannel) error
	RemoveRotationChannel(ctx context.Context, guildID, channelID string) error
	RetargetRotationChannel(ctx context.Context, guildID, oldChannelID, newChannelID string) error
}

// Plugin implements core.Plugin. It registers slash commands through
// core.CommandRouter and reconciles Scheduler jobs against
// internal/settings' current state — see the package doc for why job
// registration isn't a static Init-time loop.
type Plugin struct {
	ops      DiscordChannelOps
	archives ArchiveStore
	settings SettingsProvider
	audit    core.AuditWriter
	bus      *core.EventBus
	log      *slog.Logger
	sched    core.Scheduler
	commands *core.CommandRouter
	now      func() time.Time

	mu              sync.Mutex
	sweepRegistered map[string]bool          // guild ID -> sweep job registered
	registeredJobs  map[string]time.Duration // rotation job key -> interval it was registered with

	botUserIDMu sync.Mutex
	botUserID   string // cached result of getBotUserID, fetched at most once
}

// New constructs Plugin. settingsStore is passed directly rather than
// through core.Deps — it's a cross-cutting dependency only a couple of
// plugins need (rotation, adminconfig), unlike Deps' fields which every
// plugin gets, mirroring how internal/scheduler and internal/audit already
// take their own narrow settings-derived interfaces as constructor params.
func New(settingsStore SettingsProvider) *Plugin {
	return &Plugin{
		settings:        settingsStore,
		now:             func() time.Time { return time.Now().UTC() },
		sweepRegistered: make(map[string]bool),
		registeredJobs:  make(map[string]time.Duration),
	}
}

func (p *Plugin) Name() string { return "rotation" }

func (p *Plugin) Init(deps core.Deps) error {
	p.ops = deps.Session
	p.audit = deps.Audit
	p.bus = deps.Bus
	p.log = deps.Logger
	p.sched = deps.Scheduler
	p.commands = deps.Commands
	p.archives = NewPostgresArchiveStore(deps.DB.Pool)

	p.registerCommands()

	deps.Bus.Subscribe(core.EventConfigChanged, p.Name(), func(ctx context.Context, ev core.Event) {
		p.reconcile(ev.GuildID)
	})
	return nil
}

func (p *Plugin) Start(ctx context.Context) error { return nil }

func (p *Plugin) Shutdown(ctx context.Context) error { return nil }

// getBotUserID resolves the bot's own user ID, fetched once via the REST
// "@me" endpoint and cached for the process lifetime (it can't change while
// running). Not resolved at Init time — the session isn't open yet then
// (spec.MD's Plugin lifecycle: no gateway/REST calls in Init) — deferred
// until the first rotation actually needs it, well after Open() succeeds.
// A failed attempt is retried on the next call rather than cached, in case
// of a transient API error.
func (p *Plugin) getBotUserID() (string, error) {
	p.botUserIDMu.Lock()
	defer p.botUserIDMu.Unlock()
	if p.botUserID != "" {
		return p.botUserID, nil
	}
	me, err := p.ops.User("@me")
	if err != nil {
		return "", fmt.Errorf("rotation: resolve bird user ID: %w", err)
	}
	p.botUserID = me.ID
	return p.botUserID, nil
}

// SyncGuild reconciles guildID's Scheduler jobs against its current
// settings. Call once per guild right after internal/settings.Store.Refresh
// — at startup for every guild the bot is already in, and again whenever
// the bot joins a new one (both driven by discordgo's GuildCreate event; see
// cmd/bot/main.go).
func (p *Plugin) SyncGuild(guildID string) {
	p.reconcile(guildID)
}

// reconcile registers a Scheduler job for every currently-configured
// rotating channel that doesn't have one yet (or whose interval changed —
// Unregister+Register, since the Scheduler has no in-place spec update),
// unregisters jobs for channels no longer configured, and ensures exactly
// one archive-sweep job per guild.
func (p *Plugin) reconcile(guildID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	sweepKey := scheduler.JobKey(guildID, "rotation-sweep")
	if !p.sweepRegistered[guildID] {
		if err := p.sched.Register(sweepKey, core.CronSpec{Interval: sweepInterval}, p.makeSweepJob(guildID)); err != nil {
			p.log.Error("rotation: register sweep job", "guild", guildID, "err", err)
		} else {
			p.sweepRegistered[guildID] = true
		}
	}

	guildPrefix := scheduler.JobKey(guildID, "rotation:")
	current := make(map[string]bool)
	for _, rc := range p.settings.RotationChannels(guildID) {
		// Keyed by rc.ID, NOT rc.ChannelID: ChannelID gets retargeted onto
		// the new live channel after every successful rotation (execute.go),
		// but the Scheduler persists this job's last-run/interval state under
		// this exact key string — if the key changed every rotation too,
		// that state would reset every cycle, and a job with no run history
		// is immediately due again on the Scheduler's very next tick. This
		// was a real bug: it looped, rotating (and archiving) every ~30s
		// instead of once per interval_hours.
		jobKey := scheduler.JobKey(guildID, "rotation:"+strconv.FormatInt(rc.ID, 10))
		interval := time.Duration(rc.IntervalHours) * time.Hour
		current[jobKey] = true

		if existingInterval, ok := p.registeredJobs[jobKey]; ok {
			if existingInterval == interval {
				continue
			}
			// Interval changed since registration — the Scheduler has no
			// in-place spec update, so drop and re-add.
			if err := p.sched.Unregister(jobKey); err != nil {
				p.log.Error("rotation: unregister job for interval change", "job", jobKey, "err", err)
			}
			delete(p.registeredJobs, jobKey)
		}

		rotationID := rc.ID
		if err := p.sched.Register(jobKey, core.CronSpec{Interval: interval}, p.makeRotationJob(guildID, rotationID)); err != nil {
			p.log.Error("rotation: register job", "job", jobKey, "err", err)
			continue
		}
		p.registeredJobs[jobKey] = interval
	}

	for jobKey := range p.registeredJobs {
		if strings.HasPrefix(jobKey, guildPrefix) && !current[jobKey] {
			if err := p.sched.Unregister(jobKey); err != nil {
				p.log.Error("rotation: unregister removed channel's job", "job", jobKey, "err", err)
			}
			delete(p.registeredJobs, jobKey)
		}
	}
}
