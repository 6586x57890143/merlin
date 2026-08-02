package rotation

import (
	"context"
	"fmt"

	"github.com/6586x57890143/merlin/internal/core"
)

// makeSweepJob returns the Scheduler job function that permanently deletes
// a guild's archived channels once they're past their retention window.
// One sweep job is registered per guild (not per rotating channel) — it
// covers every rotating channel's archives in that guild.
func (p *Plugin) makeSweepJob(guildID string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		return p.sweep(ctx, guildID)
	}
}

// sweep deletes every due archive for guildID. A single row's failure is
// logged and doesn't abort the rest of the sweep — one bad row shouldn't
// block deletion of others that are legitimately due.
func (p *Plugin) sweep(ctx context.Context, guildID string) error {
	due, err := p.archives.DueForDeletion(ctx, guildID, p.now())
	if err != nil {
		return fmt.Errorf("rotation sweep: query due archives: %w", err)
	}

	var firstErr error
	for _, rec := range due {
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
	ch, err := p.ops.Channel(rec.ChannelID)
	if err != nil {
		if core.IsUnknownResource(err) {
			// Already gone (e.g. manually deleted) — nothing left to sweep.
			return p.archives.Delete(ctx, rec.ChannelID)
		}
		// Anything else (rate limit, 5xx, network blip) is transient.
		// Dropping the row here would leave the archived channel alive with
		// nothing left tracking it — a silently broken retention promise,
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
	// (see execute.go's rotate) — looking it up by rec.SourceChannelID here
	// would stop finding it after the very first rotation, making every
	// archive look permanently "rescued." Empty means a pre-migration row
	// whose real category was never recorded — treat that the same as an
	// actual mismatch: don't guess, don't delete.
	if rec.ArchiveCategoryID == "" || ch.ParentID != rec.ArchiveCategoryID {
		// Rescue hatch: a mod moved this archived channel out of its archive
		// category — treat that as an implicit "keep it," stop tracking it,
		// don't delete.
		p.log.Info("rotation sweep: archived channel rescued, skipping deletion", "channel", rec.ChannelID)
		return p.archives.Delete(ctx, rec.ChannelID)
	}

	if _, err := p.ops.ChannelDelete(rec.ChannelID); err != nil {
		return fmt.Errorf("delete channel %s: %w", rec.ChannelID, err)
	}
	if err := p.audit.Record(ctx, guildID, "system", "archive.deleted", rec.ChannelID, ""); err != nil {
		p.log.Error("rotation sweep: audit failed", "channel", rec.ChannelID, "err", err)
	}
	return p.archives.Delete(ctx, rec.ChannelID)
}
