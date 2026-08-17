package roles

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAnnounceJailPostsToInvokingChannel(t *testing.T) {
	ops := newFakeOps()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	p.announceJail(context.Background(), "g1", "chan1", []string{"u1"}, time.Hour, "spamming")

	if len(ops.dmSends) != 1 {
		t.Fatalf("expected one channel post, got %d", len(ops.dmSends))
	}
	sent := ops.dmSends[0]
	if sent.channelID != "chan1" {
		t.Fatalf("expected the post to land in the invoking channel chan1, got %q", sent.channelID)
	}
	if sent.data.Embed == nil || !strings.Contains(sent.data.Embed.Description, "<@u1>") {
		t.Fatalf("expected the jailed member mentioned in the announcement, got %+v", sent.data.Embed)
	}
	if len(sent.data.Embed.Fields) != 1 || sent.data.Embed.Fields[0].Value != "spamming" {
		t.Fatalf("expected the reason attached as its own field, got %+v", sent.data.Embed.Fields)
	}
}

func TestAnnounceJailOmitsReasonFieldWhenNoneGiven(t *testing.T) {
	ops := newFakeOps()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	p.announceJail(context.Background(), "g1", "chan1", []string{"u1"}, time.Hour, "")

	if len(ops.dmSends) != 1 {
		t.Fatalf("expected one channel post, got %d", len(ops.dmSends))
	}
	if fields := ops.dmSends[0].data.Embed.Fields; len(fields) != 0 {
		t.Fatalf("expected no fields when no reason was given, got %+v", fields)
	}
}

func TestAnnounceJailNoOpWhenNobodyJailed(t *testing.T) {
	ops := newFakeOps()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	p.announceJail(context.Background(), "g1", "chan1", nil, time.Hour, "")

	if len(ops.dmSends) != 0 {
		t.Fatalf("expected no channel post for an empty jail batch, got %d", len(ops.dmSends))
	}
}

func TestAnnounceReleasePostsToInvokingChannel(t *testing.T) {
	ops := newFakeOps()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	p.announceRelease(context.Background(), "g1", "chan1", []string{"u1"})

	if len(ops.dmSends) != 1 {
		t.Fatalf("expected one channel post, got %d", len(ops.dmSends))
	}
	sent := ops.dmSends[0]
	if sent.channelID != "chan1" {
		t.Fatalf("expected the post to land in the invoking channel chan1, got %q", sent.channelID)
	}
	if sent.data.Embed == nil || !strings.Contains(sent.data.Embed.Description, "<@u1>") {
		t.Fatalf("expected the released member mentioned in the announcement, got %+v", sent.data.Embed)
	}
}

func TestAnnounceReleaseNoOpWhenNobodyReleased(t *testing.T) {
	ops := newFakeOps()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	p.announceRelease(context.Background(), "g1", "chan1", nil)

	if len(ops.dmSends) != 0 {
		t.Fatalf("expected no channel post for an empty release batch, got %d", len(ops.dmSends))
	}
}
