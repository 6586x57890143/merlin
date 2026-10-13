package roles

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
)

const (
	jrGuild = "7000005"
	jrUser  = "7000127"
	jrRole  = "7000339"
)

func newJoinRoleFixture(t *testing.T) (*Plugin, *fakeOps, *fakeStore, *fakeScripts) {
	t.Helper()
	ops, store, sc, cfg := newFakeOps(), newFakeStore(), newFakeScripts(), newFakeSettings()
	_ = cfg.SetMemberRole(context.Background(), jrGuild, jrRole)
	p := newTestPlugin(ops, store, cfg, newFakeAudit(), newFakePerms(), newFakeScheduler())
	p.scripts = sc
	sc.on[jrGuild+":"+scriptJoinRole] = true
	ops.setMember(jrGuild, jrUser, nil)
	return p, ops, store, sc
}

func hasRole(ops *fakeOps) bool {
	m, _ := ops.GuildMember(jrGuild, jrUser)
	return slices.Contains(m.Roles, jrRole)
}

func TestJoinRoleGivesTheRoleAfterTheSettle(t *testing.T) {
	p, ops, _, _ := newJoinRoleFixture(t)
	old := joinRoleSettle
	joinRoleSettle = 0
	t.Cleanup(func() { joinRoleSettle = old })

	p.HandleMemberJoin(context.Background(), jrGuild, jrUser)
	for deadline := time.Now().Add(2 * time.Second); !hasRole(ops); {
		if time.Now().After(deadline) {
			t.Fatal("role never given")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestJoinRoleSkips(t *testing.T) {
	cases := map[string]func(*fakeOps, *fakeStore, *fakeScripts){
		"script off": func(_ *fakeOps, _ *fakeStore, sc *fakeScripts) { sc.on[jrGuild+":"+scriptJoinRole] = false },
		"already held, e.g. restored by another bot": func(ops *fakeOps, _ *fakeStore, _ *fakeScripts) {
			ops.setMember(jrGuild, jrUser, []string{jrRole})
		},
		"jailed": func(_ *fakeOps, st *fakeStore, _ *fakeScripts) {
			_ = st.InsertJail(context.Background(), JailRecord{GuildID: jrGuild, UserID: jrUser, JailRoleID: "jail"})
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			p, ops, st, sc := newJoinRoleFixture(t)
			setup(ops, st, sc)
			p.giveJoinRole(context.Background(), jrGuild, jrUser)
			if len(ops.roleAddCalls) != 0 {
				t.Fatalf("role added: %v", ops.roleAddCalls)
			}
		})
	}
}

func TestJoinRoleDoesNothingWithoutAMemberRole(t *testing.T) {
	p, ops, _, sc := newJoinRoleFixture(t)
	sc.on["7000117:"+scriptJoinRole] = true
	ops.setMember("7000117", jrUser, nil)
	p.giveJoinRole(context.Background(), "7000117", jrUser)
	if len(ops.roleAddCalls) != 0 {
		t.Fatalf("role added with no member role configured: %v", ops.roleAddCalls)
	}
}

func TestJoinRoleGivesEveryJoinRoleTheMemberLacks(t *testing.T) {
	p, ops, _, _ := newJoinRoleFixture(t)
	_ = p.jailChannelConfig.AddJoinRole(context.Background(), jrGuild, "7000311")
	_ = p.jailChannelConfig.AddJoinRole(context.Background(), jrGuild, "7000314")
	_ = p.jailChannelConfig.AddJoinRole(context.Background(), jrGuild, jrRole) // already the member role
	ops.setMember(jrGuild, jrUser, []string{"7000314"})

	p.giveJoinRole(context.Background(), jrGuild, jrUser)
	want := []string{jrGuild + ":" + jrUser + ":" + jrRole, jrGuild + ":" + jrUser + ":7000311"}
	if !slices.Equal(ops.roleAddCalls, want) {
		t.Fatalf("roles added = %v, want %v", ops.roleAddCalls, want)
	}
}

// The notices: a join role edited or deleted in Discord is reported, a
// reorder is not, and nothing outside the join roles is watched at all.
func TestJoinRoleEditsAndDeletionsAreReported(t *testing.T) {
	ops, store, cfg, audit, perms := newFakeOps(), newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms()
	_ = cfg.SetMemberRole(context.Background(), jrGuild, jrRole)
	_ = cfg.AddJoinRole(context.Background(), jrGuild, "7000311")
	p := newTestPlugin(ops, store, cfg, audit, perms, newFakeScheduler())
	media := &frole{ID: "7000311", Name: "media", Position: 3,
		Permissions: discord.PermissionAttachFiles | discord.PermissionEmbedLinks}
	ops.roles[jrGuild] = []*frole{{ID: jrRole, Name: "7000321"}, media, {ID: "7000312", Name: "7000340"}}
	p.watchJoinRoles(jrGuild)

	actions := func() []string {
		var out []string
		for _, r := range audit.records {
			out = append(out, r.action)
		}
		return out
	}
	ctx := context.Background()

	moved := *media
	moved.Position = 9
	p.HandleRoleUpdated(ctx, jrGuild, moved.disgo())
	p.HandleRoleUpdated(ctx, jrGuild, discord.Role{ID: 7000312, Name: "renamed"})
	if got := actions(); len(got) != 0 {
		t.Fatalf("a reorder or an unwatched role was reported: %v", got)
	}

	stripped := moved
	stripped.Permissions = discord.PermissionEmbedLinks
	p.HandleRoleUpdated(ctx, jrGuild, stripped.disgo())
	if got := actions(); !slices.Equal(got, []string{"roles.join_role_changed"}) {
		t.Fatalf("actions after a permission edit = %v", got)
	}
	if d := audit.records[0].newValue; !strings.Contains(d, "Lost: Attach Files.") {
		t.Fatalf("edit notice does not name the lost permission: %q", d)
	}

	perms.unmanageable["7000311"] = true
	p.HandleRoleUpdated(ctx, jrGuild, stripped.disgo())
	if d := audit.records[1].newValue; !strings.Contains(d, "no longer give it out") {
		t.Fatalf("moving the role out of reach was not reported: %q", d)
	}

	p.HandleRoleDeleted(ctx, jrGuild, "7000311")
	if got := actions(); got[len(got)-1] != "roles.join_role_deleted" || !strings.Contains(audit.records[len(got)-1].newValue, "**media**") {
		t.Fatalf("deletion not reported by name: %v / %q", got, audit.records[len(got)-1].newValue)
	}
}

func TestPermNamesCountsWhatItCannotName(t *testing.T) {
	got := permNames(discord.PermissionAttachFiles | discord.PermissionBanMembers | discord.PermissionKickMembers)
	if got != "Attach Files, 2 other permissions" {
		t.Fatalf("permNames = %q", got)
	}
}

func TestJoinRoleConfigureAddAndRemove(t *testing.T) {
	ops, cfg, audit, perms := newFakeOps(), newFakeSettings(), newFakeAudit(), newFakePerms()
	p := newTestPlugin(ops, newFakeStore(), cfg, audit, perms, newFakeScheduler())
	ops.roles["7000001"] = []*frole{{ID: "7000311", Name: "media"}}
	perms.unmanageable["7000313"] = true
	rt := handlerSession(t)

	set(p, panelJail+"join", nil, "7000313")
	if len(cfg.JoinRoleIDs("7000001")) != 0 || !rt.said("merlin cannot give") {
		t.Fatalf("a role out of reach was added: %v / %v", cfg.JoinRoleIDs("7000001"), rt.bodies)
	}
	set(p, panelJail+"join", nil, "7000001")
	if len(cfg.JoinRoleIDs("7000001")) != 0 {
		t.Fatalf("@everyone was added: %v", cfg.JoinRoleIDs("7000001"))
	}

	set(p, panelJail+"join", nil, "7000311")
	if !slices.Equal(cfg.JoinRoleIDs("7000001"), []string{"7000311"}) || !rt.said("new members get <@&7000311>") {
		t.Fatalf("add: %v / %v", cfg.JoinRoleIDs("7000001"), rt.bodies)
	}
	if _, watched := p.joinRoleSeen["7000001:7000311"]; !watched {
		t.Fatal("an added join role must be watched from then on")
	}

	set(p, panelJail+"join", []string{"7000311"})
	if len(cfg.JoinRoleIDs("7000001")) != 0 || !rt.said("no join roles configured") {
		t.Fatalf("remove: %v / %v", cfg.JoinRoleIDs("7000001"), rt.bodies)
	}
	if got := auditActions(audit); !slices.Equal(got, []string{"roles.configure_jail_channels", "roles.configure_jail_channels"}) {
		t.Fatalf("audit: %v", got)
	}
}
