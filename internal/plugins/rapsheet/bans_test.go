package rapsheet

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/voice"
)

// --- timeout -----------------------------------------------------------------------

func TestTimeoutAppliesRecordsAndTells(t *testing.T) {
	h := newHarness()
	h.ops.addMember("7300007")
	rt := stubSession()
	h.p.handleTimeout(context.Background(), interaction("timeout",
		userOpt("user", "7300007"), strOpt("duration", "2h"), strOpt("category", "spam"), strOpt("reason", "flooding")))

	until, ok := h.ops.timeouts["7300007"]
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
		h.ops.addMember("7300007")
		stubSession()
		h.p.handleTimeout(context.Background(), interaction("timeout",
			userOpt("user", "7300007"), strOpt("duration", d), strOpt("category", "spam"), strOpt("reason", "r")))
		if len(h.store.all()) != 0 || len(h.ops.timeouts) != 0 {
			t.Errorf("duration %q was accepted", d)
		}
	}
}

func TestARefusedTimeoutVoidsItsEntry(t *testing.T) {
	h := newHarness()
	h.ops.addMember("7300007")
	h.ops.timeoutErr = errors.New("missing permissions")
	rt := stubSession()
	h.p.handleTimeout(context.Background(), interaction("timeout",
		userOpt("user", "7300007"), strOpt("duration", "2h"), strOpt("category", "spam"), strOpt("reason", "r")))
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
	rt := stubSession()
	h.p.handleTimeout(context.Background(), interaction("timeout",
		userOpt("user", "7300017"), strOpt("duration", "2h"), strOpt("category", "spam"), strOpt("reason", "r")))
	if len(h.store.all()) != 0 || !strings.Contains(rt.said(), "not in this server") {
		t.Errorf("entries %d said %s", len(h.store.all()), rt.said())
	}
}

func TestVoidingAStandingTimeoutLiftsIt(t *testing.T) {
	h := newHarness()
	h.ops.addMember("7300007")
	stubSession()
	h.p.handleTimeout(context.Background(), interaction("timeout",
		userOpt("user", "7300007"), strOpt("duration", "2h"), strOpt("category", "spam"), strOpt("reason", "r")))
	rt := stubSession()
	h.p.handleVoid(context.Background(), interaction("void", intOpt("case", 1), strOpt("reason", "mistake")))
	if until, ok := h.ops.timeouts["7300007"]; !ok || until != nil {
		t.Errorf("timeout not cleared: %v %v", until, ok)
	}
	if !strings.Contains(rt.said(), "timeout was lifted") {
		t.Errorf("got %s", rt.said())
	}
}

// --- kick -------------------------------------------------------------------------------

