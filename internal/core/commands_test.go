package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
)

// fakeGate is a PluginGate that enables everything by default; tests of the
// gate itself flip entries in disabled to exercise the deny path.
type fakeGate struct {
	disabled map[string]bool
}

func newFakeGate() *fakeGate { return &fakeGate{disabled: make(map[string]bool)} }

func (f *fakeGate) PluginEnabled(guildID, pluginName string) bool { return !f.disabled[pluginName] }

func newTestRouter() (*CommandRouter, *fakeAuthData) {
	auth := newFakeAuthData()
	perms := NewPermissions(nil, auth, "")
	return NewCommandRouter(perms, newFakeGate(), testLogger()), auth
}

func TestFinalizeRejectsUnsetTier(t *testing.T) {
	r, _ := newTestRouter()
	r.RegisterCommand("testplugin", &discordgo.ApplicationCommand{Name: "foo"})
	r.HandleIx("foo", "", PermSpec{}, func(ctx context.Context, ix *Ix) {})

	if err := r.Finalize(); err == nil {
		t.Fatal("expected Finalize to reject a leaf with an unset PermSpec.Tier")
	}
}

func TestFinalizeRejectsMissingActionAboveTierPublic(t *testing.T) {
	r, _ := newTestRouter()
	r.RegisterCommand("testplugin", &discordgo.ApplicationCommand{Name: "foo"})
	r.HandleIx("foo", "", PermSpec{Tier: TierMod}, func(ctx context.Context, ix *Ix) {})

	if err := r.Finalize(); err == nil {
		t.Fatal("expected Finalize to reject a TierMod leaf with no Action")
	}
}

func TestFinalizeRejectsSubcommandWithNoHandler(t *testing.T) {
	r, _ := newTestRouter()
	r.RegisterCommand("testplugin", &discordgo.ApplicationCommand{
		Name: "foo",
		Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "bar"},
		},
	})
	// No Handle call for "foo/bar" at all.

	if err := r.Finalize(); err == nil {
		t.Fatal("expected Finalize to reject a declared subcommand with no registered handler")
	}
}

func TestFinalizePassesForFullyWiredTree(t *testing.T) {
	r, _ := newTestRouter()
	r.RegisterCommand("testplugin", &discordgo.ApplicationCommand{
		Name: "foo", Description: "a test command",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type: discordgo.ApplicationCommandOptionSubCommandGroup, Name: "grp", Description: "a group",
				Options: []*discordgo.ApplicationCommandOption{
					{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "leaf", Description: "a leaf"},
				},
			},
		},
	})
	noop := func(ctx context.Context, ix *Ix) {}
	r.HandleIx("foo", "grp/leaf", PermSpec{Tier: TierPublic}, noop)

	if err := r.Finalize(); err != nil {
		t.Fatalf("expected a fully-wired tree to pass Finalize, got %v", err)
	}
}

