package core

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/voice"
)

// CommandHandler handles one fully-resolved command/subcommand invocation.
// Handlers are responsible for calling s.InteractionRespond themselves (the
// response shape (deferred, ephemeral, modal, etc.) varies too much to
// usefully wrap).
type CommandHandler func(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate)

// AutocompleteHandler returns suggestions for the currently-focused option
// of a leaf. focusedValue is the partial text the invoking user has typed so
// far. Discord caps the response at 25 choices; CommandRouter truncates for
// you if more are returned.
type AutocompleteHandler func(ctx context.Context, i *discordgo.InteractionCreate, focusedOption, focusedValue string) []*discordgo.ApplicationCommandOptionChoice

// ComponentHandler handles a message-component interaction (a button click or
// select-menu choice) whose CustomID matched a registered prefix. customID is
// the component's full CustomID, so a handler can decode whatever state it
// encoded into it (e.g. a page number, or which record a select menu row
// refers to). Components carry no other server-side session of their own.
type ComponentHandler func(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string)

// ModalHandler handles a submitted modal whose CustomID matched a registered
// prefix. customID is the modal's full CustomID, so a handler can decode
// whatever state it encoded into it (which record the modal was opened for,
// say). Modals exist here for one reason: they are the only Discord surface
// that takes free text from a member without that text ever appearing in a
// channel or in their command history, which is what a prize code needs.
type ModalHandler func(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string)

type registeredLeaf struct {
	spec         PermSpec
	handler      CommandHandler
	autocomplete AutocompleteHandler
}

type registeredComponent struct {
	pluginName string
	spec       PermSpec
	handler    ComponentHandler
}

type registeredModal struct {
	pluginName string
	spec       PermSpec
	handler    ModalHandler
}

// CommandRouter is the single owner of slash-command registration and
// dispatch (spec.MD §4a). Plugins never call session.AddHandler or
// discordgo's ApplicationCommand{Create,BulkOverwrite} themselves. This
// fixes two real gaps in the pre-Milestone-4 code: (1) two plugins each
// calling ApplicationCommandBulkOverwrite independently would silently wipe
// out each other's commands, since bulk-overwrite replaces the entire scope,
// not just the caller's own commands; (2) discordgo's own event dispatch has
// no recover() of its own (confirmed against the vendored source), so a
// panic in any one plugin's handler could take down the whole process.
// Every dispatch here runs under recover().
type CommandRouter struct {
	perms *Permissions
	gate  PluginGate
	log   *slog.Logger
	// voice supplies the wording for the refusals below. Optional: a nil
	// speaker falls back to the plain sentences, so a router built in a
	// test does not have to care that merlin has a personality.
	voice voice.Source

	topLevel       []*discordgo.ApplicationCommand
	topLevelPlugin map[string]string // top-level command name -> owning plugin name
	leaves         map[string]*registeredLeaf
	components     map[string]*registeredComponent // CustomID prefix -> handler
	modals         map[string]*registeredModal     // CustomID prefix -> handler
}

func NewCommandRouter(perms *Permissions, gate PluginGate, log *slog.Logger) *CommandRouter {
	return &CommandRouter{
		perms:          perms,
		gate:           gate,
		log:            log,
		leaves:         make(map[string]*registeredLeaf),
		topLevelPlugin: make(map[string]string),
		components:     make(map[string]*registeredComponent),
		modals:         make(map[string]*registeredModal),
	}
}

// WithVoice gives the router merlin's own wording for the refusals it
// sends. These are the messages ordinary members hit most often, by
// clicking a stale button or trying a command that is not theirs, so they
// are worth sounding like her rather than like a 403.
//
// Optional on purpose. The plain sentences are still in the code as the
// fallback, so a refusal is never silently dropped because a catalog
// failed to load.
func (r *CommandRouter) WithVoice(v voice.Source) *CommandRouter {
	r.voice = v
	return r
}

// say returns merlin's wording for key, or plain if no voice is wired.
func (r *CommandRouter) say(key voice.Key, plain string) string {
	if r.voice == nil {
		return plain
	}
	if line := r.voice.Line(context.Background(), "", key, nil); line != "" {
		return line
	}
	return plain
}

// RegisterCommand adds one top-level command, owned by pluginName (used for
// the per-guild plugin enable/disable gate, see PluginGate). cmd's Options
// may nest subcommands/subcommand groups arbitrarily (as discordgo itself
// requires); call Handle for every invocable leaf path underneath it before
// Finalize.
func (r *CommandRouter) RegisterCommand(pluginName string, cmd *discordgo.ApplicationCommand) {
	r.topLevel = append(r.topLevel, cmd)
	r.topLevelPlugin[cmd.Name] = pluginName
}

