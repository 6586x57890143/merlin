package rotation

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordguard"
	"github.com/6586x57890143/merlin/internal/settings"
)

// makeSweepJob returns the Scheduler job function that permanently deletes
// a guild's archived channels once they're past their retention window.
// One sweep job is registered per guild (not per rotating channel), and it
// covers every rotating channel's archives in that guild.
func (p *Plugin) makeSweepJob(guildID string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		// See makeRotationJob: a paused guild is a deliberate state, not a
		// failing job, and must not consume the failure budget.
		if err := p.sweep(ctx, guildID); err != nil {
			if discordguard.Skipped(err) {
				p.log.Info("rotation sweep: skipped, writes paused", "guild", guildID)
				return nil
			}
			return err
		}
		return nil
	}
}

// archiveDeadline returns the instant rec may be permanently deleted, or nil
// for "keep it, indefinitely".
//
// The deadline is re-derived from the rotation slot's *current* retention
// setting rather than read from the delete_after column written when the
// channel was archived. That column was frozen at archive time and nothing
// ever updated it, so a retention change only applied to future archives: an
// admin who widened retention (or switched to keep-forever) still watched the
// sweep permanently delete existing archives on the old, earlier schedule,
// and permanent channel deletion has no undo. The reverse leaked too:
// tightening retention left older archives sitting past the new window, which
// is the exact promise this feature exists to make (spec.MD §6).
//
// Re-deriving here matches how the rest of this plugin works: rotate()
// re-derives every step from live Discord state, and roles' sweep re-fetches
// members rather than trusting stored assumptions. Stored state describes
// what happened; the live setting decides what should happen next.
//
// Two cases can't be re-derived and fall back to the stored deadline:
// pre-migration-0013 rows, which carry no RotationID, and archives whose
// rotation slot has since been removed via /rotation configure remove (which
// promises existing archives are left untouched, so the retention they were
// created under is the only promise still standing).
func archiveDeadline(rec ArchiveRecord, lookup func(int64) (settings.RotationChannel, bool)) *time.Time {
	if rec.RotationID == nil {
		return rec.DeleteAfter
	}
	rc, ok := lookup(*rec.RotationID)
	if !ok {
		return rec.DeleteAfter
	}
	if rc.RetentionHours == nil {
		return nil // keep forever, even if this row was written under a finite retention
	}
	t := rec.ArchivedAt.Add(time.Duration(*rc.RetentionHours) * time.Hour)
	return &t
}

// sweep deletes every due archive for guildID. A single row's failure is
// logged and doesn't abort the rest of the sweep, since one bad row shouldn't
// block deletion of others that are legitimately due.
func (p *Plugin) sweep(ctx context.Context, guildID string) error {
	archives, err := p.archives.ListForGuild(ctx, guildID)
	if err != nil {
		return fmt.Errorf("rotation sweep: list archives: %w", err)
	}
	now := p.now()
	lookup := func(id int64) (settings.RotationChannel, bool) {
		return p.settings.RotationChannelByID(guildID, id)
	}

	// Dry-run is checked for the sweep as a whole, not per row: sweepOne also
	// drops its own tracking row as bookkeeping, and a rehearsal that quietly
	// forgot which archives it was still watching would leave the real sweep
	// unable to do the thing it was rehearsing.
	if p.dryRun(guildID) {
		var due []string
		for _, rec := range archives {
			if deadline := archiveDeadline(rec, lookup); deadline != nil && !deadline.After(now) {
				due = append(due, rec.ChannelID)
			}
		}
		p.log.Info("rotation sweep: dry-run, skipping deletion", "guild", guildID, "due", due)
		if err := p.audit.Record(ctx, guildID, core.ActorSystem, "archive.dryrun", strings.Join(due, ","), "would have been deleted now"); err != nil {
			p.log.Error("rotation sweep: audit dry-run", "guild", guildID, "err", err)
		}
		return nil
	}

	var firstErr error
	for _, rec := range archives {
		deadline := archiveDeadline(rec, lookup)
		if deadline == nil || deadline.After(now) {
			continue
		}
		if err := p.sweepOne(ctx, guildID, rec); err != nil {
			p.log.Error("rotation sweep: row failed", "channel", rec.ChannelID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (p *Plugin) sweepOne(ctx context.Context, guildID string, rec ArchiveRecord) error {
	ch, err := p.ops(guildID).Channel(rec.ChannelID)
	if err != nil {
		if core.IsUnknownResource(err) {
			// Already gone (e.g. manually deleted), nothing left to sweep.
			return p.archives.Delete(ctx, rec.ChannelID)
		}
		// Anything else (rate limit, 5xx, network blip) is transient.
		// Dropping the row here would leave the archived channel alive with
		// nothing left tracking it: a silently broken retention promise,
		// which is the exact failure this whole feature exists to prevent.
		// Fail loudly instead and let the next hourly sweep retry.
		return fmt.Errorf("fetch archived channel %s: %w", rec.ChannelID, err)
	}
	if ch.GuildID != guildID {
		// Confused-deputy guard: this row no longer describes a channel in
		// this guild. Drop it rather than act on it.
		return p.archives.Delete(ctx, rec.ChannelID)
	}

	// rec.ArchiveCategoryID is recorded at archive time (not re-derived from
	// the live settings.RotationChannel row), since that row's ChannelID gets
	// retargeted onto the new live channel after every successful rotation
	// (see execute.go's rotate), so looking it up by rec.SourceChannelID here
	// would stop finding it after the very first rotation, making every
	// archive look permanently "rescued." Empty means a pre-migration row
	// whose real category was never recorded: treat that the same as an
	// actual mismatch: don't guess, don't delete.
	if rec.ArchiveCategoryID == "" || ch.ParentID != rec.ArchiveCategoryID {
		// Rescue hatch: a mod moved this archived channel out of its archive
		// category, treat that as an implicit "keep it," stop tracking it,
		// don't delete.
		p.log.Info("rotation sweep: archived channel rescued, skipping deletion", "channel", rec.ChannelID)
		return p.archives.Delete(ctx, rec.ChannelID)
	}

	if _, err := p.ops(guildID).ChannelDelete(rec.ChannelID); err != nil {
		return fmt.Errorf("delete channel %s: %w", rec.ChannelID, err)
	}
	if err := p.audit.Record(ctx, guildID, core.ActorSystem, "archive.deleted", core.MentionChannel(rec.ChannelID), ""); err != nil {
		p.log.Error("rotation sweep: audit failed", "channel", rec.ChannelID, "err", err)
	}
	return p.archives.Delete(ctx, rec.ChannelID)
}
