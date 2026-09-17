// Package rapsheet is merlin's per-member moderation ledger (spec.MD §9,
// Milestone 12).
//
// Every warning, jail, timeout, kick, ban and aimod removal a member collects
// in a guild lands here as one entry with a case number, whoever or whatever
// decided it, and the sheet is what a moderator reads before deciding the
// next one. Entries carry points that decay over a guild-configured
// half-life; the score they sum to drives an escalation ladder that can
// suggest, or in a guild that has chosen to trust it, apply the next
// consequence. Accounts a moderator has linked share one sheet and one score.
//
// The plugin never imports roles or aimod. Their actions arrive over
// core.EventBus, and the jail it applies goes out through a narrow Jailer
// interface wired in cmd/bot/main.go, exactly as aimod reaches roles.
package rapsheet

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/voice"
)

// DiscordOps is the narrow slice of Discord this plugin touches.
// *discordguard.GuildOps satisfies it structurally, which is what keeps
// every destructive call behind the pause/dry-run/rate-limit gate without
// this package importing the guard.
type DiscordOps interface {
	Guild(guildID string, options ...discordgo.RequestOption) (*discordgo.Guild, error)
	// GuildMember is a live REST fetch, for the same reason roles insists on
	// one: the rank check that decides whether a target may be touched has to
	// see their actual roles, not a cached snapshot.
	GuildMember(guildID, userID string, options ...discordgo.RequestOption) (*discordgo.Member, error)
	User(userID string, options ...discordgo.RequestOption) (*discordgo.User, error)
	UserChannelCreate(recipientID string, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error)
	// The case-file forum (casefile.go): finding or creating it, opening a
	// post per member, and editing a mirrored entry in place.
	Channel(channelID string, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	GuildChannels(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Channel, error)
	GuildChannelCreateComplex(guildID string, data discordgo.GuildChannelCreateData, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	ForumThreadStartComplex(channelID string, threadData *discordgo.ThreadStart, messageData *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	ChannelMessageEditComplex(m *discordgo.MessageEdit, options ...discordgo.RequestOption) (*discordgo.Message, error)
	// The consequences this plugin applies itself (bans.go). Jail is not
	// here: it stays roles' primitive, reached through Jailer.
	GuildMemberTimeout(guildID, userID string, until *time.Time, options ...discordgo.RequestOption) error
	GuildBanCreateWithReason(guildID, userID, reason string, days int, options ...discordgo.RequestOption) error
	GuildBanDelete(guildID, userID string, options ...discordgo.RequestOption) error
	GuildMemberDeleteWithReason(guildID, userID, reason string, options ...discordgo.RequestOption) error
	// GuildRoles backs the View Audit Log check on /rapsheet status.
	GuildRoles(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Role, error)
}

// OpsProvider hands back the guild-bound Discord view. Mirrors
// rotation.OpsProvider and roles' equivalent.
type OpsProvider func(guildID string) DiscordOps

// Ranker is the narrow view of *core.Permissions this plugin depends on:
// the actor-versus-target rank check every restrictive action runs first,
// the bootstrap carve-out, and Authorize for the one button whose real bar
// is higher than the component's registered tier (applying a ban).
type Ranker interface {
	CanModerate(guildID string, actor *discordgo.Member, targetUserID string, targetRoleIDs []string) error
	IsBootstrapAdmin(userID string) bool
	Authorize(i *discordgo.InteractionCreate, spec core.PermSpec) error
}

// ModRoles is the one thing this plugin needs from internal/settings: who
// may see the case-file forum. *settings.Store satisfies it structurally.
type ModRoles interface {
	ModRoleIDs(guildID string) []string
}

// Plugin is the rapsheet plugin.
type Plugin struct {
	store    Store
	opsFor   OpsProvider
	speaker  voice.Source
	modRoles ModRoles

	// jailer is optional: the roles plugin, when wired, so jail bands can be
	// applied. Nil means they cannot, and say so.
	jailer Jailer
	// reviewer is optional: aimod's model, when wired, for summaries, the
	// weekly review and alt opinions. Nil means every one of those degrades
	// to its plain form.
	reviewer Reviewer

	// gate answers "is rapsheet enabled in this guild" for the paths the
	// CommandRouter's own check never sees: bus events and gateway handlers.
	// Nil means always enabled, which is what tests want and production
	// never has.
	gate core.PluginGate

	audit    core.AuditWriter
	log      *slog.Logger
	sched    core.Scheduler
	commands *core.CommandRouter
	perms    Ranker
	bus      *core.EventBus

	// now is injected so tests can drive decay and expiry without waiting,
	// mirroring the Scheduler's own hook.
	now func() time.Time

	// synchronous makes the detached work (bus-driven writes, the case-file
	// mirror) happen on the caller's goroutine instead of its own. Tests
	// only: production leaves it false so a slow forum post never holds a
	// command or roles' sweep.
	synchronous bool

	mu               sync.Mutex
	botID            string
	sweepRegistered  map[string]bool
	reviewRegistered map[string]bool
	// applying holds the suggestions being applied right now, so two mods
	// clicking Apply together produce one consequence.
	applying map[int64]bool
}

// New builds the plugin.
func New(store Store, opsFor OpsProvider, modRoles ModRoles, speaker voice.Source) *Plugin {
	return &Plugin{
		store:    store,
		opsFor:   opsFor,
		modRoles: modRoles,
		speaker:  speaker,
		now:      time.Now,

		sweepRegistered:  make(map[string]bool),
		reviewRegistered: make(map[string]bool),
		applying:         make(map[int64]bool),
	}
}

// WithGate wires the per-guild plugin toggle. Without it, /config plugins
// set rapsheet false would stop the commands and nothing else: entries
// would still arrive over the bus and the ledger would go on growing in a
// guild that turned it off.
func (p *Plugin) WithGate(gate core.PluginGate) *Plugin {
	p.gate = gate
	return p
}

func (p *Plugin) Name() string { return "rapsheet" }

func (p *Plugin) Init(deps core.Deps) error {
	p.audit = deps.Audit
	p.log = deps.Logger
	p.sched = deps.Scheduler
	p.commands = deps.Commands
	p.perms = deps.Perms
	p.bus = deps.Bus
	if deps.Session != nil && deps.Session.State != nil && deps.Session.State.User != nil {
		p.botID = deps.Session.State.User.ID
	}
	p.registerCommands()
	p.subscribe()
	return nil
}

func (p *Plugin) Start(context.Context) error    { return nil }
func (p *Plugin) Shutdown(context.Context) error { return nil }

// SyncGuild is called from cmd/bot/main.go on every GuildCreate. It reads
// this plugin's own tables, not internal/settings, so it does not wait on
// the settings refresh: a guild whose settings failed to load still gets
// its expiring bans lifted.
func (p *Plugin) SyncGuild(ctx context.Context, guildID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reconcileSweepJob(ctx, guildID)
	p.reconcileReviewJob(ctx, guildID)
}

// ForgetGuild drops bookkeeping when merlin leaves a guild. The rows stay:
// a re-invite finds the sheets where they were.
func (p *Plugin) ForgetGuild(guildID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sweepRegistered[guildID] {
		delete(p.sweepRegistered, guildID)
		if err := p.sched.Unregister(sweepKey(guildID)); err != nil {
			p.log.Error("rapsheet: unregister sweep job", "guild", guildID, "err", err)
		}
	}
	if p.reviewRegistered[guildID] {
		delete(p.reviewRegistered, guildID)
		if err := p.sched.Unregister(reviewKey(guildID)); err != nil {
			p.log.Error("rapsheet: unregister review job", "guild", guildID, "err", err)
		}
	}
}

func (p *Plugin) ops(guildID string) DiscordOps { return p.opsFor(guildID) }

// config reads the guild's configuration, falling back to the defaults on
// a read failure rather than refusing the command: nothing in the defaults
// acts on anybody (suggest mode posts to an empty mod channel, which is to
// say nowhere), so a transient database error costs a stale setting and
// never a wrong action.
func (p *Plugin) config(ctx context.Context, guildID string) Config {
	cfg, err := p.store.Config(ctx, guildID)
	if err != nil {
		p.log.Error("rapsheet: read config, using defaults", "guild", guildID, "err", err)
		return defaultConfig(guildID)
	}
	return cfg
}
