package roles

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/voice"
)

// The vacation script: a jail row whose marker is the island's role. These
// tests pin what differs from jail (the switch, no channel writes, the
// words) and that a move between the two keeps the snapshot and follows
// the marker whichever way it happened.

const island = "7000323"

// vacationFixture is handlerFixture with the island's role in the guild,
// configured, and the script on.
func vacationFixture() (*Plugin, *fakeOps, *fakeStore, *fakeAudit, *fakePerms, *fakeSettings) {
	p, ops, store, audit, perms, settings := handlerFixture()
	ops.roles["7000001"] = append(ops.roles["7000001"], &frole{ID: island, Name: "bahamas"})
	settings.vacationRole["7000001"] = island
	scripts := newFakeScripts()
	scripts.on["7000001:"+scriptVacation] = true
	p.scripts = scripts
	return p, ops, store, audit, perms, settings
}

func vacationInteraction(args ...map[string]any) *core.Ix {
	return rolesInteraction("", "vacation", args...)
}

func sentTo(ops *fakeOps, channelID string) []string {
	var out []string
	for _, m := range ops.dmSends {
		if m.channelID != channelID {
			continue
		}
		text := m.data.Content
		if len(m.data.Embeds) > 0 {
			text = m.data.Embeds[0].Title + " " + m.data.Embeds[0].Description
			for _, f := range m.data.Embeds[0].Fields {
				text += " " + f.Value
			}
		}
		out = append(out, text)
	}
	return out
}

func TestVacationRefusesWhileScriptOff(t *testing.T) {
	p, ops, store, _, _, _ := vacationFixture()
	p.scripts = newFakeScripts()
	rt := handlerSession(t)

	p.handleVacation(context.Background(), vacationInteraction(userArg("user", "7000102"), strArg("duration", "1h")))

	if _, ok, _ := store.GetJail(context.Background(), "7000001", "7000102"); ok || len(ops.memberEditCalls["7000102"]) != 0 {
		t.Fatal("a refusal must record nothing and edit nobody")
	}
	if !rt.said("Vacation is off") {
		t.Fatalf("expected the switch named, got %v", rt.bodies)
	}
}

func TestVacationRefusesWithoutAnIslandRole(t *testing.T) {
	p, ops, store, _, _, settings := vacationFixture()
	settings.vacationRole = map[string]string{}
	rt := handlerSession(t)

	p.handleVacation(context.Background(), vacationInteraction(userArg("user", "7000102"), strArg("duration", "1h")))

	if _, ok, _ := store.GetJail(context.Background(), "7000001", "7000102"); ok || len(ops.memberEditCalls["7000102"]) != 0 {
		t.Fatal("a refusal must record nothing and edit nobody")
	}
	if len(ops.roles["7000001"]) != 4 {
		t.Fatal("the island's role is never created by merlin")
	}
	if !rt.said("No vacation role") {
		t.Fatalf("expected the configure hint, got %v", rt.bodies)
	}
}

// A deleted configured role is cleared rather than reported forever.
func TestVacationClearsADeletedConfiguredRole(t *testing.T) {
	p, ops, _, _, _, settings := vacationFixture()
	ops.deleteRole("7000001", island)
	rt := handlerSession(t)

	p.handleVacation(context.Background(), vacationInteraction(userArg("user", "7000102"), strArg("duration", "1h")))

	if settings.vacationRole["7000001"] != "" || !rt.said("no longer exists") {
		t.Fatalf("expected the dead role cleared and named, got %q / %v", settings.vacationRole["7000001"], rt.bodies)
	}
}

