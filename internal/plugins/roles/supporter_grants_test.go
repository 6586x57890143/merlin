package roles

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
)

func TestSupporterGrantPersistsBeforeDiscordAndRetriesUnconfirmedAdd(t *testing.T) {
	const guild, user, role = "7000001", "7000102", "7000302"
	ops, store := newFakeOps(), newFakeStore()
	ops.setMember(guild, user, nil)
	p, timers, _ := newTimedPlugin(ops, store)
	ctx := context.Background()
	backer := Backer{UserID: user, Until: fixedNow.Add(time.Hour)}
	store.insertGrantErr = errors.New("database unavailable")
	if _, err := p.syncSupporter(ctx, guild, role, backer); err == nil || len(ops.roleAddCalls) != 0 {
		t.Fatal("failed persistence must prevent adding the supporter role")
	}
	store.insertGrantErr = nil
	ops.roleAddErr = errors.New("API response lost")
	if _, err := p.syncSupporter(ctx, guild, role, backer); err == nil {
		t.Fatal("expected API error")
	}
	grant, tracked, err := store.GetGrant(ctx, guild, user, role)
	if err != nil || !tracked || !grant.ExpiresAt.Equal(backer.Until) || len(timers.armed) != 1 {
		t.Fatalf("unconfirmed grant lost recovery state: %+v %v", grant, err)
	}
	ops.roleAddErr = nil
	if _, err := p.syncSupporter(ctx, guild, role, backer); err != nil {
		t.Fatal(err)
	}
	member, _ := ops.GuildMember(guild, user)
	if !slices.Contains(member.Roles, role) {
		t.Fatal("unchanged backing period prevented retrying a saved grant")
	}
	adds := len(ops.roleAddCalls)
	if _, err := p.syncSupporter(ctx, guild, role, backer); err != nil || len(ops.roleAddCalls) != adds {
		t.Fatal("healthy grant was not idempotent")
	}
}

func TestSupporterRenewalSharesMemberClaimAndSurvivesStaleExpiry(t *testing.T) {
	const guild, user, role = "7000001", "7000102", "7000302"
	ops, store := newFakeOps(), newFakeStore()
	ops.setMember(guild, user, []string{role})
	p, _, _ := newTimedPlugin(ops, store)
	ctx := context.Background()
	expired := fixedNow.Add(-time.Minute)
	_ = store.InsertGrant(ctx, GrantRecord{GuildID: guild, UserID: user, RoleID: role, ExpiresAt: &expired})
	backer := Backer{UserID: user, Until: fixedNow.Add(time.Hour)}
	if !p.claim(jailKey(guild, user)) {
		t.Fatal("unexpected active claim")
	}
	_, err := p.syncSupporter(ctx, guild, role, backer)
	p.unclaim(jailKey(guild, user))
	if !errors.Is(err, errReleaseInProgress) {
		t.Fatalf("supporter update bypassed release/jail claim: %v", err)
	}
	grant, _, _ := store.GetGrant(ctx, guild, user, role)
	if !grant.ExpiresAt.Equal(expired) {
		t.Fatal("concurrent renewal changed the record")
	}
	if _, err := p.syncSupporter(ctx, guild, role, backer); err != nil {
		t.Fatal(err)
	}
	if err := p.revokeGrantChecked(ctx, guild, user, role, core.ActorSystem, true); err != nil {
		t.Fatal(err)
	}
	grant, tracked, _ := store.GetGrant(ctx, guild, user, role)
	if !tracked || !grant.ExpiresAt.Equal(backer.Until) || len(ops.roleRemoveCalls) != 0 {
		t.Fatal("stale expiry removed a renewed supporter grant")
	}
}
