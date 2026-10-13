package core_test

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/bwmarrin/discordgo"

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

// AuthorizeIx must decide exactly as Authorize does: a plugin moved to
// disgo is checked by the same rules, for the same member, every time.
func TestAuthorizeIxDecidesAsAuthorizeDoes(t *testing.T) {
	const op, user, modRole, other = "900", "11", "21", "22"
	admin := int64(discordgo.PermissionAdministrator)
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
		raw, err := json.Marshal(ixPayload(t, c.user, edit))
		if err != nil {
			t.Fatal(err)
		}
		var ic discordgo.InteractionCreate
		if err := json.Unmarshal(raw, &ic); err != nil {
			t.Fatal(err)
		}
		old, now := perms.Authorize(&ic, c.spec) == nil, perms.AuthorizeIx(ix, c.spec) == nil
		if old != now || now != c.allow {
			t.Errorf("%s: discordgo allows=%v, disgo allows=%v, want %v", c.name, old, now, c.allow)
		}
	}
}

// ixPayload is the JSON paneltest.IxCommand decodes, for discordgo to
// decode the same bytes.
func ixPayload(t *testing.T, user string, edit func(map[string]any)) map[string]any {
	t.Helper()
	p := map[string]any{
		"id": "1000", "application_id": "2", "type": 2, "token": "t", "version": 1,
		"guild_id": paneltest.IxGuild,
		"channel":  map[string]any{"id": "101", "type": 0},
		"member": map[string]any{
			"user":        map[string]any{"id": user, "username": "member"},
			"permissions": "0", "roles": []any{}, "joined_at": "2026-01-01T00:00:00Z",
		},
		"data": map[string]any{"id": "6", "name": "x", "type": 1},
	}
	edit(p)
	return p
}

// A member with no user is refused by both, rather than authorized as
// nobody in particular.
func TestAuthorizeIxRefusesAMemberWithNoUser(t *testing.T) {
	perms := core.NewPermissions(nil, policies{}, "900")
	ix, _ := paneltest.IxCommand(t, "11", "x", func(p map[string]any) {
		delete(p["member"].(map[string]any), "user")
	})
	if perms.AuthorizeIx(ix, core.PermSpec{Tier: core.TierPublic}) == nil {
		t.Fatal("a member with no user identity was authorized")
	}
}