// TestVacationIsAJailOnTheIsland: the record is a jail row with the
// island's marker, the member holds exactly that, no channel overwrite is
// written, and every surface says vacation: audit, DM, announcement, the
// ledger (as a note, never a jail) and the reply.
func TestVacationIsAJailOnTheIsland(t *testing.T) {
	p, ops, store, audit, _, _ := vacationFixture()
	ops.channel["7000601"] = &fch{ID: "7000601", GuildID: "7000001", Type: discord.ChannelTypeGuildText}
	rec := &busRecorder{}
	bus := core.NewEventBus(testLogger())
	rec.record(bus)
	p.bus = bus
	rt := handlerSession(t)

	p.handleVacation(context.Background(), vacationInteraction(userArg("user", "7000102"), strArg("duration", "2h"), strArg("reason", "sun")))

	row, ok, _ := store.GetJail(context.Background(), "7000001", "7000102")
	if !ok || row.JailRoleID != island || !slices.Equal(row.SnapshotRoleIDs, []string{"7000302"}) {
		t.Fatalf("expected a jail row on the island with the snapshot, got %+v", row)
	}
	m, _ := ops.GuildMember("7000001", "7000102")
	if !slices.Equal(m.Roles, []string{island}) {
		t.Fatalf("expected the member stripped to the island's role, got %v", m.Roles)
	}
	if ops.permSetCalls != 0 || ops.permDeleteCalls != 0 {
		t.Fatal("vacation must never write a channel overwrite")
	}
	if got := auditActions(audit); !slices.Equal(got, []string{"roles.vacation"}) {
		t.Fatalf("audit: %v", got)
	}
	if dm := sentTo(ops, "97000102"); len(dm) != 1 || !strings.Contains(dm[0], "Sent on vacation") {
		t.Fatalf("expected a vacation DM, got %v", dm)
	}
	if ann := sentTo(ops, "7000601"); len(ann) != 1 || !strings.Contains(ann[0], "<@7000102>") || !strings.Contains(ann[0], "-# reason: sun") {
		t.Fatalf("expected a channel announcement naming them and the reason, got %v", ann)
	}
	if pl := rec.payloads(); len(pl) != 1 || pl[0].Kind != "note" || !strings.Contains(pl[0].Reason, "vacation") {
		t.Fatalf("the ledger hears a note, never a jail: %+v", pl)
	}
	if !rt.said("Member sent on vacation") {
		t.Fatalf("expected the vacation reply, got %v", rt.bodies)
	}
}

// TestJailOnAVacationerMovesThemToJail: /roles jail on somebody on the
// island is a transfer: the marker swaps, the snapshot stays, the duration
// just given is the new end, and merlin says the trip is over.
func TestJailOnAVacationerMovesThemToJail(t *testing.T) {
	p, ops, store, audit, _, _ := vacationFixture()
	ops.setMember("7000001", "7000102", []string{island})
	end := fixedNow.Add(24 * time.Hour)
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "7000001", UserID: "7000102", JailRoleID: island, SnapshotRoleIDs: []string{"7000302"}, ReleaseAt: &end})
	rt := handlerSession(t)

	p.handleJail(context.Background(), rolesInteraction("", "jail", userArg("user", "7000102"), strArg("duration", "3h"), strArg("reason", "sand everywhere")))

	row, _, _ := store.GetJail(context.Background(), "7000001", "7000102")
	if row.JailRoleID != "7000301" || !row.ReleaseAt.Equal(fixedNow.Add(3*time.Hour)) || !slices.Equal(row.SnapshotRoleIDs, []string{"7000302"}) {
		t.Fatalf("expected the row moved to jail for 3h with the snapshot intact, got %+v", row)
	}
	m, _ := ops.GuildMember("7000001", "7000102")
	if !slices.Equal(m.Roles, []string{"7000301"}) {
		t.Fatalf("expected the island's role swapped for the nest's, got %v", m.Roles)
	}
	if got := auditActions(audit); !slices.Equal(got, []string{"roles.transferred"}) {
		t.Fatalf("audit: %v", got)
	}
	if dm := sentTo(ops, "97000102"); len(dm) != 1 || !strings.Contains(dm[0], "Moved to jail") || !strings.Contains(dm[0], "sand everywhere") {
		t.Fatalf("expected the to-jail DM with the reason, got %v", dm)
	}
	if ann := sentTo(ops, "7000601"); len(ann) != 1 || !strings.Contains(ann[0], "<@7000102>") {
		t.Fatalf("expected the move announced, got %v", ann)
	}
	if !rt.said("Member moved") || !rt.said("moved to jail") {
		t.Fatalf("expected the transfer reply, got %v", rt.bodies)
	}
}

