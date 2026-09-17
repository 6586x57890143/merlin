package roles

import (
	"context"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
)

// Telling the ledger what happened.
//
// Every jail, re-sentence and release this plugin performs is published as a
// core.EventModerationAction so the rapsheet plugin can record it. The bus,
// not an injected interface, because this plugin must not care whether a
// ledger exists or is enabled in the guild: the jail already happened and
// there is nothing it could do with an error. Published after the mutation
// succeeded, beside the audit write, on the same footing: a subscriber that
// panics is isolated by the bus, and one that fails logs for itself.

// publishJailed reports a fresh jail.
func (p *Plugin) publishJailed(ctx context.Context, guildID, userID, actor, reason string, duration time.Duration, releaseAt time.Time) {
	p.publish(ctx, guildID, core.ModerationActionPayload{
		UserID: userID, Kind: "jail", ActorID: actor, Reason: reason,
		Duration: duration, EndsAt: &releaseAt, Source: p.Name(),
	})
}

// publishResentenced reports a moved end date. A note rather than a second
// jail, for the same reason the audit log gets roles.jail_resentenced and
// not a second roles.jail: nobody was jailed here, and a ledger counting
// jails would otherwise count this member twice.
func (p *Plugin) publishResentenced(ctx context.Context, guildID, userID, actor, reason string, duration time.Duration) {
	note := "sentence moved to " + core.FormatDuration(duration)
	if reason != "" {
		note += ": " + reason
	}
	p.publish(ctx, guildID, core.ModerationActionPayload{
		UserID: userID, Kind: "note", ActorID: actor, Reason: note, Source: p.Name(),
	})
}

// publishReleased reports a release, by a mod or by the clock.
func (p *Plugin) publishReleased(ctx context.Context, guildID, userID, actor string) {
	reason := "released early"
	if actor == core.ActorSystem {
		reason = "sentence served"
	}
	p.publish(ctx, guildID, core.ModerationActionPayload{
		UserID: userID, Kind: "release", ActorID: actor, Reason: reason, Source: p.Name(),
	})
}

func (p *Plugin) publish(ctx context.Context, guildID string, payload core.ModerationActionPayload) {
	if p.bus == nil {
		return
	}
	p.bus.Publish(ctx, core.Event{Type: core.EventModerationAction, GuildID: guildID, Payload: payload})
}
