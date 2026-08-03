// Package rotation implements spec.MD §6's "Refresh" channel rotation:
// periodically renaming a channel into a hidden archive (visible only to
// mods, or mods plus a configured whitelist) and replacing it with a clean
// one, reducing the window of retained history a bad-faith actor can trawl
// through for a retroactive mass-report campaign, while preserving a
// moderation trail and staying transparent to members about the retention
// policy.
//
// Configuration is entirely DB-backed (internal/settings) and mutated via
// this plugin's own /rotation configure commands (spec.MD §4a), never
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
	"github.com/6586x57890143/merlin/internal/voice"
)

// sweepInterval is how often each guild's archive-deletion sweep runs.
// Retention is denominated in hours, and retention_hours is documented as a
// minimum, not an exact-to-the-second promise, so hourly sweep slop against
// even a 1-hour retention window is acceptable.
const sweepInterval = time.Hour

// SettingsProvider is the narrow slice of internal/settings.Store this
// plugin depends on, so unit tests can use an in-memory fake instead of a
// live Postgres. Mirrors the DiscordChannelOps/ArchiveStore seams already
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
// internal/settings' current state. See the package doc for why job
// registration isn't a static Init-time loop.
type Plugin struct {
	ops      OpsProvider
	dryRun   func(guildID string) bool
	archives ArchiveStore
	settings SettingsProvider
	audit    core.AuditWriter
	bus      *core.EventBus
	log      *slog.Logger
	sched    core.Scheduler
	commands *core.CommandRouter
	now      func() time.Time
	// voice supplies the member-facing wording. An interface rather than
	// the concrete catalog so a generator can be swapped in later without
	// this plugin knowing (see internal/voice).
	voice voice.Source

	notices NoticeStore

	mu               sync.Mutex
	sweepRegistered  map[string]bool          // guild ID -> sweep job registered
	noticeRegistered map[string]bool          // guild ID -> pre-rotation notice job registered
	registeredJobs   map[string]time.Duration // rotation job key -> interval it was registered with

	botUserIDMu sync.Mutex
	botUserID   string // cached result of getBotUserID, fetched at most once
}

// New constructs Plugin. settingsStore is passed directly rather than
// through core.Deps: it's a cross-cutting dependency only a couple of
// plugins need (rotation, adminconfig), unlike Deps' fields which every
// plugin gets, mirroring how internal/scheduler and internal/audit already
// take their own narrow settings-derived interfaces as constructor params.
// OpsProvider yields the Discord ops view for one guild. It is a per-guild
// lookup rather than a single shared value because internal/discordguard
// binds each view to the guild whose pause/dry-run settings govern it.
// Most destructive Discord calls are channel-scoped and carry no guild of
// their own, so the guild has to come from the caller, which always knows it.
type OpsProvider func(guildID string) DiscordChannelOps

// rotationJobName is the per-slot half of a rotation job's Scheduler key.
// Keyed on the stable settings row ID, never the channel ID, because
// rotate() retargets a slot's channel every cycle and a key that moved with
// it would reset the job's persisted last-run state each time (migration
// 0009).
func rotationJobName(rotationID int64) string {
	return "rotation:" + strconv.FormatInt(rotationID, 10)
}

func New(settingsStore SettingsProvider, ops OpsProvider, dryRun func(guildID string) bool, speaker voice.Source) *Plugin {
	return &Plugin{
		settings:         settingsStore,
		ops:              ops,
		dryRun:           dryRun,
		voice:            speaker,
		now:              func() time.Time { return time.Now().UTC() },
		sweepRegistered:  make(map[string]bool),
		noticeRegistered: make(map[string]bool),
		registeredJobs:   make(map[string]time.Duration),
	}
}

func (p *Plugin) Name() string { return "rotation" }

func (p *Plugin) Init(deps core.Deps) error {
	p.audit = deps.Audit
	p.bus = deps.Bus
	p.log = deps.Logger
	p.sched = deps.Scheduler
	p.commands = deps.Commands
	p.archives = NewPostgresArchiveStore(deps.DB.Pool)
	p.notices = NewPostgresNoticeStore(deps.DB.Pool)

	p.registerCommands()

	deps.Bus.Subscribe(core.EventConfigChanged, p.Name(), func(ctx context.Context, ev core.Event) {
		p.reconcile(ctx, ev.GuildID)
	})
	return nil
}

func (p *Plugin) Start(ctx context.Context) error { return nil }

func (p *Plugin) Shutdown(ctx context.Context) error { return nil }

