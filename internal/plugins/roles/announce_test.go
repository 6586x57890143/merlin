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

	p.announceJail(context.Background(), "7000001", "7000602", []string{"7000102"}, time.Hour, "spamming", jailSentence)

	if len(ops.dmSends) != 1 {
		t.Fatalf("expected one channel post, got %d", len(ops.dmSends))
	}
	sent := ops.dmSends[0]
	if sent.channelID != "7000602" {
		t.Fatalf("expected the post to land in the invoking channel chan1, got %q", sent.channelID)
	}
	if len(sent.data.Embeds) > 0 {
		t.Fatalf("expected a plain message, not an embed, got %+v", sent.data.Embeds[0])
	}
	if !strings.Contains(sent.data.Content, "<@7000102>") {
		t.Fatalf("expected the jailed member mentioned in the announcement, got %q", sent.data.Content)
	}
}

func TestAnnounceJailReasonBecomesASubtextLine(t *testing.T) {
	ops := newFakeOps()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	p.announceJail(context.Background(), "7000001", "7000602", []string{"7000102"}, time.Hour, "spamming links", jailSentence)

	content := ops.dmSends[0].data.Content
	if !strings.Contains(content, "\n-# reason: spamming links") {
		t.Fatalf("expected the reason on its own subtext line, got %q", content)
	}
}

// A reason with embedded newlines could otherwise break out of the single
// subtext line it's meant to occupy, gluing an unprefixed second line of
// ordinary-looking text onto what should read as fine print.
func TestAnnounceJailReasonNewlinesAreCollapsed(t *testing.T) {
	ops := newFakeOps()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	p.announceJail(context.Background(), "7000001", "7000602", []string{"7000102"}, time.Hour, "line one\nline two", jailSentence)

	content := ops.dmSends[0].data.Content
	if strings.Contains(content, "\n-# reason: line one\nline two") {
		t.Fatalf("expected the reason's newline collapsed into the single subtext line, got %q", content)
	}
	if !strings.Contains(content, "-# reason: line one line two") {
		t.Fatalf("expected the reason preserved on one subtext line, got %q", content)
	}
}

func TestAnnounceJailReasonIsTruncated(t *testing.T) {
	ops := newFakeOps()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	long := strings.Repeat("a", maxAnnounceReasonLen*2)
	p.announceJail(context.Background(), "7000001", "7000602", []string{"7000102"}, time.Hour, long, jailSentence)

	content := ops.dmSends[0].data.Content
	if strings.Contains(content, long) {
		t.Fatal("expected an oversized reason to be truncated, got the full string in the message")
	}
	if !strings.Contains(content, "(truncated)") {
		t.Fatalf("expected a truncation marker, got %q", content)
	}
}

func TestAnnounceJailOmitsReasonLineWhenNoneGiven(t *testing.T) {
	ops := newFakeOps()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	p.announceJail(context.Background(), "7000001", "7000602", []string{"7000102"}, time.Hour, "", jailSentence)

	if strings.Contains(ops.dmSends[0].data.Content, "-#") {
		t.Fatalf("expected no subtext line when no reason was given, got %q", ops.dmSends[0].data.Content)
	}
}

func TestAnnounceJailNoOpWhenNobodyJailed(t *testing.T) {
	ops := newFakeOps()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	p.announceJail(context.Background(), "7000001", "7000602", nil, time.Hour, "", jailSentence)

	if len(ops.dmSends) != 0 {
		t.Fatalf("expected no channel post for an empty jail batch, got %d", len(ops.dmSends))
	}
}

func TestAnnounceReleasePostsToInvokingChannel(t *testing.T) {
	ops := newFakeOps()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	p.announceRelease(context.Background(), "7000001", "7000602", []string{"7000102"}, jailSentence)

	if len(ops.dmSends) != 1 {
		t.Fatalf("expected one channel post, got %d", len(ops.dmSends))
	}
	sent := ops.dmSends[0]
	if sent.channelID != "7000602" {
		t.Fatalf("expected the post to land in the invoking channel chan1, got %q", sent.channelID)
	}
	if !strings.Contains(sent.data.Content, "<@7000102>") {
		t.Fatalf("expected the released member mentioned in the announcement, got %q", sent.data.Content)
	}
}

func TestAnnounceReleaseNoOpWhenNobodyReleased(t *testing.T) {
	ops := newFakeOps()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	p.announceRelease(context.Background(), "7000001", "7000602", nil, jailSentence)

	if len(ops.dmSends) != 0 {
		t.Fatalf("expected no channel post for an empty release batch, got %d", len(ops.dmSends))
	}
}

// --- announceDestinations ---

func TestAnnounceDestinationsIncludesConfiguredAnnounceChannel(t *testing.T) {
	settings := newFakeSettings()
	settings.announce["7000001"] = "7000621"
	p := newTestPlugin(newFakeOps(), newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())

	got := p.announceDestinations("7000001", "7000623")

	want := []string{"7000623", "7000621"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestAnnounceDestinationsDedupesWhenAnnounceChannelIsTheInvokingOne(t *testing.T) {
	settings := newFakeSettings()
	settings.announce["7000001"] = "7000602"
	p := newTestPlugin(newFakeOps(), newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())

	got := p.announceDestinations("7000001", "7000602")

	if len(got) != 1 || got[0] != "7000602" {
		t.Fatalf("expected chan1 exactly once, got %v", got)
	}
}

// The visibility allowlist decides what a jailed member can see, not where a
// jail is announced: several visible rooms must not each get a copy.
func TestAnnounceDestinationsIgnoresTheVisibilityAllowlist(t *testing.T) {
	settings := newFakeSettings()
	settings.allowed["7000001"] = []string{"7000609", "7000610"}
	p := newTestPlugin(newFakeOps(), newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())

	got := p.announceDestinations("7000001", "7000602")

	if len(got) != 1 || got[0] != "7000602" {
		t.Fatalf("expected only the invoking channel, got %v", got)
	}
}

func TestAnnounceJailAlsoPostsToTheConfiguredAnnounceChannel(t *testing.T) {
	ops := newFakeOps()
	settings := newFakeSettings()
	settings.announce["7000001"] = "7000621"
	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())

	p.announceJail(context.Background(), "7000001", "7000602", []string{"7000102"}, time.Hour, "", jailSentence)

	if len(ops.dmSends) != 2 {
		t.Fatalf("expected the announcement posted to both the invoking channel and the announce channel, got %d sends", len(ops.dmSends))
	}
	posted := map[string]bool{}
	for _, s := range ops.dmSends {
		posted[s.channelID] = true
	}
	if !posted["7000602"] || !posted["7000621"] {
		t.Fatalf("expected posts to chan1 and jail-talk, got %+v", posted)
	}
}
