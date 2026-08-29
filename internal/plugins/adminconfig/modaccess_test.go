package adminconfig

import (
	"slices"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// Granting a mod role view access is a permission write, and every permission
// write is one entry in the guild's own Discord audit log. grantModRoles-
// ChannelAccess re-runs the whole mod-role list whenever either channel is
// reconfigured, so the roles that already have the grant must be filtered out
// before anything is written.
func TestRolesMissingViewAccess(t *testing.T) {
	view := int64(discordgo.PermissionViewChannel)
	ch := &discordgo.Channel{ID: "c1", PermissionOverwrites: []*discordgo.PermissionOverwrite{
		{ID: "mod-a", Type: discordgo.PermissionOverwriteTypeRole, Allow: view},
		// Allowed something else, but not view: still needs the grant.
		{ID: "mod-b", Type: discordgo.PermissionOverwriteTypeRole, Allow: int64(discordgo.PermissionSendMessages)},
		// A member overwrite with the same ID as a role must not count.
		{ID: "mod-c", Type: discordgo.PermissionOverwriteTypeMember, Allow: view},
	}}

	got := rolesMissingViewAccess(ch, []string{"mod-a", "mod-b", "mod-c", "mod-d"})
	want := []string{"mod-b", "mod-c", "mod-d"}
	if !slices.Equal(got, want) {
		t.Fatalf("rolesMissingViewAccess = %v, want %v", got, want)
	}

	if got := rolesMissingViewAccess(ch, []string{"mod-a"}); len(got) != 0 {
		t.Fatalf("expected an already-granted role to need no write, got %v", got)
	}
}

// An unreadable channel has to fall back to granting: mods seeing their own
// moderation trail matters more than a quiet audit log.
func TestRolesMissingViewAccessFailsTowardsGranting(t *testing.T) {
	roles := []string{"mod-a", "mod-b"}
	if got := rolesMissingViewAccess(nil, roles); !slices.Equal(got, roles) {
		t.Fatalf("rolesMissingViewAccess(nil) = %v, want every role %v", got, roles)
	}
}
