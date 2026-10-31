package core

import (
	"context"
	"fmt"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

// RegisterGuild sets guildID's commands to the full merged set in one
// request: a single writer across every plugin, so two plugins can't wipe
// out each other's commands (the bug this router replaces). Per guild
// rather than global so commands appear instantly instead of after
// Discord's hour of global propagation. Called for every guild as it
// arrives (cmd/bot/main.go).
func (r *CommandRouter) RegisterGuild(appID, guildID snowflake.ID) error {
	if _, err := r.rest.SetGuildCommands(appID, guildID, r.creates); err != nil {
		return fmt.Errorf("register commands for guild %s: %w", guildID, err)
	}
	return nil
}

// PurgeGlobalCommands empties Discord's global command scope. merlin
// registers per guild only, but code before Milestone 4 once created a
// global /admin and /ping, and a guild's own overwrite never touches the
// global scope, so they sat in every server's list. An empty, non-nil list
// is what clears it; every boot re-confirms that.
func (r *CommandRouter) PurgeGlobalCommands(appID snowflake.ID) error {
	if _, err := r.rest.SetGlobalCommands(appID, []discord.ApplicationCommandCreate{}); err != nil {
		return fmt.Errorf("purge global commands: %w", err)
	}
	return nil
}

// respondWindow is how long Discord waits for an interaction's first
// response, and autoDeferAfter is when the router acknowledges for a
// handler that has not: early enough that the defer itself lands inside the
// window.
const (
	respondWindow  = 3 * time.Second
	autoDeferAfter = 2 * time.Second
)

// handlerCtx is a handler's context, limit at most. A handler that has not
// answered by autoDeferAfter is deferred for (Ix.autoDefer) and carries on:
// its answer lands as an edit or a follow-up, so slow work (a key sealed and
// stored, an audit card uploaded, a panel redrawn) still finishes and is
// still shown. This used to cancel the context there instead, which killed
// the very write the member was waiting on and still left them with "the
// application did not respond".
func handlerCtx(ix *Ix, limit time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	stop := time.AfterFunc(untilAutoDefer(ix.I.ID().Time(), time.Now()), ix.autoDefer)
	return ctx, func() {
		stop.Stop()
		cancel()
	}
}

// untilAutoDefer is how long from now to wait before deferring an
// interaction created at created. Discord's clock is the snowflake's; an
// age outside the window can only be this host's clock disagreeing (Discord
// would already have dropped it), so then it is measured from arrival.
func untilAutoDefer(created, now time.Time) time.Duration {
	age := now.Sub(created)
	if age < 0 || age > respondWindow {
		return autoDeferAfter
	}
	return autoDeferAfter - age
}
