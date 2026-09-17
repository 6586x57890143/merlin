package rapsheet

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/voice"
)

// --- timeout -----------------------------------------------------------------------

func TestTimeoutAppliesRecordsAndTells(t *testing.T) {
	h := newHarness()
	h.ops.addMember("u1")
	s, rt := stubSession()
	h.p.handleTimeout(context.Background(), s, interaction("timeout",
		userOpt("user", "u1"), strOpt("duration", "2h"), strOpt("category", "spam"), strOpt("reason", "flooding")))

	until, ok := h.ops.timeouts["u1"]
	if !ok || until == nil || !until.Equal(testNow.Add(2*time.Hour)) {
		t.Fatalf("timeout applied = %v %v", until, ok)
	}
	e := h.store.all()[0]
	if e.Kind != KindTimeout || e.Duration != 2*time.Hour || e.EndsAt == nil || e.Points != 10 || e.Voided() {
		t.Errorf("entry = %+v", e)
	}
	if len(h.voice.keys) != 1 || h.voice.keys[0] != voice.KeyTimeoutNotice {
		t.Errorf("voice keys = %v", h.voice.keys)
	}
	if !h.audit.has("rapsheet.timeout") || !strings.Contains(rt.said(), "Case #1") {
		t.Errorf("audit %v said %s", h.audit.all(), rt.said())
	}
}

func TestTimeoutValidatesItsDuration(t *testing.T) {
	for _, d := range []string{"29d", "nonsense", "0m"} {
		h := newHarness()
		h.ops.addMember("u1")
		s, _ := stubSession()
		h.p.handleTimeout(context.Background(), s, interaction("timeout",
			userOpt("user", "u1"), strOpt("duration", d), strOpt("category", "spam"), strOpt("reason", "r")))
		if len(h.store.all()) != 0 || len(h.ops.timeouts) != 0 {
			t.Errorf("duration %q was accepted", d)
		}
	}
}

func TestARefusedTimeoutVoidsItsEntry(t *testing.T) {
	h := newHarness()
	h.ops.addMember("u1")
	h.ops.timeoutErr = errors.New("missing permissions")
	s, rt := stubSession()
	h.p.handleTimeout(context.Background(), s, interaction("timeout",
		userOpt("user", "u1"), strOpt("duration", "2h"), strOpt("category", "spam"), strOpt("reason", "r")))
	e := h.store.all()[0]
	if !e.Voided() || !strings.Contains(e.VoidReason, "missing permissions") {
		t.Errorf("entry = %+v", e)
	}
	if h.ops.dmOpen != 0 {
		t.Error("told the member about a timeout that did not happen")
	}
	if !strings.Contains(rt.said(), "voided") {
		t.Errorf("got %s", rt.said())
	}
}

func TestTimeoutOfSomebodyWhoLeftIsRefused(t *testing.T) {
	h := newHarness()
	s, rt := stubSession()
	h.p.handleTimeout(context.Background(), s, interaction("timeout",
		userOpt("user", "gone"), strOpt("duration", "2h"), strOpt("category", "spam"), strOpt("reason", "r")))
	if len(h.store.all()) != 0 || !strings.Contains(rt.said(), "not in this server") {
		t.Errorf("entries %d said %s", len(h.store.all()), rt.said())
	}
}

func TestVoidingAStandingTimeoutLiftsIt(t *testing.T) {
	h := newHarness()
	h.ops.addMember("u1")
	s, _ := stubSession()
	h.p.handleTimeout(context.Background(), s, interaction("timeout",
		userOpt("user", "u1"), strOpt("duration", "2h"), strOpt("category", "spam"), strOpt("reason", "r")))
	s2, rt := stubSession()
	h.p.handleVoid(context.Background(), s2, interaction("void", intOpt("case", 1), strOpt("reason", "mistake")))
	if until, ok := h.ops.timeouts["u1"]; !ok || until != nil {
		t.Errorf("timeout not cleared: %v %v", until, ok)
	}
	if !strings.Contains(rt.said(), "timeout was lifted") {
		t.Errorf("got %s", rt.said())
	}
}

// --- kick -------------------------------------------------------------------------------

