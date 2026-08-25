package discordguard

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/bwmarrin/discordgo"
)

// ErrRateLimited and ErrCircuitOpen report that a call was refused by this
// bot's own throttling rather than by Discord.
//
// Unlike ErrPaused and ErrDryRun, these are genuine failures and must be
// reported to the Scheduler as such: hitting a self-imposed cap means
// something is doing far more work than the feature was designed to, and
// that is exactly the condition the consecutive-failure alert exists to
// surface. Skipped deliberately does not match them.
var (
	ErrRateLimited = errors.New("discordguard: per-guild rate cap exceeded")
	ErrCircuitOpen = errors.New("discordguard: Discord API circuit breaker is open")
)

// Per-guild hourly caps, well below anything a correctly-behaving guild
// reaches. Rotation creates one channel per rotating channel per interval
// (hours apart), and the sweep deletes archives that are days old. Jail
// edits one member at a time. A guild bumping any of these is a bug, a bad
// config, or someone driving the bot maliciously. spec.MD §4 asks for this
// specifically so none of those can get the whole application rate-limited
// or banned by Discord.
//
// The exception is channel.permissions: /roles configure sync-channels
// legitimately writes one overwrite per channel across the entire guild, so
// its cap is sized for a large server doing that a few times an hour.
var opCaps = map[string]int{
	opChannelCreate:      20,
	opChannelEdit:        60,
	opChannelDelete:      20,
	opChannelPermissions: 1000,
	opMessageSend:        120,
	opMessagePin:         60,
	opMemberEdit:         120,
	opMemberRoleAdd:      120,
	opMemberRoleRemove:   120,
	opRoleCreate:         10,
	opRoleEdit:           30,
	// aimod deletes one message per confirmed violation, and reposts one
	// per rewrite. A guild bumping these is either under a raid or has a
	// filter misconfigured badly enough that stopping it is the right
	// outcome; both are exactly what this cap is for. Sized well above a
	// bad afternoon and well below "the bot deleted the channel".
	opMessageDelete: 300,
	// One webhook per channel, created once and reused for the life of the
	// process, so this only moves when a guild is churning channels.
	opWebhookCreate:  20,
	opWebhookExecute: 300,
	// Discord's own timeout, applied automatically only by aimod's abuse
	// ceiling, which is itself rate limited per member.
	opMemberTimeout: 60,
}

const (
	opChannelCreate      = "channel.create"
	opChannelEdit        = "channel.edit"
	opChannelDelete      = "channel.delete"
	opChannelPermissions = "channel.permissions"
	opMessageSend        = "message.send"
	opMessagePin         = "message.pin"
	opMemberEdit         = "member.edit"
	opMemberRoleAdd      = "member.role.add"
	opMemberRoleRemove   = "member.role.remove"
	opRoleCreate         = "role.create"
	opRoleEdit           = "role.edit"
	opMessageDelete      = "message.delete"
	opWebhookCreate      = "webhook.create"
	opWebhookExecute     = "webhook.execute"
	opMemberTimeout      = "member.timeout"
)

// capWindow is the period each cap is denominated over. Buckets refill
// continuously rather than resetting on a boundary, so a guild can't save up
// a burst by staying quiet until :59.
const capWindow = time.Hour

// bucket is a token bucket refilled lazily on read, with no background
// goroutine, which is fine because this bot is single-instance by design and
// nothing needs to observe the level except the call being decided.
type bucket struct {
	tokens   float64
	capacity float64
	lastFill time.Time
}

func (b *bucket) take(now time.Time) bool {
	elapsed := now.Sub(b.lastFill)
	if elapsed > 0 {
		b.tokens += b.capacity * (float64(elapsed) / float64(capWindow))
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.lastFill = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Circuit breaker tuning. Discord outages last minutes, not seconds, and
// hammering through one is how a bot earns a longer ban than the outage.
const (
	breakerThreshold = 8
	breakerCooldown  = 2 * time.Minute
)

// breaker is per guild rather than per operation class: a run of 5xx or 429
// responses is a condition of Discord or of this guild, not of one endpoint,
// and tracking it per endpoint would need each one to fail its own eight
// times before anything backed off.
type breaker struct {
	failures int
	openedAt time.Time
}

// allowRate applies the token bucket and the circuit breaker to one
// operation. Callers must have already passed the pause and dry-run checks:
// a paused guild never reaches here, so it neither spends budget nor trips
// the breaker on calls it never made.
func (g *Guard) allowRate(guildID, op string, now time.Time) error {
	g.rateMu.Lock()
	defer g.rateMu.Unlock()

	if b, ok := g.breakers[guildID]; ok && !b.openedAt.IsZero() {
		if now.Sub(b.openedAt) < breakerCooldown {
			return fmt.Errorf("%s: %w", op, ErrCircuitOpen)
		}
		// Cooldown elapsed: let exactly one call through to test the water.
		// If it fails, recordResult reopens for another full cooldown.
		b.openedAt = time.Time{}
		b.failures = breakerThreshold - 1
	}

	capacity, ok := opCaps[op]
	if !ok {
		// An operation class with no cap is a programming oversight, not a
		// licence to run unbounded: the whole point is that nothing
		// destructive is unmetered.
		return fmt.Errorf("discordguard: no rate cap defined for %q", op)
	}
	key := guildID + ":" + op
	b, exists := g.buckets[key]
	if !exists {
		b = &bucket{tokens: float64(capacity), capacity: float64(capacity), lastFill: now}
		g.buckets[key] = b
	}
	if !b.take(now) {
		return fmt.Errorf("%s: %w (cap %d per %s)", op, ErrRateLimited, capacity, capWindow)
	}
	return nil
}

// recordResult feeds one call's outcome to the guild's breaker. Only
// transient server-side failures count: a 4xx means this specific request was
// wrong (a missing channel, a role above the bot), which says nothing about
// Discord's health and must not open the breaker for everything else.
func (g *Guard) recordResult(guildID string, now time.Time, err error) {
	transient := err != nil && isTransient(err)

	g.rateMu.Lock()
	defer g.rateMu.Unlock()
	b, ok := g.breakers[guildID]
	if !ok {
		b = &breaker{}
		g.breakers[guildID] = b
	}
	if !transient {
		b.failures = 0
		return
	}
	b.failures++
	if b.failures >= breakerThreshold && b.openedAt.IsZero() {
		b.openedAt = now
		g.log.Error("discordguard: circuit breaker opened, refusing writes",
			"guild", guildID, "failures", b.failures, "cooldown", breakerCooldown)
	}
}

// isTransient reports whether err looks like Discord being unwell (a 5xx or
// a rate-limit response) rather than this request being wrong. Mirrors the
// status-vs-code reasoning in core.IsUnknownResource; duplicated rather than
// imported so this package doesn't depend on core, which would put a cycle
// between the guard and everything that wants to use it.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	var rl *discordgo.RateLimitError
	if errors.As(err, &rl) {
		return true
	}
	var restErr *discordgo.RESTError
	if !errors.As(err, &restErr) {
		// Not an HTTP response at all: connection refused, timeout, DNS.
		// The most transient thing there is.
		return true
	}
	if restErr.Response == nil {
		// A REST error we can't classify. Counting it toward opening the
		// breaker would let an unrecognized error shape stop every write in
		// the guild, so it doesn't count either way.
		return false
	}
	code := restErr.Response.StatusCode
	return code >= http.StatusInternalServerError || code == http.StatusTooManyRequests
}
