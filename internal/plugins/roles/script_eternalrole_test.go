package roles

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// The eternal-role script is exercised against the real compiled-in table:
// what these tests protect is the behaviour for the one entry that ships.
var eternal = eternalRoles[0]

type eternalFixture struct {
	p       *Plugin
	ops     *fakeOps
	store   *fakeStore
	audit   *fakeAudit
	scripts *fakeScripts
	fetched []string
}

// newEternalFixture is a guild with the origin role (icon and all) on the
// member, and the script on.
func newEternalFixture(t *testing.T) *eternalFixture {
	t.Helper()
	f := &eternalFixture{ops: newFakeOps(), store: newFakeStore(), audit: newFakeAudit(), scripts: newFakeScripts()}
	f.p = newTestPlugin(f.ops, f.store, newFakeSettings(), f.audit, newFakePerms(), newFakeScheduler())
	f.p.scripts = f.scripts
	f.p.fetch = func(ctx context.Context, url string) ([]byte, error) {
		f.fetched = append(f.fetched, url)
		return []byte("png-bytes"), nil
	}
	f.scripts.on[eternal.guildID+":"+scriptEternalRole] = true
	f.ops.roles[eternal.guildID] = []*discordgo.Role{
		{ID: "everyone", Name: "@everyone", Position: 0},
		{ID: eternal.roleID, Name: "neumale", Color: 0xABCDEF, Hoist: true, Mentionable: true, Permissions: 1 << 10, UnicodeEmoji: "", Icon: "orig-hash", Position: 5},
		{ID: "other", Name: "other", Position: 7},
	}
	f.ops.setMember(eternal.guildID, eternal.userID, []string{eternal.roleID, "other"})
	return f
}

func (f *eternalFixture) enforce(t *testing.T) {
	t.Helper()
	if err := f.p.enforceEternalRoles(context.Background(), eternal.guildID); err != nil {
		t.Fatalf("enforce: %v", err)
	}
}