// getBotUserID resolves the bot's own user ID, fetched once via the REST
// "@me" endpoint and cached for the process lifetime (it can't change while
// running). Not resolved at Init time, since the session isn't open yet then
// (spec.MD's Plugin lifecycle: no gateway/REST calls in Init). Deferred
// until the first rotation actually needs it, well after Open() succeeds.
// A failed attempt is retried on the next call rather than cached, in case
// of a transient API error.
func (p *Plugin) getBotUserID(guildID string) (string, error) {
	p.botUserIDMu.Lock()
	defer p.botUserIDMu.Unlock()
	if p.botUserID != "" {
		return p.botUserID, nil
	}
	me, err := p.ops(guildID).User("@me")
	if err != nil {
		return "", fmt.Errorf("rotation: resolve bird user ID: %w", err)
	}
	p.botUserID = me.ID
	return p.botUserID, nil
}

// SyncGuild reconciles guildID's Scheduler jobs against its current
// settings. Call once per guild right after internal/settings.Store.Refresh:
// at startup for every guild the bot is already in, and again whenever
// the bot joins a new one (both driven by discordgo's GuildCreate event; see
// cmd/bot/main.go).
func (p *Plugin) SyncGuild(ctx context.Context, guildID string) {
	p.reconcile(ctx, guildID)
}

// HandleChannelDeleted reports that a channel this guild rotates was deleted
// out from under the configuration.
//
// The rotation job for it will now fail on every run, because the channel it is
// configured against no longer exists, and the Scheduler's own backoff and
// consecutive-failure alert will eventually say so. But that alert arrives
// after five failures and names a job key, not a channel, and by then
// whoever deleted the channel has long since moved on. Saying it once,
// immediately, in the audit log, is the difference between "a mod deleted
// #general-chat this morning" and an unexplained wedged job.
//
// The configuration is deliberately left in place. Removing a rotation slot
// discards the archive retention it promised, and this event can't
// distinguish "we're done with this channel" from "someone deleted the wrong
// thing and wants it back". That call belongs to an admin, via
// /rotation configure remove.
func (p *Plugin) HandleChannelDeleted(ctx context.Context, guildID, channelID string) {
	if _, ok := p.settings.RotationChannel(guildID, channelID); !ok {
		return
	}
	p.log.Warn("rotation: configured rotating channel was deleted", "guild", guildID, "channel", channelID)
	if err := p.audit.Record(ctx, guildID, core.ActorSystem, "rotation.channel_deleted", core.MentionChannel(channelID),
		"the channel this rotation is configured against was deleted; rotation will fail until it is reconfigured with /rotation configure"); err != nil {
		p.log.Error("rotation: audit deleted channel", "guild", guildID, "err", err)
	}
}

// ForgetGuild drops guildID's job-registration bookkeeping after the bot has
// been removed from it. The Scheduler jobs themselves are unregistered by the
// caller (cmd/bot/main.go's GuildDelete handler); this clears the maps that
// track what *is* registered, so that if the bot is later re-added, reconcile
// sees an empty slate and registers everything again rather than believing
// jobs it no longer has are still in place.
func (p *Plugin) ForgetGuild(guildID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sweepRegistered, guildID)
	prefix := scheduler.JobKey(guildID, "")
	for jobKey := range p.registeredJobs {
		if strings.HasPrefix(jobKey, prefix) {
			delete(p.registeredJobs, jobKey)
		}
	}
}

// deferFirstRotation seeds channelID's rotation job as if it had just run,
// so a channel a mod just added to rotation waits a full interval before its
// first real rotation instead of firing on the Scheduler's very next tick.
// A brand-new job has no persisted last-run state, and the Scheduler treats
// that as immediately due: correct for jobs like rotation-sweep that should
// start working right away, wrong here (a channel that was just configured
// shouldn't rotate before its interval has even elapsed once).
//
// Must be called only for a channel just added via handleAdd, never from
// reconcile itself: reconcile also runs on every restart for channels that
// already have real run history, and seeding those would incorrectly reset
// an overdue rotation's clock. Only the caller that just inserted a brand
// new row knows that distinction. reconcile alone can't tell "never
// registered in this process" apart from "genuinely never run."
//
// This runs after UpsertRotationChannel's own synchronous EventConfigChanged
// publish has already registered the job (Init's bus subscription calls
// reconcile), so there's a narrow window, bounded by the Scheduler's 30s
// tick, where a tick could fire before this seed lands. Accepting that tiny
// window over adding cross-package plumbing to close it matches this
// package's existing tolerance for narrow timing windows (see rotate's
// dual-visible-channel window and its rationale).
func (p *Plugin) deferFirstRotation(ctx context.Context, guildID, channelID string) {
	rc, ok := p.settings.RotationChannel(guildID, channelID)
	if !ok {
		return
	}
	jobKey := scheduler.JobKey(guildID, rotationJobName(rc.ID))
	if err := p.sched.Seed(ctx, jobKey, p.now()); err != nil {
		p.log.Error("rotation: defer first run for new channel", "job", jobKey, "err", err)
	}
}

