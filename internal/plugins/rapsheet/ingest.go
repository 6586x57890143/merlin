package rapsheet

import (
	"context"
	"strings"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
)

// Ingestion: what other plugins did, arriving over the bus.
//
// roles publishes every jail, re-sentence and release; aimod publishes every
// removal and rewrite, and every undo. Neither imports this package or
// knows whether it is enabled. What arrives here is written through the
// same record() path a moderator's /rapsheet warn takes, so downstream
// nothing can tell them apart, and it is keyed on the publisher's own
// (source, ref) so an event delivered twice is written once.
//
// The handler runs on the publisher's goroutine (the bus is synchronous)
// and must not hold it: the write happens on a detached context in a
// goroutine of its own, because the publisher's context is often a command
// about to return, and a slow forum post must never stall roles' sweep.

// ladderReasonPrefix marks a jail the ladder itself asked roles for. It
// comes back over the bus like any other jail, and without this it would be
// recorded twice: once as the ladder's own consequence row, once as roles'
// report of having done it.
const ladderReasonPrefix = "rapsheet #"

// ingestTimeout bounds one ingested write, mirror included.
const ingestTimeout = 20 * time.Second

// enabled is the gate check for paths outside the CommandRouter.
func (p *Plugin) enabled(guildID string) bool {
	return p.gate == nil || p.gate.PluginEnabled(guildID, p.Name())
}

func (p *Plugin) subscribe() {
	if p.bus == nil {
		return
	}
	p.bus.Subscribe(core.EventModerationAction, p.Name(), p.handleModerationAction)
	p.bus.Subscribe(core.EventModerationReversed, p.Name(), p.handleModerationReversed)
}

// ingestKinds is what a publisher may call an action. Anything else is a
// publisher this build does not know, logged and dropped rather than
// written under a kind the CHECK constraint would refuse anyway.
var ingestKinds = map[string]Kind{
	"jail":    KindJail,
	"release": KindRelease,
	"removal": KindRemoval,
	"note":    KindNote,
	"timeout": KindTimeout,
	"kick":    KindKick,
	"ban":     KindBan,
	"unban":   KindUnban,
}

func (p *Plugin) handleModerationAction(_ context.Context, ev core.Event) {
	payload, ok := ev.Payload.(core.ModerationActionPayload)
	if !ok || !p.enabled(ev.GuildID) {
		return
	}
	if payload.Source == string(SourceRoles) && strings.HasPrefix(payload.Reason, ladderReasonPrefix) {
		return
	}
	kind, ok := ingestKinds[payload.Kind]
	if !ok {
		p.log.Warn("rapsheet: unknown moderation kind on the bus, dropped", "kind", payload.Kind, "source", payload.Source)
		return
	}
	source := Source(payload.Source)
	switch source {
	case SourceRoles, SourceAIMod, SourceDiscord:
	default:
		p.log.Warn("rapsheet: unknown moderation source on the bus, dropped", "source", payload.Source)
		return
	}
	category := Category(payload.Category)
	if category == "" {
		// A jail or release names no rule of Discord's; it is the server's
		// own decision.
		category = CategoryServerRule
	}
	in := newEntry{
		GuildID: ev.GuildID, UserID: payload.UserID, Kind: kind, Category: category,
		ActorID: payload.ActorID, Reason: payload.Reason, Duration: payload.Duration,
		EndsAt: payload.EndsAt, Source: source, Ref: payload.Ref,
	}
	p.detached(func(ctx context.Context) {
		cfg := p.config(ctx, ev.GuildID)
		if _, _, err := p.record(ctx, cfg, in); err != nil {
			p.log.Error("rapsheet: ingest moderation action",
				"guild", ev.GuildID, "user", payload.UserID, "kind", payload.Kind, "source", payload.Source, "err", err)
		}
	})
}

func (p *Plugin) handleModerationReversed(_ context.Context, ev core.Event) {
	payload, ok := ev.Payload.(core.ModerationReversedPayload)
	if !ok || !p.enabled(ev.GuildID) || payload.Ref == "" {
		return
	}
	p.detached(func(ctx context.Context) {
		e, found, err := p.store.EntryByRef(ctx, ev.GuildID, Source(payload.Source), payload.Ref)
		if err != nil {
			p.log.Error("rapsheet: look up reversed entry", "guild", ev.GuildID, "source", payload.Source, "ref", payload.Ref, "err", err)
			return
		}
		if !found || e.Voided() {
			return
		}
		by := payload.ActorID
		if by == "" {
			by = core.ActorSystem
		}
		reason := payload.Reason
		if reason == "" {
			reason = "reversed"
		}
		now := p.now()
		if err := p.store.Void(ctx, ev.GuildID, e.ID, by, reason, now); err != nil {
			p.log.Error("rapsheet: void reversed entry", "guild", ev.GuildID, "case", e.ID, "err", err)
			return
		}
		e.VoidedAt, e.VoidedBy, e.VoidReason = &now, by, reason
		p.afterAmend(ctx, e)
	})
}

// detached runs fn off the publisher's goroutine, under its own timeout
// and its own recover. The bus recovers a panic in the handler itself, but
// not in a goroutine the handler starts.
func (p *Plugin) detached(fn func(ctx context.Context)) {
	run := func() {
		defer func() {
			if r := recover(); r != nil {
				p.log.Error("rapsheet: ingest panicked", "panic", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), ingestTimeout)
		defer cancel()
		fn(ctx)
	}
	if p.synchronous {
		run()
		return
	}
	go run()
}
