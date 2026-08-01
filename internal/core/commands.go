package core

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// CommandHandler handles one fully-resolved command/subcommand invocation.
// Handlers are responsible for calling s.InteractionRespond themselves (the
// response shape — deferred, ephemeral, modal, etc. — varies too much to
// usefully wrap).
type CommandHandler func(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate)

// AutocompleteHandler returns suggestions for the currently-focused option
// of a leaf. focusedValue is the partial text the invoking user has typed so
// far. Discord caps the response at 25 choices; CommandRouter truncates for
// you if more are returned.
type AutocompleteHandler func(ctx context.Context, i *discordgo.InteractionCreate, focusedOption, focusedValue string) []*discordgo.ApplicationCommandOptionChoice

type registeredLeaf struct {
	spec         PermSpec
	handler      CommandHandler
	autocomplete AutocompleteHandler
}

// CommandRouter is the single owner of slash-command registration and
// dispatch (spec.MD §4a). Plugins never call session.AddHandler or
// discordgo's ApplicationCommand{Create,BulkOverwrite} themselves. This
// fixes two real gaps in the pre-Milestone-4 code: (1) two plugins each
// calling ApplicationCommandBulkOverwrite independently would silently wipe
// out each other's commands, since bulk-overwrite replaces the entire scope,
// not just the caller's own commands; (2) discordgo's own event dispatch has
// no recover() of its own (confirmed against the vendored source), so a
// panic in any one plugin's handler could take down the whole process —
// every dispatch here runs under recover().
type CommandRouter struct {
	perms *Permissions
	log   *slog.Logger

	topLevel []*discordgo.ApplicationCommand
	leaves   map[string]*registeredLeaf
}

func NewCommandRouter(perms *Permissions, log *slog.Logger) *CommandRouter {
	return &CommandRouter{perms: perms, log: log, leaves: make(map[string]*registeredLeaf)}
}

// RegisterCommand adds one top-level command. cmd's Options may nest
// subcommands/subcommand groups arbitrarily (as discordgo itself requires);
// call Handle for every invocable leaf path underneath it before Finalize.
func (r *CommandRouter) RegisterCommand(cmd *discordgo.ApplicationCommand) {
	r.topLevel = append(r.topLevel, cmd)
}

// Handle registers the handler and required PermSpec for one leaf path.
// path is empty for a bare top-level command (e.g. "ping"), a single
// segment for a direct subcommand ("run-now"), or "group/subcommand" for a
// subcommand nested in a group ("configure/set-interval") — mirroring
// exactly the nesting depth Discord itself allows (max two levels below the
// top-level command). spec.Tier must not be the zero value, and Action must
// be set for anything above TierPublic — both are validated at Finalize, not
// here, so registration order doesn't matter.
func (r *CommandRouter) Handle(topLevelName, path string, spec PermSpec, handler CommandHandler) {
	r.leaves[leafKey(topLevelName, path)] = &registeredLeaf{spec: spec, handler: handler}
}

// Autocomplete attaches fn to an already-Handle'd leaf. Panics if the leaf
// doesn't exist yet — call Handle for the same path first.
func (r *CommandRouter) Autocomplete(topLevelName, path string, fn AutocompleteHandler) {
	key := leafKey(topLevelName, path)
	leaf, ok := r.leaves[key]
	if !ok {
		panic(fmt.Sprintf("core: Autocomplete registered before Handle for %q", key))
	}
	leaf.autocomplete = fn
}

// Actions returns every distinct, non-empty PermSpec.Action currently
// registered — used by /config permissions grant/revoke's autocomplete so
// admins can discover valid action names without reading source code.
func (r *CommandRouter) Actions() []string {
	seen := make(map[string]bool)
	var out []string
	for _, leaf := range r.leaves {
		if leaf.spec.Action == "" || seen[leaf.spec.Action] {
			continue
		}
		seen[leaf.spec.Action] = true
		out = append(out, leaf.spec.Action)
	}
	return out
}

func leafKey(topLevelName, path string) string {
	if path == "" {
		return topLevelName
	}
	return topLevelName + "/" + path
}

// Finalize validates every registered leaf has a valid PermSpec (fail
// closed: a plugin author who forgets to set Tier is caught at startup, not
// discovered later as an accidentally-public command) and that every leaf
// path reachable from a registered top-level command's Options tree has a
// matching handler (catching the opposite mistake: a subcommand that exists
// in Discord's UI but silently does nothing). Call once, after every
// plugin's Init has run and before the session opens.
func (r *CommandRouter) Finalize() error {
	for key, leaf := range r.leaves {
		if leaf.spec.Tier == tierUnset {
			return fmt.Errorf("core: command %q registered with no PermSpec.Tier", key)
		}
		if leaf.spec.Tier != TierPublic && leaf.spec.Action == "" {
			return fmt.Errorf("core: command %q is above TierPublic but has no PermSpec.Action", key)
		}
	}
	for _, cmd := range r.topLevel {
		for _, path := range leafPaths(cmd) {
			key := leafKey(cmd.Name, path)
			if _, ok := r.leaves[key]; !ok {
				return fmt.Errorf("core: command %q has no registered handler (call Handle)", key)
			}
		}
	}
	return nil
}

