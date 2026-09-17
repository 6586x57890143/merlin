package roles

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

const (
	etGuild = "g-eternal"
	etUser  = "u-eternal"
	etRole  = "r-eternal"
	etOwner = "owner"
)

type eternalFixture struct {
	p       *Plugin
	ops     *fakeOps
	store   *fakeStore
	audit   *fakeAudit
	perms   *fakePerms
	scripts *fakeScripts
	fetched []string
}

// newEternalFixture is a guild owned by etOwner with the role (icon and all)
// on the member, the script on, and the role added as eternal via the real
// add path, so every test starts from a captured copy.
func newEternalFixture(t *testing.T) *eternalFixture {
	t.Helper()
	f := &eternalFixture{ops: newFakeOps(), store: newFakeStore(), audit: newFakeAudit(), perms: newFakePerms(), scripts: newFakeScripts()}
	f.p = newTestPlugin(f.ops, f.store, newFakeSettings(), f.audit, f.perms, newFakeScheduler())
	f.p.scripts = f.scripts
	f.p.fetch = func(ctx context.Context, url string) ([]byte, error) {
		f.fetched = append(f.fetched, url)
		return []byte("png-bytes"), nil
	}
	f.scripts.on[etGuild+":"+scriptEternalRole] = true
	f.ops.guildOwner = etOwner
	f.ops.roles[etGuild] = []*discordgo.Role{
		{ID: "everyone", Name: "@everyone", Position: 0},
		{ID: etRole, Name: "neumale", Color: 0xABCDEF, Hoist: true, Mentionable: true, Permissions: 1 << 10, Icon: "orig-hash", Position: 5},
		{ID: "other", Name: "other", Position: 7},
	}
	f.ops.setMember(etGuild, etUser, []string{etRole, "other"})
	if _, err := f.p.addEternalRole(context.Background(), etGuild, etUser, etRole, etOwner); err != nil {
		t.Fatalf("add: %v", err)
	}
	return f
}

func (f *eternalFixture) enforce(t *testing.T) {
	t.Helper()
	if err := f.p.enforceEternalRoles(context.Background(), etGuild); err != nil {
		t.Fatalf("enforce: %v", err)
	}
}

