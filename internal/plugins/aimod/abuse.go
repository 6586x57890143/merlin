package aimod

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"sync"
	"time"
	"unicode"

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
	// meterWindow is the window the deep ceiling is counted over, and the
	// span sanction.go looks back over for pending flags.
	meterWindow = 10 * time.Minute
	// maxBurstScans is the tightest scan ceiling, the 30 second one, and so
	// how many messages a member can have scanned at a single instant.
	maxBurstScans = 15
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

// scanCeilings is how many of one member's messages may reach the fast pass
// over each window, every one of which has to hold at once.
//
// One ceiling (it was 40 per ten minutes) cannot tell a heated argument from
// somebody feeding the model. An argument is fast but short: a person typing
// as quickly as they can manages a message every few seconds for a few
// minutes, then slows. Draining a budget has to be sustained, because the
// fast pass is a twentieth of a batch and one burst costs nothing. So the
// short windows are generous and the allowed rate falls as the window grows,
// from 30 a minute over 30 seconds to about 2 a minute over a day. A member
// arguing flat out for half an hour is still scanned throughout; one posting
// all afternoon at a pace no conversation keeps up is not.
//
// Each rate is roughly double a fast typist's at that span, since running
// out means their messages go unread by the model, and a later message that
// mattered is the one that gets missed. Similar-message spam never counts
// against these at all (see spamSketch), which is most of what lets them be
// this loose.
var scanCeilings = []struct {
	window time.Duration
	max    int
}{
	{30 * time.Second, maxBurstScans},
	{2 * time.Minute, 40},
	{5 * time.Minute, 80},
	{10 * time.Minute, 120},
	{30 * time.Minute, 300},
	{time.Hour, 480},
	{3 * time.Hour, 1000},
	{6 * time.Hour, 1500},
	{12 * time.Hour, 2200},
	{24 * time.Hour, 3000},
}

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

// meterEntry is one member's counts.
//
// The deep ceiling keeps exact timestamps: it is a dozen entries. The scan
// ceilings cannot, since a day at 3000 is 3000 timestamps per member, so each
// window is an approximate sliding count (windowCount). Neither is a plain
// counter with a reset, which would let a member stay quiet until the
// boundary and then burst straight through it.
type meterEntry struct {
	scans []windowCount
	deep  []time.Time
	// recent is the member's last few messages, sketched, for spam.
	recent []recentSketch
}

// windowCount is the usual two-bucket approximation of a sliding window:
// the count in the current fixed bucket, plus the previous bucket's count
// weighted by how much of it still overlaps the window. Three numbers per
// window instead of a timestamp per message, and a burst either side of a
// bucket boundary still reads as one burst.
type windowCount struct {
	start     time.Time
	cur, prev int
}

func (w *windowCount) roll(now time.Time, size time.Duration) {
	b := now.Truncate(size)
	switch {
	case b.Equal(w.start):
	case b.Equal(w.start.Add(size)):
		w.prev, w.cur, w.start = w.cur, 0, b
	default:
		w.prev, w.cur, w.start = 0, 0, b
	}
}

func (w *windowCount) estimate(now time.Time, size time.Duration) float64 {
	overlap := 1 - float64(now.Sub(w.start))/float64(size)
	return float64(w.prev)*overlap + float64(w.cur)
}

type userMeter struct {
	mu      sync.Mutex
	entries map[meterKey]*meterEntry
}

func newUserMeter() *userMeter {
	return &userMeter{entries: make(map[meterKey]*meterEntry)}
}

