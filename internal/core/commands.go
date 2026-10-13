package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/rest"

	"github.com/6586x57890143/merlin/internal/obs"
	"github.com/6586x57890143/merlin/internal/voice"
)

// IxCommandHandler handles one fully-resolved command/subcommand invocation.
// Handlers answer through the Ix themselves: the response shape (deferred,
// ephemeral, modal, ...) varies too much to usefully wrap.
//
// IxAutocompleteHandler returns suggestions for the currently-focused option
// of a leaf; focusedValue is what the invoking user has typed so far. Discord
// caps the response at 25 choices; the router truncates for you.
//
// IxComponentHandler handles a message-component interaction (a button click
// or select-menu choice) whose CustomID matched a registered prefix, and
// IxModalHandler a submitted modal. customID is the full CustomID, so a
// handler can decode whatever state it encoded into it. Modals exist here
// because they are the only Discord surface that takes free text from a
// member without it appearing in a channel or their command history.
type (
	IxCommandHandler      func(ctx context.Context, ix *Ix)
	IxAutocompleteHandler func(ctx context.Context, ix *Ix, focusedOption, focusedValue string) []discord.AutocompleteChoice
	IxComponentHandler    func(ctx context.Context, ix *Ix, customID string)
	IxModalHandler        func(ctx context.Context, ix *Ix, customID string)
)

type registeredLeaf struct {
	spec         PermSpec
	handler      IxCommandHandler
	autocomplete IxAutocompleteHandler
}

type registeredComponent struct {
	pluginName string
	spec       PermSpec
	handler    IxComponentHandler
}

type registeredModal struct {
	pluginName string
	spec       PermSpec
	handler    IxModalHandler
}

