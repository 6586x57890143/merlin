package roles

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/disgoorg/disgo/discord"
)

func TestCategoryPropagationCannotUndoChildAccess(t *testing.T) {
	ops, settings := newFakeOps(), newFakeSettings()
	ops.propagateCategories = true
	settings.conversation["7000001"] = []string{"7000601"}
	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())
	// Parent and child start synchronized with exactly the child's desired
	// permissions. A diff made before the category write would skip the child.
	allow, deny := jailOverwriteFor(discord.ChannelTypeGuildText, accessConversation)
	for _, id := range []string{"7000600", "7000601"} {
		ops.channel[id] = &fch{ID: id, GuildID: "7000001", Type: discord.ChannelTypeGuildText,
			PermissionOverwrites: []*fow{{ID: "7000301", Type: discord.PermissionOverwriteTypeRole, Allow: allow, Deny: deny}}}
	}
	ops.channel["7000600"].Type = discord.ChannelTypeGuildCategory
	ops.channel["7000601"].ParentID = "7000600"
	if err := p.syncAllJailChannelOverwrites("7000001", "7000301"); err != nil {
		t.Fatal(err)
	}
	ow := findOverwrite(ops.channel["7000601"].Disgo(), "7000301", discord.PermissionOverwriteTypeRole)
	if ow == nil || ow.Allow != allow || ow.Deny != deny {
		t.Fatalf("child lost conversation access after propagation: %+v", ow)
	}
	if ops.permSetCalls != 2 {
		t.Fatalf("category and child should both be reconciled: %d", ops.permSetCalls)
	}
	if err := p.syncAllJailChannelOverwrites("7000001", "7000301"); err != nil {
		t.Fatal(err)
	}
	if ops.permSetCalls != 2 {
		t.Fatal("repeat sync rewrote unchanged permissions")
	}
}

func TestMemberOverwriteCleanupRequiresLatestBotProvenance(t *testing.T) {
	for _, mode := range []string{"unknown", "moderator", "moderator_after_bot", "bot", "unreadable"} {
		t.Run(mode, func(t *testing.T) {
			ops := newFakeOps()
			ops.channel["7000601"] = lockedRoom("7000601", discord.ChannelTypeGuildText, memberDeny("7000102"))
			p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
			recordOwnedOverwrite(p, ops, "7000601", "7000102")
			switch mode {
			case "unknown":
				ops.auditLog = nil
			case "moderator":
				ops.auditLog[0].UserID = core.ID("7000199")
			case "moderator_after_bot":
				event := ops.auditLog[0]
				event.ID++
				event.UserID = core.ID("7000199")
				event.ActionType = discord.AuditLogEventChannelOverwriteUpdate
				ops.auditLog = append(ops.auditLog, event)
			case "unreadable":
				ops.auditLogErr = errors.New("missing View Audit Log")
			}
			_, err := p.clearOwnedMemberOverwrites(context.Background(), "7000001", "7000102")
			if (err != nil) != (mode == "unreadable") {
				t.Fatalf("cleanup error: %v", err)
			}
			kept := hasOverwrite(ops.channel["7000601"], "7000102", discord.PermissionOverwriteTypeMember)
			if kept != (mode != "bot") {
				t.Fatalf("unexpected cleanup for %s: kept=%v", mode, kept)
			}
		})
	}
}

func TestSettingsSyncFailureIsReportedAndRetried(t *testing.T) {
	p, ops, _, _, _, settings := vacationFixture()
	ops.channel["7000601"] = &fch{ID: "7000601", GuildID: "7000001", Type: discord.ChannelTypeGuildText}
	ops.permWriteErr = errors.New("missing Manage Roles")
	rt := handlerSession(t)
	set(p, panelJail+"vacconversation", nil, "7000601")
	if !slices.Contains(settings.vacationConversation["7000001"], "7000601") || !rt.said("was not synced") || !rt.said("sweep will retry") || rt.said("Channel allowed") {
		t.Fatal("failed sync must persist intent and report unapplied access")
	}
	ops.permWriteErr = nil
	if err := p.sweep(context.Background(), "7000001"); err != nil {
		t.Fatal(err)
	}
	bits := discord.PermissionViewChannel | discord.PermissionReadMessageHistory | discord.PermissionSendMessages
	ow := findOverwrite(ops.channel["7000601"].Disgo(), island, discord.PermissionOverwriteTypeRole)
	if ow == nil || ow.Allow&bits != bits || ow.Deny&bits != 0 {
		t.Fatalf("sweep did not repair saved access: %+v", ow)
	}
	if p.visibilityPending["7000001"] != 0 || !p.visibilityReady["7000001"] {
		t.Fatal("successful retry should clear pending work")
	}
}

