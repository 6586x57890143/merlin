package roles

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	jrGuild = "g-joinrole"
	jrUser  = "u-joinrole"
	jrRole  = "r-joinrole"
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
	sc.on["elsewhere:"+scriptJoinRole] = true
	ops.setMember("elsewhere", jrUser, nil)
	p.giveJoinRole(context.Background(), "elsewhere", jrUser)
	if len(ops.roleAddCalls) != 0 {
		t.Fatalf("role added with no member role configured: %v", ops.roleAddCalls)
	}
}

func TestJoinRoleGivesEveryJoinRoleTheMemberLacks(t *testing.T) {
	p, ops, _, _ := newJoinRoleFixture(t)
	_ = p.jailChannelConfig.AddJoinRole(context.Background(), jrGuild, "r-media")
	_ = p.jailChannelConfig.AddJoinRole(context.Background(), jrGuild, "r-held")
	_ = p.jailChannelConfig.AddJoinRole(context.Background(), jrGuild, jrRole) // already the member role
	ops.setMember(jrGuild, jrUser, []string{"r-held"})

	p.giveJoinRole(context.Background(), jrGuild, jrUser)
	want := []string{jrGuild + ":" + jrUser + ":" + jrRole, jrGuild + ":" + jrUser + ":r-media"}
	if !slices.Equal(ops.roleAddCalls, want) {
		t.Fatalf("roles added = %v, want %v", ops.roleAddCalls, want)
	}
}

// The notices: a join role edited or deleted in Discord is reported, a
// reorder is not, and nothing outside the join roles is watched at all.
func TestJoinRoleEditsAndDeletionsAreReported(t *testing.T) {
	ops, store, cfg, audit, perms := newFakeOps(), newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms()
	_ = cfg.SetMemberRole(context.Background(), jrGuild, jrRole)
	_ = cfg.AddJoinRole(context.Background(), jrGuild, "r-media")
	p := newTestPlugin(ops, store, cfg, audit, perms, newFakeScheduler())
	media := &discordgo.Role{ID: "r-media", Name: "media", Position: 3,
		Permissions: discordgo.PermissionAttachFiles | discordgo.PermissionEmbedLinks}
	ops.roles[jrGuild] = []*discordgo.Role{{ID: jrRole, Name: "melted"}, media, {ID: "r-other", Name: "other"}}
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
	p.HandleRoleUpdated(ctx, jrGuild, &moved)
	p.HandleRoleUpdated(ctx, jrGuild, &discordgo.Role{ID: "r-other", Name: "renamed"})
	if got := actions(); len(got) != 0 {
		t.Fatalf("a reorder or an unwatched role was reported: %v", got)
	}

	stripped := moved
	stripped.Permissions = discordgo.PermissionEmbedLinks
	p.HandleRoleUpdated(ctx, jrGuild, &stripped)
	if got := actions(); !slices.Equal(got, []string{"roles.join_role_changed"}) {
		t.Fatalf("actions after a permission edit = %v", got)
	}
	if d := audit.records[0].newValue; !strings.Contains(d, "Lost: Attach Files.") {
		t.Fatalf("edit notice does not name the lost permission: %q", d)
	}

	perms.unmanageable["r-media"] = true
	p.HandleRoleUpdated(ctx, jrGuild, &stripped)
	if d := audit.records[1].newValue; !strings.Contains(d, "no longer give it out") {
		t.Fatalf("moving the role out of reach was not reported: %q", d)
	}

	p.HandleRoleDeleted(ctx, jrGuild, "r-media")
	if got := actions(); got[len(got)-1] != "roles.join_role_deleted" || !strings.Contains(audit.records[len(got)-1].newValue, "**media**") {
		t.Fatalf("deletion not reported by name: %v / %q", got, audit.records[len(got)-1].newValue)
	}
}

func TestPermNamesCountsWhatItCannotName(t *testing.T) {
	got := permNames(discordgo.PermissionAttachFiles | discordgo.PermissionBanMembers | discordgo.PermissionKickMembers)
	if got != "Attach Files, 2 other permissions" {
		t.Fatalf("permNames = %q", got)
	}
}

func TestJoinRoleConfigureAddAndRemove(t *testing.T) {
	ops, cfg, audit, perms := newFakeOps(), newFakeSettings(), newFakeAudit(), newFakePerms()
	p := newTestPlugin(ops, newFakeStore(), cfg, audit, perms, newFakeScheduler())
	ops.roles["g1"] = []*discordgo.Role{{ID: "r-media", Name: "media"}}
	perms.unmanageable["r-high"] = true
	s, rt := handlerSession(t)

	set(p, s, panelJail+"join", nil, "r-high")
	if len(cfg.JoinRoleIDs("g1")) != 0 || !rt.said("merlin cannot give") {
		t.Fatalf("a role out of reach was added: %v / %v", cfg.JoinRoleIDs("g1"), rt.bodies)
	}
	set(p, s, panelJail+"join", nil, "g1")
	if len(cfg.JoinRoleIDs("g1")) != 0 {
		t.Fatalf("@everyone was added: %v", cfg.JoinRoleIDs("g1"))
	}

	set(p, s, panelJail+"join", nil, "r-media")
	if !slices.Equal(cfg.JoinRoleIDs("g1"), []string{"r-media"}) || !rt.said("new members get <@&r-media>") {
		t.Fatalf("add: %v / %v", cfg.JoinRoleIDs("g1"), rt.bodies)
	}
	if _, watched := p.joinRoleSeen["g1:r-media"]; !watched {
		t.Fatal("an added join role must be watched from then on")
	}

	set(p, s, panelJail+"join", []string{"r-media"})
	if len(cfg.JoinRoleIDs("g1")) != 0 || !rt.said("no join roles configured") {
		t.Fatalf("remove: %v / %v", cfg.JoinRoleIDs("g1"), rt.bodies)
	}
	if got := auditActions(audit); !slices.Equal(got, []string{"roles.configure_jail_channels", "roles.configure_jail_channels"}) {
		t.Fatalf("audit: %v", got)
	}
}
