package core_test

import (
	"strconv"
	"testing"

	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/paneltest"
)

// policies is GuildAuthData with every list a test sets.
type policies struct {
	mods, admins []string
	policy       core.ActionPolicy
}

func (p policies) ModRoleIDs(string) []string                    { return p.mods }
func (p policies) AdminUserIDs(string) []string                  { return p.admins }
func (p policies) ActionPolicy(string, string) core.ActionPolicy { return p.policy }

// Authorize reads the member off a real interaction and decides by the
// rules Authorize documents: deny, then tier, then allow.
func TestAuthorizeDecides(t *testing.T) {
	const op, user, modRole, other = "900", "11", "21", "22"
	admin := int64(discord.PermissionAdministrator)
	for _, c := range []struct {
		name  string
		user  string
		roles []string
		perms int64
		set   policies
		spec  core.PermSpec
		allow bool
	}{
		{"public", user, nil, 0, policies{}, core.PermSpec{Tier: core.TierPublic}, true},
		{"admin tier, nobody", user, nil, 0, policies{}, core.PermSpec{Tier: core.TierAdmin, Action: "a"}, false},
		{"admin tier, Administrator bit", user, nil, admin, policies{}, core.PermSpec{Tier: core.TierAdmin, Action: "a"}, true},
		{"admin tier, listed admin", user, nil, 0, policies{admins: []string{user}}, core.PermSpec{Tier: core.TierAdmin, Action: "a"}, true},
		{"admin tier, bootstrap", op, nil, 0, policies{}, core.PermSpec{Tier: core.TierAdmin, Action: "a"}, true},
		{"mod tier, mod role", user, []string{modRole}, 0, policies{mods: []string{modRole}}, core.PermSpec{Tier: core.TierMod, Action: "a"}, true},
		{"mod tier, other role", user, []string{other}, 0, policies{mods: []string{modRole}}, core.PermSpec{Tier: core.TierMod, Action: "a"}, false},
		{"mod tier, Administrator counts as admin", user, nil, admin, policies{}, core.PermSpec{Tier: core.TierMod, Action: "a"}, true},
		{"denied user beats Administrator", user, nil, admin, policies{policy: core.ActionPolicy{DenyUserIDs: []string{user}}}, core.PermSpec{Tier: core.TierAdmin, Action: "a"}, false},
		{"denied role", user, []string{other}, admin, policies{policy: core.ActionPolicy{DenyRoleIDs: []string{other}}}, core.PermSpec{Tier: core.TierAdmin, Action: "a"}, false},
		{"bootstrap cannot be denied", op, nil, 0, policies{policy: core.ActionPolicy{DenyUserIDs: []string{op}}}, core.PermSpec{Tier: core.TierAdmin, Action: "a"}, true},
		{"override raises public", user, nil, 0, policies{policy: core.ActionPolicy{RequiredTier: core.TierAdmin}}, core.PermSpec{Tier: core.TierPublic, Action: "a"}, false},
		{"stored public override ignored", user, nil, 0, policies{policy: core.ActionPolicy{RequiredTier: core.TierPublic}}, core.PermSpec{Tier: core.TierAdmin, Action: "a"}, false},
		{"allow-listed role", user, []string{other}, 0, policies{policy: core.ActionPolicy{AllowRoleIDs: []string{other}}}, core.PermSpec{Tier: core.TierAdmin, Action: "a"}, true},
		{"allow-listed user", user, nil, 0, policies{policy: core.ActionPolicy{AllowUserIDs: []string{user}}}, core.PermSpec{Tier: core.TierMod, Action: "a"}, true},
	} {
		perms := core.NewPermissions(nil, c.set, op)
		roles := []any{}
		for _, r := range c.roles {
			roles = append(roles, r)
		}
		edit := func(p map[string]any) {
			m := p["member"].(map[string]any)
			m["user"].(map[string]any)["id"] = c.user
			m["roles"] = roles
			m["permissions"] = strconv.FormatInt(c.perms, 10)
		}
		ix, _ := paneltest.IxCommand(t, c.user, "x", edit)
		if got := perms.Authorize(ix, c.spec) == nil; got != c.allow {
			t.Errorf("%s: allows=%v, want %v", c.name, got, c.allow)
		}
	}
}

// A member with no user is refused, rather than authorized as
// nobody in particular.
func TestAuthorizeRefusesAMemberWithNoUser(t *testing.T) {
	perms := core.NewPermissions(nil, policies{}, "900")
	ix, _ := paneltest.IxCommand(t, "11", "x", func(p map[string]any) {
		delete(p["member"].(map[string]any), "user")
	})
	if perms.Authorize(ix, core.PermSpec{Tier: core.TierPublic}) == nil {
		t.Fatal("a member with no user identity was authorized")
	}
}
