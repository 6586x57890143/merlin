package core

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
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

// asked is one member asking in one guild: what Authorize reads off an
// interaction. who is nil for an interaction with no member.
type asked struct {
	guildID string
	who     *invoker
}

// ask is Authorize's decision for a.
func (p *Permissions) ask(a asked, spec PermSpec) error { return p.authorize(a.guildID, a.who, spec) }

func memberInteraction(guildID, userID string, roles []string) asked {
	return asked{guildID, &invoker{userID: userID, roles: roles}}
}

// memberInteractionWithPerms is memberInteraction plus the Discord
// permissions Discord attaches to every real interaction, to test the
// Administrator-bit path to TierAdmin apart from the DB-listed ones.
func memberInteractionWithPerms(guildID, userID string, roles []string, perms discord.Permissions) asked {
	a := memberInteraction(guildID, userID, roles)
	a.who.administrator = perms.Has(discord.PermissionAdministrator)
	return a
}

func TestAuthorizePublicTierAlwaysPasses(t *testing.T) {
	perms := NewPermissions(nil, newFakeAuthData(), "")
	if err := perms.ask(asked{}, PermSpec{Tier: TierPublic}); err != nil {
		t.Fatalf("expected TierPublic to always pass, got %v", err)
	}
}

func TestAuthorizeModTierRequiresModRoleOrAdmin(t *testing.T) {
	auth := newFakeAuthData()
	auth.modRoles["g1"] = []string{"modrole"}
	perms := NewPermissions(nil, auth, "")

	if err := perms.ask(memberInteraction("g1", "u1", []string{"modrole"}), PermSpec{Tier: TierMod, Action: "test"}); err != nil {
		t.Fatalf("expected mod role to pass TierMod, got %v", err)
	}
	if err := perms.ask(memberInteraction("g1", "u1", []string{"otherrole"}), PermSpec{Tier: TierMod, Action: "test"}); err == nil {
		t.Fatal("expected a non-mod, non-whitelisted member to fail TierMod")
	}
}

func TestAuthorizeAdminSatisfiesModTier(t *testing.T) {
	auth := newFakeAuthData()
	auth.admins["g1"] = []string{"adminuser"}
	perms := NewPermissions(nil, auth, "")

	if err := perms.ask(memberInteraction("g1", "adminuser", nil), PermSpec{Tier: TierMod, Action: "test"}); err != nil {
		t.Fatalf("expected admin to satisfy TierMod, got %v", err)
	}
}

func TestAuthorizeAdminTierRejectsMod(t *testing.T) {
	auth := newFakeAuthData()
	auth.modRoles["g1"] = []string{"modrole"}
	perms := NewPermissions(nil, auth, "")

	if err := perms.ask(memberInteraction("g1", "u1", []string{"modrole"}), PermSpec{Tier: TierAdmin, Action: "test"}); err == nil {
		t.Fatal("expected a mod (non-admin) to fail TierAdmin")
	}
}