// Plugins returns every distinct plugin name that has registered a top-level
// command, used by /config plugins enable/disable's autocomplete.
func (r *CommandRouter) Plugins() []string {
	seen := make(map[string]bool, len(r.topLevelPlugin))
	var out []string
	for _, name := range r.topLevelPlugin {
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// Handle registers the handler and required PermSpec for one leaf path.
// path is empty for a bare top-level command (e.g. "ping"), a single
// segment for a direct subcommand ("run-now"), or "group/subcommand" for a
// subcommand nested in a group ("configure/set-interval"), mirroring
// exactly the nesting depth Discord itself allows (max two levels below the
// top-level command). spec.Tier must not be the zero value, and Action must
// be set for anything above TierPublic. Both are validated at Finalize, not
// here, so registration order doesn't matter.
func (r *CommandRouter) Handle(topLevelName, path string, spec PermSpec, handler CommandHandler) {
	r.leaves[leafKey(topLevelName, path)] = &registeredLeaf{spec: spec, handler: handler}
}

// Autocomplete attaches fn to an already-Handle'd leaf. Panics if the leaf
// doesn't exist yet: call Handle for the same path first.
func (r *CommandRouter) Autocomplete(topLevelName, path string, fn AutocompleteHandler) {
	key := leafKey(topLevelName, path)
	leaf, ok := r.leaves[key]
	if !ok {
		panic(fmt.Sprintf("core: Autocomplete registered before Handle for %q", key))
	}
	leaf.autocomplete = fn
}

// HandleComponent registers fn for every message-component interaction
// (button click, select-menu choice) whose CustomID starts with prefix.
// Callers namespace their own prefixes (e.g. "rotation:list:") to avoid
// collisions; the longest matching prefix wins if more than one matches.
// pluginName and spec are checked exactly like a slash command's (plugin
// enabled, then Authorize) before fn runs: a component interaction is its
// own fresh interaction, not an extension of whatever originally rendered
// it, so it gets no exemption from either check.
func (r *CommandRouter) HandleComponent(pluginName, prefix string, spec PermSpec, fn ComponentHandler) {
	r.components[prefix] = &registeredComponent{pluginName: pluginName, spec: spec, handler: fn}
}

// HandleModal registers fn for every modal submission whose CustomID starts
// with prefix. Same rules as HandleComponent: callers namespace their own
// prefixes, the longest match wins, and pluginName and spec are re-checked
// before fn runs. A modal is opened from an earlier interaction that was
// already authorized, but the submission arrives as its own interaction
// minutes later and gets no exemption for that.
func (r *CommandRouter) HandleModal(pluginName, prefix string, spec PermSpec, fn ModalHandler) {
	r.modals[prefix] = &registeredModal{pluginName: pluginName, spec: spec, handler: fn}
}

// Actions returns every distinct, non-empty PermSpec.Action currently
// registered, used by /config permissions grant/revoke's autocomplete so
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
	for prefix, rc := range r.components {
		if rc.spec.Tier == tierUnset {
			return fmt.Errorf("core: component %q registered with no PermSpec.Tier", prefix)
		}
		if rc.spec.Tier != TierPublic && rc.spec.Action == "" {
			return fmt.Errorf("core: component %q is above TierPublic but has no PermSpec.Action", prefix)
		}
	}
	// Modals are validated on the same terms as components, and for the same
	// reason: a submission arrives as its own interaction minutes after
	// whatever opened it, so an unset tier here is exactly as dangerous as
	// one on a command.
	for prefix, rm := range r.modals {
		if rm.spec.Tier == tierUnset {
			return fmt.Errorf("core: modal %q registered with no PermSpec.Tier", prefix)
		}
		if rm.spec.Tier != TierPublic && rm.spec.Action == "" {
			return fmt.Errorf("core: modal %q is above TierPublic but has no PermSpec.Action", prefix)
		}
	}
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
// guildID with the full merged command set: a single writer across every
// plugin, so two plugins independently bulk-overwriting can't silently wipe
// out each other's commands (the bug this router replaces). Scoped
// per-guild rather than global so commands appear instantly instead of
// waiting on Discord's up-to-an-hour global-propagation delay. Call once per
// guild the bot is in at startup, and again whenever it joins a new one
// (both naturally driven by discordgo's GuildCreate event, see
// cmd/bot/main.go).
func (r *CommandRouter) RegisterGuild(s *discordgo.Session, appID, guildID string) error {
	if _, err := s.ApplicationCommandBulkOverwrite(appID, guildID, r.topLevel); err != nil {
		return fmt.Errorf("register commands for guild %s: %w", guildID, err)
	}
	return nil
}

// PurgeGlobalCommands removes every application command currently registered
// in Discord's global scope. This bot registers exclusively per-guild
// (RegisterGuild), and nothing in this codebase should ever create a global
// command, but pre-Milestone-4 code once did (a global "/admin" from the
// original Scheduler plugin, a global "/ping" from the original ping plugin,
// both via session.ApplicationCommandCreate(..., "", cmd) with an empty
// guildID). Global commands are a completely separate scope from any guild's
// commands, so RegisterGuild's per-guild BulkOverwrite never touched them:
// they've sat there as duplicate/orphaned entries in every guild's command
// list ever since, one of them ("/admin") pointing at a handler that no
// longer exists.
//
// Pass an explicit empty (non-nil) slice, not nil: discordgo marshals a nil
// slice as JSON `null`, which is not the same request as an empty array `[]`
// and may not clear anything, the exact class of bug already fixed once in
// this codebase for channel permission overwrites (see core.channels.go).
//
// Call once at startup, before opening the session. Bulk-overwriting an
// already-empty global scope is a harmless no-op, so this needs no
// one-time-only guard: every boot just re-confirms the invariant.
func (r *CommandRouter) PurgeGlobalCommands(s *discordgo.Session, appID string) error {
	if _, err := s.ApplicationCommandBulkOverwrite(appID, "", []*discordgo.ApplicationCommand{}); err != nil {
		return fmt.Errorf("purge global commands: %w", err)
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
	case discordgo.InteractionMessageComponent:
		r.dispatchComponent(s, i)
	case discordgo.InteractionModalSubmit:
		r.dispatchModal(s, i)
	}
}

// matchComponent finds the registered component whose prefix is the
// longest match for customID, or nil if none matches. Split out from
// dispatchComponent so the matching logic itself is unit-testable without
// needing a live Discord session.
func (r *CommandRouter) matchComponent(customID string) *registeredComponent {
	var matched *registeredComponent
	var matchedPrefix string
	for prefix, rc := range r.components {
		if strings.HasPrefix(customID, prefix) && len(prefix) > len(matchedPrefix) {
			matched, matchedPrefix = rc, prefix
		}
	}
	return matched
}

// dispatchComponent resolves a button/select-menu interaction to whichever
// registered prefix its CustomID starts with (longest match wins, so two
// plugins can't accidentally shadow each other), then runs the exact same
// plugin-enabled + Authorize checks dispatchCommand does: a component
// interaction is its own fresh interaction, never exempt from either check
// just because it followed a successful command.
func (r *CommandRouter) dispatchComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Error("component handler panicked", "panic", rec)
			r.respondEphemeral(s, i, r.say(voice.KeyBroken, "Something went wrong handling that."))
		}
	}()

	// Same fail-closed guard as dispatchCommand: GuildID is the settings
	// cache key both checks below depend on.
	if i.GuildID == "" {
		r.respondEphemeral(s, i, r.say(voice.KeyGuildOnly, "This only works inside a server."))
		return
	}

	customID := i.MessageComponentData().CustomID
	matched := r.matchComponent(customID)
	if matched == nil {
		r.log.Error("component dispatch: no handler registered", "custom_id", customID)
		r.respondEphemeral(s, i, r.say(voice.KeyStaleComponent, "This button or menu isn't wired up yet."))
		return
	}

	if !r.gate.PluginEnabled(i.GuildID, matched.pluginName) {
		r.respondEphemeral(s, i, r.say(voice.KeyPluginDisabled, "This feature is disabled in this server."))
		return
	}
	if err := r.perms.Authorize(i, matched.spec); err != nil {
		r.respondEphemeral(s, i, r.say(voice.KeyDenied, "You are not allowed to do that."))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	matched.handler(ctx, s, i, customID)
}