func TestKickTellsThenRemoves(t *testing.T) {
	h := newHarness()
	h.ops.addMember("u1")
	s, _ := stubSession()
	h.p.handleKick(context.Background(), s, interaction("kick", userOpt("user", "u1"), strOpt("category", "server_rule"), strOpt("reason", "r")))
	if len(h.ops.kicked) != 1 || h.ops.kicked[0] != "u1" {
		t.Fatalf("kicked = %v", h.ops.kicked)
	}
	if h.ops.dmOpen != 1 || h.voice.keys[0] != voice.KeyKickNotice {
		t.Errorf("DM before the kick: opened %d keys %v", h.ops.dmOpen, h.voice.keys)
	}
	if e := h.store.all()[0]; e.Kind != KindKick || e.Points != 25 || e.Voided() {
		t.Errorf("entry = %+v", e)
	}
	if !h.audit.has("rapsheet.kick") {
		t.Error("kick not audited")
	}
}

func TestKickRefusesStaffAndTheAbsent(t *testing.T) {
	h := newHarness()
	h.ops.addMember("admin-1")
	h.ranker.admins["admin-1"] = true
	s, _ := stubSession()
	h.p.handleKick(context.Background(), s, interaction("kick", userOpt("user", "admin-1"), strOpt("category", "spam"), strOpt("reason", "r")))
	s2, rt := stubSession()
	h.p.handleKick(context.Background(), s2, interaction("kick", userOpt("user", "gone"), strOpt("category", "spam"), strOpt("reason", "r")))
	if len(h.ops.kicked) != 0 || len(h.store.all()) != 0 || !strings.Contains(rt.said(), "not in this server") {
		t.Errorf("kicked %v entries %d said %s", h.ops.kicked, len(h.store.all()), rt.said())
	}
}

// --- ban ----------------------------------------------------------------------------------