// trimStamps drops timestamps that have fallen out of the deep window.
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
// whether they are under every ceiling. A refused message is not counted,
// so a member who hits the 30 second ceiling is back as soon as it eases
// rather than digging themselves a deeper hole in the day's.
func (m *userMeter) allowScan(guildID, userID string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entryLocked(guildID, userID, now)
	for i, c := range scanCeilings {
		e.scans[i].roll(now, c.window)
		if e.scans[i].estimate(now, c.window)+1 > float64(c.max) {
			return false
		}
	}
	for i := range e.scans {
		e.scans[i].cur++
	}
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

// idle reports that nothing in an entry still counts against anybody.
func (e *meterEntry) idle(now time.Time) bool {
	if len(trimStamps(e.deep, now)) > 0 {
		return false
	}
	for i, c := range scanCeilings {
		e.scans[i].roll(now, c.window)
		if e.scans[i].estimate(now, c.window) > 0 {
			return false
		}
	}
	for _, r := range e.recent {
		if now.Sub(r.at) < spamWindow {
			return false
		}
	}
	return true
}

func (m *userMeter) entryLocked(guildID, userID string, now time.Time) *meterEntry {
	key := meterKey{guildID: guildID, userID: userID}
	if e, ok := m.entries[key]; ok {
		return e
	}
	if len(m.entries) >= meterMax {
		for k, e := range m.entries {
			if e.idle(now) {
				delete(m.entries, k)
			}
		}
		if len(m.entries) >= meterMax {
			clear(m.entries)
		}
	}
	e := &meterEntry{scans: make([]windowCount, len(scanCeilings))}
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

// Similar-message spam: the same thing posted over and over with a word, a
// number or an emoji changed each time.
//
// Exact repeats are already free (seenClean skips clean ones at rung 0 and
// dedupeCache answers judged ones from the stored verdict). A near repeat is
// neither, so each one was a fresh scan against the member's ceiling: a
// spammer could empty their own quota, and every scan it spent was money
// learning the same thing again.
//
// A message is spam when it closely resembles at least spamRepeats of the
// member's own messages in the last spamWindow. The first few of a run are
// still scanned, which is what makes skipping the rest safe: if the run
// violates anything, those first ones say so, and enforce calls
// forgetSimilar on a confirmed violation, so the next variants are scanned
// in their turn rather than riding on the pass the first ones got. Skipped
// spam draws nothing from the scan ceilings, so a flood leaves the member's
// quota for whatever they say next.
const (
	spamWindow = 2 * time.Minute
	// spamRepeats is how many close matches in the window make a message
	// spam. Three, so a member saying "no" four times in a row is spam and a
	// member saying it twice is having an argument.
	spamRepeats = 3
	// spamSimilarity is the estimated share of three-letter shingles two
	// messages must have in common. High on purpose: "buy now at x.com 12"
	// and "buy now at x.com 13" are the same message, while two replies in
	// one argument share words but not most of their letters.
	spamSimilarity = 0.7
	// spamRecent bounds what is kept per member.
	spamRecent = 12
	// sketchSize is the number of MinHash values per message: the estimate
	// is good to about a tenth, and 128 bytes per message kept.
	sketchSize = 32
)

type recentSketch struct {
	at     time.Time
	sketch [sketchSize]uint32
}

// spamSketch is a MinHash of a message's three-letter shingles, after
// lowercasing, collapsing every digit to 0 (so a counter or a changing
// number does not make each copy new) and dropping everything that is not
// a letter or a digit. Nothing of the text survives it, which is what lets
// it sit in memory for two minutes under this plugin's retention rules.
func spamSketch(text string) [sketchSize]uint32 {
	var norm []rune
	for _, r := range strings.ToLower(text) {
		switch {
		case unicode.IsDigit(r):
			norm = append(norm, '0')
		case unicode.IsLetter(r):
			norm = append(norm, r)
		}
	}
	var sk [sketchSize]uint32
	for i := range sk {
		sk[i] = math.MaxUint32
	}
	add := func(shingle []rune) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(string(shingle)))
		base := h.Sum32()
		for i := range sk {
			if v := mix32(base ^ uint32(i)*0x9e3779b9); v < sk[i] {
				sk[i] = v
			}
		}
	}
	if len(norm) < 3 {
		add(norm)
		return sk
	}
	for i := 0; i+3 <= len(norm); i++ {
		add(norm[i : i+3])
	}
	return sk
}

// mix32 is murmur3's finaliser, turning one hash into an independent-looking
// one per sketch slot.
func mix32(h uint32) uint32 {
	h ^= h >> 16
	h *= 0x85ebca6b
	h ^= h >> 13
	h *= 0xc2b2ae35
	h ^= h >> 16
	return h
}

func similarity(a, b [sketchSize]uint32) float64 {
	same := 0
	for i := range a {
		if a[i] == b[i] {
			same++
		}
	}
	return float64(same) / sketchSize
}

// similarSpam records a message against its member and reports whether it
// is spam. Every message is recorded, spam included, so a run is still
// recognised however long it goes on.
func (m *userMeter) similarSpam(guildID, userID, text string, now time.Time) bool {
	sk := spamSketch(text)
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entryLocked(guildID, userID, now)
	matches := 0
	kept := e.recent[:0]
	for _, r := range e.recent {
		if now.Sub(r.at) >= spamWindow {
			continue
		}
		kept = append(kept, r)
		if similarity(sk, r.sketch) >= spamSimilarity {
			matches++
		}
	}
	e.recent = append(kept, recentSketch{at: now, sketch: sk})
	if len(e.recent) > spamRecent {
		e.recent = e.recent[len(e.recent)-spamRecent:]
	}
	return matches >= spamRepeats
}

// forgetSimilar drops a member's spam history, so what they post next is
// scanned rather than skipped as more of the same. Called when a violation
// is confirmed against them: a run that turned out to violate something is
// exactly the run whose later copies should be read.
func (m *userMeter) forgetSimilar(guildID, userID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[meterKey{guildID: guildID, userID: userID}]; ok {
		e.recent = nil
	}
}
