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

// respondBy is Discord's 3s window for an interaction's first response,
// less room for the error reply itself.
const respondBy = 2500 * time.Millisecond

// handlerCtx is a handler's context: limit at most, and cancelled at
// respondBy from Discord creating the interaction if nothing has answered
// by then, so a slow call fails in time for the member to be told why
// rather than seeing "the application did not respond". A handler that has
// answered or deferred keeps the rest of limit for its work; skua's
// handlers are cut at respondBy outright, merlin's defer and carry on.
func handlerCtx(ix *Ix, limit time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	stop := time.AfterFunc(time.Until(ix.I.ID().Time().Add(respondBy)), func() {
		if !ix.answering.Load() {
			cancel()
		}
	})
	return ctx, func() {
		stop.Stop()
		cancel()
	}
}