// matchModal is matchComponent for modal submissions. Split out for the same
// reason: the matching itself is worth testing without a live session.
func (r *CommandRouter) matchModal(customID string) *registeredModal {
	var matched *registeredModal
	var matchedPrefix string
	for prefix, rm := range r.modals {
		if strings.HasPrefix(customID, prefix) && len(prefix) > len(matchedPrefix) {
			matched, matchedPrefix = rm, prefix
		}
	}
	return matched
}

// dispatchModal resolves a submitted modal to whichever registered prefix its
// CustomID starts with, then runs the same plugin-enabled and Authorize
// checks every other dispatch runs.
//
// Deliberately a copy of dispatchComponent's shape rather than a shared
// generic: the two differ only in which map they read, and the checks are the
// part that must not drift, so having them written out where a reviewer can
// see them is worth twenty duplicated lines.
func (r *CommandRouter) dispatchModal(s *discordgo.Session, i *discordgo.InteractionCreate) {
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Error("modal handler panicked", "panic", rec)
			r.respondEphemeral(s, i, r.say(voice.KeyBroken, "Something went wrong handling that."))
		}
	}()

	if i.GuildID == "" {
		r.respondEphemeral(s, i, r.say(voice.KeyGuildOnly, "This only works inside a server."))
		return
	}

	customID := i.ModalSubmitData().CustomID
	matched := r.matchModal(customID)
	if matched == nil {
		r.log.Error("modal dispatch: no handler registered", "custom_id", customID)
		r.respondEphemeral(s, i, r.say(voice.KeyStaleComponent, "This form isn't wired up yet."))
		return
	}

	if !r.gate.PluginEnabled(i.GuildID, matched.pluginName) {
		r.respondEphemeral(s, i, r.say(voice.KeyPluginDisabled, "This feature is disabled in this server."))
		return
	}
	if err := r.perms.Authorize(i, matched.spec); err != nil {
		r.respondEphemeral(s, i, r.say(voice.KeyDenied, "You are not allowed to do that."))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	matched.handler(ctx, s, i, customID)
}

