package rapsheet

import (
	"context"
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// newEntry is what a caller knows about an entry before it is written.
// Points are not on it: they are resolved here, once, from the guild's
// table, so no caller can score an entry differently from another.
type newEntry struct {
	GuildID  string
	UserID   string
	Kind     Kind
	Category Category
	ActorID  string
	Reason   string
	Duration time.Duration
	EndsAt   *time.Time
	Source   Source
	Ref      string
	Band     int
	// PointsOverride is a moderator saying this one is worth more or less
	// than the category default. Zero means the default.
	PointsOverride int
	// Identity is the member's user object when the caller has one for free
	// (an interaction's Resolved.Users, a gateway event), so the case file's
	// snapshot is refreshed without a REST call.
	Identity *discordgo.User
}

// record is the one path every entry is written through, whatever produced
// it: a command, the bus, the audit log, the ladder. Points, the case file
// and the after-effects (mirroring into the forum, running the ladder) all
// hang off here, which is what keeps a warning typed by a mod and a removal
// aimod made indistinguishable to everything downstream.
//
// It returns the written entry and whether anything was written; a
// duplicate external ref is the ordinary "seen this event already" case and
// comes back as (zero, false, nil).
func (p *Plugin) record(ctx context.Context, cfg Config, in newEntry) (Entry, bool, error) {
	if in.Category == "" {
		in.Category = CategoryOther
	}
	if !validCategory(in.Category) {
		return Entry{}, false, fmt.Errorf("rapsheet: unknown category %q", in.Category)
	}
	if in.ActorID == "" {
		in.ActorID = core.ActorSystem
	}
	now := p.now()
	e := Entry{
		GuildID:   in.GuildID,
		UserID:    in.UserID,
		Kind:      in.Kind,
		Category:  in.Category,
		Points:    pointsFor(cfg, in.Kind, in.Category, in.ActorID, in.PointsOverride),
		Band:      in.Band,
		ActorID:   in.ActorID,
		Reason:    in.Reason,
		Duration:  in.Duration,
		EndsAt:    in.EndsAt,
		Source:    in.Source,
		Ref:       in.Ref,
		CreatedAt: now,
	}

	// The case file first, so the member is on file even if the entry
	// itself then fails to write: a row with no entries is harmless, an
	// entry with no case file has no thread to mirror into. Best effort,
	// because it is a snapshot for later convenience and never a
	// precondition.
	p.ensureCaseFile(ctx, in.GuildID, in.UserID, in.Identity)

	id, err := p.store.Insert(ctx, e)
	if err != nil {
		return Entry{}, false, err
	}
	if id == 0 {
		return Entry{}, false, nil
	}
	e.ID = id
	p.afterRecord(ctx, cfg, e)
	return e, true, nil
}

// afterRecord is everything that follows a written entry and must never
// fail it: the forum mirror, and (a later slice) the ladder.
func (p *Plugin) afterRecord(ctx context.Context, cfg Config, e Entry) {
	_ = ctx
	p.mirror(cfg, e)
}

// ensureCaseFile makes sure the member is on file and refreshes the
// identity snapshot when a user object is to hand. Without one it fetches
// the user once; a failure there is logged and costs nothing but a blank
// name in the alt matcher until the next entry.
func (p *Plugin) ensureCaseFile(ctx context.Context, guildID, userID string, u *discordgo.User) {
	if u == nil {
		if fetched, err := p.ops(guildID).User(userID); err == nil {
			u = fetched
		}
	}
	cf := CaseFile{GuildID: guildID, UserID: userID}
	if u != nil {
		cf.Username, cf.GlobalName, cf.AvatarHash = u.Username, u.GlobalName, u.Avatar
	}
	if err := p.store.UpsertCaseFile(ctx, cf); err != nil {
		p.log.Error("rapsheet: upsert case file", "guild", guildID, "user", userID, "err", err)
	}
}

// group is the member's link group, or just the member if the lookup
// fails. Failing toward the single account is the lenient direction: a
// score that misses a linked alt is lower, never higher.
func (p *Plugin) group(ctx context.Context, guildID, userID string) []string {
	ids, err := p.store.Group(ctx, guildID, userID)
	if err != nil {
		p.log.Error("rapsheet: read link group", "guild", guildID, "user", userID, "err", err)
		return []string{userID}
	}
	return ids
}

// sheet is everything a reader of one member's record needs: their group,
// the entries that still bear on the score, and the score itself.
type sheet struct {
	Group   []string
	Entries []Entry
	Score   float64
	Rec     Recommendation
}

// scoreWindow bounds how far back the score reads. Past six half-lives an
// entry is worth under two percent of its points, so the sum is unchanged to
// the eye and the query stays proportional to recent activity rather than
// to a member's lifetime.
const scoreWindow = 6

// maxSheetEntries bounds one read. A member with more than this in six
// half-lives is a member whose sheet nobody is reading line by line.
const maxSheetEntries = 500

func (p *Plugin) loadSheet(ctx context.Context, cfg Config, guildID, userID string) (sheet, error) {
	ids := p.group(ctx, guildID, userID)
	now := p.now()
	entries, err := p.store.Entries(ctx, guildID, ids, now.Add(-scoreWindow*cfg.HalfLife), maxSheetEntries)
	if err != nil {
		return sheet{}, err
	}
	score := Score(entries, now, cfg.HalfLife)
	return sheet{Group: ids, Entries: entries, Score: score, Rec: Ladder(score, cfg.Bands)}, nil
}

// standing is the strongest consequence still in force across entries.
func standing(entries []Entry, now time.Time) (Entry, bool) {
	var best Entry
	found := false
	for _, e := range entries {
		if !e.Standing(now) {
			continue
		}
		if !found || kindStrength(e.Kind) > kindStrength(best.Kind) ||
			(kindStrength(e.Kind) == kindStrength(best.Kind) && later(e.EndsAt, best.EndsAt)) {
			best, found = e, true
		}
	}
	return best, found
}

func kindStrength(k Kind) int {
	switch k {
	case KindTimeout:
		return ActionTimeout.Strength()
	case KindJail:
		return ActionJail.Strength()
	case KindBan:
		return ActionBan.Strength()
	}
	return 0
}

// later reports whether a ends after b, treating nil (no end) as latest.
func later(a, b *time.Time) bool {
	if a == nil {
		return b != nil
	}
	if b == nil {
		return false
	}
	return a.After(*b)
}
