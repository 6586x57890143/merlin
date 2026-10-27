package roles

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/disgoorg/disgo/discord"
)

type interleavedChannelConfig struct {
	JailChannelConfig
	addReadOnly func(context.Context, string, string) error
	marker      func(context.Context, string, string) error
	vacation    func(context.Context, string, string) error
}

func (c *interleavedChannelConfig) AddJailAllowedChannel(ctx context.Context, g, ch string) error {
	return c.addReadOnly(ctx, g, ch)
}
func (c *interleavedChannelConfig) SetJailMarkerRole(ctx context.Context, g, id string) error {
	return c.marker(ctx, g, id)
}
func (c *interleavedChannelConfig) SetVacationRole(ctx context.Context, g, id string) error {
	return c.vacation(ctx, g, id)
}

func TestSettingsSaveCannotLoseRetryToConcurrentSweep(t *testing.T) {
	p, ops, _, _, _, settings := handlerFixture()
	settings.markerRole["7000001"] = "7000301"
	settings.conversation["7000001"] = []string{"7000601"}
	ops.channel["7000601"] = &fch{ID: "7000601", GuildID: "7000001", Type: discord.ChannelTypeGuildText}
	p.jailChannelConfig = &interleavedChannelConfig{JailChannelConfig: settings, addReadOnly: func(ctx context.Context, g, ch string) error {
		// The sweep finishes while the DB write is still in flight.
		if err := p.reconcileVisibility(g, false); err != nil {
			t.Fatal(err)
		}
		if err := settings.AddJailAllowedChannel(ctx, g, ch); err != nil {
			t.Fatal(err)
		}
		return errors.New("write committed but cache refresh failed")
	}}
	handlerSession(t)
	set(p, panelJail+"allow", nil, "7000601")
	if p.visibilityPending["7000001"] == 0 {
		t.Fatal("sweep consumed retry before the settings write finished")
	}
	if err := p.reconcileVisibility("7000001", false); err != nil {
		t.Fatal(err)
	}
	ow := findOverwrite(ops.channel["7000601"].Disgo(), "7000301", discord.PermissionOverwriteTypeRole)
	if ow.Allow&discord.PermissionSendMessages != 0 || ow.Deny&discord.PermissionSendMessages == 0 {
		t.Fatal("saved read-only mode was not retried")
	}
}

func TestVacationReconcilesEvenWhenJailMarkerIsMissing(t *testing.T) {
	p, ops, _, _, _, settings := vacationFixture()
	settings.markerRole["7000001"] = "7999999"
	settings.vacationAllowed["7000001"] = []string{"7000601"}
	ops.channel["7000601"] = lockedRoom("7000601", discord.ChannelTypeGuildText, &fow{ID: island, Type: discord.PermissionOverwriteTypeRole, Allow: discord.PermissionSendMessages | discord.PermissionViewChannel})
	if err := p.reconcileVisibility("7000001", false); err == nil {
		t.Fatal("missing jail marker must still be reported")
	}
	ow := findOverwrite(ops.channel["7000601"].Disgo(), island, discord.PermissionOverwriteTypeRole)
	if ow.Allow&discord.PermissionSendMessages != 0 || ow.Deny&discord.PermissionSendMessages == 0 || ow.Allow&discord.PermissionReadMessageHistory == 0 {
		t.Fatal("jail failure blocked vacation migration")
	}
}

func TestVacationRefusesUnreconciledMarker(t *testing.T) {
	for _, cached := range []bool{false, true} {
		p, ops, _, _, _, _ := vacationFixture()
		if cached {
			p.vacationRoleID["7000001"] = island
			p.visibilityReady = map[string]bool{"7000001": true}
			p.markVisibilityDirty("7000001")
		}
		ops.channel["7000601"] = &fch{ID: "7000601", GuildID: "7000001", Type: discord.ChannelTypeGuildText}
		ops.permWriteErr = errors.New("permission denied")
		if _, err := p.resolveVacationRole("7000001"); err == nil {
			t.Fatal("vacation resolved a partially unrestricted marker")
		}
		ops.permWriteErr = nil
		if _, err := p.resolveVacationRole("7000001"); err != nil {
			t.Fatal(err)
		}
		if ow := ops.overwrites[overwriteKey{"7000601", island}]; ow.deny&discord.PermissionViewChannel == 0 {
			t.Fatal("vacation retry did not establish restrictions")
		}
	}
}