func (f *eternalFixture) role(id string) *discordgo.Role {
	for _, r := range f.ops.roles[eternal.guildID] {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (f *eternalFixture) memberRoles() []string {
	m, _ := f.ops.GuildMember(eternal.guildID, eternal.userID)
	return m.Roles
}

func (f *eternalFixture) copyRec(t *testing.T) EternalRoleRecord {
	t.Helper()
	rec, ok, _ := f.store.GetEternalRole(context.Background(), eternal.guildID, eternal.userID, eternal.roleID)
	if !ok {
		t.Fatal("no eternal copy stored")
	}
	return rec
}

func TestEternalRoleFirstRunCapturesTheLiveRole(t *testing.T) {
	f := newEternalFixture(t)
	f.enforce(t)

	rec := f.copyRec(t)
	if rec.RoleID != eternal.roleID || rec.Name != "neumale" || rec.Color != 0xABCDEF || !rec.Hoist || !rec.Mentionable ||
		rec.Permissions != 1<<10 || rec.IconHash != "orig-hash" || string(rec.Icon) != "png-bytes" || !rec.CapturedAt.Equal(fixedNow) {
		t.Fatalf("copy is not a faithful capture: %+v", rec)
	}
	if len(f.fetched) != 1 || !strings.Contains(f.fetched[0], "orig-hash") {
		t.Fatalf("icon should have been fetched from the CDN once, got %v", f.fetched)
	}
	if !slices.Contains(auditActions(f.audit), "roles.eternal_captured") {
		t.Fatalf("capture should be audited, got %v", auditActions(f.audit))
	}
	// Nothing was wrong, so nothing else happened.
	if len(f.ops.roles[eternal.guildID]) != 3 || len(f.audit.records) != 1 {
		t.Fatalf("a matching role on the member should be left alone: roles=%d audits=%v", len(f.ops.roles[eternal.guildID]), auditActions(f.audit))
	}
}

func TestEternalRoleIsGivenBackWhenRemoved(t *testing.T) {
	f := newEternalFixture(t)
	f.enforce(t) // capture
	f.ops.setMember(eternal.guildID, eternal.userID, []string{"other"})

	f.enforce(t)
	if !slices.Contains(f.memberRoles(), eternal.roleID) {
		t.Fatalf("role should be back on the member, roles=%v", f.memberRoles())
	}
	if !slices.Contains(auditActions(f.audit), "roles.eternal_reassigned") {
		t.Fatalf("re-add should be audited, got %v", auditActions(f.audit))
	}
}

func TestEternalRoleIsRecreatedWhenDeleted(t *testing.T) {
	f := newEternalFixture(t)
	f.enforce(t)
	f.ops.deleteRole(eternal.guildID, eternal.roleID)
	f.ops.setMember(eternal.guildID, eternal.userID, []string{"other"})

	f.enforce(t)
	rec := f.copyRec(t)
	if rec.RoleID == eternal.roleID {
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
	before := len(f.ops.roles[eternal.guildID])
	f.enforce(t)
	if len(f.ops.roles[eternal.guildID]) != before {
		t.Fatal("a second pass over a healthy state must not create another role")
	}
}

func TestEternalRoleEditedIsReplacedAndPlacedAbove(t *testing.T) {
	f := newEternalFixture(t)
	f.enforce(t)
	f.role(eternal.roleID).Name = "renamed by a moody admin"
	f.role(eternal.roleID).Color = 0

	f.enforce(t)
	rec := f.copyRec(t)
	if rec.RoleID == eternal.roleID {
		t.Fatal("copy should retarget to the fresh role")
	}
	fresh := f.role(rec.RoleID)
	if fresh == nil || fresh.Name != "neumale" || fresh.Color != 0xABCDEF {
		t.Fatalf("fresh role should match the copy: %+v", fresh)
	}
	if old := f.role(eternal.roleID); old == nil || old.Name != "renamed by a moody admin" {
		t.Fatal("the edited role is the admin's; it must be left alone")
	}
	if fresh.Position != 6 {
		t.Fatalf("fresh role should sit directly above the edited one (5), got %d", fresh.Position)
	}
	roles := f.memberRoles()
	if !slices.Contains(roles, rec.RoleID) || !slices.Contains(roles, eternal.roleID) {
		t.Fatalf("member keeps the edited role and gains the fresh one, got %v", roles)
	}
}

func TestEternalRoleIconRefusedFallsBackToNoIcon(t *testing.T) {
	f := newEternalFixture(t)
	f.enforce(t)
	f.ops.deleteRole(eternal.guildID, eternal.roleID)
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
	f.enforce(t)
	f.store.jails[eternal.guildID+":"+eternal.userID] = JailRecord{GuildID: eternal.guildID, UserID: eternal.userID, JailRoleID: "jail"}
	f.ops.setMember(eternal.guildID, eternal.userID, []string{"jail"})

	f.enforce(t)
	if slices.Contains(f.memberRoles(), eternal.roleID) {
		t.Fatal("a jailed member's roles belong to the jail, not the script")
	}
}

func TestEternalRoleDoesNothingWhileOff(t *testing.T) {
	f := newEternalFixture(t)
	f.scripts.on = map[string]bool{}
	f.ops.setMember(eternal.guildID, eternal.userID, []string{"other"})

	f.enforce(t)
	if _, ok, _ := f.store.GetEternalRole(context.Background(), eternal.guildID, eternal.userID, eternal.roleID); ok {
		t.Fatal("nothing is captured while the script is off")
	}
	if slices.Contains(f.memberRoles(), eternal.roleID) {
		t.Fatal("nothing is enforced while the script is off")
	}

	// An unreadable switch is the same as off.
	f.scripts.err = errors.New("db down")
	f.enforce(t)
	if slices.Contains(f.memberRoles(), eternal.roleID) {
		t.Fatal("an unreadable switch must read as off")
	}
}

func TestEternalRoleNeverInventsARole(t *testing.T) {
	f := newEternalFixture(t)
	f.ops.deleteRole(eternal.guildID, eternal.roleID)

	f.enforce(t)
	if _, ok, _ := f.store.GetEternalRole(context.Background(), eternal.guildID, eternal.userID, eternal.roleID); ok {
		t.Fatal("no live role to copy means no copy")
	}
	if len(f.ops.roles[eternal.guildID]) != 2 {
		t.Fatal("no copy means nothing to recreate from")
	}
}

func TestEternalRoleMemberGoneIsNotAnError(t *testing.T) {
	f := newEternalFixture(t)
	f.enforce(t)
	f.ops.memberFetchErr = &discordgo.RESTError{Message: &discordgo.APIErrorMessage{Code: discordgo.ErrCodeUnknownMember}}
	f.enforce(t)

	f.ops.memberFetchErr = errors.New("502")
	if err := f.p.enforceEternalRoles(context.Background(), eternal.guildID); err == nil {
		t.Fatal("a failed member fetch must surface so the sweep retries")
	}
}

func TestEternalRoleSweepAndHandlersReachTheScript(t *testing.T) {
	f := newEternalFixture(t)
	f.ops.setMember(eternal.guildID, eternal.userID, []string{"other"})
	if err := f.p.sweep(context.Background(), eternal.guildID); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !slices.Contains(f.memberRoles(), eternal.roleID) {
		t.Fatal("the sweep should have run the script")
	}

	f.ops.setMember(eternal.guildID, eternal.userID, []string{"other"})
	f.p.HandleMemberUpdate(context.Background(), eternal.guildID, eternal.userID, []string{"other"})
	if !slices.Contains(f.memberRoles(), eternal.roleID) {
		t.Fatal("a member update should have run the script")
	}

	f.ops.deleteRole(eternal.guildID, eternal.roleID)
	f.p.HandleRoleDeleted(context.Background(), eternal.guildID, eternal.roleID)
	if f.copyRec(t).RoleID == eternal.roleID {
		t.Fatal("a role delete should have run the script")
	}

	f.role(f.copyRec(t).RoleID).Name = "edited"
	f.p.HandleRoleUpdated(context.Background(), eternal.guildID)
	if f.role(f.copyRec(t).RoleID).Name != "neumale" {
		t.Fatal("a role update should have run the script")
	}

	// A member nobody is eternal for costs nothing.
	f.ops.setMember(eternal.guildID, "someone-else", nil)
	f.p.HandleMemberJoin(context.Background(), eternal.guildID, "someone-else")
}

func TestEternalRoleIconFetchFailureCapturesWithoutIcon(t *testing.T) {
	f := newEternalFixture(t)
	f.p.fetch = func(context.Context, string) ([]byte, error) { return nil, errors.New("cdn down") }
	f.enforce(t)
	rec := f.copyRec(t)
	if rec.IconHash != "orig-hash" || len(rec.Icon) != 0 {
		t.Fatalf("hash kept, bytes absent: %+v", rec)
	}
	// The standing role still matches its hash, so nothing is recreated...
	f.enforce(t)
	if f.copyRec(t).RoleID != eternal.roleID {
		t.Fatal("a role whose icon merely was not captured is not a diverged role")
	}
	// ...and a recreate comes back without a picture rather than not at all.
	f.ops.deleteRole(eternal.guildID, eternal.roleID)
	f.enforce(t)
	if got := f.role(f.copyRec(t).RoleID); got == nil || got.Icon != "" {
		t.Fatalf("recreated without icon: %+v", got)
	}
}

func boolArg(name string, v bool) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionBoolean, Value: v}
}

// Turning a script on answers with a warning, not a success, runs the
// script once so the copy exists immediately, and is audited. Turning it
// off keeps the copy.
func TestScriptsSetWarnsAndCapturesOnEnable(t *testing.T) {
	f := newEternalFixture(t)
	f.scripts.on = map[string]bool{}
	s, rt := handlerSession(t)
	i := rolesInteraction("scripts", "set", strArg("script", scriptEternalRole), boolArg("enabled", true))
	i.GuildID = eternal.guildID

	f.p.handleScriptsSet(context.Background(), s, i)
	if !f.scripts.on[eternal.guildID+":"+scriptEternalRole] {
		t.Fatal("script should be on")
	}
	if !rt.said("Script on: eternal-role") || !rt.said("overrides normal admin authority") {
		t.Fatalf("enable must answer with the warning, got %v", rt.bodies)
	}
	if _, ok, _ := f.store.GetEternalRole(context.Background(), eternal.guildID, eternal.userID, eternal.roleID); !ok {
		t.Fatal("enabling should capture the copy right away")
	}
	if got := auditActions(f.audit); !slices.Contains(got, "roles.script_enabled") {
		t.Fatalf("audit: %v", got)
	}

	i = rolesInteraction("scripts", "set", strArg("script", scriptEternalRole), boolArg("enabled", false))
	i.GuildID = eternal.guildID
	f.p.handleScriptsSet(context.Background(), s, i)
	if f.scripts.on[eternal.guildID+":"+scriptEternalRole] {
		t.Fatal("script should be off")
	}
	if _, ok, _ := f.store.GetEternalRole(context.Background(), eternal.guildID, eternal.userID, eternal.roleID); !ok {
		t.Fatal("turning a script off keeps the copy, or disable-edit-enable would launder an edit into it")
	}
	if !rt.said("Script off") || !slices.Contains(auditActions(f.audit), "roles.script_disabled") {
		t.Fatalf("disable should be confirmed and audited, got %v", auditActions(f.audit))
	}
}

func TestScriptsSetRefusesUnknownScript(t *testing.T) {
	f := newEternalFixture(t)
	s, rt := handlerSession(t)
	f.p.handleScriptsSet(context.Background(), s, rolesInteraction("scripts", "set", strArg("script", "nope"), boolArg("enabled", true)))
	if !rt.said("Unknown script") {
		t.Fatalf("got %v", rt.bodies)
	}
}

func TestScriptsListShowsState(t *testing.T) {
	f := newEternalFixture(t)
	s, rt := handlerSession(t)
	i := rolesInteraction("scripts", "list")
	i.GuildID = eternal.guildID
	f.p.handleScriptsList(context.Background(), s, i)
	if !rt.said("`eternal-role`: on") {
		t.Fatalf("got %v", rt.bodies)
	}
	f.scripts.err = errors.New("db down")
	f.p.handleScriptsList(context.Background(), s, i)
	if !rt.said("treated as off") {
		t.Fatalf("got %v", rt.bodies)
	}
}