func TestKickTellsThenRemoves(t *testing.T) {
	h := newHarness()
	h.ops.addMember("7300007")
	stubSession()
	h.p.handleKick(context.Background(), interaction("kick", userOpt("user", "7300007"), strOpt("category", "server_rule"), strOpt("reason", "r")))
	if len(h.ops.kicked) != 1 || h.ops.kicked[0] != "7300007" {
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
	h.ops.addMember("7300006")
	h.ranker.admins["7300006"] = true
	stubSession()
	h.p.handleKick(context.Background(), interaction("kick", userOpt("user", "7300006"), strOpt("category", "spam"), strOpt("reason", "r")))
	rt := stubSession()
	h.p.handleKick(context.Background(), interaction("kick", userOpt("user", "7300017"), strOpt("category", "spam"), strOpt("reason", "r")))
	if len(h.ops.kicked) != 0 || len(h.store.all()) != 0 || !strings.Contains(rt.said(), "not in this server") {
		t.Errorf("kicked %v entries %d said %s", h.ops.kicked, len(h.store.all()), rt.said())
	}
}

// --- ban ----------------------------------------------------------------------------------

func banCmd(user string, opts ...map[string]any) *core.Ix {
	base := []map[string]any{userOpt("user", user), strOpt("category", "threats"), strOpt("reason", "r")}
	return interaction("ban", append(base, opts...)...)
}

func TestTemporaryBanTellsBansAndArmsTheSweep(t *testing.T) {
	h := newHarness()
	h.ops.addMember("7300007")
	rt := stubSession()
	h.p.handleBan(context.Background(), banCmd("7300007", strOpt("duration", "7d"), intOpt("delete_message_days", 1)))

	if reason, ok := h.ops.bans["7300007"]; !ok || !strings.HasPrefix(reason, "rapsheet #1: ") {
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
	h.ops.addMember("7300007")

	rt := stubSession()
	h.p.handleBan(context.Background(), banCmd("7300007"))
	if len(h.ops.bans) != 0 || !strings.Contains(rt.said(), "permanent: true") {
		t.Errorf("a ban with neither duration nor permanent went through: %s", rt.said())
	}
	rt2 := stubSession()
	h.p.handleBan(context.Background(), banCmd("7300007", strOpt("duration", "7d"), boolOpt("permanent", true)))
	if len(h.ops.bans) != 0 || !strings.Contains(rt2.said(), "not both") {
		t.Errorf("a ban with both went through: %s", rt2.said())
	}
	rt3 := stubSession()
	h.p.handleBan(context.Background(), banCmd("7300007", strOpt("duration", "400d")))
	if len(h.ops.bans) != 0 || !strings.Contains(rt3.said(), "at most") {
		t.Errorf("an over-long ban went through: %s", rt3.said())
	}

	stubSession()
	h.p.handleBan(context.Background(), banCmd("7300007", boolOpt("permanent", true)))
	if _, ok := h.ops.bans["7300007"]; !ok {
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
	stubSession()
	h.p.handleBan(context.Background(), banCmd("7300017", strOpt("duration", "1d")))
	if _, ok := h.ops.bans["7300017"]; !ok {
		t.Fatal("could not ban somebody who left")
	}
	rt := stubSession()
	h.p.handleBan(context.Background(), banCmd("7300017", strOpt("duration", "2d")))
	if len(h.store.all()) != 1 || !strings.Contains(rt.said(), "already banned") {
		t.Errorf("entries %d said %s", len(h.store.all()), rt.said())
	}
}

func TestARefusedBanVoidsItsEntry(t *testing.T) {
	h := newHarness()
	h.ops.addMember("7300007")
	h.ops.banErr = errors.New("missing permissions")
	stubSession()
	h.p.handleBan(context.Background(), banCmd("7300007", strOpt("duration", "1d")))
	if e := h.store.all()[0]; !e.Voided() || e.Standing(testNow) {
		t.Errorf("entry = %+v", e)
	}
	if h.sched.has(sweepKey(testGuild)) {
		t.Error("a voided ban armed the sweep")
	}
}

func TestUnbanLiftsTheLedgersBanTooAndDisarmsTheSweep(t *testing.T) {
	h := newHarness()
	h.ops.addMember("7300007")
	stubSession()
	h.p.handleBan(context.Background(), banCmd("7300007", strOpt("duration", "7d")))

	rt := stubSession()
	h.p.handleUnban(context.Background(), interaction("unban", userOpt("user", "7300007"), strOpt("reason", "appeal accepted")))
	if _, still := h.ops.bans["7300007"]; still {
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
	rt3 := stubSession()
	h.p.handleUnban(context.Background(), interaction("unban", userOpt("user", "7300015"), strOpt("reason", "r")))
	if len(h.store.all()) != 2 || !strings.Contains(rt3.said(), "not banned") {
		t.Errorf("entries %d said %s", len(h.store.all()), rt3.said())
	}
}

// --- the sweep ---------------------------------------------------------------------------

func TestTheSweepLiftsServedBansAndOnlyThose(t *testing.T) {
	h := newHarness()
	h.ops.addMember("7300007")
	h.ops.addMember("7300008")
	stubSession()
	h.p.handleBan(context.Background(), banCmd("7300007", strOpt("duration", "1h")))
	stubSession()
	h.p.handleBan(context.Background(), banCmd("7300008", strOpt("duration", "3h")))

	h.p.now = func() time.Time { return testNow.Add(2 * time.Hour) }
	if err := h.sched.RunNow(context.Background(), sweepKey(testGuild)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(h.ops.unbanned) != 1 || h.ops.unbanned[0] != "7300007" {
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
	if unban == nil || unban.ActorID != core.ActorSystem || unban.Source != SourceSweep || unban.UserID != "7300007" {
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
	h.ops.addMember("7300007")
	h.ops.addMember("7300008")
	stubSession()
	h.p.handleBan(context.Background(), banCmd("7300007", strOpt("duration", "1h")))
	stubSession()
	h.p.handleBan(context.Background(), banCmd("7300008", strOpt("duration", "1h")))
	// A human unbanned u1 in the client.
	delete(h.ops.bans, "7300007")
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
	if unbans != 1 || len(h.ops.unbanned) != 1 || h.ops.unbanned[0] != "7300008" {
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
	_, _ = h.store.Insert(context.Background(), Entry{GuildID: testGuild, UserID: "7300007", Kind: KindBan, ActorID: modID, Source: SourceCommand, Duration: time.Hour, EndsAt: &ends})
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

func auditEvent(actor, target string, action discord.AuditLogEvent, reason string, changes ...discord.AuditLogChange) discord.AuditLogEntry {
	t := core.ID(target)
	e := discord.AuditLogEntry{
		ID: core.ID(target + strconv.Itoa(int(action)%10)), UserID: core.ID(actor), TargetID: &t,
		ActionType: action, Changes: changes,
	}
	if reason != "" {
		e.Reason = &reason
	}
	return e
}

// change is one audit-log change of key to v, as Discord writes it.
func change(key discord.AuditLogChangeKey, v any) discord.AuditLogChange {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return discord.AuditLogChange{Key: key, NewValue: raw}
}

func TestBansKicksAndTimeoutsDoneThroughDiscordAreRecorded(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	h.p.HandleAuditLogEntry(ctx, testGuild, auditEvent("7300005", "7300007", discord.AuditLogEventMemberBanAdd, "raiding"))
	h.p.HandleAuditLogEntry(ctx, testGuild, auditEvent("7300005", "7300008", discord.AuditLogEventMemberKick, ""))
	until := testNow.Add(time.Hour).Format(time.RFC3339Nano)
	key := discord.AuditLogChangeKeyCommunicationDisabledUntil
	h.p.HandleAuditLogEntry(ctx, testGuild, auditEvent("7300005", "7300009", discord.AuditLogEventMemberUpdate, "",
		change(key, until)))
	// A member update that is not a timeout is ignored.
	nick := discord.AuditLogChangeKeyNick
	h.p.HandleAuditLogEntry(ctx, testGuild, auditEvent("7300005", "7300010", discord.AuditLogEventMemberUpdate, "",
		change(nick, "new nick")))
	// So is anything merlin did herself.
	h.p.HandleAuditLogEntry(ctx, testGuild, auditEvent("7300003", "7300011", discord.AuditLogEventMemberBanAdd, "rapsheet #9"))
	// And a redelivery.
	h.p.HandleAuditLogEntry(ctx, testGuild, auditEvent("7300005", "7300007", discord.AuditLogEventMemberBanAdd, "raiding"))

	entries := h.store.all()
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3: %+v", len(entries), entries)
	}
	ban, kick, timeout := entries[0], entries[1], entries[2]
	if ban.Kind != KindBan || ban.EndsAt != nil || ban.ActorID != "7300005" || ban.Reason != "raiding" || ban.Source != SourceDiscord || ban.Points != 25 {
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
	h.ops.addMember("7300007")
	stubSession()
	h.p.handleBan(context.Background(), banCmd("7300007", strOpt("duration", "7d")))
	h.p.HandleAuditLogEntry(context.Background(), testGuild, auditEvent("7300005", "7300007", discord.AuditLogEventMemberBanRemove, "appeal"))
	entries := h.store.all()
	if entries[0].LiftedAt == nil || entries[1].Kind != KindUnban || entries[1].ActorID != "7300005" {
		t.Errorf("entries = %+v", entries)
	}
	// A cleared timeout arrives as a release.
	key := discord.AuditLogChangeKeyCommunicationDisabledUntil
	h.p.HandleAuditLogEntry(context.Background(), testGuild, auditEvent("7300005", "7300009", discord.AuditLogEventMemberUpdate, "",
		change(key, nil)))
	if last := h.store.all()[2]; last.Kind != KindRelease {
		t.Errorf("cleared timeout = %+v", last)
	}
}

func TestAuditLogIngestionRespectsTheGateAndIgnoresJunk(t *testing.T) {
	h := newHarness()
	h.p.gate = fakeGate{disabled: map[string]bool{testGuild: true}}
	h.p.HandleAuditLogEntry(context.Background(), testGuild, auditEvent("7300005", "7300007", discord.AuditLogEventMemberBanAdd, "r"))
	h.p.gate = nil
	h.p.HandleAuditLogEntry(context.Background(), testGuild, discord.AuditLogEntry{})
	h.p.HandleAuditLogEntry(context.Background(), "", auditEvent("7300005", "7300007", discord.AuditLogEventMemberBanAdd, "r"))
	h.p.HandleAuditLogEntry(context.Background(), testGuild, auditEvent("7300005", "7300027", discord.AuditLogEventChannelDelete, ""))
	if n := len(h.store.all()); n != 0 {
		t.Errorf("%d entries from ignored events", n)
	}
}

func TestStatusReportsTheAuditLogPermission(t *testing.T) {
	h := newHarness()
	h.ops.addMember("7300003", "7300019")
	h.ops.roles = []discord.Role{{ID: core.ID(testGuild)}, {ID: core.ID("7300019"), Permissions: discord.PermissionManageRoles}}
	rt := stubSession()
	h.p.handleStatus(context.Background(), interaction("status"))
	if !strings.Contains(rt.said(), "does not hold View Audit Log") {
		t.Errorf("got %s", rt.said())
	}
	h.ops.roles[1].Permissions |= discord.PermissionViewAuditLog
	rt2 := stubSession()
	h.p.handleStatus(context.Background(), interaction("status"))
	if !strings.Contains(rt2.said(), "View Audit Log held") {
		t.Errorf("got %s", rt2.said())
	}
}