func TestAmbiguousMarkerSaveInvalidatesCachedRole(t *testing.T) {
	for _, vacation := range []bool{false, true} {
		p, ops, _, _, _, settings := vacationFixture()
		ops.roles["7000001"] = append(ops.roles["7000001"], &frole{ID: "7000399"})
		p.jailRoleID["7000001"], p.vacationRoleID["7000001"] = "7000301", island
		cfg := &interleavedChannelConfig{JailChannelConfig: settings}
		cfg.marker = func(ctx context.Context, g, id string) error {
			_ = settings.SetJailMarkerRole(ctx, g, id)
			return errors.New("refresh failed")
		}
		cfg.vacation = func(ctx context.Context, g, id string) error {
			_ = settings.SetVacationRole(ctx, g, id)
			return errors.New("refresh failed")
		}
		p.jailChannelConfig = cfg
		ix := rolesInteraction("", "settings")
		if vacation {
			p.setVacationRole(context.Background(), ix, "7000399")
			if id, err := p.resolveVacationRole("7000001"); err != nil || id != "7000399" {
				t.Fatalf("old vacation marker survived committed save: %s %v", id, err)
			}
		} else {
			p.setMarkerRole(context.Background(), ix, "7000399")
			if id, err := p.existingJailRole("7000001"); err != nil || id != "7000399" {
				t.Fatalf("old jail marker survived committed save: %s %v", id, err)
			}
		}
	}
}

func TestStaleConfinementWorkCannotUndoCompletedRelease(t *testing.T) {
	p, ops, store, _, _, _ := vacationFixture()
	ops.setMember("7000001", "7000102", []string{"7000301"})
	rec := activeJail("7000102", []string{"7000302"})
	ctx := context.Background()
	_ = store.InsertJail(ctx, rec)
	if err := p.releaseJail(ctx, "7000001", "7000102", rec, core.ActorSystem); err != nil {
		t.Fatal(err)
	}
	edits := len(ops.memberEditCalls["7000102"])
	if err := p.reapplyIfEvaded(ctx, "7000001", rec); err != nil {
		t.Fatal(err)
	}
	p.HandleMemberUpdate(ctx, "7000001", "7000102", []string{"7000301", "7000303"})
	if err := p.transferJail(ctx, "7000001", jailTarget{userID: "7000102", roles: []string{"7000301"}}, rec, island, rec.ReleaseAt); !errors.Is(err, ErrNotJailed) {
		t.Fatalf("stale transfer: %v", err)
	}
	if len(ops.memberEditCalls["7000102"]) != edits {
		t.Fatal("stale confinement work edited a released member")
	}
}

func TestHandTransferKeepsLatestDeadlineAndIgnoresStaleGatewayRoles(t *testing.T) {
	p, ops, store, _, _, _ := vacationFixture()
	ctx := context.Background()
	rec := activeJail("7000102", []string{"7000302"})
	_ = store.InsertJail(ctx, rec)
	later := fixedNow.Add(4 * time.Hour)
	_ = store.SetJailRelease(ctx, "7000001", "7000102", &later)
	ops.setMember("7000001", "7000102", []string{island})
	if err := p.reapplyIfEvaded(ctx, "7000001", rec); err != nil {
		t.Fatal(err)
	}
	current, _, _ := store.GetJail(ctx, "7000001", "7000102")
	if current.JailRoleID != island || !current.ReleaseAt.Equal(later) {
		t.Fatal("stale transfer lost sentence extension")
	}
	p.HandleMemberUpdate(ctx, "7000001", "7000102", []string{"7000301"})
	current, _, _ = store.GetJail(ctx, "7000001", "7000102")
	if current.JailRoleID != island {
		t.Fatal("stale gateway event reversed a completed transfer")
	}
}

