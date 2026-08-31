package core

import (
	"context"
	"testing"

	"github.com/bwmarrin/discordgo"
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
	r.Handle("foo", "", PermSpec{}, func(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {})

	if err := r.Finalize(); err == nil {
		t.Fatal("expected Finalize to reject a leaf with an unset PermSpec.Tier")
	}
}

func TestFinalizeRejectsMissingActionAboveTierPublic(t *testing.T) {
	r, _ := newTestRouter()
	r.RegisterCommand("testplugin", &discordgo.ApplicationCommand{Name: "foo"})
	r.Handle("foo", "", PermSpec{Tier: TierMod}, func(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {})

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
		Name: "foo",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type: discordgo.ApplicationCommandOptionSubCommandGroup, Name: "grp",
				Options: []*discordgo.ApplicationCommandOption{
					{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "leaf"},
				},
			},
		},
	})
	noop := func(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {}
	r.Handle("foo", "grp/leaf", PermSpec{Tier: TierPublic}, noop)

	if err := r.Finalize(); err != nil {
		t.Fatalf("expected a fully-wired tree to pass Finalize, got %v", err)
	}
}

func TestResolveLeafFlatCommand(t *testing.T) {
	data := discordgo.ApplicationCommandInteractionData{
		Name: "ping",
	}
	path, _ := resolveLeaf(data)
	if path != "" {
		t.Fatalf("expected empty path for a flat command, got %q", path)
	}
}

func TestResolveLeafDirectSubcommand(t *testing.T) {
	data := discordgo.ApplicationCommandInteractionData{
		Options: []*discordgo.ApplicationCommandInteractionDataOption{
			{Name: "run-now", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandInteractionDataOption{
				{Name: "job", Type: discordgo.ApplicationCommandOptionString, Value: "rotation:123"},
			}},
		},
	}
	path, args := resolveLeaf(data)
	if path != "run-now" {
		t.Fatalf("expected path %q, got %q", "run-now", path)
	}
	if len(args) != 1 || args[0].Name != "job" {
		t.Fatalf("expected the job argument to surface, got %+v", args)
	}
}

func TestResolveLeafNestedGroup(t *testing.T) {
	data := discordgo.ApplicationCommandInteractionData{
		Options: []*discordgo.ApplicationCommandInteractionDataOption{
			{Name: "configure", Type: discordgo.ApplicationCommandOptionSubCommandGroup, Options: []*discordgo.ApplicationCommandInteractionDataOption{
				{Name: "add", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandInteractionDataOption{
					{Name: "channel", Type: discordgo.ApplicationCommandOptionChannel, Value: "999"},
				}},
			}},
		},
	}
	path, args := resolveLeaf(data)
	if path != "configure/add" {
		t.Fatalf("expected path %q, got %q", "configure/add", path)
	}
	if len(args) != 1 || args[0].Name != "channel" {
		t.Fatalf("expected the channel argument to surface, got %+v", args)
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
	r.HandleComponent("testplugin", "rotation:list:", PermSpec{}, func(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {})

	if err := r.Finalize(); err == nil {
		t.Fatal("expected Finalize to reject a component with an unset PermSpec.Tier")
	}
}

func TestFinalizeRejectsComponentMissingActionAboveTierPublic(t *testing.T) {
	r, _ := newTestRouter()
	r.HandleComponent("testplugin", "rotation:list:", PermSpec{Tier: TierMod}, func(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {})

	if err := r.Finalize(); err == nil {
		t.Fatal("expected Finalize to reject a TierMod component with no Action")
	}
}

func TestMatchComponentLongestPrefixWins(t *testing.T) {
	r, _ := newTestRouter()
	generic := func(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {}
	// Two prefixes both match "rotation:list:page:2" ("rotation:" and
	// "rotation:list:"), tagged with different plugin names purely so the
	// test can tell which one matchComponent actually picked.
	r.HandleComponent("shorter-prefix-plugin", "rotation:", PermSpec{Tier: TierPublic}, generic)
	r.HandleComponent("longer-prefix-plugin", "rotation:list:", PermSpec{Tier: TierPublic}, generic)

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
	r.HandleComponent("rotation", "rotation:list:", PermSpec{Tier: TierPublic}, func(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {})

	if matched := r.matchComponent("scheduler:list:page:1"); matched != nil {
		t.Fatalf("expected no match for an unrelated CustomID, got %+v", matched)
	}
}

func TestActionsDedupesAndSkipsEmpty(t *testing.T) {
	r, _ := newTestRouter()
	r.RegisterCommand("testplugin", &discordgo.ApplicationCommand{Name: "a"})
	r.RegisterCommand("testplugin", &discordgo.ApplicationCommand{Name: "b"})
	r.RegisterCommand("testplugin", &discordgo.ApplicationCommand{Name: "c"})
	noop := func(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {}
	r.Handle("a", "", PermSpec{Tier: TierPublic}, noop)
	r.Handle("b", "", PermSpec{Tier: TierMod, Action: "shared.action"}, noop)
	r.Handle("c", "", PermSpec{Tier: TierAdmin, Action: "shared.action"}, noop)

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
	generic := func(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {}
	r.HandleModal("shorter-prefix-plugin", "contest:", PermSpec{Tier: TierPublic}, generic)
	r.HandleModal("longer-prefix-plugin", "contest:prize:", PermSpec{Tier: TierPublic}, generic)

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
	r.HandleModal("testplugin", "contest:prize:", PermSpec{}, func(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {})

	if err := r.Finalize(); err == nil {
		t.Fatal("expected Finalize to reject a modal with no declared tier")
	}
}

// ModalValues exists so the two-level walk into Discord's nested ActionsRows
// has one implementation. An absent optional field reads as empty rather
// than missing, which is what every caller wants anyway.
func TestModalValuesReadsEveryFieldAndToleratesJunk(t *testing.T) {
	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		Type: discordgo.InteractionModalSubmit,
		Data: discordgo.ModalSubmitInteractionData{
			CustomID: "contest:prize:c1",
			Components: []discordgo.MessageComponent{
				&discordgo.ActionsRow{Components: []discordgo.MessageComponent{
					&discordgo.TextInput{CustomID: "title", Value: "a steam key"},
				}},
				&discordgo.ActionsRow{Components: []discordgo.MessageComponent{
					&discordgo.TextInput{CustomID: "code", Value: ""},
				}},
				// Anything that is not an ActionsRow of TextInputs is skipped
				// rather than panicked on: Discord may add component types
				// this build has never heard of.
				&discordgo.Button{CustomID: "not-a-field"},
			},
		},
	}}

	got := ModalValues(i)
	if got["title"] != "a steam key" {
		t.Errorf("title = %q", got["title"])
	}
	if v, ok := got["code"]; !ok || v != "" {
		t.Errorf("an empty optional field should read as empty, got %q %v", v, ok)
	}
	if _, ok := got["not-a-field"]; ok {
		t.Error("a non-text component was read as a field")
	}
}