// reconcileSweepJob keeps guildID's archive-sweep job registered exactly when
// there is something it could legitimately act on.
//
// This job permanently deletes channels, so it deliberately does not exist by
// default: it used to be registered for every guild the bot could see, the
// moment it saw it, whether or not that guild had ever configured rotation:
// a deletion job armed in servers that never opted into one, and (since a job
// with no run history is immediately due) firing within one Scheduler tick of
// startup. It was harmless only by accident, because the table it reads
// happened to be empty.
//
// hasRotation alone isn't sufficient to *unregister*: /rotation configure
// remove explicitly promises existing archives are left untouched, so a guild
// with no rotating channels can still have archives whose retention windows
// are still owed. Pending archives therefore keep the job alive on their own.
// A failed lookup leaves the current registration exactly as it is, neither
// arming a sweep we can't justify nor dropping one that's still owed work.
func (p *Plugin) reconcileSweepJob(ctx context.Context, guildID string, hasRotation bool) {
	registered := p.sweepRegistered[guildID]

	needed := hasRotation
	if !needed {
		// len() of the same rows the sweep itself reads, rather than a
		// dedicated COUNT: reconcile runs on config changes and GuildCreate,
		// not on a tick, and a guild's archive set is bounded by rotation
		// frequency times retention.
		archives, err := p.archives.ListForGuild(ctx, guildID)
		if err != nil {
			p.log.Error("rotation: check pending archives for sweep job", "guild", guildID, "err", err)
			return
		}
		needed = len(archives) > 0
	}

	sweepKey := scheduler.JobKey(guildID, "rotation-sweep")
	switch {
	case needed && !registered:
		if err := p.sched.Register(sweepKey, core.CronSpec{Schedule: core.IntervalSchedule{Interval: sweepInterval}}, p.makeSweepJob(guildID)); err != nil {
			p.log.Error("rotation: register sweep job", "guild", guildID, "err", err)
			return
		}
		p.sweepRegistered[guildID] = true
	case !needed && registered:
		if err := p.sched.Unregister(sweepKey); err != nil {
			p.log.Error("rotation: unregister sweep job", "guild", guildID, "err", err)
			return
		}
		delete(p.sweepRegistered, guildID)
	}
}

// reconcile registers a Scheduler job for every currently-configured
// rotating channel that doesn't have one yet (or whose interval changed:
// Unregister+Register, since the Scheduler has no in-place spec update),
// unregisters jobs for channels no longer configured, and ensures exactly
// one archive-sweep job per guild.
func (p *Plugin) reconcile(ctx context.Context, guildID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	channels := p.settings.RotationChannels(guildID)
	p.reconcileSweepJob(ctx, guildID, len(channels) > 0)
	p.reconcileNoticeJob(guildID)

	guildPrefix := scheduler.JobKey(guildID, "rotation:")
	current := make(map[string]bool)
	for _, rc := range channels {
		// Keyed by rc.ID, NOT rc.ChannelID: ChannelID gets retargeted onto
		// the new live channel after every successful rotation (execute.go),
		// but the Scheduler persists this job's last-run/interval state under
		// this exact key string. If the key changed every rotation too,
		// that state would reset every cycle, and a job with no run history
		// is immediately due again on the Scheduler's very next tick. This
		// was a real bug: it looped, rotating (and archiving) every ~30s
		// instead of once per interval_hours.
		jobKey := scheduler.JobKey(guildID, rotationJobName(rc.ID))
		interval := time.Duration(rc.IntervalMinutes) * time.Minute
		current[jobKey] = true

		if existingInterval, ok := p.registeredJobs[jobKey]; ok {
			if existingInterval == interval {
				continue
			}
			// Interval changed since registration: the Scheduler has no
			// in-place spec update, so drop and re-add.
			if err := p.sched.Unregister(jobKey); err != nil {
				p.log.Error("rotation: unregister job for interval change", "job", jobKey, "err", err)
			}
			delete(p.registeredJobs, jobKey)
		}

		rotationID := rc.ID
		if err := p.sched.Register(jobKey, core.CronSpec{Schedule: core.IntervalSchedule{Interval: interval}}, p.makeRotationJob(guildID, rotationID)); err != nil {
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
