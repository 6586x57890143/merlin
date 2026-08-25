package roles

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
)

// JailAutomatic jails a member without a moderator interaction behind it.
//
// The entry point for automated moderation (today: internal/plugins/aimod's
// sanction ladder). It exists so an automated caller cannot end up
// reimplementing any part of jail: the same applyJail runs, so the record
// is still written before the roles are stripped, the same marker role is
// resolved or created, the sweep releases it on the same schedule, and
// /roles release and /roles list see it exactly as they see a hand-made
// jail. An automatic jail is a jail, not a second thing shaped like one.
//
// Callers reach this through a narrow interface they declare themselves, so
// no plugin imports this package: the wiring happens once in cmd/bot/main.go,
// the same seam every other cross-package dependency in this codebase uses.
//
// Two differences from the command path, both deliberate:
//
// There is no actor, so the rank check runs with a nil one. That is not a
// weakening: core.CanModerate with a nil actor refuses every admin-equivalent
// target and allows everyone else, which is precisely the rule an automated
// jail needs. A bot that can be induced into jailing an admin is a bot that
// can be used to take a server over, and no automatic trigger is ever worth
// that. It fails closed on unresolvable guild state, like the command path.
//
// An existing sentence is extended, never shortened. A moderator moving a
// release date earlier is them exercising judgement; an automated caller
// doing it would mean a member could shorten their own jail by committing
// another offence, which inverts the entire point.
func (p *Plugin) JailAutomatic(ctx context.Context, guildID, userID string, duration time.Duration, reason string, targetConsented bool) error {
	if duration <= 0 {
		return errors.New("roles: automatic jail needs a positive duration")
	}
	// Nothing waives the bootstrap carve-out, including the holder of that
	// identity opting themselves in. It is the operator's guaranteed way back
	// into every guild, and a consent flag that could disable it would make
	// "lock yourself out permanently" a thing a single command can do.
	if p.perms.IsBootstrapAdmin(userID) {
		return errors.New("roles: the bootstrap admin cannot be jailed automatically")
	}

	member, err := p.ops(guildID).GuildMember(guildID, userID)
	if err != nil {
		return fmt.Errorf("roles: fetch member for automatic jail: %w", err)
	}
	// targetConsented is set only when the member has put themselves on the
	// caller's opt-in list, which is why it may skip a rank check that exists
	// to stop this bot being aimed at a guild's admin team. It is consent to
	// be moderated, not an authorization bypass: nothing but the target's own
	// prior choice can set it.
	if !targetConsented {
		if err := p.perms.CanModerate(guildID, nil, userID, member.Roles); err != nil {
			return fmt.Errorf("roles: automatic jail refused: %w", err)
		}
	}

	jailRoleID, err := p.resolveJailRole(guildID)
	if err != nil {
		return err
	}

	_, err = p.applyJail(ctx, guildID, userID, jailRoleID, member.Roles, duration, core.ActorSystem, reason)
	if !errors.Is(err, ErrAlreadyJailed) {
		return err
	}

	// Already serving. Extend to the later of the two ends, never earlier.
	// Only release_at moves: the stored snapshot holds their real pre-jail
	// roles and is the only copy, so re-recording it now would capture the
	// marker role alone. Same reasoning as jailMany's redate branch.
	rec, ok, err := p.store.GetJail(ctx, guildID, userID)
	if err != nil {
		return fmt.Errorf("roles: read existing jail: %w", err)
	}
	if !ok {
		// Released in the gap between the insert losing its race and this
		// read. Nothing to extend, and re-jailing here would race the same
		// way again; the next offence will jail them cleanly.
		return nil
	}
	if rec.ReleaseAt == nil {
		// Indefinite already. Nothing an automatic escalation can add.
		return nil
	}
	releaseAt := p.now().Add(duration)
	if !releaseAt.After(*rec.ReleaseAt) {
		return nil
	}
	if err := p.store.SetJailRelease(ctx, guildID, userID, &releaseAt); err != nil {
		return fmt.Errorf("roles: extend jail: %w", err)
	}
	return nil
}