func banCmd(user string, opts ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.InteractionCreate {
	base := []*discordgo.ApplicationCommandInteractionDataOption{userOpt("user", user), strOpt("category", "threats"), strOpt("reason", "r")}
	return interaction("ban", append(base, opts...)...)
}

func TestTemporaryBanTellsBansAndArmsTheSweep(t *testing.T) {
	h := newHarness()
	h.ops.addMember("u1")
	s, rt := stubSession()
	h.p.handleBan(context.Background(), s, banCmd("u1", strOpt("duration", "7d"), intOpt("delete_message_days", 1)))

	if reason, ok := h.ops.bans["u1"]; !ok || !strings.HasPrefix(reason, "rapsheet #1: ") {
		t.Fatalf("ban = %q %v", reason, ok)
	}
	if h.voice.keys[0] != voice.KeyBanNotice || h.ops.dmOpen != 1 {
		t.Errorf("DM before the ban: keys %v opened %d", h.voice.keys, h.ops.dmOpen)
	}
	e := h.store.all()[0]
	if e.Kind != KindBan || e.Duration != 7*24*time.Hour || e.EndsAt == nil || e.Points != 100 || !e.Standing(testNow) {
		t.Errorf("entry = %+v", e)
	}
	if !h.sched.has(sweepKey(testGuild)) {
		t.Error("a pending temporary ban should arm the sweep")
	}
	if !h.audit.has("rapsheet.ban") || !strings.Contains(rt.said(), "Case #1") {
		t.Errorf("audit %v said %s", h.audit.all(), rt.said())
	}
}

func TestPermanentBanNeedsSayingSoAndArmsNoSweep(t *testing.T) {
	h := newHarness()
	h.ops.addMember("u1")

	s, rt := stubSession()
	h.p.handleBan(context.Background(), s, banCmd("u1"))
	if len(h.ops.bans) != 0 || !strings.Contains(rt.said(), "permanent: true") {
		t.Errorf("a ban with neither duration nor permanent went through: %s", rt.said())
	}
	s2, rt2 := stubSession()
	h.p.handleBan(context.Background(), s2, banCmd("u1", strOpt("duration", "7d"), boolOpt("permanent", true)))
	if len(h.ops.bans) != 0 || !strings.Contains(rt2.said(), "not both") {
		t.Errorf("a ban with both went through: %s", rt2.said())
	}
	s3, rt3 := stubSession()
	h.p.handleBan(context.Background(), s3, banCmd("u1", strOpt("duration", "400d")))
	if len(h.ops.bans) != 0 || !strings.Contains(rt3.said(), "at most") {
		t.Errorf("an over-long ban went through: %s", rt3.said())
	}

	s4, _ := stubSession()
	h.p.handleBan(context.Background(), s4, banCmd("u1", boolOpt("permanent", true)))
	if _, ok := h.ops.bans["u1"]; !ok {
		t.Fatal("permanent ban not applied")
	}
	e := h.store.all()[0]
	if e.Kind != KindBan || e.EndsAt != nil || e.Duration != 0 || !e.Standing(testNow) {
		t.Errorf("entry = %+v", e)
	}
	if h.voice.keys[len(h.voice.keys)-1] != voice.KeyBanPermanentNotice {
		t.Errorf("voice keys = %v", h.voice.keys)
	}
	if h.sched.has(sweepKey(testGuild)) {
		t.Error("a permanent ban has nothing to sweep")
	}
}

func TestBanWorksOnSomebodyWhoAlreadyLeftAndRefusesADoubleBan(t *testing.T) {
	h := newHarness()
	s, _ := stubSession()
	h.p.handleBan(context.Background(), s, banCmd("gone", strOpt("duration", "1d")))
	if _, ok := h.ops.bans["gone"]; !ok {
		t.Fatal("could not ban somebody who left")
	}
	s2, rt := stubSession()
	h.p.handleBan(context.Background(), s2, banCmd("gone", strOpt("duration", "2d")))
	if len(h.store.all()) != 1 || !strings.Contains(rt.said(), "already banned") {
		t.Errorf("entries %d said %s", len(h.store.all()), rt.said())
	}
}

func TestARefusedBanVoidsItsEntry(t *testing.T) {
	h := newHarness()
	h.ops.addMember("u1")
	h.ops.banErr = errors.New("missing permissions")
	s, _ := stubSession()
	h.p.handleBan(context.Background(), s, banCmd("u1", strOpt("duration", "1d")))
	if e := h.store.all()[0]; !e.Voided() || e.Standing(testNow) {
		t.Errorf("entry = %+v", e)
	}
	if h.sched.has(sweepKey(testGuild)) {
		t.Error("a voided ban armed the sweep")
	}
}

func TestUnbanLiftsTheLedgersBanTooAndDisarmsTheSweep(t *testing.T) {
	h := newHarness()
	h.ops.addMember("u1")
	s, _ := stubSession()
	h.p.handleBan(context.Background(), s, banCmd("u1", strOpt("duration", "7d")))

	s2, rt := stubSession()
	h.p.handleUnban(context.Background(), s2, interaction("unban", userOpt("user", "u1"), strOpt("reason", "appeal accepted")))
	if _, still := h.ops.bans["u1"]; still {
		t.Fatal("still banned")
	}
	entries := h.store.all()
	if len(entries) != 2 || entries[0].LiftedAt == nil || entries[1].Kind != KindUnban || entries[1].ActorID != modID {
		t.Errorf("entries = %+v", entries)
	}
	if h.sched.has(sweepKey(testGuild)) {
		t.Error("nothing left to sweep, job still registered")
	}
	if !strings.Contains(rt.said(), "unbanned") || !h.audit.has("rapsheet.unban") {
		t.Errorf("said %s audit %v", rt.said(), h.audit.all())
	}

	// Unbanning somebody who is not banned says so and writes nothing.
	s3, rt3 := stubSession()
	h.p.handleUnban(context.Background(), s3, interaction("unban", userOpt("user", "u9"), strOpt("reason", "r")))
	if len(h.store.all()) != 2 || !strings.Contains(rt3.said(), "not banned") {
		t.Errorf("entries %d said %s", len(h.store.all()), rt3.said())
	}
}

// --- the sweep ---------------------------------------------------------------------------

func TestTheSweepLiftsServedBansAndOnlyThose(t *testing.T) {
	h := newHarness()
	h.ops.addMember("u1")
	h.ops.addMember("u2")
	s, _ := stubSession()
	h.p.handleBan(context.Background(), s, banCmd("u1", strOpt("duration", "1h")))
	s2, _ := stubSession()
	h.p.handleBan(context.Background(), s2, banCmd("u2", strOpt("duration", "3h")))

	h.p.now = func() time.Time { return testNow.Add(2 * time.Hour) }
	if err := h.sched.RunNow(context.Background(), sweepKey(testGuild)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(h.ops.unbanned) != 1 || h.ops.unbanned[0] != "u1" {
		t.Fatalf("unbanned = %v, want just u1", h.ops.unbanned)
	}
	entries := h.store.all()
	if entries[0].LiftedAt == nil || entries[1].LiftedAt != nil {
		t.Errorf("lifted marks: %v %v", entries[0].LiftedAt, entries[1].LiftedAt)
	}
	var unban *Entry
	for i := range entries {
		if entries[i].Kind == KindUnban {
			unban = &entries[i]
		}
	}
	if unban == nil || unban.ActorID != core.ActorSystem || unban.Source != SourceSweep || unban.UserID != "u1" {
		t.Errorf("unban entry = %+v", unban)
	}
	if !h.sched.has(sweepKey(testGuild)) {
		t.Error("u2's ban is still pending; the sweep should stay registered")
	}

	h.p.now = func() time.Time { return testNow.Add(4 * time.Hour) }
	_ = h.sched.RunNow(context.Background(), sweepKey(testGuild))
	if h.sched.has(sweepKey(testGuild)) {
		t.Error("everything served; the sweep should unregister itself")
	}
}

func TestTheSweepUntracksABanAHumanAlreadyLiftedAndRetriesATransientFailure(t *testing.T) {
	h := newHarness()
	h.ops.addMember("u1")
	h.ops.addMember("u2")
	s, _ := stubSession()
	h.p.handleBan(context.Background(), s, banCmd("u1", strOpt("duration", "1h")))
	s2, _ := stubSession()
	h.p.handleBan(context.Background(), s2, banCmd("u2", strOpt("duration", "1h")))
	// A human unbanned u1 in the client.
	delete(h.ops.bans, "u1")
	h.p.now = func() time.Time { return testNow.Add(2 * time.Hour) }

	h.ops.unbanErr = errors.New("discord is down")
	if err := h.sched.RunNow(context.Background(), sweepKey(testGuild)); err == nil {
		t.Error("a transient failure should be returned so the Scheduler backs off")
	}
	for _, e := range h.store.all() {
		if e.LiftedAt != nil {
			t.Errorf("a failed unban was marked lifted: %+v", e)
		}
	}

	h.ops.unbanErr = nil
	if err := h.sched.RunNow(context.Background(), sweepKey(testGuild)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	entries := h.store.all()
	if entries[0].LiftedAt == nil || entries[1].LiftedAt == nil {
		t.Errorf("both should be lifted now: %+v", entries)
	}
	unbans := 0
	for _, e := range entries {
		if e.Kind == KindUnban {
			unbans++
		}
	}
	if unbans != 1 || len(h.ops.unbanned) != 1 || h.ops.unbanned[0] != "u2" {
		t.Errorf("the human's unban must not be recorded again: unbans=%d unbanned=%v", unbans, h.ops.unbanned)
	}
}

func TestSyncGuildArmsTheSweepOnlyWhereABanIsPending(t *testing.T) {
	h := newHarness()
	h.p.SyncGuild(context.Background(), testGuild)
	if h.sched.has(sweepKey(testGuild)) {
		t.Error("no bans, job registered")
	}
	ends := testNow.Add(time.Hour)
	_, _ = h.store.Insert(context.Background(), Entry{GuildID: testGuild, UserID: "u1", Kind: KindBan, ActorID: modID, Source: SourceCommand, Duration: time.Hour, EndsAt: &ends})
	h.p.SyncGuild(context.Background(), testGuild)
	if !h.sched.has(sweepKey(testGuild)) {
		t.Error("a pending ban on restart should arm the sweep")
	}
	h.p.ForgetGuild(testGuild)
	if h.sched.has(sweepKey(testGuild)) {
		t.Error("ForgetGuild left the job registered")
	}
}

// --- Discord's own audit log -----------------------------------------------------------------

func auditEvent(actor, target string, action discordgo.AuditLogAction, reason string, changes ...*discordgo.AuditLogChange) *discordgo.GuildAuditLogEntryCreate {
	return &discordgo.GuildAuditLogEntryCreate{GuildID: testGuild, AuditLogEntry: &discordgo.AuditLogEntry{
		ID: "log-" + target + "-" + string(rune('0'+int(action)%10)), UserID: actor, TargetID: target,
		ActionType: &action, Reason: reason, Changes: changes,
	}}
}

func TestBansKicksAndTimeoutsDoneThroughDiscordAreRecorded(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	h.p.HandleAuditLogEntry(ctx, "merlin-1", auditEvent("mod-2", "u1", discordgo.AuditLogActionMemberBanAdd, "raiding"))
	h.p.HandleAuditLogEntry(ctx, "merlin-1", auditEvent("mod-2", "u2", discordgo.AuditLogActionMemberKick, ""))
	until := testNow.Add(time.Hour).Format(time.RFC3339Nano)
	key := discordgo.AuditLogChangeKeyCommunicationDisabledUntil
	h.p.HandleAuditLogEntry(ctx, "merlin-1", auditEvent("mod-2", "u3", discordgo.AuditLogActionMemberUpdate, "",
		&discordgo.AuditLogChange{Key: &key, NewValue: until}))
	// A member update that is not a timeout is ignored.
	nick := discordgo.AuditLogChangeKeyNick
	h.p.HandleAuditLogEntry(ctx, "merlin-1", auditEvent("mod-2", "u4", discordgo.AuditLogActionMemberUpdate, "",
		&discordgo.AuditLogChange{Key: &nick, NewValue: "new nick"}))
	// So is anything merlin did herself.
	h.p.HandleAuditLogEntry(ctx, "merlin-1", auditEvent("merlin-1", "u5", discordgo.AuditLogActionMemberBanAdd, "rapsheet #9"))
	// And a redelivery.
	h.p.HandleAuditLogEntry(ctx, "merlin-1", auditEvent("mod-2", "u1", discordgo.AuditLogActionMemberBanAdd, "raiding"))

	entries := h.store.all()
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3: %+v", len(entries), entries)
	}
	ban, kick, timeout := entries[0], entries[1], entries[2]
	if ban.Kind != KindBan || ban.EndsAt != nil || ban.ActorID != "mod-2" || ban.Reason != "raiding" || ban.Source != SourceDiscord || ban.Points != 25 {
		t.Errorf("ban = %+v", ban)
	}
	if kick.Kind != KindKick || kick.Reason != "kicked through Discord" {
		t.Errorf("kick = %+v", kick)
	}
	if timeout.Kind != KindTimeout || timeout.EndsAt == nil || timeout.Duration <= 0 {
		t.Errorf("timeout = %+v", timeout)
	}
	if h.sched.has(sweepKey(testGuild)) {
		t.Error("a permanent client ban has nothing to sweep")
	}
}

func TestAnUnbanThroughDiscordLiftsTheLedgersBan(t *testing.T) {
	h := newHarness()
	h.ops.addMember("u1")
	s, _ := stubSession()
	h.p.handleBan(context.Background(), s, banCmd("u1", strOpt("duration", "7d")))
	h.p.HandleAuditLogEntry(context.Background(), "merlin-1", auditEvent("mod-2", "u1", discordgo.AuditLogActionMemberBanRemove, "appeal"))
	entries := h.store.all()
	if entries[0].LiftedAt == nil || entries[1].Kind != KindUnban || entries[1].ActorID != "mod-2" {
		t.Errorf("entries = %+v", entries)
	}
	// A cleared timeout arrives as a release.
	key := discordgo.AuditLogChangeKeyCommunicationDisabledUntil
	h.p.HandleAuditLogEntry(context.Background(), "merlin-1", auditEvent("mod-2", "u3", discordgo.AuditLogActionMemberUpdate, "",
		&discordgo.AuditLogChange{Key: &key, NewValue: nil}))
	if last := h.store.all()[2]; last.Kind != KindRelease {
		t.Errorf("cleared timeout = %+v", last)
	}
}

func TestAuditLogIngestionRespectsTheGateAndIgnoresJunk(t *testing.T) {
	h := newHarness()
	h.p.gate = fakeGate{disabled: map[string]bool{testGuild: true}}
	h.p.HandleAuditLogEntry(context.Background(), "merlin-1", auditEvent("mod-2", "u1", discordgo.AuditLogActionMemberBanAdd, "r"))
	h.p.gate = nil
	h.p.HandleAuditLogEntry(context.Background(), "merlin-1", nil)
	h.p.HandleAuditLogEntry(context.Background(), "merlin-1", &discordgo.GuildAuditLogEntryCreate{GuildID: testGuild, AuditLogEntry: &discordgo.AuditLogEntry{}})
	h.p.HandleAuditLogEntry(context.Background(), "merlin-1", auditEvent("mod-2", "c1", discordgo.AuditLogActionChannelDelete, ""))
	if n := len(h.store.all()); n != 0 {
		t.Errorf("%d entries from ignored events", n)
	}
}

func TestStatusReportsTheAuditLogPermission(t *testing.T) {
	h := newHarness()
	h.ops.addMember("merlin-1", "bot-role")
	h.ops.roles = []*discordgo.Role{{ID: testGuild}, {ID: "bot-role", Permissions: discordgo.PermissionManageRoles}}
	s, rt := stubSession()
	h.p.handleStatus(context.Background(), s, interaction("status"))
	if !strings.Contains(rt.said(), "does not hold View Audit Log") {
		t.Errorf("got %s", rt.said())
	}
	h.ops.roles[1].Permissions |= discordgo.PermissionViewAuditLogs
	s2, rt2 := stubSession()
	h.p.handleStatus(context.Background(), s2, interaction("status"))
	if !strings.Contains(rt2.said(), "View Audit Log held") {
		t.Errorf("got %s", rt2.said())
	}
}