// TestCommandTransferIsAnnouncedOnce: the role edit behind a command
// transfer fires Discord's own GUILD_MEMBER_UPDATE while the row still
// names the old marker, and HandleMemberUpdate must not read that as a mod
// swapping the roles by hand and voice the same move a second time.
func TestCommandTransferIsAnnouncedOnce(t *testing.T) {
	p, ops, store, audit, _, settings := vacationFixture()
	settings.announce["7000001"] = "7000601"
	ops.setMember("7000001", "7000102", []string{island})
	end := fixedNow.Add(24 * time.Hour)
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "7000001", UserID: "7000102", JailRoleID: island, SnapshotRoleIDs: []string{"7000302"}, ReleaseAt: &end})
	ops.onMemberEdit = func(guildID, userID string, roles []string) {
		p.HandleMemberUpdate(context.Background(), guildID, userID, roles)
	}
	handlerSession(t)

	p.handleJail(context.Background(), rolesInteraction("", "jail", userArg("user", "7000102"), strArg("duration", "3h")))

	if got := auditActions(audit); !slices.Equal(got, []string{"roles.transferred"}) {
		t.Fatalf("one move, one audit entry, got %v", got)
	}
	if ann := sentTo(ops, "7000601"); len(ann) != 1 {
		t.Fatalf("one move, one announcement, got %v", ann)
	}
	if dm := sentTo(ops, "97000102"); len(dm) != 1 {
		t.Fatalf("one move, one DM, got %v", dm)
	}
	row, _, _ := store.GetJail(context.Background(), "7000001", "7000102")
	if row.JailRoleID != "7000301" || !row.ReleaseAt.Equal(fixedNow.Add(3*time.Hour)) {
		t.Fatalf("the command's own transfer must still land, got %+v", row)
	}
}

// TestVacationOnAJailedMemberParolesThemAndClearsTheHardening: the reverse
// move, plus the one thing jail leaves behind that the island must not
// inherit: a member-level deny from a hardened jail.
func TestVacationOnAJailedMemberParolesThemAndClearsTheHardening(t *testing.T) {
	p, ops, store, audit, _, _ := vacationFixture()
	recordOwnedOverwrite(p, ops, "7000606", "7000102")
	ops.setMember("7000001", "7000102", []string{"7000301"})
	ops.channel["7000606"] = &fch{ID: "7000606", GuildID: "7000001", Type: discord.ChannelTypeGuildText,
		PermissionOverwrites: []*fow{
			{ID: "7000316", Type: discord.PermissionOverwriteTypeRole, Allow: discord.PermissionViewChannel},
		}}
	if err := setOverwrite(ops, core.ID("7000606"), core.ID("7000102"), discord.PermissionOverwriteTypeMember, 0, discord.PermissionViewChannel); err != nil {
		t.Fatal(err)
	}
	end := fixedNow.Add(time.Hour)
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "7000001", UserID: "7000102", JailRoleID: "7000301", SnapshotRoleIDs: []string{"7000302"}, ReleaseAt: &end})
	rt := handlerSession(t)

	p.handleVacation(context.Background(), vacationInteraction(userArg("user", "7000102"), strArg("duration", "2d")))

	row, _, _ := store.GetJail(context.Background(), "7000001", "7000102")
	if row.JailRoleID != island || !row.ReleaseAt.Equal(fixedNow.Add(48*time.Hour)) || !slices.Equal(row.SnapshotRoleIDs, []string{"7000302"}) {
		t.Fatalf("expected the row moved to the island for 2d, got %+v", row)
	}
	m, _ := ops.GuildMember("7000001", "7000102")
	if !slices.Equal(m.Roles, []string{island}) {
		t.Fatalf("expected the nest's role swapped for the island's, got %v", m.Roles)
	}
	if _, still := ops.overwrites[overwriteKey{"7000606", "7000102"}]; still {
		t.Fatal("the jail's member-level deny must not follow them to the beach")
	}
	if got := auditActions(audit); !slices.Equal(got, []string{"roles.transferred"}) {
		t.Fatalf("audit: %v", got)
	}
	if dm := sentTo(ops, "97000102"); len(dm) != 1 || !strings.Contains(dm[0], "Moved to vacation") {
		t.Fatalf("expected the from-jail DM, got %v", dm)
	}
	if !rt.said("moved to vacation") {
		t.Fatalf("got %v", rt.bodies)
	}
}