// RegisterGuild performs exactly one ApplicationCommandBulkOverwrite for
// guildID with the full merged command set — single writer across every
// plugin, so two plugins independently bulk-overwriting can't silently wipe
// out each other's commands (the bug this router replaces) — scoped
// per-guild rather than global so commands appear instantly instead of
// waiting on Discord's up-to-an-hour global-propagation delay. Call once per
// guild the bot is in at startup, and again whenever it joins a new one
// (both naturally driven by discordgo's GuildCreate event — see
// cmd/bot/main.go).
func (r *CommandRouter) RegisterGuild(s *discordgo.Session, appID, guildID string) error {
	if _, err := s.ApplicationCommandBulkOverwrite(appID, guildID, r.topLevel); err != nil {
		return fmt.Errorf("register commands for guild %s: %w", guildID, err)
	}
	return nil
}

// leafPaths walks cmd's Options tree and returns every invocable leaf path
// beneath it (mirroring resolveLeaf's own walk, so registration-time
// validation and dispatch-time resolution can never disagree).
func leafPaths(cmd *discordgo.ApplicationCommand) []string {
	hasSub := false
	for _, opt := range cmd.Options {
		if opt.Type == discordgo.ApplicationCommandOptionSubCommand || opt.Type == discordgo.ApplicationCommandOptionSubCommandGroup {
			hasSub = true
			break
		}
	}
	if !hasSub {
		return []string{""}
	}
	var paths []string
	for _, opt := range cmd.Options {
		walkOption(opt, nil, &paths)
	}
	return paths
}

func walkOption(opt *discordgo.ApplicationCommandOption, prefix []string, out *[]string) {
	segs := append(append([]string(nil), prefix...), opt.Name)
	if opt.Type == discordgo.ApplicationCommandOptionSubCommandGroup {
		for _, child := range opt.Options {
			walkOption(child, segs, out)
		}
		return
	}
	*out = append(*out, strings.Join(segs, "/"))
}

// HandleInteraction is the single session.AddHandler callback for the whole
// bot; construct it once (core.NewCommandRouter) and register it once.
func (r *CommandRouter) HandleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		r.dispatchCommand(s, i)
	case discordgo.InteractionApplicationCommandAutocomplete:
		r.dispatchAutocomplete(s, i)
	}
}

func (r *CommandRouter) dispatchCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Error("command handler panicked", "panic", rec)
			respondEphemeral(s, i, "Something went wrong running that command.")
		}
	}()

	data := i.ApplicationCommandData()
	path, _ := resolveLeaf(data)
	key := leafKey(data.Name, path)
	leaf, ok := r.leaves[key]
	if !ok {
		r.log.Error("command dispatch: no handler registered", "key", key)
		respondEphemeral(s, i, "This command isn't wired up yet.")
		return
	}

	if err := r.perms.Authorize(i, leaf.spec); err != nil {
		respondEphemeral(s, i, "You are not allowed to run this command.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	leaf.handler(ctx, s, i)
}

func (r *CommandRouter) dispatchAutocomplete(s *discordgo.Session, i *discordgo.InteractionCreate) {
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Error("autocomplete handler panicked", "panic", rec)
		}
	}()

	data := i.ApplicationCommandData()
	path, args := resolveLeaf(data)
	key := leafKey(data.Name, path)
	leaf, ok := r.leaves[key]
	if !ok || leaf.autocomplete == nil {
		return
	}

	var focusedName, focusedValue string
	for _, a := range args {
		if a.Focused {
			focusedName = a.Name
			if sv, ok := a.Value.(string); ok {
				focusedValue = sv
			}
			break
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	choices := leaf.autocomplete(ctx, i, focusedName, focusedValue)
	if len(choices) > 25 {
		choices = choices[:25]
	}
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{Choices: choices},
	})
}

// resolveLeaf walks data's Options while they're SubCommand/SubCommandGroup
// nodes, returning the joined path of subcommand names and the final
// argument list (either the leaf subcommand's own options, or, for a
// flat/no-subcommand top-level command, data.Options itself).
func resolveLeaf(data discordgo.ApplicationCommandInteractionData) (path string, args []*discordgo.ApplicationCommandInteractionDataOption) {
	var segs []string
	opts := data.Options
	for len(opts) > 0 && (opts[0].Type == discordgo.ApplicationCommandOptionSubCommand || opts[0].Type == discordgo.ApplicationCommandOptionSubCommandGroup) {
		segs = append(segs, opts[0].Name)
		isLeaf := opts[0].Type == discordgo.ApplicationCommandOptionSubCommand
		opts = opts[0].Options
		if isLeaf {
			break
		}
	}
	return strings.Join(segs, "/"), opts
}

// LeafArgs resolves i's invoked subcommand/subcommand-group path (the exact
// walk dispatchCommand itself performs) and returns its arguments keyed by
// name, so handlers that want direct-by-name access don't need to re-walk
// the Options tree themselves — every plugin handler should use this instead
// of hand-rolling the same walk, so there's exactly one implementation to
// keep in sync with Discord's nesting rules.
func LeafArgs(i *discordgo.InteractionCreate) map[string]*discordgo.ApplicationCommandInteractionDataOption {
	_, args := resolveLeaf(i.ApplicationCommandData())
	m := make(map[string]*discordgo.ApplicationCommandInteractionDataOption, len(args))
	for _, a := range args {
		m[a.Name] = a
	}
	return m
}

func respondEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, msg string) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: msg,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}