func (f *eternalFixture) role(id string) *discordgo.Role {
	for _, r := range f.ops.roles[etGuild] {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (f *eternalFixture) memberRoles() []string {
	m, _ := f.ops.GuildMember(etGuild, etUser)
	return m.Roles
}

func (f *eternalFixture) copyRec(t *testing.T) EternalRoleRecord {
	t.Helper()
	recs, _ := f.store.ListEternalRoles(context.Background(), etGuild)
	for _, r := range recs {
		if r.UserID == etUser && r.OriginRoleID == etRole {
			return r
		}
	}
	t.Fatal("no eternal copy stored")
	return EternalRoleRecord{}
}

func TestEternalAddCapturesTheLiveRole(t *testing.T) {
	f := newEternalFixture(t)

	rec := f.copyRec(t)
	if rec.RoleID != etRole || rec.Name != "neumale" || rec.Color != 0xABCDEF || !rec.Hoist || !rec.Mentionable ||
		rec.Permissions != 1<<10 || rec.IconHash != "orig-hash" || string(rec.Icon) != "png-bytes" || !rec.CapturedAt.Equal(fixedNow) {
		t.Fatalf("copy is not a faithful capture: %+v", rec)
	}
	if len(f.fetched) != 1 || !strings.Contains(f.fetched[0], "orig-hash") {
		t.Fatalf("icon should have been fetched from the CDN once, got %v", f.fetched)
	}
	if got := auditActions(f.audit); !slices.Contains(got, "roles.eternal_added") {
		t.Fatalf("add should be audited, got %v", got)
	}

	// Nothing is wrong, so enforcing changes nothing.
	f.enforce(t)
	if len(f.ops.roles[etGuild]) != 3 || len(f.audit.records) != 1 {
		t.Fatalf("a matching role on the member should be left alone: roles=%d audits=%v", len(f.ops.roles[etGuild]), auditActions(f.audit))
	}

	// A second add for the same pair is refused: the copy is taken once.
	f.role(etRole).Name = "edited"
	if _, err := f.p.addEternalRole(context.Background(), etGuild, etUser, etRole, etOwner); !errors.Is(err, errEternalExists) {
		t.Fatalf("second add: %v", err)
	}
	if f.copyRec(t).Name != "neumale" {
		t.Fatal("a refused add must not touch the copy")
	}
}

func TestEternalAddRefusesMissingAndManagedRoles(t *testing.T) {
	f := newEternalFixture(t)
	if _, err := f.p.addEternalRole(context.Background(), etGuild, "someone", "nope", etOwner); err == nil {
		t.Fatal("a role that does not exist cannot be copied")
	}
	f.ops.roles[etGuild] = append(f.ops.roles[etGuild], &discordgo.Role{ID: "bot-role", Name: "Some Bot", Managed: true})
	if _, err := f.p.addEternalRole(context.Background(), etGuild, "someone", "bot-role", etOwner); err == nil {
		t.Fatal("an integration-managed role cannot be assigned, so it cannot be eternal")
	}
}

func TestEternalRoleIsGivenBackWhenRemoved(t *testing.T) {
	f := newEternalFixture(t)
	f.ops.setMember(etGuild, etUser, []string{"other"})

	f.enforce(t)
	if !slices.Contains(f.memberRoles(), etRole) {
		t.Fatalf("role should be back on the member, roles=%v", f.memberRoles())
	}
	if !slices.Contains(auditActions(f.audit), "roles.eternal_reassigned") {
		t.Fatalf("re-add should be audited, got %v", auditActions(f.audit))
	}
}

func TestEternalRoleIsRecreatedWhenDeleted(t *testing.T) {
	f := newEternalFixture(t)
	f.ops.deleteRole(etGuild, etRole)
	f.ops.setMember(etGuild, etUser, []string{"other"})

	f.enforce(t)
	rec := f.copyRec(t)
	if rec.RoleID == etRole {
		t.Fatal("copy should retarget to the recreated role")
	}
	got := f.role(rec.RoleID)
	if got == nil || got.Name != "neumale" || got.Color != 0xABCDEF || !got.Hoist || !got.Mentionable || got.Permissions != 1<<10 || got.Icon == "" {
		t.Fatalf("recreated role is not a faithful copy: %+v", got)
	}
	if rec.IconHash != got.Icon {
		t.Fatalf("copy should carry Discord's hash of the re-upload, copy=%q live=%q", rec.IconHash, got.Icon)
	}
	if !slices.Contains(f.memberRoles(), rec.RoleID) {
		t.Fatalf("recreated role should be on the member, roles=%v", f.memberRoles())
	}
	if len(f.ops.reorders) != 0 {
		t.Fatal("a deleted role has nothing to sit above; no reorder expected")
	}
	if !slices.Contains(auditActions(f.audit), "roles.eternal_recreated") {
		t.Fatalf("recreate should be audited, got %v", auditActions(f.audit))
	}

	// And it settles: a second pass changes nothing.
	before := len(f.ops.roles[etGuild])
	f.enforce(t)
	if len(f.ops.roles[etGuild]) != before {
		t.Fatal("a second pass over a healthy state must not create another role")
	}
}

func TestEternalRoleEditedIsReplacedAndPlacedAbove(t *testing.T) {
	f := newEternalFixture(t)
	f.role(etRole).Name = "renamed by a moody admin"
	f.role(etRole).Color = 0

	f.enforce(t)
	rec := f.copyRec(t)
	if rec.RoleID == etRole {
		t.Fatal("copy should retarget to the fresh role")
	}
	fresh := f.role(rec.RoleID)
	if fresh == nil || fresh.Name != "neumale" || fresh.Color != 0xABCDEF {
		t.Fatalf("fresh role should match the copy: %+v", fresh)
	}
	if old := f.role(etRole); old == nil || old.Name != "renamed by a moody admin" {
		t.Fatal("the edited role is the admin's; it must be left alone")
	}
	if fresh.Position != 6 {
		t.Fatalf("fresh role should sit directly above the edited one (5), got %d", fresh.Position)
	}
	roles := f.memberRoles()
	if !slices.Contains(roles, rec.RoleID) || !slices.Contains(roles, etRole) {
		t.Fatalf("member keeps the edited role and gains the fresh one, got %v", roles)
	}
}

func TestEternalRoleIconRefusedFallsBackToNoIcon(t *testing.T) {
	f := newEternalFixture(t)
	f.ops.deleteRole(etGuild, etRole)
	f.ops.roleCreateErr = true

	f.enforce(t)
	rec := f.copyRec(t)
	got := f.role(rec.RoleID)
	if got == nil || got.Name != "neumale" || got.Icon != "" {
		t.Fatalf("second attempt should create the role without its icon: %+v", got)
	}
	if len(rec.Icon) == 0 {
		t.Fatal("the stored icon bytes must survive a guild that cannot take them today")
	}
}

func TestEternalRoleYieldsToJail(t *testing.T) {
	f := newEternalFixture(t)
	f.store.jails[etGuild+":"+etUser] = JailRecord{GuildID: etGuild, UserID: etUser, JailRoleID: "jail"}
	f.ops.setMember(etGuild, etUser, []string{"jail"})

	f.enforce(t)
	if slices.Contains(f.memberRoles(), etRole) {
		t.Fatal("a jailed member's roles belong to the jail, not the script")
	}
}

func TestEternalRoleDoesNothingWhileOff(t *testing.T) {
	f := newEternalFixture(t)
	f.scripts.on = map[string]bool{}
	f.ops.setMember(etGuild, etUser, []string{"other"})

	f.enforce(t)
	if slices.Contains(f.memberRoles(), etRole) {
		t.Fatal("nothing is enforced while the script is off")
	}

	// An unreadable switch is the same as off.
	f.scripts.err = errors.New("db down")
	f.enforce(t)
	if slices.Contains(f.memberRoles(), etRole) {
		t.Fatal("an unreadable switch must read as off")
	}
}

func TestEternalRoleMemberGoneIsNotAnError(t *testing.T) {
	f := newEternalFixture(t)
	f.ops.memberFetchErr = &discordgo.RESTError{Message: &discordgo.APIErrorMessage{Code: discordgo.ErrCodeUnknownMember}}
	f.enforce(t)

	f.ops.memberFetchErr = errors.New("502")
	if err := f.p.enforceEternalRoles(context.Background(), etGuild); err == nil {
		t.Fatal("a failed member fetch must surface so the sweep retries")
	}
}

func TestEternalRoleSweepAndHandlersReachTheScript(t *testing.T) {
	f := newEternalFixture(t)
	f.ops.setMember(etGuild, etUser, []string{"other"})
	if err := f.p.sweep(context.Background(), etGuild); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !slices.Contains(f.memberRoles(), etRole) {
		t.Fatal("the sweep should have run the script")
	}

	f.ops.setMember(etGuild, etUser, []string{"other"})
	f.p.HandleMemberUpdate(context.Background(), etGuild, etUser, []string{"other"})
	if !slices.Contains(f.memberRoles(), etRole) {
		t.Fatal("a member update should have run the script")
	}

	f.ops.deleteRole(etGuild, etRole)
	f.p.HandleRoleDeleted(context.Background(), etGuild, etRole)
	if f.copyRec(t).RoleID == etRole {
		t.Fatal("a role delete should have run the script")
	}

	f.role(f.copyRec(t).RoleID).Name = "edited"
	f.p.HandleRoleUpdated(context.Background(), etGuild)
	if f.role(f.copyRec(t).RoleID).Name != "neumale" {
		t.Fatal("a role update should have run the script")
	}

	// A member nobody is eternal for costs nothing.
	f.ops.setMember(etGuild, "someone-else", nil)
	f.p.HandleMemberJoin(context.Background(), etGuild, "someone-else")
}

func TestEternalAddIconFetchFailureCapturesHashOnly(t *testing.T) {
	f := newEternalFixture(t)
	f.p.fetch = func(context.Context, string) ([]byte, error) { return nil, errors.New("cdn down") }
	f.ops.roles[etGuild] = append(f.ops.roles[etGuild], &discordgo.Role{ID: "r2", Name: "second", Icon: "hash-2"})
	rec, err := f.p.addEternalRole(context.Background(), etGuild, etUser, "r2", etOwner)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if rec.IconHash != "hash-2" || len(rec.Icon) != 0 {
		t.Fatalf("hash kept, bytes absent: %+v", rec)
	}
	// The standing role still matches its hash, so nothing is recreated...
	f.enforce(t)
	if len(f.ops.roles[etGuild]) != 4 {
		t.Fatal("a role whose icon merely was not captured is not a diverged role")
	}
	// ...and a recreate comes back without a picture rather than not at all.
	f.ops.deleteRole(etGuild, "r2")
	f.enforce(t)
	recs, _ := f.store.ListEternalRoles(context.Background(), etGuild)
	i := slices.IndexFunc(recs, func(r EternalRoleRecord) bool { return r.OriginRoleID == "r2" })
	if got := f.role(recs[i].RoleID); got == nil || got.Icon != "" || got.Name != "second" {
		t.Fatalf("recreated without icon: %+v", got)
	}
}

// --- commands ---

func boolArg(name string, v bool) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionBoolean, Value: v}
}