// TestSameSentenceStillRedates: vacation on somebody already on vacation
// moves the end, exactly as jail on jail does, and is audited as such.
func TestSameSentenceStillRedates(t *testing.T) {
	p, ops, store, audit, _, _ := vacationFixture()
	ops.setMember("7000001", "7000102", []string{island})
	end := fixedNow.Add(time.Hour)
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "7000001", UserID: "7000102", JailRoleID: island, SnapshotRoleIDs: []string{"7000302"}, ReleaseAt: &end})
	rt := handlerSession(t)

	p.handleVacation(context.Background(), vacationInteraction(userArg("user", "7000102"), strArg("duration", "5h")))

	row, _, _ := store.GetJail(context.Background(), "7000001", "7000102")
	if row.JailRoleID != island || !row.ReleaseAt.Equal(fixedNow.Add(5*time.Hour)) {
		t.Fatalf("expected a re-date on the island, got %+v", row)
	}
	if got := auditActions(audit); !slices.Equal(got, []string{"roles.vacation_resentenced"}) || !rt.said("Sentence updated") {
		t.Fatalf("audit %v, replies %v", got, rt.bodies)
	}
}

// TestHandSwapIsRecognised: a mod who removes the nest's role and adds the
// island's by hand has transferred the member. The row follows the marker
// and keeps its end, the move is audited as merlin's own observation, and
// the guild's announcement channel hears it even with no invoking channel.
// Both directions, and from both the gateway handler and the sweep.
func TestHandSwapIsRecognised(t *testing.T) {
	for _, tc := range []struct{ name, from, to string }{
		{"jail to island", "7000301", island},
		{"island to jail", island, "7000301"},
	} {
		for _, via := range []string{"member update", "sweep"} {
			t.Run(tc.name+" via "+via, func(t *testing.T) {
				p, ops, store, audit, _, settings := vacationFixture()
				settings.announce["7000001"] = "7000624"
				p.jailRoleID["7000001"] = "7000301" // as any earlier /roles jail would have left it
				end := fixedNow.Add(time.Hour)
				_ = store.InsertJail(context.Background(), JailRecord{GuildID: "7000001", UserID: "7000102", JailRoleID: tc.from, SnapshotRoleIDs: []string{"7000302"}, JailedAt: fixedNow.Add(-time.Hour), ReleaseAt: &end})
				ops.setMember("7000001", "7000102", []string{tc.to, "7000303"})

				if via == "sweep" {
					if err := p.reapplyEvadedJails(context.Background(), "7000001"); err != nil {
						t.Fatal(err)
					}
				} else {
					p.HandleMemberUpdate(context.Background(), "7000001", "7000102", []string{tc.to, "7000303"})
				}

				row, ok, _ := store.GetJail(context.Background(), "7000001", "7000102")
				if !ok || row.JailRoleID != tc.to || !row.ReleaseAt.Equal(end) || !slices.Equal(row.SnapshotRoleIDs, []string{"7000302"}) {
					t.Fatalf("expected the row to follow the marker and keep its end, got %+v ok=%v", row, ok)
				}
				m, _ := ops.GuildMember("7000001", "7000102")
				if !slices.Equal(m.Roles, []string{tc.to}) {
					t.Fatalf("expected the regranted role stripped along the way, got %v", m.Roles)
				}
				if got := auditActions(audit); !slices.Equal(got, []string{"roles.transferred"}) || audit.records[0].actorID != core.ActorSystem {
					t.Fatalf("audit: %+v", audit.records)
				}
				if len(sentTo(ops, "97000102")) != 1 || len(sentTo(ops, "7000624")) != 1 {
					t.Fatalf("expected a DM and one announcement in the configured channel, got %+v", ops.dmSends)
				}
			})
		}
	}
}

// TestHandSwapNeedsTheOtherMarker: a removed marker without the other one
// on is still the mod-released case, and stays untouched.
func TestHandSwapNeedsTheOtherMarker(t *testing.T) {
	p, ops, store, audit, _, _ := vacationFixture()
	end := fixedNow.Add(time.Hour)
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "7000001", UserID: "7000102", JailRoleID: "7000301", SnapshotRoleIDs: []string{"7000302"}, JailedAt: fixedNow.Add(-time.Hour), ReleaseAt: &end})
	ops.setMember("7000001", "7000102", []string{"7000302"})

	p.HandleMemberUpdate(context.Background(), "7000001", "7000102", []string{"7000302"})

	row, _, _ := store.GetJail(context.Background(), "7000001", "7000102")
	if row.JailRoleID != "7000301" || len(auditActions(audit)) != 0 || len(ops.memberEditCalls["7000102"]) != 0 {
		t.Fatalf("a manual release must be left alone, got %+v / %v", row, auditActions(audit))
	}
}

