package core

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

// fakeAuthData is an in-memory GuildAuthData for tests.
type fakeAuthData struct {
	modRoles map[string][]string
	admins   map[string][]string
	policies map[string]map[string]ActionPolicy
}

func newFakeAuthData() *fakeAuthData {
	return &fakeAuthData{
		modRoles: make(map[string][]string),
		admins:   make(map[string][]string),
		policies: make(map[string]map[string]ActionPolicy),
	}
}

func (f *fakeAuthData) ModRoleIDs(guildID string) []string   { return f.modRoles[guildID] }
func (f *fakeAuthData) AdminUserIDs(guildID string) []string { return f.admins[guildID] }
func (f *fakeAuthData) ActionPolicy(guildID, action string) ActionPolicy {
	byAction, ok := f.policies[guildID]
	if !ok {
		return ActionPolicy{}
	}
	return byAction[action]
}

func memberInteraction(guildID, userID string, roles []string) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		GuildID: guildID,
		Member:  &discordgo.Member{Roles: roles, User: &discordgo.User{ID: userID}},
	}}
}

// memberInteractionWithPerms is memberInteraction plus a Discord permission
// bitmask, mirroring what Discord itself attaches to Member.Permissions on
// every real interaction — used to test the Administrator-bit path to
// TierAdmin independent of the DB-listed admin/mod-role paths.
func memberInteractionWithPerms(guildID, userID string, roles []string, perms int64) *discordgo.InteractionCreate {
	i := memberInteraction(guildID, userID, roles)
	i.Member.Permissions = perms
	return i
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

func TestAuthorizeBootstrapAdminAlwaysSatisfiesAdminTier(t *testing.T) {
	perms := NewPermissions(nil, newFakeAuthData(), "bootstrap-user")
	if err := perms.Authorize(memberInteraction("unconfigured-guild", "bootstrap-user", nil), PermSpec{Tier: TierAdmin, Action: "test"}); err != nil {
		t.Fatalf("expected bootstrap admin to satisfy TierAdmin even in an unconfigured guild, got %v", err)
	}
}

func TestAuthorizeWhitelistGrantsAccessIndependentOfTier(t *testing.T) {
	auth := newFakeAuthData()
	auth.policies["g1"] = map[string]ActionPolicy{
		"rotation.configure": {AllowUserIDs: []string{"whitelisted-user"}},
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

func TestAuthorizeDiscordAdministratorSatisfiesAdminTier(t *testing.T) {
	perms := NewPermissions(nil, newFakeAuthData(), "")
	i := memberInteractionWithPerms("g1", "u1", nil, discordgo.PermissionAdministrator)
	if err := perms.Authorize(i, PermSpec{Tier: TierAdmin, Action: "test"}); err != nil {
		t.Fatalf("expected a Discord Administrator to satisfy TierAdmin with no DB admin entry, got %v", err)
	}
}

func TestAuthorizeModRoleWithoutAdministratorBitStillFailsAdminTier(t *testing.T) {
	auth := newFakeAuthData()
	auth.modRoles["g1"] = []string{"modrole"}
	perms := NewPermissions(nil, auth, "")

	// Holding a lesser Discord permission (not Administrator) must not widen
	// the mod path into satisfying TierAdmin.
	i := memberInteractionWithPerms("g1", "u1", []string{"modrole"}, discordgo.PermissionManageGuild)
	if err := perms.Authorize(i, PermSpec{Tier: TierAdmin, Action: "test"}); err == nil {
		t.Fatal("expected a mod role holder without the Administrator bit to fail TierAdmin")
	}
}

func TestAuthorizeDenyWinsOverAllowAndAdministratorBit(t *testing.T) {
	auth := newFakeAuthData()
	auth.policies["g1"] = map[string]ActionPolicy{
		"rotation.configure": {AllowUserIDs: []string{"u1"}, DenyUserIDs: []string{"u1"}},
	}
	perms := NewPermissions(nil, auth, "")

	// u1 is both allow-granted and Discord Administrator, but explicitly
	// denied for this specific action — deny must still win.
	i := memberInteractionWithPerms("g1", "u1", nil, discordgo.PermissionAdministrator)
	if err := perms.Authorize(i, PermSpec{Tier: TierAdmin, Action: "rotation.configure"}); err == nil {
		t.Fatal("expected an explicit deny to override both the allow-grant and the Administrator bit")
	}
}

func TestAuthorizeDenyNeverAppliesToBootstrapAdmin(t *testing.T) {
	auth := newFakeAuthData()
	auth.policies["g1"] = map[string]ActionPolicy{
		"config.mutate": {DenyUserIDs: []string{"bootstrap-user"}},
	}
	perms := NewPermissions(nil, auth, "bootstrap-user")

	if err := perms.Authorize(memberInteraction("g1", "bootstrap-user", nil), PermSpec{Tier: TierAdmin, Action: "config.mutate"}); err != nil {
		t.Fatalf("expected the bootstrap admin to be immune to deny-listing, got %v", err)
	}
}

func TestAuthorizeTierOverrideTightensModToAdmin(t *testing.T) {
	auth := newFakeAuthData()
	auth.modRoles["g1"] = []string{"modrole"}
	auth.policies["g1"] = map[string]ActionPolicy{
		"rotation.configure": {RequiredTier: TierAdmin},
	}
	perms := NewPermissions(nil, auth, "")

	// The command's own PermSpec says TierMod, but the guild has overridden
	// this action to TierAdmin — a plain mod role must no longer be enough.
	i := memberInteraction("g1", "u1", []string{"modrole"})
	if err := perms.Authorize(i, PermSpec{Tier: TierMod, Action: "rotation.configure"}); err == nil {
		t.Fatal("expected the guild's Admin-only override to reject a mod-role holder")
	}
}

func TestAuthorizeTierOverrideLoosensAdminToMod(t *testing.T) {
	auth := newFakeAuthData()
	auth.modRoles["g1"] = []string{"modrole"}
	auth.policies["g1"] = map[string]ActionPolicy{
		"rotation.configure_structural": {RequiredTier: TierMod},
	}
	perms := NewPermissions(nil, auth, "")

	// The command's own PermSpec says TierAdmin, but the guild has loosened
	// this action to TierMod — a plain mod role should now be enough.
	i := memberInteraction("g1", "u1", []string{"modrole"})
	if err := perms.Authorize(i, PermSpec{Tier: TierAdmin, Action: "rotation.configure_structural"}); err != nil {
		t.Fatalf("expected the guild's Mod-allowed override to accept a mod-role holder, got %v", err)
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
