// Package discordguard is the single chokepoint every destructive Discord
// call passes through.
//
// The bot's dangerous operations are irreversible from Discord's side: a
// permanently deleted archive channel, a member stripped of every role, a
// permission overwrite written across an entire guild. Before this package
// existed there was no way to stop any of them short of pushing a commit and
// waiting for a deploy. Guard adds two controls that work from inside
// Discord, at the speed an incident actually moves:
//
//   - pause — refuse every mutating call, leaving reads and inspection
//     commands working so an admin can still see what the bot believes.
//   - dry-run — let a rotation or sweep make its full decision and write its
//     full audit trail while touching nothing, so a feature whose failure
//     mode has no undo can be rehearsed against the real guild first.
//
// It hooks in where the plugins already had a seam. Each mutating plugin
// defines a narrow ops interface (rotation.DiscordChannelOps,
// roles.DiscordMemberOps) that *discordgo.Session satisfies structurally,
// and assigns it once during Init. GuildOps implements the union of those
// interfaces, so substituting it at that one assignment guards every call
// site at once — and a future plugin that follows the same convention is
// covered without having to remember a flag check.
package discordguard

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
)

// ErrPaused and ErrDryRun report that a mutating call was deliberately not
// made. Callers must distinguish them from a genuine failure: a paused guild
// has nothing wrong with it, so a Scheduler job that hits one has to return
// success or it will burn down its consecutive-failure budget and alert
// #bird-status about an operator's own deliberate choice.
var (
	ErrPaused = errors.New("discordguard: destructive actions are paused")
	ErrDryRun = errors.New("discordguard: dry-run mode, no Discord call made")
)

// Skipped reports whether err means "deliberately not done" rather than
// "failed". Scheduler jobs should translate a Skipped error into a nil
// return; see rotation and roles' job wrappers.
func Skipped(err error) bool {
	return errors.Is(err, ErrPaused) || errors.Is(err, ErrDryRun)
}

// GuildGate is the narrow slice of internal/settings.Store this package
// needs — the per-guild half of the controls. Reads come from the same
// in-memory cache as every other setting, so a pause takes effect on the
// next call with no database round trip.
type GuildGate interface {
	WritesPaused(guildID string) bool
	WritesDryRun(guildID string) bool
}