// TestVacationEvasionIsReappliedWithoutHardening: leaving and rejoining to
// dodge a vacation gets the island's role back exactly as a jail would, and
// never the member-level channel denies that harden a jail.
func TestVacationEvasionIsReappliedWithoutHardening(t *testing.T) {
	p, ops, store, audit, _, _ := vacationFixture()
	ops.channel["7000606"] = &fch{ID: "7000606", GuildID: "7000001", Type: discord.ChannelTypeGuildText,
		PermissionOverwrites: []*fow{
			{ID: "7000316", Type: discord.PermissionOverwriteTypeRole, Allow: discord.PermissionViewChannel},
		}}
	end := fixedNow.Add(time.Hour)
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "7000001", UserID: "7000102", JailRoleID: island, SnapshotRoleIDs: []string{"7000302"}, JailedAt: fixedNow.Add(-time.Hour), ReleaseAt: &end})
	ops.setMemberJoined("7000001", "7000102", nil, fixedNow.Add(-time.Minute))

	if err := p.reapplyEvadedJails(context.Background(), "7000001"); err != nil {
		t.Fatal(err)
	}
	m, _ := ops.GuildMember("7000001", "7000102")
	if !slices.Equal(m.Roles, []string{island}) {
		t.Fatalf("expected the island's role re-applied, got %v", m.Roles)
	}
	if ops.permSetCalls != 0 {
		t.Fatal("a vacation is never hardened with member-level denies")
	}
	if got := auditActions(audit); !slices.Equal(got, []string{"roles.jail_reapplied"}) {
		t.Fatalf("audit: %v", got)
	}
}

// TestReleaseFromVacationSaysSo: the release path is jail's, and the DM,
// the announcement and the audit detail say where they came back from.
func TestReleaseFromVacationSaysSo(t *testing.T) {
	p, ops, store, audit, _, _ := vacationFixture()
	// The real catalog with a fixed pick, so the assertion below is about
	// which key was used and not about which of its lines the dice chose:
	// the first vacation.over line names the vacation, and no
	// moderation.release line does.
	sp, err := voice.New(testLogger(), voice.WithRand(func(int) int { return 0 }))
	if err != nil {
		t.Fatal(err)
	}
	p.voice = sp
	ops.setMember("7000001", "7000102", []string{island})
	end := fixedNow.Add(time.Hour)
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "7000001", UserID: "7000102", JailRoleID: island, SnapshotRoleIDs: []string{"7000302"}, ReleaseAt: &end})
	rt := handlerSession(t)

	p.handleRelease(context.Background(), rolesInteraction("", "release", userArg("user", "7000102")))

	if _, ok, _ := store.GetJail(context.Background(), "7000001", "7000102"); ok {
		t.Fatal("expected the row gone")
	}
	m, _ := ops.GuildMember("7000001", "7000102")
	if !slices.Equal(m.Roles, []string{"7000302"}) {
		t.Fatalf("expected the snapshot restored, got %v", m.Roles)
	}
	if len(audit.records) != 1 || !strings.Contains(audit.records[0].newValue, "from=vacation") {
		t.Fatalf("audit: %+v", audit.records)
	}
	if dm := sentTo(ops, "97000102"); len(dm) != 1 || !strings.Contains(dm[0], "your vacation in The Melting Pot is over") {
		t.Fatalf("expected the vacation-over DM, got %v", dm)
	}
	if !rt.said("released from vacation") {
		t.Fatalf("got %v", rt.bodies)
	}
}