// decode is Discord's JSON for an interaction, decoded as the router does.
func decode(t *testing.T, p map[string]any) *Ix {
	t.Helper()
	p["id"], p["application_id"], p["token"], p["version"], p["guild_id"] = "1000", "2", "t", 1, "100"
	p["member"] = map[string]any{"user": map[string]any{"id": "11", "username": "m"}, "permissions": "0", "roles": []any{}, "joined_at": "2026-01-01T00:00:00Z"}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := NewIx(raw, rest.New(rest.NewClient("test")))
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

// The leaf a command routes to, and the arguments a handler then reads,
// for each nesting depth Discord allows.
func TestCommandsRouteToTheirLeaf(t *testing.T) {
	for _, c := range []struct {
		options []any
		leaf    string
		arg     string
	}{
		{nil, "ping", ""},
		{[]any{map[string]any{"name": "run-now", "type": 1, "options": []any{
			map[string]any{"name": "job", "type": 3, "value": "rotation:123"},
		}}}, "ping/run-now", "job"},
		{[]any{map[string]any{"name": "configure", "type": 2, "options": []any{
			map[string]any{"name": "add", "type": 1, "options": []any{
				map[string]any{"name": "channel", "type": 3, "value": "999"},
			}},
		}}}, "ping/configure/add", "channel"},
	} {
		data := map[string]any{"id": "6", "name": "ping", "type": 1}
		if c.options != nil {
			data["options"] = c.options
		}
		ix := decode(t, map[string]any{"type": 2, "data": data})
		cmd := ix.I.(discord.ApplicationCommandInteraction).Data.(discord.SlashCommandInteractionData)
		if got := leafOf(cmd.CommandPath()); got != c.leaf {
			t.Errorf("leaf %q, want %q", got, c.leaf)
		}
		if _, ok := ix.Args()[c.arg]; c.arg != "" && !ok {
			t.Errorf("%s: argument %q did not surface: %v", c.leaf, c.arg, ix.Args())
		}
	}
}

func TestPluginsDedupes(t *testing.T) {
	r, _ := newTestRouter()
	r.RegisterCommand("rotation", &discordgo.ApplicationCommand{Name: "rotation"})
	r.RegisterCommand("rotation", &discordgo.ApplicationCommand{Name: "rotation-other"})
	r.RegisterCommand("scheduler", &discordgo.ApplicationCommand{Name: "scheduler"})

	plugins := r.Plugins()
	if len(plugins) != 2 {
		t.Fatalf("expected 2 deduped plugin names, got %+v", plugins)
	}
}

func TestFinalizeRejectsComponentWithUnsetTier(t *testing.T) {
	r, _ := newTestRouter()
	r.HandleComponentIx("testplugin", "rotation:list:", PermSpec{}, func(ctx context.Context, ix *Ix, customID string) {})

	if err := r.Finalize(); err == nil {
		t.Fatal("expected Finalize to reject a component with an unset PermSpec.Tier")
	}
}

func TestFinalizeRejectsComponentMissingActionAboveTierPublic(t *testing.T) {
	r, _ := newTestRouter()
	r.HandleComponentIx("testplugin", "rotation:list:", PermSpec{Tier: TierMod}, func(ctx context.Context, ix *Ix, customID string) {})

	if err := r.Finalize(); err == nil {
		t.Fatal("expected Finalize to reject a TierMod component with no Action")
	}
}

func TestMatchComponentLongestPrefixWins(t *testing.T) {
	r, _ := newTestRouter()
	generic := func(ctx context.Context, ix *Ix, customID string) {}
	// Two prefixes both match "rotation:list:page:2" ("rotation:" and
	// "rotation:list:"), tagged with different plugin names purely so the
	// test can tell which one matchComponent actually picked.
	r.HandleComponentIx("shorter-prefix-plugin", "rotation:", PermSpec{Tier: TierPublic}, generic)
	r.HandleComponentIx("longer-prefix-plugin", "rotation:list:", PermSpec{Tier: TierPublic}, generic)

	matched := r.matchComponent("rotation:list:page:2")
	if matched == nil {
		t.Fatal("expected a match")
	}
	if matched.pluginName != "longer-prefix-plugin" {
		t.Fatalf("expected the longer, more specific prefix to win, got plugin %q", matched.pluginName)
	}
}

func TestMatchComponentReturnsNilWhenNoPrefixMatches(t *testing.T) {
	r, _ := newTestRouter()
	r.HandleComponentIx("rotation", "rotation:list:", PermSpec{Tier: TierPublic}, func(ctx context.Context, ix *Ix, customID string) {})

	if matched := r.matchComponent("scheduler:list:page:1"); matched != nil {
		t.Fatalf("expected no match for an unrelated CustomID, got %+v", matched)
	}
}

func TestActionsDedupesAndSkipsEmpty(t *testing.T) {
	r, _ := newTestRouter()
	r.RegisterCommand("testplugin", &discordgo.ApplicationCommand{Name: "a"})
	r.RegisterCommand("testplugin", &discordgo.ApplicationCommand{Name: "b"})
	r.RegisterCommand("testplugin", &discordgo.ApplicationCommand{Name: "c"})
	noop := func(ctx context.Context, ix *Ix) {}
	r.HandleIx("a", "", PermSpec{Tier: TierPublic}, noop)
	r.HandleIx("b", "", PermSpec{Tier: TierMod, Action: "shared.action"}, noop)
	r.HandleIx("c", "", PermSpec{Tier: TierAdmin, Action: "shared.action"}, noop)

	actions := r.Actions()
	if len(actions) != 1 || actions[0] != "shared.action" {
		t.Fatalf("expected exactly one deduped action, got %+v", actions)
	}
}

// Modals route exactly like components, so the matching rule has to be the
// same one: longest prefix wins, and an unregistered CustomID matches
// nothing rather than falling into whichever handler happened to be first.
func TestMatchModalLongestPrefixWins(t *testing.T) {
	r, _ := newTestRouter()
	generic := func(ctx context.Context, ix *Ix, customID string) {}
	r.HandleModalIx("shorter-prefix-plugin", "contest:", PermSpec{Tier: TierPublic}, generic)
	r.HandleModalIx("longer-prefix-plugin", "contest:prize:", PermSpec{Tier: TierPublic}, generic)

	matched := r.matchModal("contest:prize:abc123")
	if matched == nil {
		t.Fatal("expected a match")
	}
	if matched.pluginName != "longer-prefix-plugin" {
		t.Fatalf("expected the longer, more specific prefix to win, got plugin %q", matched.pluginName)
	}
	if matched := r.matchModal("roles:something:1"); matched != nil {
		t.Fatalf("expected no match for an unrelated CustomID, got %+v", matched)
	}
}

// A modal submission is its own fresh interaction that arrives minutes after
// whatever opened it, so a forgotten tier here would be exactly as dangerous
// as one on a command. Finalize has to refuse it the same way.
func TestFinalizeRejectsAModalWithNoTier(t *testing.T) {
	r, _ := newTestRouter()
	r.HandleModalIx("testplugin", "contest:prize:", PermSpec{}, func(ctx context.Context, ix *Ix, customID string) {})

	if err := r.Finalize(); err == nil {
		t.Fatal("expected Finalize to reject a modal with no declared tier")
	}
}

// ModalValues exists so the two-level walk into Discord's nested ActionsRows
// has one implementation. An absent optional field reads as empty rather
// than missing, which is what every caller wants anyway.
func TestModalValuesReadsEveryFieldAndToleratesJunk(t *testing.T) {
	ix := decode(t, map[string]any{"type": 5, "data": map[string]any{"custom_id": "contest:prize:c1", "components": []any{
		map[string]any{"type": 1, "components": []any{map[string]any{"type": 4, "custom_id": "title", "value": "a steam key"}}},
		map[string]any{"type": 1, "components": []any{map[string]any{"type": 4, "custom_id": "code", "value": ""}}},
		// The layout ui.Field draws: a Label around the input.
		map[string]any{"type": 18, "component": map[string]any{"type": 4, "custom_id": "details", "value": "boxed"}},
		// A text display carries no value and is not read as a field.
		map[string]any{"type": 10, "content": "not a field"},
	}}})

	got := ix.ModalValues()
	if got["title"] != "a steam key" || got["details"] != "boxed" {
		t.Errorf("values %v", got)
	}
	if v, ok := got["code"]; !ok || v != "" {
		t.Errorf("an empty optional field should read as empty, got %q %v", v, ok)
	}
	if len(got) != 3 {
		t.Errorf("read %d fields, want 3: %v", len(got), got)
	}
}

// Discord rejects a whole BulkOverwrite over one bad definition, and the
// guild silently keeps its old command set. Finalize refuses those at
// startup instead. Each case is one thing Discord would 400 on.
func TestFinalizeRefusesWhatDiscordWouldReject(t *testing.T) {
	long := strings.Repeat("x", 101)
	cases := map[string]*discordgo.ApplicationCommand{
		"empty description": {Name: "foo", Description: ""},
		"long description":  {Name: "foo", Description: long},
		"uppercase name":    {Name: "Foo", Description: "d"},
		"long option desc":  {Name: "foo", Description: "d", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "leaf", Description: long}}},
		"too many choices":  {Name: "foo", Description: "d", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "leaf", Description: "d", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionString, Name: "pick", Description: "d", Choices: make26()}}}}},
		"long choice name":  {Name: "foo", Description: "d", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "leaf", Description: "d", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionString, Name: "pick", Description: "d", Choices: []*discordgo.ApplicationCommandOptionChoice{{Name: long, Value: "v"}}}}}}},
		"too many options":  {Name: "foo", Description: "d", Options: make26Subcommands()},
	}
	for name, cmd := range cases {
		r, _ := newTestRouter()
		r.RegisterCommand("testplugin", cmd)
		noop := func(ctx context.Context, ix *Ix) {}
		for _, path := range leafPaths(cmd) {
			r.HandleIx("foo", path, PermSpec{Tier: TierPublic}, noop)
		}
		if err := r.Finalize(); err == nil {
			t.Errorf("%s: Finalize accepted a command Discord would reject", name)
		}
	}
}

func make26() []*discordgo.ApplicationCommandOptionChoice {
	out := make([]*discordgo.ApplicationCommandOptionChoice, 26)
	for i := range out {
		out[i] = &discordgo.ApplicationCommandOptionChoice{Name: fmt.Sprintf("c%d", i), Value: fmt.Sprintf("v%d", i)}
	}
	return out
}

func make26Subcommands() []*discordgo.ApplicationCommandOption {
	out := make([]*discordgo.ApplicationCommandOption, 26)
	for i := range out {
		out[i] = &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionSubCommand, Name: fmt.Sprintf("s%d", i), Description: "d"}
	}
	return out
}