// Session is the union of the REST methods the mutating plugins use.
// *discordgo.Session satisfies it structurally, exactly as it satisfies the
// per-plugin interfaces this one is assembled from.
type Session interface {
	Channel(channelID string, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	GuildChannels(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Channel, error)
	ThreadsActive(channelID string, options ...discordgo.RequestOption) (*discordgo.ThreadsList, error)
	GuildChannelCreateComplex(guildID string, data discordgo.GuildChannelCreateData, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	ChannelEditComplex(channelID string, data *discordgo.ChannelEdit, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	ChannelDelete(channelID string, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	ChannelMessages(channelID string, limit int, beforeID, afterID, aroundID string, options ...discordgo.RequestOption) ([]*discordgo.Message, error)
	ChannelMessageSend(channelID, content string, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessageSendEmbed(channelID string, embed *discordgo.MessageEmbed, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessagePin(channelID, messageID string, options ...discordgo.RequestOption) error
	ChannelPermissionSet(channelID, targetID string, targetType discordgo.PermissionOverwriteType, allow, deny int64, options ...discordgo.RequestOption) error
	ChannelPermissionDelete(channelID, targetID string, options ...discordgo.RequestOption) error
	User(userID string, options ...discordgo.RequestOption) (*discordgo.User, error)
	GuildMember(guildID, userID string, options ...discordgo.RequestOption) (*discordgo.Member, error)
	GuildMembers(guildID string, after string, limit int, options ...discordgo.RequestOption) ([]*discordgo.Member, error)
	GuildMemberEdit(guildID, userID string, data *discordgo.GuildMemberParams, options ...discordgo.RequestOption) (*discordgo.Member, error)
	GuildMemberRoleAdd(guildID, userID, roleID string, options ...discordgo.RequestOption) error
	GuildMemberRoleRemove(guildID, userID, roleID string, options ...discordgo.RequestOption) error
	GuildRoles(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Role, error)
	GuildRoleCreate(guildID string, data *discordgo.RoleParams, options ...discordgo.RequestOption) (*discordgo.Role, error)
}

// Guard holds the process-wide stop and the per-guild gate. One is
// constructed in cmd/bot/main.go and shared by every plugin.
type Guard struct {
	session Session
	gate    GuildGate
	log     *slog.Logger
	global  atomic.Bool
	journal Journal
	// now is injectable so the governor's time-based behavior (bucket refill,
	// breaker cooldown) is testable without sleeping.
	now func() time.Time

	rateMu   sync.Mutex
	buckets  map[string]*bucket  // guildID:op -> token bucket
	breakers map[string]*breaker // guildID -> circuit breaker
}

func New(session Session, gate GuildGate, log *slog.Logger, pauseAllWrites bool) *Guard {
	g := &Guard{
		session:  session,
		gate:     gate,
		log:      log,
		now:      func() time.Time { return time.Now().UTC() },
		buckets:  make(map[string]*bucket),
		breakers: make(map[string]*breaker),
	}
	g.global.Store(pauseAllWrites)
	return g
}

// PauseAll engages or releases the process-wide stop. Set from
// MERLIN_PAUSE_ALL_WRITES at startup; exposed as a method so a future
// operator command can reach it without a restart.
func (g *Guard) PauseAll(paused bool) { g.global.Store(paused) }

// Paused reports whether guildID's destructive actions are currently
// refused, by either the process-wide stop or the guild's own setting.
func (g *Guard) Paused(guildID string) bool {
	return g.global.Load() || g.gate.WritesPaused(guildID)
}

// DryRun reports whether guildID is rehearsing rather than acting.
//
// Multi-step operations must check this themselves before they start:
// rotation's create/populate/reveal/archive sequence needs coherent
// intermediate state that a per-call wrapper cannot synthesize, so it
// short-circuits at the top of rotate() instead of discovering dry-run
// halfway through and having to unwind. GuildOps still refuses every
// individual write underneath, so forgetting that check costs a clean
// refusal, never a mutation.
func (g *Guard) DryRun(guildID string) bool {
	return g.gate.WritesDryRun(guildID)
}

// For returns an ops view bound to guildID. The binding is explicit rather
// than resolved from a channel ID at call time: most destructive calls are
// channel-scoped and carry no guild, and inferring one from discordgo's
// gateway cache would make the emergency stop depend on that cache being
// populated — including for a channel the bot created moments earlier, whose
// ChannelCreate event may not have arrived yet.
func (g *Guard) For(guildID string) *GuildOps {
	return &GuildOps{guard: g, guildID: guildID}
}

// GuildOps implements rotation.DiscordChannelOps and roles.DiscordMemberOps
// for one guild. Reads delegate straight through; writes go through allow.
type GuildOps struct {
	guard   *Guard
	guildID string
}

// allow is the one place a destructive call is permitted or refused.
//
// Order matters: pause and dry-run are checked before the rate cap and
// breaker, so a guild that is deliberately stopped neither spends its budget
// nor trips the breaker on calls it never made.
func (o *GuildOps) allow(op string) error {
	if o.guard.Paused(o.guildID) {
		o.guard.log.Warn("discordguard: refused write, paused", "guild", o.guildID, "op", op)
		return fmt.Errorf("%s: %w", op, ErrPaused)
	}
	if o.guard.DryRun(o.guildID) {
		o.guard.log.Info("discordguard: skipped write, dry-run", "guild", o.guildID, "op", op)
		return fmt.Errorf("%s: %w", op, ErrDryRun)
	}
	return o.guard.allowRate(o.guildID, op, o.guard.now())
}

// record feeds the outcome of a permitted call to the guild's circuit
// breaker and its journal entry, and passes the error straight through so
// call sites stay a single return statement.
func (o *GuildOps) record(journalID int64, err error) error {
	o.guard.recordResult(o.guildID, o.guard.now(), err)
	o.finishJournal(journalID, err)
	return err
}

// --- reads: never gated ---

func (o *GuildOps) Channel(channelID string, options ...discordgo.RequestOption) (*discordgo.Channel, error) {
	return o.guard.session.Channel(channelID, options...)
}

func (o *GuildOps) GuildChannels(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Channel, error) {
	return o.guard.session.GuildChannels(guildID, options...)
}

func (o *GuildOps) ThreadsActive(channelID string, options ...discordgo.RequestOption) (*discordgo.ThreadsList, error) {
	return o.guard.session.ThreadsActive(channelID, options...)
}

func (o *GuildOps) ChannelMessages(channelID string, limit int, beforeID, afterID, aroundID string, options ...discordgo.RequestOption) ([]*discordgo.Message, error) {
	return o.guard.session.ChannelMessages(channelID, limit, beforeID, afterID, aroundID, options...)
}

func (o *GuildOps) User(userID string, options ...discordgo.RequestOption) (*discordgo.User, error) {
	return o.guard.session.User(userID, options...)
}

func (o *GuildOps) GuildMember(guildID, userID string, options ...discordgo.RequestOption) (*discordgo.Member, error) {
	return o.guard.session.GuildMember(guildID, userID, options...)
}

// GuildMembers pages the guild's member list. A read, so ungated like the
// rest — but note it is the one read here whose cost scales with guild size,
// and it requires the privileged GUILD_MEMBERS intent to return anything.
func (o *GuildOps) GuildMembers(guildID string, after string, limit int, options ...discordgo.RequestOption) ([]*discordgo.Member, error) {
	return o.guard.session.GuildMembers(guildID, after, limit, options...)
}

func (o *GuildOps) GuildRoles(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Role, error) {
	return o.guard.session.GuildRoles(guildID, options...)
}

// --- writes: gated ---

func (o *GuildOps) GuildChannelCreateComplex(guildID string, data discordgo.GuildChannelCreateData, options ...discordgo.RequestOption) (*discordgo.Channel, error) {
	if err := o.allow(opChannelCreate); err != nil {
		return nil, err
	}
	jid := o.beginJournal(opChannelCreate, data.Name)
	v, err := o.guard.session.GuildChannelCreateComplex(guildID, data, options...)
	return v, o.record(jid, err)
}

func (o *GuildOps) ChannelEditComplex(channelID string, data *discordgo.ChannelEdit, options ...discordgo.RequestOption) (*discordgo.Channel, error) {
	if err := o.allow(opChannelEdit); err != nil {
		return nil, err
	}
	jid := o.beginJournal(opChannelEdit, channelID)
	v, err := o.guard.session.ChannelEditComplex(channelID, data, options...)
	return v, o.record(jid, err)
}

func (o *GuildOps) ChannelDelete(channelID string, options ...discordgo.RequestOption) (*discordgo.Channel, error) {
	if err := o.allow(opChannelDelete); err != nil {
		return nil, err
	}
	jid := o.beginJournal(opChannelDelete, channelID)
	v, err := o.guard.session.ChannelDelete(channelID, options...)
	return v, o.record(jid, err)
}

func (o *GuildOps) ChannelMessageSend(channelID, content string, options ...discordgo.RequestOption) (*discordgo.Message, error) {
	if err := o.allow(opMessageSend); err != nil {
		return nil, err
	}
	jid := o.beginJournal(opMessageSend, channelID)
	v, err := o.guard.session.ChannelMessageSend(channelID, content, options...)
	return v, o.record(jid, err)
}

func (o *GuildOps) ChannelMessageSendEmbed(channelID string, embed *discordgo.MessageEmbed, options ...discordgo.RequestOption) (*discordgo.Message, error) {
	if err := o.allow(opMessageSend); err != nil {
		return nil, err
	}
	jid := o.beginJournal(opMessageSend, channelID)
	v, err := o.guard.session.ChannelMessageSendEmbed(channelID, embed, options...)
	return v, o.record(jid, err)
}

func (o *GuildOps) ChannelMessagePin(channelID, messageID string, options ...discordgo.RequestOption) error {
	if err := o.allow(opMessagePin); err != nil {
		return err
	}
	jid := o.beginJournal(opMessagePin, channelID)
	return o.record(jid, o.guard.session.ChannelMessagePin(channelID, messageID, options...))
}

func (o *GuildOps) ChannelPermissionSet(channelID, targetID string, targetType discordgo.PermissionOverwriteType, allow, deny int64, options ...discordgo.RequestOption) error {
	if err := o.allow(opChannelPermissions); err != nil {
		return err
	}
	jid := o.beginJournal(opChannelPermissions, channelID)
	return o.record(jid, o.guard.session.ChannelPermissionSet(channelID, targetID, targetType, allow, deny, options...))
}

func (o *GuildOps) ChannelPermissionDelete(channelID, targetID string, options ...discordgo.RequestOption) error {
	if err := o.allow(opChannelPermissions); err != nil {
		return err
	}
	jid := o.beginJournal(opChannelPermissions, channelID)
	return o.record(jid, o.guard.session.ChannelPermissionDelete(channelID, targetID, options...))
}

func (o *GuildOps) GuildMemberEdit(guildID, userID string, data *discordgo.GuildMemberParams, options ...discordgo.RequestOption) (*discordgo.Member, error) {
	if err := o.allow(opMemberEdit); err != nil {
		return nil, err
	}
	jid := o.beginJournal(opMemberEdit, userID)
	v, err := o.guard.session.GuildMemberEdit(guildID, userID, data, options...)
	return v, o.record(jid, err)
}

func (o *GuildOps) GuildMemberRoleAdd(guildID, userID, roleID string, options ...discordgo.RequestOption) error {
	if err := o.allow(opMemberRoleAdd); err != nil {
		return err
	}
	jid := o.beginJournal(opMemberRoleAdd, userID)
	return o.record(jid, o.guard.session.GuildMemberRoleAdd(guildID, userID, roleID, options...))
}

func (o *GuildOps) GuildMemberRoleRemove(guildID, userID, roleID string, options ...discordgo.RequestOption) error {
	if err := o.allow(opMemberRoleRemove); err != nil {
		return err
	}
	jid := o.beginJournal(opMemberRoleRemove, userID)
	return o.record(jid, o.guard.session.GuildMemberRoleRemove(guildID, userID, roleID, options...))
}

func (o *GuildOps) GuildRoleCreate(guildID string, data *discordgo.RoleParams, options ...discordgo.RequestOption) (*discordgo.Role, error) {
	if err := o.allow(opRoleCreate); err != nil {
		return nil, err
	}
	jid := o.beginJournal(opRoleCreate, data.Name)
	v, err := o.guard.session.GuildRoleCreate(guildID, data, options...)
	return v, o.record(jid, err)
}