// TestJailAutomaticOnAVacationerMovesThemToJail: an automatic sanction is
// the stricter sentence, so it ends the trip, for the later of the two ends.
func TestJailAutomaticOnAVacationerMovesThemToJail(t *testing.T) {
	p, ops, store, _, _, _ := vacationFixture()
	ops.setMember("7000001", "7000102", []string{island})
	end := fixedNow.Add(24 * time.Hour)
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "7000001", UserID: "7000102", JailRoleID: island, SnapshotRoleIDs: []string{"7000302"}, ReleaseAt: &end})

	if err := p.JailAutomatic(context.Background(), "7000001", "7000102", time.Hour, "policy", false); err != nil {
		t.Fatal(err)
	}
	row, _, _ := store.GetJail(context.Background(), "7000001", "7000102")
	if row.JailRoleID != "7000301" || !row.ReleaseAt.Equal(end) || !slices.Equal(row.SnapshotRoleIDs, []string{"7000302"}) {
		t.Fatalf("expected the row in jail until the later end with the snapshot intact, got %+v", row)
	}
	m, _ := ops.GuildMember("7000001", "7000102")
	if !slices.Equal(m.Roles, []string{"7000301"}) {
		t.Fatalf("got %v", m.Roles)
	}
}

// TestVacationRoleConfigureSyncsDenyByDefault: setting the island's role
// hides every managed channel from it except the allowlist, merging with
// what the guild already set rather than replacing it, and reads the result
// back; clearing says so.
func TestVacationRoleConfigureSyncsDenyByDefault(t *testing.T) {
	p, ops, _, audit, _, settings := vacationFixture()
	settings.vacationAllowed["7000001"] = []string{"7000607"}
	view, send, connect := discord.PermissionViewChannel, discord.PermissionSendMessages, discord.PermissionConnect
	// Read-only replaces participation bits but preserves the attachment deny.
	ops.channel["7000607"] = &fch{ID: "7000607", GuildID: "7000001", Type: discord.ChannelTypeGuildText,
		PermissionOverwrites: []*fow{{ID: "7000310", Type: discord.PermissionOverwriteTypeRole,
			Allow: discord.PermissionAddReactions, Deny: discord.PermissionAttachFiles}}}
	ops.channel["7000608"] = &fch{ID: "7000608", GuildID: "7000001", Type: discord.ChannelTypeGuildVoice}
	ops.channel["7000617"] = &fch{ID: "7000617", GuildID: "7000001", Type: discord.ChannelTypeGuildText,
		PermissionOverwrites: []*fow{{ID: "7000310", Type: discord.PermissionOverwriteTypeRole, Allow: send}}}
	p.vacationRoleID["7000001"] = island // a stale cache from before the change
	rt := handlerSession(t)

	set(p, panelJail+"vacrole", nil, "7000310")

	if settings.vacationRole["7000001"] != "7000310" {
		t.Fatalf("expected the role saved, got %q", settings.vacationRole["7000001"])
	}
	if _, cached := p.vacationRoleID["7000001"]; cached {
		t.Fatal("the cache must be dropped when the role changes")
	}
	beach := ops.overwrites[overwriteKey{"7000607", "7000310"}]
	if beach.allow != view|discord.PermissionReadMessageHistory || beach.deny&(send|discord.PermissionAttachFiles) != send|discord.PermissionAttachFiles {
		t.Fatalf("the beach should be visible with the guild's own bits kept, got %+v", beach)
	}
	if vc := ops.overwrites[overwriteKey{"7000608", "7000310"}]; vc.allow != 0 || vc.deny != view|connect {
		t.Fatalf("an unlisted voice room is hidden, got %+v", vc)
	}
	if office := ops.overwrites[overwriteKey{"7000617", "7000310"}]; office.allow != 0 || office.deny != view {
		t.Fatalf("an unlisted text room loses managed access, got %+v", office)
	}
	if !rt.said("<#7000607>") || !rt.said("<#7000617>") || !rt.said("Unrelated overwrite bits") {
		t.Fatalf("expected the read-back, got %v", rt.bodies)
	}
	if got := auditActions(audit); !slices.Equal(got, []string{"roles.configure_jail_channels"}) {
		t.Fatalf("audit: %v", got)
	}

	set(p, panelJail+"vacrole", nil)
	if settings.vacationRole["7000001"] != "" || !rt.said("Vacation role cleared") {
		t.Fatalf("expected the role cleared, got %q / %v", settings.vacationRole["7000001"], rt.bodies)
	}
}

func TestVacationRoleConfigureWithNoOverwritesSaysSo(t *testing.T) {
	p, _, _, _, _, _ := vacationFixture()
	rt := handlerSession(t)
	set(p, panelJail+"vacrole", nil, "7000310")
	if !rt.said("no channel overwrites of its own yet") {
		t.Fatalf("got %v", rt.bodies)
	}
}

