package core

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"time"

	"github.com/bwmarrin/discordgo"
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

// creates is every command as disgo sends it, converted once at Finalize.
// The plugins still define theirs on discordgo's types; a definition whose
// JSON changes on the way is refused at boot rather than registered wrong.
func creates(cmds []*discordgo.ApplicationCommand) ([]discord.ApplicationCommandCreate, error) {
	out := make([]discord.ApplicationCommandCreate, 0, len(cmds))
	for _, cmd := range cmds {
		// discordgo leaves out a slash command's type, 1, as its zero.
		typed := *cmd
		if typed.Type == 0 {
			typed.Type = discordgo.ChatApplicationCommand
		}
		raw, err := json.Marshal(typed)
		if err != nil {
			return nil, fmt.Errorf("core: command %q: %w", cmd.Name, err)
		}
		// Options are interfaces in disgo, read through its own decoder.
		var in struct {
			discord.SlashCommandCreate
			Options []discord.UnmarshalApplicationCommandOption `json:"options"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, fmt.Errorf("core: command %q does not read as a slash command: %w", cmd.Name, err)
		}
		c := in.SlashCommandCreate
		for _, o := range in.Options {
			c.Options = append(c.Options, o.ApplicationCommandOption)
		}
		back, err := json.Marshal(c)
		if err != nil {
			return nil, fmt.Errorf("core: command %q: %w", cmd.Name, err)
		}
		if !sameJSON(raw, back) {
			return nil, fmt.Errorf("core: command %q changes on its way to disgo:\n%s\n%s", cmd.Name, raw, back)
		}
		out = append(out, c)
	}
	return out, nil
}

// sameJSON is whether a and b say the same, counting an absent field and a
// zero one alike: each library omits a different set of zeros.
func sameJSON(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(meaningful(x), meaningful(y))
}

func meaningful(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range maps.All(t) {
			if e = meaningful(e); e != nil {
				out[k] = e
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, e := range t {
			out = append(out, meaningful(e))
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case bool:
		if !t {
			return nil
		}
	case string:
		if t == "" {
			return nil
		}
	case float64:
		if t == 0 {
			return nil
		}
	}
	return v
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