func (r *CommandRouter) dispatchCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Error("command handler panicked", "panic", rec)
			r.respondEphemeral(s, i, r.say(voice.KeyBroken, "Something went wrong running that command."))
		}
	}()

	// Every command is registered per guild, so a DM interaction shouldn't be
	// reachable, but i.GuildID feeds straight into the settings cache key
	// for both the plugin gate and Authorize, and an empty key there resolves
	// to a guild with no mod roles and no admins. Refusing outright is a
	// cheap guard on the one path that must never fail open, and it costs
	// nothing on the path that actually happens.
	if i.GuildID == "" {
		r.respondEphemeral(s, i, r.say(voice.KeyGuildOnly, "This command only works inside a server."))
		return
	}

	data := i.ApplicationCommandData()
	path, _ := resolveLeaf(data)
	key := leafKey(data.Name, path)
	leaf, ok := r.leaves[key]
	if !ok {
		r.log.Error("command dispatch: no handler registered", "key", key)
		r.respondEphemeral(s, i, r.say(voice.KeyStaleComponent, "This command isn't wired up yet."))
		return
	}

	if !r.gate.PluginEnabled(i.GuildID, r.topLevelPlugin[data.Name]) {
		r.respondEphemeral(s, i, r.say(voice.KeyPluginDisabled, "This feature is disabled in this server."))
		return
	}

	if err := r.perms.Authorize(i, leaf.spec); err != nil {
		r.respondEphemeral(s, i, r.say(voice.KeyDenied, "You are not allowed to run this command."))
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

	if !r.gate.PluginEnabled(i.GuildID, r.topLevelPlugin[data.Name]) {
		return
	}
	if err := r.perms.Authorize(i, leaf.spec); err != nil {
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
// the Options tree themselves. Every plugin handler should use this instead
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

// errCodeInteractionAlreadyAcknowledged is Discord's 40060, returned when an
// interaction has already been responded to or deferred. discordgo doesn't
// export a constant for it.
const errCodeInteractionAlreadyAcknowledged = 40060

// respondEphemeral answers an interaction privately, falling back to a
// follow-up if it has already been acknowledged.
//
// The fallback matters most on the path that needs it least often: the panic
// recovery in dispatchCommand runs whether or not the handler had already
// responded before panicking. A handler that deferred, did some work, and
// then panicked would have this reply rejected with 40060, and the error
// was previously discarded, so the user saw nothing at all and the logs said
// nothing either. A panic mid-command is exactly when someone needs to be
// told something went wrong.
func (r *CommandRouter) respondEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, msg string) {
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: msg,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
	if err == nil {
		return
	}
	if HasDiscordErrorCode(err, errCodeInteractionAlreadyAcknowledged) {
		if _, ferr := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
			Content: msg,
			Flags:   discordgo.MessageFlagsEphemeral,
		}); ferr != nil {
			r.log.Error("interaction follow-up failed after already-acknowledged", "err", ferr)
		}
		return
	}
	r.log.Error("interaction response failed", "err", err)
}

// ModalValues returns a submitted modal's text inputs keyed by their
// CustomID. Discord nests every input inside its own ActionsRow, so reading
// one field means two levels of type assertion; this exists so that walk has
// exactly one implementation, for the same reason LeafArgs does.
//
// A missing key reads as the empty string, which is what an optional field
// means anyway, so callers check for content rather than for presence.
func ModalValues(i *discordgo.InteractionCreate) map[string]string {
	out := make(map[string]string)
	for _, row := range i.ModalSubmitData().Components {
		ar, ok := row.(*discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, c := range ar.Components {
			if in, ok := c.(*discordgo.TextInput); ok {
				out[in.CustomID] = in.Value
			}
		}
	}
	return out
}