// TestMeltingPotHasAnIslandByDefault: the Melting Pot's role is the default
// there and nowhere else, and clearing a configured role falls back to it.
func TestMeltingPotHasAnIslandByDefault(t *testing.T) {
	p, _, _, _, _, _ := vacationFixture()
	if p.configuredVacationRole(meltingPotGuildID) != meltingPotDefaultVacationRoleID {
		t.Fatal("the Melting Pot's island should be the default there")
	}
	if p.configuredVacationRole("7000117") != "" {
		t.Fatal("no other guild gets a default")
	}
	if p.sentenceFor(meltingPotGuildID, meltingPotDefaultVacationRoleID) != vacationSentence || p.sentenceFor("7000001", "7000301") != jailSentence {
		t.Fatal("sentenceFor should read the marker")
	}
	if _, ok := p.counterpartMarker("7000628", "7000301"); ok {
		t.Fatal("no island, no counterpart")
	}
}

// TestScriptsListAndEnableShowTheIsland: the switch surfaces name the role
// in play, and enabling the script needs nothing enforced.
func TestScriptsListAndEnableShowTheIsland(t *testing.T) {
	p, _, _, audit, _, _ := vacationFixture()
	rt := handlerSession(t)
	showPage(p, "scripts")
	if !rt.said("`vacation` is **on**") || !rt.said("island role <@&"+island+">") {
		t.Fatalf("got %v", rt.bodies)
	}
	set(p, panelScripts+"on:"+scriptVacation, nil)
	if !rt.said("Script on: vacation") || !rt.said("island's role as the marker") {
		t.Fatalf("got %v", rt.bodies)
	}
	if got := auditActions(audit); !slices.Equal(got, []string{"roles.script_enabled"}) {
		t.Fatalf("audit: %v", got)
	}
}

// TestListSaysOnVacation: /roles list tells the two apart.
func TestListSaysOnVacation(t *testing.T) {
	p, _, store, _, _, _ := vacationFixture()
	end := fixedNow.Add(time.Hour)
	_ = store.InsertJail(context.Background(), JailRecord{GuildID: "7000001", UserID: "7000102", JailRoleID: island, ReleaseAt: &end})
	rt := handlerSession(t)
	p.handleList(context.Background(), rolesInteraction("", "list", userArg("user", "7000102")))
	if !rt.said("On vacation") {
		t.Fatalf("got %v", rt.bodies)
	}
}

// TestRoleDeletedDropsTheIslandCache: the island's role going away forgets
// the cache and nothing more; it is never recreated.
func TestRoleDeletedDropsTheIslandCache(t *testing.T) {
	p, ops, _, _, _, _ := vacationFixture()
	if _, err := p.resolveVacationRole("7000001"); err != nil {
		t.Fatal(err)
	}
	before := len(ops.roles["7000001"])
	p.HandleRoleDeleted(context.Background(), "7000001", island)
	if _, cached := p.vacationRoleID["7000001"]; cached {
		t.Fatal("cache should be dropped")
	}
	if len(ops.roles["7000001"]) != before {
		t.Fatal("nothing should be created")
	}
}

