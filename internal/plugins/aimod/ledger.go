package aimod

import (
	"context"
	"strconv"

	"github.com/6586x57890143/merlin/internal/core"
)

// Telling the ledger what happened.
//
// Every removal and rewrite this plugin performs, and every undo, is
// published on core.EventBus so the rapsheet plugin can keep the member's
// record. The bus rather than an injected interface, for the reason roles
// uses it too: the message is already gone and there is nothing this plugin
// could do with an error from a ledger. Published beside the audit write,
// after the action succeeded.
//
// The sanction that may follow a removal is not published from here. It goes
// through roles.JailAutomatic, and roles publishes the jail itself; what this
// plugin publishes is the offence, which is what carries the points.

// publishRemoval reports a removed or rewritten message. Ref is the incident
// id, which is what /aimod undo will name when it takes the action back.
func (p *Plugin) publishRemoval(ctx context.Context, guildID string, incidentID int64, c candidate, action Action, bucket Bucket, v deepVerdict) {
	if p.bus == nil {
		return
	}
	p.bus.Publish(ctx, core.Event{Type: core.EventModerationAction, GuildID: guildID, Payload: core.ModerationActionPayload{
		UserID:   c.AuthorID,
		Kind:     "removal",
		Category: string(bucket),
		ActorID:  core.ActorSystem,
		Reason:   "message " + pastTense(action) + " automatically: " + v.Reason,
		Source:   p.Name(),
		Ref:      strconv.FormatInt(incidentID, 10),
	}})
}

// publishReversed reports that a moderator undid the incident.
func (p *Plugin) publishReversed(ctx context.Context, guildID string, incidentID int64, actorID string) {
	if p.bus == nil {
		return
	}
	p.bus.Publish(ctx, core.Event{Type: core.EventModerationReversed, GuildID: guildID, Payload: core.ModerationReversedPayload{
		Source:  p.Name(),
		Ref:     strconv.FormatInt(incidentID, 10),
		ActorID: actorID,
		Reason:  "undone by a moderator",
	}})
}

func pastTense(a Action) string {
	switch a {
	case ActionRewrite:
		return "rewritten"
	case ActionRemove:
		return "removed"
	}
	return string(a)
}
