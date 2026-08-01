package core

import (
	"context"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func newTestRouter() (*CommandRouter, *fakeAuthData) {
	auth := newFakeAuthData()
	perms := NewPermissions(nil, auth, "")
	return NewCommandRouter(perms, testLogger()), auth
}

func TestFinalizeRejectsUnsetTier(t *testing.T) {
	r, _ := newTestRouter()
	r.RegisterCommand(&discordgo.ApplicationCommand{Name: "foo"})
	r.Handle("foo", "", PermSpec{}, func(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {})

	if err := r.Finalize(); err == nil {
		t.Fatal("expected Finalize to reject a leaf with an unset PermSpec.Tier")
	}
}

func TestFinalizeRejectsMissingActionAboveTierPublic(t *testing.T) {
	r, _ := newTestRouter()
	r.RegisterCommand(&discordgo.ApplicationCommand{Name: "foo"})
	r.Handle("foo", "", PermSpec{Tier: TierMod}, func(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {})

	if err := r.Finalize(); err == nil {
		t.Fatal("expected Finalize to reject a TierMod leaf with no Action")
	}
}

func TestFinalizeRejectsSubcommandWithNoHandler(t *testing.T) {
	r, _ := newTestRouter()
	r.RegisterCommand(&discordgo.ApplicationCommand{
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
	r.RegisterCommand(&discordgo.ApplicationCommand{
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

func TestActionsDedupesAndSkipsEmpty(t *testing.T) {
	r, _ := newTestRouter()
	r.RegisterCommand(&discordgo.ApplicationCommand{Name: "a"})
	r.RegisterCommand(&discordgo.ApplicationCommand{Name: "b"})
	r.RegisterCommand(&discordgo.ApplicationCommand{Name: "c"})
	noop := func(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {}
	r.Handle("a", "", PermSpec{Tier: TierPublic}, noop)
	r.Handle("b", "", PermSpec{Tier: TierMod, Action: "shared.action"}, noop)
	r.Handle("c", "", PermSpec{Tier: TierAdmin, Action: "shared.action"}, noop)

	actions := r.Actions()
	if len(actions) != 1 || actions[0] != "shared.action" {
		t.Fatalf("expected exactly one deduped action, got %+v", actions)
	}
}
