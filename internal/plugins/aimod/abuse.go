package aimod

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordguard"
)

// Defence against the obvious attack on a feature that spends money per
// message: post enough that the server's daily budget is gone.
//
// The daily budget already caps what that costs in dollars. The damage it
// does not cap is the second-order one, and it is the worse of the two: a
// guild whose budget is exhausted by lunchtime is a guild with no AI
// moderation for the rest of the day, which is exactly what somebody
// planning to post something genuinely reportable would want to arrange
// first. So the budget is the ceiling on the bill, and the per-member meters
// below are the ceiling on any one person's share of it.
//
// Two meters rather than one, because the two rungs cost two orders of
// magnitude apart. The fast pass is a twentieth of a batch; the deep pass is
// a whole policy file for one message. Somebody who has worked out a
// phrasing that trips the fast filter can drive the expensive rung on
// demand, so that one gets the tighter ceiling.

const (
	// meterWindow is the sliding window both ceilings are counted over.
	meterWindow = 10 * time.Minute
	// maxUserScans is how many of one member's messages reach the fast pass
	// per window. Far above ordinary chatter (a talkative member in a busy
	// channel is well under this) and far below what it takes to matter.
	maxUserScans = 40
	// maxUserDeep is how many of one member's messages reach the deep pass
	// per window.
	//
	// Raised from five, because what it counts changed. Repeats of text
	// already judged are now answered from the cached verdict and cost
	// nothing (see dedupeCache), so this is consumed only by *distinct*
	// content nobody has looked at yet. Five was throttling real moderation
	// during an argument on a server spending five percent of its budget,
	// which is the wrong thing to be protecting.
	//
	// Still bounded, because the ceiling is what stops one member driving
	// the expensive rung on demand once they have worked out a phrasing that
	// trips it, and a dozen distinct flagged messages from one person in ten
	// minutes is a moderation problem a human should see either way.
	maxUserDeep = 12
	// meterMax bounds the meter map, like dedupeMax bounds the dedupe cache.
	meterMax = 8192
)

// SanctionAction is what happens to a member behind a confirmed violation or
// a member draining the scan budget.
type SanctionAction string

const (
	// SanctionOff never jails. The meters still apply: a member over their
	// ceiling simply stops being scanned for the rest of the window.
	SanctionOff SanctionAction = "off"
	// SanctionFlag reports to the audit log and lets a human decide. The
	// default, because an automatic jail is a real punishment and a guild
	// should switch it on deliberately rather than discover it.
	SanctionFlag SanctionAction = "flag"
	// SanctionJail jails the member for a length that scales with the
	// severity of what they posted and how often they have done it. See
	// sanction.go.
	SanctionJail SanctionAction = "jail"
)

// SanctionActions lists every sanction action, for command choices.
var SanctionActions = []SanctionAction{SanctionOff, SanctionFlag, SanctionJail}

// meterKey identifies one member in one guild.
type meterKey struct {
	guildID string
	userID  string
}

// meterEntry is a sliding-window count. Timestamps rather than a counter
// plus a reset time, so a member cannot save up quota by staying quiet until
// the boundary and then bursting through it.
type meterEntry struct {
	scans []time.Time
	deep  []time.Time
}

type userMeter struct {
	mu      sync.Mutex
	entries map[meterKey]*meterEntry
}

func newUserMeter() *userMeter {
	return &userMeter{entries: make(map[meterKey]*meterEntry)}
}

// trimStamps drops timestamps that have fallen out of the window.
func trimStamps(stamps []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-meterWindow)
	kept := stamps[:0]
	for _, at := range stamps {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	return kept
}

// allowScan records one fast-pass message against a member and reports
// whether they are still under the ceiling.
func (m *userMeter) allowScan(guildID, userID string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entryLocked(guildID, userID, now)
	e.scans = trimStamps(e.scans, now)
	if len(e.scans) >= maxUserScans {
		return false
	}
	e.scans = append(e.scans, now)
	return true
}

// allowDeep records one deep-pass escalation and reports whether the member
// is still under the ceiling. The second return says whether this call is
// the one that crossed it, so the audit entry and any sanction fire exactly
// once per window rather than on every message after it.
func (m *userMeter) allowDeep(guildID, userID string, now time.Time) (allowed, justCrossed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entryLocked(guildID, userID, now)
	e.deep = trimStamps(e.deep, now)
	switch {
	case len(e.deep) < maxUserDeep:
		e.deep = append(e.deep, now)
		return true, false
	case len(e.deep) == maxUserDeep:
		// Push one past the ceiling so the equality above is true exactly
		// once until the window slides.
		e.deep = append(e.deep, now)
		return false, true
	default:
		return false, false
	}
}

func (m *userMeter) entryLocked(guildID, userID string, now time.Time) *meterEntry {
	key := meterKey{guildID: guildID, userID: userID}
	if e, ok := m.entries[key]; ok {
		return e
	}
	if len(m.entries) >= meterMax {
		for k, e := range m.entries {
			if len(trimStamps(e.scans, now)) == 0 && len(trimStamps(e.deep, now)) == 0 {
				delete(m.entries, k)
			}
		}
		if len(m.entries) >= meterMax {
			clear(m.entries)
		}
	}
	e := &meterEntry{}
	m.entries[key] = e
	return e
}