func TestEternalRoleYieldsToMemberClaim(t *testing.T) {
	f := newEternalFixture(t)
	f.ops.setMember(etGuild, etUser, nil)
	if !f.p.claim(jailKey(etGuild, etUser)) {
		t.Fatal("unexpected busy claim")
	}
	f.enforce(t)
	f.p.unclaim(jailKey(etGuild, etUser))
	if len(f.memberRoles()) != 0 {
		t.Fatal("eternal script bypassed in-flight member mutation")
	}
	f.enforce(t)
	if len(f.memberRoles()) == 0 {
		t.Fatal("eternal script did not resume after claim released")
	}
}

func TestAutomaticTransferReturnsPreservedDeadline(t *testing.T) {
	for _, indefinite := range []bool{false, true} {
		p, ops, store, _, _, _ := vacationFixture()
		ctx := context.Background()
		rec := activeJail("7000102", []string{"7000302"})
		rec.JailRoleID = island
		if indefinite {
			rec.ReleaseAt = nil
		} else {
			rec.ReleaseAt = ptrTime(fixedNow.Add(4 * time.Hour))
		}
		_ = store.InsertJail(ctx, rec)
		ops.setMember("7000001", "7000102", []string{island})
		end, err := p.transferJailWithDeadline(ctx, "7000001", jailTarget{userID: "7000102", roles: []string{island}}, "7000301", ptrTime(fixedNow.Add(time.Hour)), true)
		if err != nil {
			t.Fatal(err)
		}
		if (end == nil) != indefinite || (end != nil && !end.Equal(*rec.ReleaseAt)) {
			t.Fatalf("transfer announced a different deadline: %v", end)
		}
		current, _, _ := store.GetJail(ctx, "7000001", "7000102")
		if current.JailRoleID != "7000301" || (current.ReleaseAt == nil) != indefinite || (end != nil && !current.ReleaseAt.Equal(*end)) {
			t.Fatal("returned deadline differs from stored sentence")
		}
	}
}

func TestJoinRoleYieldsToMemberClaim(t *testing.T) {
	p, ops, _, _ := newJoinRoleFixture(t)
	if !p.claim(jailKey(jrGuild, jrUser)) {
		t.Fatal("unexpected active claim")
	}
	p.giveJoinRole(context.Background(), jrGuild, jrUser)
	p.unclaim(jailKey(jrGuild, jrUser))
	if hasRole(ops) {
		t.Fatal("join script bypassed in-flight member mutation")
	}
	p.giveJoinRole(context.Background(), jrGuild, jrUser)
	if !hasRole(ops) {
		t.Fatal("join role did not resume after claim released")
	}
}

func TestColdMeltingPotSyncFindsNamedFallbackWithoutPreferredRole(t *testing.T) {
	ops, settings := newFakeOps(), newFakeSettings()
	const role, channel = "7000398", "7000601"
	ops.roles[meltingPotGuildID] = []*frole{{ID: role, Name: jailRoleName}}
	settings.allowed[meltingPotGuildID] = []string{channel}
	ops.channel[channel] = &fch{ID: channel, GuildID: meltingPotGuildID, Type: discord.ChannelTypeGuildText,
		PermissionOverwrites: []*fow{{ID: role, Type: discord.PermissionOverwriteTypeRole, Allow: discord.PermissionViewChannel | discord.PermissionSendMessages}}}
	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())
	if id, err := p.existingJailRole(meltingPotGuildID); err != nil || id != role {
		t.Fatalf("cold fallback: %s %v", id, err)
	}
	if _, err := p.syncJailOne(meltingPotGuildID, channel); err != nil {
		t.Fatal(err)
	}
	ow := findOverwrite(ops.channel[channel].Disgo(), role, discord.PermissionOverwriteTypeRole)
	if ow.Allow&discord.PermissionSendMessages != 0 || ow.Deny&discord.PermissionSendMessages == 0 || ow.Allow&discord.PermissionReadMessageHistory == 0 {
		t.Fatal("cold fallback marker was not migrated to read-only")
	}
}
