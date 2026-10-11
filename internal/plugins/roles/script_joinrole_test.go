package roles

import (
	"context"
	"slices"
	"testing"
	"time"
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