func TestInitialMarkerSyncFailureCannotClaimSuccess(t *testing.T) {
	ops := newFakeOps()
	ops.channel["7000601"] = &fch{ID: "7000601", GuildID: "7000001", Type: discord.ChannelTypeGuildText}
	ops.permWriteErr = errors.New("missing Manage Roles")
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	if _, err := p.resolveJailRole("7000001"); err == nil {
		t.Fatal("failed initial restrictions reported a usable marker")
	}
	ops.permWriteErr = nil
	if err := p.reconcileVisibility("7000001", false); err != nil {
		t.Fatal(err)
	}
	if len(ops.roles["7000001"]) != 1 {
		t.Fatal("retry must reuse the created marker")
	}
	ow := findOverwrite(ops.channel["7000601"].Disgo(), ops.roles["7000001"][0].ID, discord.PermissionOverwriteTypeRole)
	if ow == nil || ow.Deny&discord.PermissionViewChannel == 0 {
		t.Fatal("startup/sweep must finish failed creation")
	}
}

func TestStartupMigratesLegacySendAllowToReadOnly(t *testing.T) {
	ops, settings := newFakeOps(), newFakeSettings()
	settings.markerRole["7000001"] = "7000301"
	settings.allowed["7000001"] = []string{"7000601"}
	ops.roles["7000001"] = []*frole{{ID: "7000301"}}
	ops.channel["7000601"] = lockedRoom("7000601", discord.ChannelTypeGuildText,
		&fow{ID: "7000301", Type: discord.PermissionOverwriteTypeRole, Allow: discord.PermissionViewChannel | discord.PermissionSendMessages, Deny: discord.PermissionReadMessageHistory})
	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())
	if err := p.sweep(context.Background(), "7000001"); err != nil {
		t.Fatal(err)
	}
	ow := findOverwrite(ops.channel["7000601"].Disgo(), "7000301", discord.PermissionOverwriteTypeRole)
	bits := discord.PermissionViewChannel | discord.PermissionReadMessageHistory
	if ow.Allow&bits != bits || ow.Allow&discord.PermissionSendMessages != 0 || ow.Deny&discord.PermissionSendMessages == 0 {
		t.Fatalf("legacy mode not migrated: %+v", ow)
	}
	writes := ops.permSetCalls
	if err := p.sweep(context.Background(), "7000001"); err != nil {
		t.Fatal(err)
	}
	if ops.permSetCalls != writes {
		t.Fatal("healthy sweeps must not repeat the migration")
	}
}

func TestExpiredAndRevokedGrantsNeverReturnFromJailSnapshot(t *testing.T) {
	for _, revokeFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "release_before_expiry_sweep", true: "revoke_before_release"}[revokeFirst], func(t *testing.T) {
			ops, store := newFakeOps(), newFakeStore()
			ops.setMember("7000001", "7000102", []string{"7000301"})
			ops.roles["7000001"] = []*frole{{ID: "7000301"}, {ID: "7000302"}, {ID: "7000303"}}
			p, _, _ := newTimedPlugin(ops, store)
			rec := JailRecord{GuildID: "7000001", UserID: "7000102", JailRoleID: "7000301", SnapshotRoleIDs: []string{"7000302", "7000303"}}
			ctx := context.Background()
			_ = store.InsertJail(ctx, rec)
			expires := fixedNow.Add(-time.Minute)
			if revokeFirst {
				expires = fixedNow.Add(time.Hour)
			} // Explicit revoke of a still-valid grant also prunes the snapshot.
			_ = store.InsertGrant(ctx, GrantRecord{GuildID: "7000001", UserID: "7000102", RoleID: "7000303", ExpiresAt: &expires})
			if revokeFirst {
				if err := p.revokeGrant(ctx, "7000001", "7000102", "7000303", core.ActorSystem); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.releaseJail(ctx, "7000001", "7000102", rec, core.ActorSystem); err != nil {
				t.Fatal(err)
			}
			m, _ := ops.GuildMember("7000001", "7000102")
			if !slices.Equal(m.Roles, []string{"7000302"}) {
				t.Fatalf("revoked/expired grant restored: %v", m.Roles)
			}
		})
	}
}