// CommandRouter is the single owner of slash-command registration and
// dispatch (spec.MD §4a). Plugins never listen for interactions or set
// commands themselves. This fixes two real gaps in the pre-Milestone-4
// code: (1) two plugins each bulk-overwriting commands independently would
// silently wipe out each other's, since bulk-overwrite replaces the entire
// scope, not just the caller's own commands; (2) a panic in any one
// plugin's handler could take down the whole process. Every dispatch here
// runs under recover().
type CommandRouter struct {
	perms *Permissions
	gate  PluginGate
	log   *slog.Logger
	// voice supplies the wording for the refusals below. Optional: a nil
	// speaker falls back to the plain sentences, so a router built in a
	// test does not have to care that merlin has a personality.
	voice voice.Source
	// rest answers interactions handled as an Ix. Optional until a plugin
	// registers one: NewIx refuses without it, and the refusal is logged.
	rest rest.Rest

	topLevel       []discord.SlashCommandCreate
	creates        []discord.ApplicationCommandCreate // topLevel as RegisterGuild sends it, from Finalize
	topLevelPlugin map[string]string                  // top-level command name -> owning plugin name
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

// WithRest gives the router the REST client every Ix answers through
// (NewRest).
func (r *CommandRouter) WithRest(c rest.Rest) *CommandRouter {
	r.rest = c
	return r
}

// Perms is the router's permission checker, for a panel drawing a control
// disabled for somebody the router would refuse.
func (r *CommandRouter) Perms() *Permissions {
	if r == nil {
		return nil
	}
	return r.perms
}

// RegisterCommand adds one top-level command, owned by pluginName (used for
// the per-guild plugin enable/disable gate, see PluginGate). cmd's Options
// may nest subcommands and subcommand groups as Discord allows; call Handle
// for every invocable leaf path underneath it before Finalize.
func (r *CommandRouter) RegisterCommand(pluginName string, cmd discord.SlashCommandCreate) {
	r.topLevel = append(r.topLevel, cmd)
	r.topLevelPlugin[cmd.Name] = pluginName
}

// Plugins returns every distinct plugin name that has registered a top-level
// command, which is what /config's plugins page lists.
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

// Tiers is each top-level command and the lowest tier any leaf under it
// asks for: who can run something there. It's what merlin tells cygnet
// about her commands; read it after Finalize.
func (r *CommandRouter) Tiers() map[string]PermTier {
	out := make(map[string]PermTier, len(r.topLevel))
	for key, leaf := range r.leaves {
		name, _, _ := strings.Cut(key, "/")
		if have, ok := out[name]; !ok || leaf.spec.Tier < have {
			out[name] = leaf.spec.Tier
		}
	}
	return out
}

// HandleIx registers the handler and required PermSpec for one leaf path.
// path is empty for a bare top-level command (e.g. "ping"), a single
// segment for a direct subcommand ("run-now"), or "group/subcommand" for a
// subcommand nested in a group ("configure/set-interval"), mirroring
// exactly the nesting depth Discord itself allows (max two levels below the
// top-level command). spec.Tier must not be the zero value, and Action must
// be set for anything above TierPublic. Both are validated at Finalize, not
// here, so registration order doesn't matter.
func (r *CommandRouter) HandleIx(topLevelName, path string, spec PermSpec, handler IxCommandHandler) {
	r.leaves[leafKey(topLevelName, path)] = &registeredLeaf{spec: spec, handler: handler}
}

// AutocompleteIx attaches fn to an already-registered leaf. Panics if the
// leaf doesn't exist yet: call HandleIx for the same path first.
func (r *CommandRouter) AutocompleteIx(topLevelName, path string, fn IxAutocompleteHandler) {
	key := leafKey(topLevelName, path)
	leaf, ok := r.leaves[key]
	if !ok {
		panic(fmt.Sprintf("core: AutocompleteIx registered before Handle for %q", key))
	}
	leaf.autocomplete = fn
}

// HandleComponentIx registers fn for every message-component interaction
// (button click, select-menu choice) whose CustomID starts with prefix.
// Callers namespace their own prefixes (e.g. "rotation:list:") to avoid
// collisions; the longest matching prefix wins if more than one matches.
// pluginName and spec are checked exactly like a slash command's (plugin
// enabled, then Authorize) before fn runs: a component interaction is its
// own fresh interaction, not an extension of whatever originally rendered
// it, so it gets no exemption from either check.
func (r *CommandRouter) HandleComponentIx(pluginName, prefix string, spec PermSpec, fn IxComponentHandler) {
	r.components[prefix] = &registeredComponent{pluginName: pluginName, spec: spec, handler: fn}
}

// HandleModalIx registers fn for every modal submission whose CustomID
// starts with prefix. Same rules as HandleComponentIx: callers namespace
// their own prefixes, the longest match wins, and pluginName and spec are
// re-checked before fn runs. A modal is opened from an earlier interaction
// that was already authorized, but the submission arrives as its own
// interaction minutes later and gets no exemption for that.
func (r *CommandRouter) HandleModalIx(pluginName, prefix string, spec PermSpec, fn IxModalHandler) {
	r.modals[prefix] = &registeredModal{pluginName: pluginName, spec: spec, handler: fn}
}

// Actions returns every distinct, non-empty PermSpec.Action currently
// registered, which is what /config's permissions page offers. Components
// and modals count as well as commands: once a plugin's settings move onto a
// panel, its configure action lives on the panel's controls, and an action
// missing from this list is one a guild can no longer retier or block.
func (r *CommandRouter) Actions() []string {
	seen := make(map[string]bool)
	var out []string
	add := func(a string) {
		if a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for _, leaf := range r.leaves {
		add(leaf.spec.Action)
	}
	for _, c := range r.components {
		add(c.spec.Action)
	}
	for _, m := range r.modals {
		add(m.spec.Action)
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
		if err := validateCommandShape(cmd); err != nil {
			return err
		}
	}
	r.creates = make([]discord.ApplicationCommandCreate, len(r.topLevel))
	for i, cmd := range r.topLevel {
		r.creates[i] = cmd
	}
	return nil
}

// Discord's limits on a command definition. Checked here because of how
// they fail in production: RegisterGuild is one BulkOverwrite of the
// guild's whole command set, so one option description a few characters
// over the limit makes Discord reject the entire request, the guild keeps
// whatever set it had before, every other command goes on working, and the
// new one simply never appears. That is the quietest possible failure, it
// is logged once per guild on a host nobody is watching, and it happened:
// /rapsheet shipped with two descriptions over 100 characters and was
// invisible in every guild until somebody asked where it was. Refusing to
// boot is the honest version.
const (
	maxCommandNameLen        = 32
	maxCommandDescriptionLen = 100
	maxCommandOptions        = 25
	maxOptionChoices         = 25
	maxChoiceNameLen         = 100
	maxChoiceStringValueLen  = 100
)

func validateCommandShape(cmd discord.SlashCommandCreate) error {
	if err := validateNameAndDescription(cmd.Name, cmd.Name, cmd.Description); err != nil {
		return err
	}
	return validateOptions(cmd.Name, cmd.Options)
}

func validateNameAndDescription(path, name, description string) error {
	if n := len(name); n == 0 || n > maxCommandNameLen {
		return fmt.Errorf("core: command %q: name %q is %d characters; Discord allows 1-%d", path, name, n, maxCommandNameLen)
	}
	if name != strings.ToLower(name) {
		return fmt.Errorf("core: command %q: name %q must be lowercase", path, name)
	}
	if n := len(description); n == 0 || n > maxCommandDescriptionLen {
		return fmt.Errorf("core: command %q: description is %d characters; Discord allows 1-%d: %q", path, n, maxCommandDescriptionLen, description)
	}
	return nil
}

func validateOptions(path string, opts []discord.ApplicationCommandOption) error {
	if len(opts) > maxCommandOptions {
		return fmt.Errorf("core: command %q has %d options; Discord allows %d", path, len(opts), maxCommandOptions)
	}
	for _, o := range opts {
		p := path + "/" + o.OptionName()
		if err := validateNameAndDescription(p, o.OptionName(), o.OptionDescription()); err != nil {
			return err
		}
		var err error
		switch o := o.(type) {
		case discord.ApplicationCommandOptionSubCommand:
			err = validateOptions(p, o.Options)
		case discord.ApplicationCommandOptionSubCommandGroup:
			subs := make([]discord.ApplicationCommandOption, len(o.Options))
			for i, s := range o.Options {
				subs[i] = s
			}
			err = validateOptions(p, subs)
		case discord.ApplicationCommandOptionString:
			err = validateChoices(p, o.Choices, func(c discord.ApplicationCommandOptionChoiceString) int { return len(c.Value) })
		case discord.ApplicationCommandOptionInt:
			err = validateChoices(p, o.Choices, func(discord.ApplicationCommandOptionChoiceInt) int { return 0 })
		case discord.ApplicationCommandOptionFloat:
			err = validateChoices(p, o.Choices, func(discord.ApplicationCommandOptionChoiceFloat) int { return 0 })
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// validateChoices holds an option's choices to Discord's limits; valueLen
// is a string choice's value length, zero for a number.
func validateChoices[C discord.ApplicationCommandOptionChoice](p string, choices []C, valueLen func(C) int) error {
	if len(choices) > maxOptionChoices {
		return fmt.Errorf("core: option %q has %d choices; Discord allows %d", p, len(choices), maxOptionChoices)
	}
	for _, c := range choices {
		if n := len(c.ChoiceName()); n == 0 || n > maxChoiceNameLen {
			return fmt.Errorf("core: option %q: choice name %q is %d characters; Discord allows 1-%d", p, c.ChoiceName(), n, maxChoiceNameLen)
		}
		if valueLen(c) > maxChoiceStringValueLen {
			return fmt.Errorf("core: option %q: choice %q has a value over %d characters", p, c.ChoiceName(), maxChoiceStringValueLen)
		}
	}
	return nil
}

// leafPaths walks cmd's Options tree and returns every invocable leaf path
// beneath it (mirroring resolveLeaf's own walk, so registration-time
// validation and dispatch-time resolution can never disagree).
func leafPaths(cmd discord.SlashCommandCreate) []string {
	var paths []string
	for _, opt := range cmd.Options {
		switch o := opt.(type) {
		case discord.ApplicationCommandOptionSubCommand:
			paths = append(paths, o.Name)
		case discord.ApplicationCommandOptionSubCommandGroup:
			for _, sub := range o.Options {
				paths = append(paths, o.Name+"/"+sub.Name)
			}
		}
	}
	if paths == nil {
		return []string{""}
	}
	return paths
}

// OnInteraction is the gateway listener for every interaction, registered
// once (core.NewGateway). Plugins never listen for interactions themselves.
func (r *CommandRouter) OnInteraction(e *events.InteractionCreate) {
	r.route(IxOf(e.Interaction, r.rest))
}

// HandleRaw routes one interaction from Discord's JSON, as the gateway
// would deliver it. One that does not decode cannot be answered, since its
// token is in what failed to decode, so it is logged and dropped.
func (r *CommandRouter) HandleRaw(raw []byte) {
	ix, err := NewIx(raw, r.rest)
	if err != nil {
		r.log.Error("interaction not decoded", "err", err)
		return
	}
	r.route(ix)
}

func (r *CommandRouter) route(ix *Ix) {
	switch i := ix.I.(type) {
	case discord.ApplicationCommandInteraction:
		r.dispatchCommand(ix, i)
	case discord.AutocompleteInteraction:
		r.dispatchAutocomplete(ix, i)
	case discord.ComponentInteraction:
		r.dispatchComponent(ix, i.Data.CustomID())
	case discord.ModalSubmitInteraction:
		r.dispatchModal(ix, i.Data.CustomID)
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
func (r *CommandRouter) dispatchComponent(ix *Ix, customID string) {
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Error("component handler panicked", "panic", rec)
			ix.Tell(r.say(voice.KeyBroken, "Something went wrong handling that."))
		}
	}()

	// Same fail-closed guard as dispatchCommand: GuildID is the settings
	// cache key both checks below depend on.
	if ix.GuildID == "" {
		ix.Tell(r.say(voice.KeyGuildOnly, "This only works inside a server."))
		return
	}

	matched := r.matchComponent(customID)
	if matched == nil {
		r.log.Error("component dispatch: no handler registered", "custom_id", customID)
		ix.Tell(r.say(voice.KeyStaleComponent, "This button or menu isn't wired up yet."))
		return
	}

	if !r.gate.PluginEnabled(ix.GuildID, matched.pluginName) {
		ix.Tell(r.say(voice.KeyPluginDisabled, "This feature is disabled in this server."))
		return
	}
	if err := r.perms.AuthorizeIx(ix, matched.spec); err != nil {
		ix.Tell(r.say(voice.KeyDenied, "You are not allowed to do that."))
		return
	}

	ctx, cancel := handlerCtx(ix, 30*time.Second)
	defer cancel()
	timed(matched.pluginName, ix, func() { matched.handler(ctx, ix, customID) })
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
func (r *CommandRouter) dispatchModal(ix *Ix, customID string) {
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Error("modal handler panicked", "panic", rec)
			ix.Tell(r.say(voice.KeyBroken, "Something went wrong handling that."))
		}
	}()

	if ix.GuildID == "" {
		ix.Tell(r.say(voice.KeyGuildOnly, "This only works inside a server."))
		return
	}

	matched := r.matchModal(customID)
	if matched == nil {
		r.log.Error("modal dispatch: no handler registered", "custom_id", customID)
		ix.Tell(r.say(voice.KeyStaleComponent, "This form isn't wired up yet."))
		return
	}

	if !r.gate.PluginEnabled(ix.GuildID, matched.pluginName) {
		ix.Tell(r.say(voice.KeyPluginDisabled, "This feature is disabled in this server."))
		return
	}
	if err := r.perms.AuthorizeIx(ix, matched.spec); err != nil {
		ix.Tell(r.say(voice.KeyDenied, "You are not allowed to do that."))
		return
	}

	ctx, cancel := handlerCtx(ix, 30*time.Second)
	defer cancel()
	timed(matched.pluginName, ix, func() { matched.handler(ctx, ix, customID) })
}

// leafOf is the registered leaf key a command path names: CommandPath is
// "/name/group/sub", the key "name/group/sub".
func leafOf(commandPath string) string { return strings.TrimPrefix(commandPath, "/") }

func (r *CommandRouter) dispatchCommand(ix *Ix, i discord.ApplicationCommandInteraction) {
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Error("command handler panicked", "panic", rec)
			ix.Tell(r.say(voice.KeyBroken, "Something went wrong running that command."))
		}
	}()

	// Every command is registered per guild, so a DM interaction shouldn't be
	// reachable, but GuildID feeds straight into the settings cache key for
	// both the plugin gate and Authorize, and an empty key there resolves to
	// a guild with no mod roles and no admins. Refusing outright is a cheap
	// guard on the one path that must never fail open, and it costs nothing
	// on the path that actually happens.
	if ix.GuildID == "" {
		ix.Tell(r.say(voice.KeyGuildOnly, "This command only works inside a server."))
		return
	}

	data, ok := i.Data.(discord.SlashCommandInteractionData)
	if !ok {
		ix.Tell(r.say(voice.KeyStaleComponent, "This command isn't wired up yet."))
		return
	}
	key := leafOf(data.CommandPath())
	leaf, ok := r.leaves[key]
	if !ok {
		r.log.Error("command dispatch: no handler registered", "key", key)
		ix.Tell(r.say(voice.KeyStaleComponent, "This command isn't wired up yet."))
		return
	}
	plugin := r.topLevelPlugin[data.CommandName()]

	if !r.gate.PluginEnabled(ix.GuildID, plugin) {
		ix.Tell(r.say(voice.KeyPluginDisabled, "This feature is disabled in this server."))
		return
	}

	if err := r.perms.AuthorizeIx(ix, leaf.spec); err != nil {
		ix.Tell(r.say(voice.KeyDenied, "You are not allowed to run this command."))
		return
	}

	ctx, cancel := handlerCtx(ix, 30*time.Second)
	defer cancel()
	timed(plugin, ix, func() { leaf.handler(ctx, ix) })
}

// timed records an interaction's two legs against plugin: in, from Discord
// creating it to merlin dispatching it (the gateway, decoding, scheduling),
// and run, the handler itself, in a trace region named for the plugin.
func timed(plugin string, ix *Ix, fn func()) {
	if d, ok := obs.Since(ix.I.ID().String()); ok {
		obs.Default.Add(plugin, obs.In, d)
	}
	start := time.Now()
	obs.Region(plugin, fn)
	obs.Default.Add(plugin, obs.Run, time.Since(start))
}

func (r *CommandRouter) dispatchAutocomplete(ix *Ix, i discord.AutocompleteInteraction) {
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Error("autocomplete handler panicked", "panic", rec)
		}
	}()

	key := leafOf(i.Data.CommandPath())
	leaf, ok := r.leaves[key]
	if !ok || leaf.autocomplete == nil {
		return
	}

	if !r.gate.PluginEnabled(ix.GuildID, r.topLevelPlugin[i.Data.CommandName]) {
		return
	}
	if err := r.perms.AuthorizeIx(ix, leaf.spec); err != nil {
		return
	}

	focused := i.Data.Focused()
	var focusedValue string
	_ = json.Unmarshal(focused.Value, &focusedValue)

	ctx, cancel := handlerCtx(ix, 5*time.Second)
	defer cancel()
	choices := leaf.autocomplete(ctx, ix, focused.Name, focusedValue)
	if len(choices) > 25 {
		choices = choices[:25]
	}
	_ = ix.respond(discord.InteractionResponseTypeAutocompleteResult, discord.AutocompleteResult{Choices: choices})
}
