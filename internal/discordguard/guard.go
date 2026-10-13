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
//   - pause: refuse every mutating call, leaving reads and inspection
//     commands working so an admin can still see what the bot believes.
//   - dry-run: let a rotation or sweep make its full decision and write its
//     full audit trail while touching nothing, so a feature whose failure
//     mode has no undo can be rehearsed against the real guild first.
//
// It hooks in where the plugins already had a seam. Each mutating plugin
// defines a narrow ops interface (rotation.DiscordChannelOps,
// roles.DiscordMemberOps) shaped as disgo's REST client has the calls, and
// is handed a GuildOps for it. GuildOps implements the union of those
// interfaces, so every call site is guarded at that one assignment, and a
// future plugin that follows the same convention is covered without having
// to remember a flag check.
package discordguard

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/disgoorg/disgo/rest"
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
// needs: the per-guild half of the controls. Reads come from the same
// in-memory cache as every other setting, so a pause takes effect on the
// next call with no database round trip.
type GuildGate interface {
	WritesPaused(guildID string) bool
	WritesDryRun(guildID string) bool
}

// Guard holds the process-wide stop and the per-guild gate. One is
// constructed in cmd/bot/main.go and shared by every plugin.
type Guard struct {
	rest    rest.Rest
	gate    GuildGate
	log     *slog.Logger
	global  atomic.Bool
	journal Journal
	// now is injectable so the governor's time-based behavior (bucket refill,
	// breaker cooldown) is testable without sleeping.
	now func() time.Time

	buckets  sync.Map // bucketKey -> *atomic.Int64, the cell's arrival time
	breakers sync.Map // guildID -> *breaker
}

// New guards the calls merlin makes through r.
func New(r rest.Rest, gate GuildGate, log *slog.Logger, pauseAllWrites bool) *Guard {
	g := &Guard{
		rest: r,
		gate: gate,
		log:  log,
		now:  func() time.Time { return time.Now().UTC() },
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
// channel-scoped and carry no guild, and inferring one from the gateway
// cache would make the emergency stop depend on that cache being
// populated, including for a channel the bot created moments earlier, whose
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
