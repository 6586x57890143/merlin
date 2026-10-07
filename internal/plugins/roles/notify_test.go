package roles

import (
	"context"
	"errors"
	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/voice"
	"strings"
	"testing"
	"time"
)

// A jailed member should be told what happened, when it ends, and where.
// "Where" matters more than it looks: most people are in a lot of servers,
// and a DM that just says access was removed is not actionable.
func TestJailNoticeSaysWhereAndWhenItEnds(t *testing.T) {
	ops := newFakeOps()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	releaseAt := fixedNow.Add(3 * time.Hour)
	p.notifyJailed(context.Background(), "g1", "u1", &releaseAt, "", jailSentence)

	if len(ops.dmSends) != 1 {
		t.Fatalf("DMs sent = %d, want 1", len(ops.dmSends))
	}
	sent := ops.dmSends[0]
	if sent.channelID != "dm:u1" {
		t.Errorf("notice went to %q, not the member's DM channel", sent.channelID)
	}
	if sent.data.Embed == nil {
		t.Fatal("notice was not an embed")
	}
	body := sent.data.Embed.Description
	if !strings.Contains(body, "The Melting Pot") {
		t.Errorf("notice never names the server: %q", body)
	}
	// Discord's relative-timestamp markup, so the reader sees it in their
	// own timezone rather than having to convert from UTC while annoyed.
	if !strings.Contains(body, "<t:") {
		t.Errorf("notice does not carry a rendered timestamp: %q", body)
	}
	if strings.ContainsAny(body, "{}") {
		t.Errorf("notice leaked a placeholder: %q", body)
	}
}

// A reason, when a mod gave one, rides in its own field rather than being
// substituted into the sentence: an optional placeholder would make every
// line carrying it fall back exactly when it is missing.
func TestJailNoticeCarriesTheReasonSeparately(t *testing.T) {
	ops := newFakeOps()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	p.notifyJailed(context.Background(), "g1", "u1", ptrTime(fixedNow.Add(time.Hour)), "spamming the same link", jailSentence)
	if len(ops.dmSends) != 1 {
		t.Fatalf("DMs sent = %d, want 1", len(ops.dmSends))
	}
	fields := ops.dmSends[0].data.Embed.Fields
	if len(fields) != 1 || !strings.Contains(fields[0].Value, "spamming the same link") {
		t.Errorf("the reason did not survive into the notice: %+v", fields)
	}

	// With no reason there should be no empty field hanging off the embed.
	ops.dmSends = nil
	p.notifyJailed(context.Background(), "g1", "u2", ptrTime(fixedNow.Add(time.Hour)), "", jailSentence)
	if got := len(ops.dmSends[0].data.Embed.Fields); got != 0 {
		t.Errorf("fields = %d with no reason given, want 0", got)
	}
}

// Plenty of people have DMs closed. That must never turn a successful
// release into a failed one: the roles are already restored by the time the
// notice is attempted, and reporting failure would have the sweep retry a
// release that already happened.
func TestAFailedDMDoesNotFailTheRelease(t *testing.T) {
	ops := newFakeOps()
	store := newFakeStore()
	p := newTestPlugin(ops, store, newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	ops.dmErr = errors.New("cannot send messages to this user")

	rec := JailRecord{
		GuildID:         "g1",
		UserID:          "u1",
		SnapshotRoleIDs: []string{"role-a"},
		JailRoleID:      "jail-role",
		JailedAt:        fixedNow,
	}
	ops.setMember("g1", "u1", []string{"jail-role"})
	store.jails[jailKey("g1", "u1")] = rec

	if err := p.releaseJail(context.Background(), "g1", "u1", rec, core.ActorSystem); err != nil {
		t.Fatalf("a member with DMs closed made the release fail: %v", err)
	}
	if _, ok := store.jails[jailKey("g1", "u1")]; ok {
		t.Error("the jail record survived a successful release")
	}
	if len(ops.dmSends) != 0 {
		t.Errorf("a DM was recorded despite the channel failing to open: %d", len(ops.dmSends))
	}
}

// A guild lookup that fails is a reason to be vague, not a reason to say
// nothing: the member still needs to know their roles are back.
func TestNoticeStillSendsWhenTheGuildNameIsUnavailable(t *testing.T) {
	ops := newFakeOps()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	ops.guildErr = errors.New("500 internal server error")
	p.notifyReleased(context.Background(), "g1", "u1", jailSentence)

	if len(ops.dmSends) != 1 {
		t.Fatalf("DMs sent = %d, want 1", len(ops.dmSends))
	}
	body := ops.dmSends[0].data.Embed.Description
	if !strings.Contains(body, "this server") {
		t.Errorf("expected the vague fallback naming, got %q", body)
	}
	if strings.ContainsAny(body, "{}") {
		t.Errorf("notice leaked a placeholder: %q", body)
	}
}

// Every line that says when a sentence ends needs a twin for a sentence that
// never does, or a forever jail would DM a member that it was temporary.
func TestEverySentenceLineHasAForeverTwin(t *testing.T) {
	for _, sn := range []sentence{jailSentence, vacationSentence} {
		for _, k := range []voice.Key{sn.noticeKey, sn.announceKey, sn.intoKey, sn.intoAnnKey} {
			if foreverKeys[k] == "" {
				t.Errorf("%s has no forever twin in foreverKeys", k)
			}
		}
	}
}

// A forever sentence's DM and channel post name no end, in every path: a
// fresh jail or vacation, and a transfer onto one.
func TestForeverSentenceNamesNoEnd(t *testing.T) {
	for _, sn := range []sentence{jailSentence, vacationSentence} {
		ops := newFakeOps()
		p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
		p.notifyJailed(context.Background(), "g1", "u1", nil, "r", sn)
		p.notifyMoved(context.Background(), "g1", "u1", nil, sn, "r")
		p.announceJail(context.Background(), "g1", "chan1", []string{"u1"}, foreverSentence, "r", sn)
		p.announceMoved(context.Background(), "g1", "chan1", []string{"u1"}, nil, sn)
		if len(ops.dmSends) != 4 {
			t.Fatalf("%s: sends = %d, want 4", sn.name, len(ops.dmSends))
		}
		for _, s := range ops.dmSends {
			body := s.data.Content
			if s.data.Embed != nil {
				body = s.data.Embed.Description
			}
			if body == "" || strings.Contains(body, "<t:") || strings.ContainsAny(body, "{}") ||
				strings.Contains(body, "temporary") || strings.Contains(body, "isn't permanent") {
				t.Errorf("%s: forever sentence posted %q", sn.name, body)
			}
		}
	}
}
