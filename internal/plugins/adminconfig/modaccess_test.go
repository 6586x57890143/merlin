package adminconfig

import (
	"slices"
	"testing"
)

// Granting a mod role view access is a permission write, and every permission
// write is one entry in the guild's own Discord audit log. grantModRoles-
// ChannelAccess re-runs the whole mod-role list whenever either channel is
// reconfigured, so the roles that already have the grant must be filtered out
// before anything is written.
func TestRolesMissingViewAccess(t *testing.T) {
	ch := textChannel(t, `[
		{"id":"201","type":0,"allow":"1024","deny":"0"},
		{"id":"202","type":0,"allow":"2048","deny":"0"},
		{"id":"203","type":1,"allow":"1024","deny":"0"}]`)
	// 202 is allowed something else but not view; 203 is a member overwrite
	// sharing a role's ID, which must not count.
	got := rolesMissingViewAccess(ch, []string{"201", "202", "203", "204"})
	want := []string{"202", "203", "204"}
	if !slices.Equal(got, want) {
		t.Fatalf("rolesMissingViewAccess = %v, want %v", got, want)
	}

	if got := rolesMissingViewAccess(ch, []string{"201"}); len(got) != 0 {
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