func TestAuthorizeBootstrapAdminAlwaysSatisfiesAdminTier(t *testing.T) {
	perms := NewPermissions(nil, newFakeAuthData(), "bootstrap-user")
	if err := perms.ask(memberInteraction("unconfigured-guild", "bootstrap-user", nil), PermSpec{Tier: TierAdmin, Action: "test"}); err != nil {
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
	if err := perms.ask(memberInteraction("g1", "whitelisted-user", nil), PermSpec{Tier: TierAdmin, Action: "rotation.configure"}); err != nil {
		t.Fatalf("expected whitelisted user to pass even TierAdmin for their granted action, got %v", err)
	}
	// The same user has no grant for a different action.
	if err := perms.ask(memberInteraction("g1", "whitelisted-user", nil), PermSpec{Tier: TierAdmin, Action: "other.action"}); err == nil {
		t.Fatal("expected the whitelist grant to be scoped to its own action only")
	}
}

func TestAuthorizeDiscordAdministratorSatisfiesAdminTier(t *testing.T) {
	perms := NewPermissions(nil, newFakeAuthData(), "")
	i := memberInteractionWithPerms("g1", "u1", nil, discord.PermissionAdministrator)
	if err := perms.ask(i, PermSpec{Tier: TierAdmin, Action: "test"}); err != nil {
		t.Fatalf("expected a Discord Administrator to satisfy TierAdmin with no DB admin entry, got %v", err)
	}
}

func TestAuthorizeModRoleWithoutAdministratorBitStillFailsAdminTier(t *testing.T) {
	auth := newFakeAuthData()
	auth.modRoles["g1"] = []string{"modrole"}
	perms := NewPermissions(nil, auth, "")

	// Holding a lesser Discord permission (not Administrator) must not widen
	// the mod path into satisfying TierAdmin.
	i := memberInteractionWithPerms("g1", "u1", []string{"modrole"}, discord.PermissionManageGuild)
	if err := perms.ask(i, PermSpec{Tier: TierAdmin, Action: "test"}); err == nil {
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
	// denied for this specific action: deny must still win.
	i := memberInteractionWithPerms("g1", "u1", nil, discord.PermissionAdministrator)
	if err := perms.ask(i, PermSpec{Tier: TierAdmin, Action: "rotation.configure"}); err == nil {
		t.Fatal("expected an explicit deny to override both the allow-grant and the Administrator bit")
	}
}

func TestAuthorizeDenyNeverAppliesToBootstrapAdmin(t *testing.T) {
	auth := newFakeAuthData()
	auth.policies["g1"] = map[string]ActionPolicy{
		"config.mutate": {DenyUserIDs: []string{"bootstrap-user"}},
	}
	perms := NewPermissions(nil, auth, "bootstrap-user")

	if err := perms.ask(memberInteraction("g1", "bootstrap-user", nil), PermSpec{Tier: TierAdmin, Action: "config.mutate"}); err != nil {
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
	// this action to TierAdmin: a plain mod role must no longer be enough.
	i := memberInteraction("g1", "u1", []string{"modrole"})
	if err := perms.ask(i, PermSpec{Tier: TierMod, Action: "rotation.configure"}); err == nil {
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
	// this action to TierMod: a plain mod role should now be enough.
	i := memberInteraction("g1", "u1", []string{"modrole"})
	if err := perms.ask(i, PermSpec{Tier: TierAdmin, Action: "rotation.configure_structural"}); err != nil {
		t.Fatalf("expected the guild's Mod-allowed override to accept a mod-role holder, got %v", err)
	}
}

func TestAuthorizeNoMemberContextFails(t *testing.T) {
	perms := NewPermissions(nil, newFakeAuthData(), "")
	if err := perms.ask(asked{guildID: "g1"}, PermSpec{Tier: TierMod, Action: "test"}); err == nil {
		t.Fatal("expected a missing Member to fail authorization")
	}
}

// gatewayState is a gateway cache holding one guild (100) with roles and
// the bot (900) as a member with botRoles.
func gatewayState(owner snowflake.ID, roles []discord.Role, botRoles ...snowflake.ID) cache.Caches {
	c := cache.New(cache.WithCaches(cache.FlagGuilds, cache.FlagRoles, cache.FlagMembers))
	c.SetSelfUser(discord.OAuth2User{User: discord.User{ID: 900}})
	c.AddGuild(discord.Guild{ID: 100, OwnerID: owner})
	for _, r := range roles {
		r.GuildID = 100
		c.AddRole(r)
	}
	c.AddMember(discord.Member{GuildID: 100, User: discord.User{ID: 900}, RoleIDs: botRoles})
	return c
}

func TestCanManageRoleHierarchy(t *testing.T) {
	const low, botRole, high, boosted = "201", "205", "210", "301"
	perms := NewPermissions(gatewayState(1, []discord.Role{
		{ID: 201, Position: 1},
		{ID: 205, Position: 5},
		{ID: 210, Position: 10},
		{ID: 301, Position: 1, Managed: true},
	}, 205), newFakeAuthData(), "")

	if err := perms.CanManageRole("100", low); err != nil {
		t.Fatalf("expected bot to manage role below it, got %v", err)
	}
	if err := perms.CanManageRole("100", high); err == nil {
		t.Fatal("expected denial for role above bot's top role")
	}
	if err := perms.CanManageRole("100", botRole); err == nil {
		t.Fatal("expected denial for role at same position as bot's top role")
	}
	if err := perms.CanManageRole("100", boosted); err == nil {
		t.Fatal("expected denial for a role managed by an integration, even below bot's top role")
	}
	if err := perms.CanManageRole("100", "999"); err == nil {
		t.Fatal("expected denial for a role the guild does not have")
	}
}

// TestAuthorizeTierOverrideCanRaiseAPublicAction is the regression for a
// fail-open in the one function that has to fail closed: Authorize used to
// short-circuit on the command's *compiled-in* TierPublic before applying
// the guild's RequiredTier override, so `/config permissions set-tier` on a
// public action was silently a no-op: the guild saw a success message and
// got no restriction at all.
func TestAuthorizeTierOverrideCanRaiseAPublicAction(t *testing.T) {
	auth := newFakeAuthData()
	auth.admins["g1"] = []string{"adminuser"}
	auth.policies["g1"] = map[string]ActionPolicy{
		"public.thing": {RequiredTier: TierAdmin},
	}
	perms := NewPermissions(nil, auth, "")
	spec := PermSpec{Tier: TierPublic, Action: "public.thing"}

	if err := perms.ask(memberInteraction("g1", "nobody", nil), spec); err == nil {
		t.Fatal("guild raised this public action to Admins only, but a random member still passed")
	}
	if err := perms.ask(memberInteraction("g1", "adminuser", nil), spec); err != nil {
		t.Fatalf("expected an admin to pass the raised tier, got %v", err)
	}
	// A guild that never customized the action must still get plain public
	// behavior: the override is the exception, not a new default.
	if err := perms.ask(memberInteraction("g2", "nobody", nil), spec); err != nil {
		t.Fatalf("expected an un-overridden public action to stay public, got %v", err)
	}
}

// moderationState is a gateway cache holding one guild with an
// Administrator-carrying role (201), an ordinary mod role (202), a member
// role (203) and a distinct owner (500): the things CanModerate has to tell
// apart for an arbitrary target.
func moderationState() cache.Caches {
	return gatewayState(500, []discord.Role{
		{ID: 201, Permissions: discord.PermissionAdministrator},
		{ID: 202, Permissions: discord.PermissionKickMembers},
		{ID: 203},
	})
}

func actorMember(userID string, roles []string) *Actor {
	return &Actor{ID: userID, RoleIDs: roles}
}

// TestCanModerateProtectsAdminsFromMods pins down the privilege inversion the
// tier model alone can't see: /roles jail is TierMod and strips roles, so
// without this check a mod could strip an admin's Administrator bit, and
// with it that admin's TierAdmin access to this bot, without ever running a
// command above their own tier. A rogue mod could dismantle the whole admin
// team one jail at a time and every individual action would look authorized.
func TestCanModerateProtectsAdminsFromMods(t *testing.T) {
	auth := newFakeAuthData()
	auth.admins["100"] = []string{"db-listed-admin"}
	perms := NewPermissions(moderationState(), auth, "bootstrap-user")

	mod := actorMember("mod", []string{"202"})

	for _, tc := range []struct {
		name         string
		targetUserID string
		targetRoles  []string
	}{
		{"admin by Discord's Administrator bit", "301", []string{"201"}},
		{"admin listed in the guild's settings", "db-listed-admin", nil},
		{"the guild owner", "500", nil},
		{"the bootstrap admin", "bootstrap-user", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := perms.CanModerate("100", mod, tc.targetUserID, tc.targetRoles); err == nil {
				t.Fatalf("a mod was allowed to act against %s", tc.name)
			}
		})
	}

	if err := perms.CanModerate("100", mod, "regular", []string{"203"}); err != nil {
		t.Fatalf("a mod must still be able to act against an ordinary member, got %v", err)
	}
}

// TestCanModerateAllowsAdminsAndPeerMods records the deliberate limits of the
// rule: it protects admins from *non*-admins only. Admins are peers and can
// act on each other, and mod-on-mod moderation stays allowed: a mod acting
// against a peer is ordinary moderation, and admins can always intervene.
func TestCanModerateAllowsAdminsAndPeerMods(t *testing.T) {
	auth := newFakeAuthData()
	auth.admins["100"] = []string{"db-listed-admin"}
	perms := NewPermissions(moderationState(), auth, "bootstrap-user")

	admin := actorMember("db-listed-admin", nil)
	if err := perms.CanModerate("100", admin, "301", []string{"201"}); err != nil {
		t.Fatalf("an admin must be able to act against a fellow admin, got %v", err)
	}

	mod := actorMember("mod", []string{"202"})
	if err := perms.CanModerate("100", mod, "other-mod", []string{"202"}); err != nil {
		t.Fatalf("mod-on-mod moderation must stay allowed, got %v", err)
	}
}

// TestCanModerateProtectsBootstrapFromAdminsToo: the bootstrap identity is
// the operator's guaranteed way back into a guild, so like the deny-list's
// own carve-out, no in-guild action may disable it, not even an admin's.
func TestCanModerateProtectsBootstrapFromAdminsToo(t *testing.T) {
	auth := newFakeAuthData()
	auth.admins["100"] = []string{"db-listed-admin"}
	perms := NewPermissions(moderationState(), auth, "bootstrap-user")

	admin := actorMember("db-listed-admin", nil)
	if err := perms.CanModerate("100", admin, "bootstrap-user", nil); err == nil {
		t.Fatal("the bootstrap admin must not be targetable, even by another admin")
	}
}

// TestCanModerateFailsClosedWithoutGuildState verifies the direction this
// errs in when it can't tell: if the guild isn't resolvable we cannot know
// whether the target is an admin, so the action is refused. Allowing it would
// turn a cache miss into the exact escalation this check exists to stop.
func TestCanModerateFailsClosedWithoutGuildState(t *testing.T) {
	perms := NewPermissions(nil, newFakeAuthData(), "bootstrap-user")
	mod := actorMember("mod", []string{"202"})
	if err := perms.CanModerate("100", mod, "target", []string{"whatever"}); err == nil {
		t.Fatal("expected a refusal when the guild's state can't be resolved")
	}
}

// TestAuthorizeIgnoresStoredPublicTierOverride is defense in depth against a
// stored value rather than against a user: /config permissions set-tier only
// ever offers Mod/Admin, so a TierPublic override can only come from a
// corrupt row, a hand-edited database, or a future import path. Honoring one
// would strip every check off a privileged action, the single direction an
// override must never be able to move a command in. Loosening Admin to Mod
// stays supported (TestAuthorizeTierOverrideLoosensAdminToMod).
func TestAuthorizeIgnoresStoredPublicTierOverride(t *testing.T) {
	auth := newFakeAuthData()
	auth.policies["g1"] = map[string]ActionPolicy{
		"config.mutate": {RequiredTier: TierPublic},
	}
	perms := NewPermissions(nil, auth, "bootstrap-user")

	i := memberInteraction("g1", "nobody", nil)
	if err := perms.ask(i, PermSpec{Tier: TierAdmin, Action: "config.mutate"}); err == nil {
		t.Fatal("a stored public tier override made an admin-only action available to everyone")
	}
}

// TestHasDiscordErrorCodeDistinguishesAbsences: one API call can report
// several different absences (GuildMemberEdit answers Unknown Member for a
// target who left and Unknown Role for a deleted role), and a caller reacting
// to the wrong one acts on the wrong state.
func TestHasDiscordErrorCodeDistinguishesAbsences(t *testing.T) {
	roleGone := restErr(http.StatusNotFound, rest.JSONErrorCodeUnknownRole)
	memberGone := restErr(http.StatusNotFound, rest.JSONErrorCodeUnknownMember)

	if !HasDiscordErrorCode(roleGone, int(rest.JSONErrorCodeUnknownRole)) {
		t.Error("expected the unknown-role error to match the unknown-role code")
	}
	if HasDiscordErrorCode(memberGone, int(rest.JSONErrorCodeUnknownRole)) {
		t.Error("an unknown *member* must not read as an unknown role")
	}
	if !HasDiscordErrorCode(fmt.Errorf("edit: %w", roleGone), int(rest.JSONErrorCodeUnknownRole)) {
		t.Error("expected a wrapped error to still match")
	}
	if HasDiscordErrorCode(errors.New("dial tcp"), int(rest.JSONErrorCodeUnknownRole)) {
		t.Error("a non-REST error must not match any code")
	}
	// Both are still "gone" for the coarser question.
	if !IsUnknownResource(memberGone) || !IsUnknownResource(roleGone) {
		t.Error("both absences must still count as unknown resources")
	}
}

// Settings that cannot be read cannot say who is denied, so nobody but the
// bootstrap identity passes: not a Discord Administrator, not a public tier
// (a stored override may have raised it), not a whitelist.
func TestAuthorizeUnknownSettingsAdmitOnlyBootstrap(t *testing.T) {
	auth := newFakeAuthData()
	auth.policies["g1"] = map[string]ActionPolicy{
		"config.mutate": {Unknown: true},
		"ping.use":      {Unknown: true},
	}
	perms := NewPermissions(nil, auth, "bootstrap-user")

	admin := memberInteractionWithPerms("g1", "u1", nil, discord.PermissionAdministrator)
	if err := perms.ask(admin, PermSpec{Tier: TierAdmin, Action: "config.mutate"}); err == nil {
		t.Error("a Discord Administrator passed while the guild's deny list was unknown")
	}
	if err := perms.ask(memberInteraction("g1", "u2", nil), PermSpec{Tier: TierPublic, Action: "ping.use"}); err == nil {
		t.Error("a public action passed while its tier override was unknown")
	}
	if err := perms.ask(memberInteraction("g1", "bootstrap-user", nil), PermSpec{Tier: TierAdmin, Action: "config.mutate"}); err != nil {
		t.Errorf("the bootstrap identity was locked out of recovery: %v", err)
	}
}
