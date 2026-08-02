package roles

import (
	"context"
	"fmt"

	"github.com/6586x57890143/merlin/internal/discordguard"
)

// makeSweepJob returns the Scheduler job function that releases due jails
// and removes due grants for one guild. One sweep job per guild, running
// every sweepInterval (see roles.go).
func (p *Plugin) makeSweepJob(guildID string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		// A paused guild is a deliberate operator state, not a failing job —
		// see rotation.makeRotationJob for the full reasoning. Due jails stay
		// due and are released on the first sweep after the pause is lifted.
		if err := p.sweep(ctx, guildID); err != nil {
			if discordguard.Skipped(err) {
				p.log.Info("roles sweep: skipped, writes paused", "guild", guildID)
				return nil
			}
			return err
		}
		return nil
	}
}

// sweep processes every due jail and grant for guildID. A single row's
// failure is logged and doesn't abort the rest of the sweep — mirrors
// rotation.sweep's "one bad row doesn't block the others" policy.
func (p *Plugin) sweep(ctx context.Context, guildID string) error {
	// Release and revoke both untrack their row as part of doing the work, so
	// letting them run under dry-run would forget the jails and grants the
	// real sweep still owes. Skipping wholesale keeps a rehearsing guild's
	// pending work intact, and every jail stays due for whenever dry-run ends.
	if p.dryRun(guildID) {
		p.log.Info("roles sweep: dry-run, skipping release/revoke", "guild", guildID)
		return nil
	}

	var firstErr error

	// Before releasing anyone, put back the jails people walked out of.
	// Discord drops a member's roles when they leave, so a jailed member who
	// leaves and rejoins returns with no marker role and full access, while
	// the record still says they're jailed. Checked every sweep, so the
	// escape lasts at most one tick.
	if err := p.reapplyEvadedJails(ctx, guildID); err != nil {
		firstErr = err
	}

	dueJails, err := p.store.DueJails(ctx, guildID, p.now())
	if err != nil {
		return fmt.Errorf("roles sweep: query due jails: %w", err)
	}
	for _, rec := range dueJails {
		if err := p.releaseJail(ctx, guildID, rec.UserID, rec); err != nil {
			p.log.Error("roles sweep: release jail failed", "guild", guildID, "user", rec.UserID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	dueGrants, err := p.store.DueGrants(ctx, guildID, p.now())
	if err != nil {
		return fmt.Errorf("roles sweep: query due grants: %w", err)
	}
	for _, rec := range dueGrants {
		if err := p.revokeGrant(ctx, guildID, rec.UserID, rec.RoleID, "system"); err != nil {
			p.log.Error("roles sweep: revoke grant failed", "guild", guildID, "user", rec.UserID, "role", rec.RoleID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	return firstErr
}
