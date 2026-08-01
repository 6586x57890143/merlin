package core

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

// fakeAuthData is an in-memory GuildAuthData for tests.
type fakeAuthData struct {
	modRoles  map[string][]string
	admins    map[string][]string
	overrides map[string]map[string]struct{ roleIDs, userIDs []string }
}

func newFakeAuthData() *fakeAuthData {
	return &fakeAuthData{
		modRoles:  make(map[string][]string),
		admins:    make(map[string][]string),
		overrides: make(map[string]map[string]struct{ roleIDs, userIDs []string }),
	}
}

func (f *fakeAuthData) ModRoleIDs(guildID string) []string   { return f.modRoles[guildID] }
func (f *fakeAuthData) AdminUserIDs(guildID string) []string { return f.admins[guildID] }
func (f *fakeAuthData) ActionOverride(guildID, action string) (roleIDs, userIDs []string) {
	byAction, ok := f.overrides[guildID]
	if !ok {
		return nil, nil
	}
	o, ok := byAction[action]
	if !ok {
		return nil, nil
	}
	return o.roleIDs, o.userIDs
}

func memberInteraction(guildID, userID string, roles []string) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		GuildID: guildID,
		Member:  &discordgo.Member{Roles: roles, User: &discordgo.User{ID: userID}},
	}}
}

func TestAuthorizePublicTierAlwaysPasses(t *testing.T) {
	perms := NewPermissions(nil, newFakeAuthData(), "")
	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{}}
	if err := perms.Authorize(i, PermSpec{Tier: TierPublic}); err != nil {
		t.Fatalf("expected TierPublic to always pass, got %v", err)
	}
}

func TestAuthorizeModTierRequiresModRoleOrAdmin(t *testing.T) {
	auth := newFakeAuthData()
	auth.modRoles["g1"] = []string{"modrole"}
	perms := NewPermissions(nil, auth, "")

	if err := perms.Authorize(memberInteraction("g1", "u1", []string{"modrole"}), PermSpec{Tier: TierMod, Action: "test"}); err != nil {
		t.Fatalf("expected mod role to pass TierMod, got %v", err)
	}
	if err := perms.Authorize(memberInteraction("g1", "u1", []string{"otherrole"}), PermSpec{Tier: TierMod, Action: "test"}); err == nil {
		t.Fatal("expected a non-mod, non-whitelisted member to fail TierMod")
	}
}

func TestAuthorizeAdminSatisfiesModTier(t *testing.T) {
	auth := newFakeAuthData()
	auth.admins["g1"] = []string{"adminuser"}
	perms := NewPermissions(nil, auth, "")

	if err := perms.Authorize(memberInteraction("g1", "adminuser", nil), PermSpec{Tier: TierMod, Action: "test"}); err != nil {
		t.Fatalf("expected admin to satisfy TierMod, got %v", err)
	}
}

func TestAuthorizeAdminTierRejectsMod(t *testing.T) {
	auth := newFakeAuthData()
	auth.modRoles["g1"] = []string{"modrole"}
	perms := NewPermissions(nil, auth, "")

	if err := perms.Authorize(memberInteraction("g1", "u1", []string{"modrole"}), PermSpec{Tier: TierAdmin, Action: "test"}); err == nil {
		t.Fatal("expected a mod (non-admin) to fail TierAdmin")
	}
}

func TestAuthorizeBreakGlassAdminAlwaysSatisfiesAdminTier(t *testing.T) {
	perms := NewPermissions(nil, newFakeAuthData(), "breakglass-user")
	if err := perms.Authorize(memberInteraction("unconfigured-guild", "breakglass-user", nil), PermSpec{Tier: TierAdmin, Action: "test"}); err != nil {
		t.Fatalf("expected break-glass admin to satisfy TierAdmin even in an unconfigured guild, got %v", err)
	}
}

func TestAuthorizeWhitelistGrantsAccessIndependentOfTier(t *testing.T) {
	auth := newFakeAuthData()
	auth.overrides["g1"] = map[string]struct{ roleIDs, userIDs []string }{
		"rotation.configure": {userIDs: []string{"whitelisted-user"}},
	}
	perms := NewPermissions(nil, auth, "")

	// Not a mod, not an admin, but explicitly whitelisted for this action.
	if err := perms.Authorize(memberInteraction("g1", "whitelisted-user", nil), PermSpec{Tier: TierAdmin, Action: "rotation.configure"}); err != nil {
		t.Fatalf("expected whitelisted user to pass even TierAdmin for their granted action, got %v", err)
	}
	// The same user has no grant for a different action.
	if err := perms.Authorize(memberInteraction("g1", "whitelisted-user", nil), PermSpec{Tier: TierAdmin, Action: "other.action"}); err == nil {
		t.Fatal("expected the whitelist grant to be scoped to its own action only")
	}
}

func TestAuthorizeNoMemberContextFails(t *testing.T) {
	perms := NewPermissions(nil, newFakeAuthData(), "")
	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{GuildID: "g1"}}
	if err := perms.Authorize(i, PermSpec{Tier: TierMod, Action: "test"}); err == nil {
		t.Fatal("expected a missing Member to fail authorization")
	}
}

func TestCanManageRoleHierarchy(t *testing.T) {
	session, err := discordgo.New("Bot faketoken")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	session.State.User = &discordgo.User{ID: "bot"}

	guild := &discordgo.Guild{
		ID: "g1",
		Roles: []*discordgo.Role{
			{ID: "low", Position: 1},
			{ID: "bot-role", Position: 5},
			{ID: "high", Position: 10},
		},
	}
	if err := session.State.GuildAdd(guild); err != nil {
		t.Fatalf("GuildAdd: %v", err)
	}
	if err := session.State.MemberAdd(&discordgo.Member{
		GuildID: "g1",
		User:    &discordgo.User{ID: "bot"},
		Roles:   []string{"bot-role"},
	}); err != nil {
		t.Fatalf("MemberAdd: %v", err)
	}

	perms := NewPermissions(session, newFakeAuthData(), "")

	if err := perms.CanManageRole("g1", "low"); err != nil {
		t.Fatalf("expected bot to manage role below it, got %v", err)
	}
	if err := perms.CanManageRole("g1", "high"); err == nil {
		t.Fatal("expected denial for role above bot's top role")
	}
	if err := perms.CanManageRole("g1", "bot-role"); err == nil {
		t.Fatal("expected denial for role at same position as bot's top role")
	}
}