// TestVacationAllowlistIsOneWriteAndAuditQuiet: listing a channel writes
// that one channel, a second identical sync writes nothing (every write is
// a line in the guild's audit log), and unlisting hides it again.
func TestVacationAllowlistIsOneWriteAndAuditQuiet(t *testing.T) {
	p, ops, _, audit, _, settings := vacationFixture()
	view, send := discord.PermissionViewChannel, discord.PermissionSendMessages
	ops.channel["7000607"] = &fch{ID: "7000607", GuildID: "7000001", Type: discord.ChannelTypeGuildText}
	ops.channel["7000617"] = &fch{ID: "7000617", GuildID: "7000001", Type: discord.ChannelTypeGuildText}
	rt := handlerSession(t)

	set(p, panelJail+"vacallow", nil, "7000607")
	if !slices.Equal(settings.vacationAllowed["7000001"], []string{"7000607"}) || !rt.said("Channel allowed") {
		t.Fatalf("expected the channel listed, got %v / %v", settings.vacationAllowed["7000001"], rt.bodies)
	}
	if ow := ops.overwrites[overwriteKey{"7000607", island}]; ow.allow != view|discord.PermissionReadMessageHistory || ow.deny&send == 0 {
		t.Fatalf("expected read-only access, got %+v", ow)
	}
	if ops.permSetCalls != 1 {
		t.Fatalf("exactly one overwrite written, got %d", ops.permSetCalls)
	}
	if got := auditActions(audit); !slices.Equal(got, []string{"roles.configure_jail_channels"}) {
		t.Fatalf("audit: %v", got)
	}

	// A full sync now writes only the office (never touched) and nothing
	// for the beach, which already matches.
	if err := p.syncAllVacationOverwrites("7000001"); err != nil {
		t.Fatal(err)
	}
	if ops.permSetCalls != 2 {
		t.Fatalf("a sync that changes one channel writes one channel, got %d writes", ops.permSetCalls)
	}
	if err := p.syncAllVacationOverwrites("7000001"); err != nil {
		t.Fatal(err)
	}
	if ops.permSetCalls != 2 {
		t.Fatalf("a sync that changes nothing writes nothing, got %d writes", ops.permSetCalls)
	}

	set(p, panelJail+"vacallow", []string{"7000607"})
	if ow := ops.overwrites[overwriteKey{"7000607", island}]; ow.allow != 0 || ow.deny&view == 0 || len(settings.vacationAllowed["7000001"]) != 0 {
		t.Fatalf("expected the beach hidden again, got %+v / %v", ow, settings.vacationAllowed["7000001"])
	}
	if !rt.said("Channel access updated") {
		t.Fatalf("got %v", rt.bodies)
	}
}

// TestVacationSyncNeedsARole: with no role configured the allowlist is
// recorded and nothing is written; turning the script on runs the one
// unprompted full sync.
func TestVacationSyncNeedsARole(t *testing.T) {
	p, ops, _, _, _, settings := vacationFixture()
	settings.vacationRole = map[string]string{}
	ops.channel["7000607"] = &fch{ID: "7000607", GuildID: "7000001", Type: discord.ChannelTypeGuildText}
	rt := handlerSession(t)

	set(p, panelJail+"vacallow", nil, "7000607")
	if ops.permSetCalls != 0 || !rt.said("Channel allowed") {
		t.Fatalf("no role, nothing to write; got %d writes / %v", ops.permSetCalls, rt.bodies)
	}

	settings.vacationRole["7000001"] = island
	p.scripts = newFakeScripts()
	set(p, panelScripts+"on:"+scriptVacation, nil)
	if ow, ok := ops.overwrites[overwriteKey{"7000607", island}]; !ok || ow.allow&discord.PermissionViewChannel == 0 {
		t.Fatalf("enabling the script should sync the island, got %+v ok=%v", ow, ok)
	}
}

// The migration persists the former implicit beach default. Clearing it
// must stay cleared instead of restoring the default on the next sync.
func TestMeltingPotBeachDefaultCanBeCleared(t *testing.T) {
	p, _, _, _, _, settings := vacationFixture()
	settings.vacationAllowed[meltingPotGuildID] = []string{meltingPotDefaultVacationChannelID}
	if got := p.vacationAllowlist(meltingPotGuildID); !slices.Equal(got, []string{meltingPotDefaultVacationChannelID}) {
		t.Fatalf("got %v", got)
	}
	settings.vacationAllowed[meltingPotGuildID] = nil
	if got := p.vacationAllowlist(meltingPotGuildID); len(got) != 0 {
		t.Fatalf("clearing the list must persist, got %v", got)
	}
	if got := p.vacationAllowlist("7000117"); got != nil {
		t.Fatalf("no default elsewhere, got %v", got)
	}
}

// TestListChannelsShowsTheVacationAllowlist: the one place both lists are
// read side by side says which is which.
func TestListChannelsShowsTheVacationAllowlist(t *testing.T) {
	p, _, _, _, _, settings := vacationFixture()
	settings.vacationAllowed["7000001"] = []string{"7000607"}
	rt := handlerSession(t)
	showPage(p, "vacation")
	if !rt.said("Read-only") || !rt.said("Conversation") || !rt.said("Now: <#7000607>") {
		t.Fatalf("got %v", rt.bodies)
	}
}