// forgetGuild drops a guild's meters after the bot leaves it.
func (m *userMeter) forgetGuild(guildID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.entries {
		if k.guildID == guildID {
			delete(m.entries, k)
		}
	}
}

// handleAbuse responds to a member crossing the deep-pass ceiling.
//
// Always audits, whatever the configured action, including when it is off. A
// member generating enough flagged content to exhaust a ceiling is something
// a moderator should know about even in a guild that has asked the bot not
// to act on it, and it is the only warning an admin gets that their budget
// is being drained on purpose.
func (p *Plugin) handleAbuse(ctx context.Context, cfg Config, c candidate) {
	if cfg.SanctionAction == SanctionJail && cfg.Mode == ModeEnforce {
		// sanctionForAbuse writes its own, more specific audit entry.
		p.sanctionForAbuse(ctx, cfg, c)
		return
	}

	detail := fmt.Sprintf("%s tripped the moderation filter more than %d times in %s in %s. "+
		"Their messages are no longer being sent to a model for the rest of that window. "+
		"This is what deliberate budget exhaustion looks like, though a heated argument can do it honestly too.",
		core.MentionUser(c.AuthorID), maxUserDeep, core.FormatDuration(meterWindow), core.MentionChannel(c.ChannelID))
	if err := p.auditWriter.Record(ctx, cfg.GuildID, core.ActorSystem, "aimod.abuse_detected", c.AuthorID, detail); err != nil {
		p.log.Error("aimod: audit abuse notice", "guild", cfg.GuildID, "err", err)
	}
}

// timeoutMember applies Discord's own communication timeout.
//
// Only ever reached as jailOrTimeout's fallback; see sanction.go for why
// jail is the primary and this is not.
//
// The rank guard here is the same gap core.Permissions.CanModerate exists to
// close for /roles jail, arrived at from the other side: nothing about
// "posted five flagged messages" says the poster is an ordinary member, and
// an automatic action that can silence a moderator is one somebody will
// eventually learn to aim. It refuses outright for the guild owner and for
// anyone holding a role carrying Administrator, Moderate Members or Manage
// Messages, and it fails closed: a guild whose roles cannot be read gets no
// automatic timeout at all.
func (p *Plugin) timeoutMember(ctx context.Context, guildID, userID string, duration time.Duration, targetConsented bool) error {
	if duration > maxDiscordTimeout {
		duration = maxDiscordTimeout
	}

	if p.privilege != nil && p.privilege.IsBootstrapAdmin(userID) {
		return fmt.Errorf("aimod: the bootstrap admin cannot be sanctioned automatically")
	}

	guild, err := p.ops(guildID).Guild(guildID)
	if err != nil {
		return fmt.Errorf("aimod: read guild for rank check: %w", err)
	}
	// Discord refuses to time out a guild owner at all, so this is a clearer
	// error rather than a policy choice, and it holds even with consent.
	if guild.OwnerID == userID {
		return fmt.Errorf("aimod: Discord does not allow timing out the guild owner")
	}
	if targetConsented {
		// They asked for this. Skip the staff-rank refusal below; the two
		// absolute carve-outs above still stand.
		return p.applyTimeout(ctx, guildID, userID, duration)
	}
	member, err := p.ops(guildID).GuildMember(guildID, userID)
	if err != nil {
		return fmt.Errorf("aimod: read member for rank check: %w", err)
	}

	const staffBits = discordgo.PermissionAdministrator |
		discordgo.PermissionModerateMembers |
		discordgo.PermissionManageMessages
	byID := make(map[string]*discordgo.Role, len(guild.Roles))
	for _, r := range guild.Roles {
		byID[r.ID] = r
	}
	for _, roleID := range member.Roles {
		r, ok := byID[roleID]
		if !ok {
			// A role the guild read did not include is a role this check
			// cannot clear, and clearing it by default is how a carve-out
			// gets bypassed. Refuse.
			return fmt.Errorf("aimod: refusing to time out %s, their roles could not be fully resolved", userID)
		}
		if r.Permissions&staffBits != 0 {
			return fmt.Errorf("aimod: refusing to time out %s, they hold a moderator-level role", userID)
		}
	}

	return p.applyTimeout(ctx, guildID, userID, duration)
}

func (p *Plugin) applyTimeout(_ context.Context, guildID, userID string, duration time.Duration) error {
	until := p.now().Add(duration)
	err := p.ops(guildID).GuildMemberTimeout(guildID, userID, &until)
	if discordguard.Skipped(err) {
		// Paused or dry-run: the guild deliberately stopped the bot acting.
		return nil
	}
	return err
}

// maxDiscordTimeout is Discord's own ceiling on a communication timeout. The
// sanction ladder can compute longer than this, which is one more reason
// jail is the primary mechanism and this is the fallback: jail has no such
// limit because this bot enforces it itself.
const maxDiscordTimeout = 28 * 24 * time.Hour
