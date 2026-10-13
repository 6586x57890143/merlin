package core

import (
	"context"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// A command's tier is the lowest any leaf under it asks for.
func TestTiers(t *testing.T) {
	r := NewCommandRouter(nil, nil, nil)
	r.RegisterCommand("roles", &discordgo.ApplicationCommand{Name: "roles"})
	r.RegisterCommand("ping", &discordgo.ApplicationCommand{Name: "ping"})
	noop := func(context.Context, *Ix) {}
	r.HandleIx("roles", "jail", PermSpec{Tier: TierMod, Action: "roles.jail"}, noop)
	r.HandleIx("roles", "configure/sync-channels", PermSpec{Tier: TierAdmin, Action: "roles.configure"}, noop)
	r.HandleIx("ping", "", PermSpec{Tier: TierPublic}, noop)
	got := r.Tiers()
	if len(got) != 2 || got["roles"] != TierMod || got["ping"] != TierPublic {
		t.Fatalf("tiers %v", got)
	}
}