func TestReleaseRereadsExtensionUnderMemberClaim(t *testing.T) {
	ops, store := newFakeOps(), newFakeStore()
	ops.setMember("7000001", "7000102", []string{"7000301"})
	p, _, _ := newTimedPlugin(ops, store)
	rec := JailRecord{GuildID: "7000001", UserID: "7000102", JailRoleID: "7000301", ReleaseAt: ptrTime(fixedNow)}
	ctx := context.Background()
	_ = store.InsertJail(ctx, rec)
	later := fixedNow.Add(time.Hour)
	if err := p.redateJail(ctx, "7000001", "7000102", &later, false); err != nil {
		t.Fatal(err)
	}
	if err := p.releaseJailChecked(ctx, "7000001", "7000102", rec, core.ActorSystem, true); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.GetJail(ctx, "7000001", "7000102"); !ok || len(ops.memberEditCalls["7000102"]) != 0 {
		t.Fatal("stale release overrode extension")
	}
	if !p.claim(jailKey("7000001", "7000102")) {
		t.Fatal("unexpected active claim")
	}
	defer p.unclaim(jailKey("7000001", "7000102"))
	if err := p.redateJail(ctx, "7000001", "7000102", nil, false); !errors.Is(err, errReleaseInProgress) {
		t.Fatalf("extension must not acknowledge success during release: %v", err)
	}
}

func TestGrantFailuresRetainRecoverableState(t *testing.T) {
	for _, failure := range []string{"persistence", "discord"} {
		t.Run(failure, func(t *testing.T) {
			ops, store := newFakeOps(), newFakeStore()
			ops.setMember("7000001", "7000102", nil)
			p, timers, _ := newTimedPlugin(ops, store)
			if failure == "persistence" {
				store.insertGrantErr = errors.New("database down")
			} else {
				ops.roleAddErr = errors.New("ambiguous API failure")
			}
			rt := handlerSession(t)
			p.handleGrant(context.Background(), rolesInteraction("", "grant", userArg("user", "7000102"), roleArg("role", "7000302"), strArg("duration", "1h")))
			if !deferredFirst(rt) {
				t.Fatal("grant must defer before external writes")
			}
			_, tracked, _ := store.GetGrant(context.Background(), "7000001", "7000102", "7000302")
			if failure == "persistence" {
				if len(ops.roleAddCalls) != 0 || tracked || !rt.said("Failed to save grant record") {
					t.Fatal("failed persistence must prevent Discord mutation")
				}
			} else if !tracked || len(timers.armed) != 1 || !rt.said("tracking retained") {
				t.Fatal("ambiguous API failure lost expiry state")
			}
		})
	}
}

func TestEternalEmojiFallbackConverges(t *testing.T) {
	f := newEternalFixture(t)
	rec := f.copyRec(t)
	rec.Icon, rec.IconHash, rec.UnicodeEmoji = nil, "", "🐦"
	if err := f.store.PutEternalRole(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	f.ops.deleteRole(etGuild, etRole)
	f.ops.roleCreateErr = true
	f.enforce(t)
	first := f.copyRec(t)
	if first.UnicodeEmoji != "" {
		t.Fatal("fallback must store the effective emoji")
	}
	count := len(f.ops.roles[etGuild])
	f.enforce(t)
	if f.copyRec(t).RoleID != first.RoleID || len(f.ops.roles[etGuild]) != count {
		t.Fatal("fallback recreated a matching role again")
	}
}

func TestJailDoesNotSnapshotARevokedGrantFromStaleTarget(t *testing.T) {
	ops, store := newFakeOps(), newFakeStore()
	ops.setMember("7000001", "7000102", []string{"7000302"})
	p, _, _ := newTimedPlugin(ops, store)
	// A grant was revoked after target resolution, before the jail claim.
	target := jailTarget{userID: "7000102", roles: []string{"7000302", "7000303"}}
	if _, err := p.applyJail(context.Background(), "7000001", "7000301", target, time.Hour, core.ActorSystem, ""); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := store.GetJail(context.Background(), "7000001", "7000102")
	if !slices.Equal(rec.SnapshotRoleIDs, []string{"7000302"}) {
		t.Fatalf("captured revoked grant from stale target: %v", rec.SnapshotRoleIDs)
	}
}

func TestQueuedSyncUsesLatestChannelModes(t *testing.T) {
	ops, settings := newFakeOps(), newFakeSettings()
	ops.channel["7000601"] = &fch{ID: "7000601", GuildID: "7000001", Type: discord.ChannelTypeGuildText}
	settings.conversation["7000001"] = []string{"7000601"}
	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())
	queued := p.jailMarker("7000001", "7000301")
	// The caller captured this marker before waiting for another sync;
	// a later edit moved the channel to read-only while it waited.
	_ = settings.AddJailAllowedChannel(context.Background(), "7000001", "7000601")
	if err := p.syncAllVisibility("7000001", queued); err != nil {
		t.Fatal(err)
	}
	ow := findOverwrite(ops.channel["7000601"].Disgo(), "7000301", discord.PermissionOverwriteTypeRole)
	if ow.Allow&discord.PermissionSendMessages != 0 || ow.Deny&discord.PermissionSendMessages == 0 {
		t.Fatal("queued sync restored outdated conversation access")
	}
}