func scriptsInteraction(actor, sub string, args ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.InteractionCreate {
	i := rolesInteraction("scripts", sub, args...)
	i.GuildID = etGuild
	i.Member.User.ID = actor
	return i
}

// Turning a script on answers with a warning, not a success, runs the
// script once so anything defined is put right immediately, and is
// audited. Turning it off keeps the definitions and copies.
func TestScriptsSetWarnsAndEnforcesOnEnable(t *testing.T) {
	f := newEternalFixture(t)
	f.scripts.on = map[string]bool{}
	f.ops.setMember(etGuild, etUser, []string{"other"})
	s, rt := handlerSession(t)

	f.p.handleScriptsSet(context.Background(), s, scriptsInteraction("admin", "set", strArg("script", scriptEternalRole), boolArg("enabled", true)))
	if !f.scripts.on[etGuild+":"+scriptEternalRole] {
		t.Fatal("script should be on")
	}
	if !rt.said("Script on: eternal-role") || !rt.said("overrides normal admin authority") || !rt.said("<@"+etUser+"> keeps <@&"+etRole+">") {
		t.Fatalf("enable must answer with the warning and what it protects, got %v", rt.bodies)
	}
	if !slices.Contains(f.memberRoles(), etRole) {
		t.Fatal("enabling should enforce right away")
	}
	if got := auditActions(f.audit); !slices.Contains(got, "roles.script_enabled") {
		t.Fatalf("audit: %v", got)
	}

	f.p.handleScriptsSet(context.Background(), s, scriptsInteraction("admin", "set", strArg("script", scriptEternalRole), boolArg("enabled", false)))
	if f.scripts.on[etGuild+":"+scriptEternalRole] {
		t.Fatal("script should be off")
	}
	f.copyRec(t) // still there
	if !rt.said("Script off") || !slices.Contains(auditActions(f.audit), "roles.script_disabled") {
		t.Fatalf("disable should be confirmed and audited, got %v", auditActions(f.audit))
	}
}

func TestScriptsSetRefusesUnknownScript(t *testing.T) {
	f := newEternalFixture(t)
	s, rt := handlerSession(t)
	f.p.handleScriptsSet(context.Background(), s, scriptsInteraction("admin", "set", strArg("script", "nope"), boolArg("enabled", true)))
	if !rt.said("Unknown script") {
		t.Fatalf("got %v", rt.bodies)
	}
}

func TestScriptsListShowsStateAndDefinitions(t *testing.T) {
	f := newEternalFixture(t)
	s, rt := handlerSession(t)
	f.p.handleScriptsList(context.Background(), s, scriptsInteraction("admin", "list"))
	if !rt.said("`eternal-role`: on: <@" + etUser + "> keeps <@&" + etRole + ">") {
		t.Fatalf("got %v", rt.bodies)
	}
	f.scripts.err = errors.New("db down")
	f.p.handleScriptsList(context.Background(), s, scriptsInteraction("admin", "list"))
	if !rt.said("treated as off") {
		t.Fatalf("got %v", rt.bodies)
	}
}

// Defining is for the owner or the operator. An admin, TierAdmin or not,
// is refused before anything is read or written: they can only turn the
// script off.
func TestEternalAddAndRemoveAreOwnerOrOperatorOnly(t *testing.T) {
	f := newEternalFixture(t)
	f.perms.bootstrapID = "operator"
	f.ops.roles[etGuild] = append(f.ops.roles[etGuild], &discordgo.Role{ID: "r2", Name: "second"})
	s, rt := handlerSession(t)

	f.p.handleEternalAdd(context.Background(), s, scriptsInteraction("admin", "eternal-add", userArg("user", "u2"), roleArg("role", "r2")))
	if !rt.said("only the server owner") {
		t.Fatalf("an admin must be refused, got %v", rt.bodies)
	}
	if recs, _ := f.store.ListEternalRoles(context.Background(), etGuild); len(recs) != 1 {
		t.Fatal("a refused add must write nothing")
	}
	f.p.handleEternalRemove(context.Background(), s, scriptsInteraction("admin", "eternal-remove", userArg("user", etUser), roleArg("role", etRole)))
	f.copyRec(t)

	f.p.handleEternalAdd(context.Background(), s, scriptsInteraction("operator", "eternal-add", userArg("user", "u2"), roleArg("role", "r2")))
	if !rt.said("Eternal role added") {
		t.Fatalf("the operator may add, got %v", rt.bodies)
	}
	f.p.handleEternalAdd(context.Background(), s, scriptsInteraction(etOwner, "eternal-add", userArg("user", "u2"), roleArg("role", "r2")))
	if !rt.said("already has that role") {
		t.Fatalf("a duplicate is refused, got %v", rt.bodies)
	}

	f.p.handleEternalRemove(context.Background(), s, scriptsInteraction(etOwner, "eternal-remove", userArg("user", "u2"), roleArg("role", "r2")))
	if !rt.said("Eternal role removed") || !slices.Contains(auditActions(f.audit), "roles.eternal_removed") {
		t.Fatalf("the owner may remove, got %v / %v", rt.bodies, auditActions(f.audit))
	}
	if recs, _ := f.store.ListEternalRoles(context.Background(), etGuild); len(recs) != 1 {
		t.Fatal("remove should drop exactly that row")
	}
	f.p.handleEternalRemove(context.Background(), s, scriptsInteraction(etOwner, "eternal-remove", userArg("user", "u2"), roleArg("role", "r2")))
	if !rt.said("does not keep") {
		t.Fatalf("removing what is not there is an error, got %v", rt.bodies)
	}

	// An unreadable guild fails closed.
	f.ops.guildErr = errors.New("502")
	f.p.handleEternalAdd(context.Background(), s, scriptsInteraction(etOwner, "eternal-add", userArg("user", "u3"), roleArg("role", "r2")))
	if !rt.said("could not read this server") {
		t.Fatalf("got %v", rt.bodies)
	}
}

// Adding while the script is off stores the definition and says so.
func TestEternalAddWhileOffSaysSo(t *testing.T) {
	f := newEternalFixture(t)
	f.scripts.on = map[string]bool{}
	f.ops.roles[etGuild] = append(f.ops.roles[etGuild], &discordgo.Role{ID: "r2", Name: "second"})
	s, rt := handlerSession(t)
	f.p.handleEternalAdd(context.Background(), s, scriptsInteraction(etOwner, "eternal-add", userArg("user", "u2"), roleArg("role", "r2")))
	if !rt.said("Eternal role added") || !rt.said("The script is **off**") {
		t.Fatalf("got %v", rt.bodies)
	}
}
