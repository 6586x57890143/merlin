package core

import (
	"testing"

	"github.com/disgoorg/disgo/discord"
)

func TestOverwritesRoundTripAndCompare(t *testing.T) {
	a := []discord.PermissionOverwrite{
		discord.RolePermissionOverwrite{RoleID: 2, Allow: discord.PermissionViewChannel},
		discord.MemberPermissionOverwrite{UserID: 1, Deny: discord.PermissionSendMessages},
	}
	b := Disgo(Overwrites(a))
	if !SameOverwrites(a, []discord.PermissionOverwrite{b[1], b[0]}) {
		t.Fatalf("a round trip or a reorder changed the list: %v vs %v", a, b)
	}
	if SameOverwrites(a, a[:1]) {
		t.Fatal("a shorter list compared equal")
	}
	if got := SortOverwrites(Overwrites(a)); got[0].Type != discord.PermissionOverwriteTypeRole {
		t.Fatalf("roles sort first: %v", got)
	}
}
