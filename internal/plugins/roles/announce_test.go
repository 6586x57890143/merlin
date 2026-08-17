package roles

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
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
	if sent.data.Embed != nil {
		t.Fatalf("expected a plain message, not an embed, got %+v", sent.data.Embed)
	}
	if !strings.Contains(sent.data.Content, "<@u1>") {
		t.Fatalf("expected the jailed member mentioned in the announcement, got %q", sent.data.Content)
	}
}

func TestAnnounceJailReasonBecomesASubtextLine(t *testing.T) {
	ops := newFakeOps()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	p.announceJail(context.Background(), "g1", "chan1", []string{"u1"}, time.Hour, "spamming links")

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

	p.announceJail(context.Background(), "g1", "chan1", []string{"u1"}, time.Hour, "line one\nline two")

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
	p.announceJail(context.Background(), "g1", "chan1", []string{"u1"}, time.Hour, long)

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

	p.announceJail(context.Background(), "g1", "chan1", []string{"u1"}, time.Hour, "")

	if strings.Contains(ops.dmSends[0].data.Content, "-#") {
		t.Fatalf("expected no subtext line when no reason was given, got %q", ops.dmSends[0].data.Content)
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
	if !strings.Contains(sent.data.Content, "<@u1>") {
		t.Fatalf("expected the released member mentioned in the announcement, got %q", sent.data.Content)
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

// --- announceDestinations ---

func TestAnnounceDestinationsIncludesOnlyAllowlistedTextChannels(t *testing.T) {
	ops := newFakeOps()
	ops.channel["text1"] = &discordgo.Channel{ID: "text1", GuildID: "g1", Type: discordgo.ChannelTypeGuildText}
	ops.channel["news1"] = &discordgo.Channel{ID: "news1", GuildID: "g1", Type: discordgo.ChannelTypeGuildNews}
	ops.channel["voice1"] = &discordgo.Channel{ID: "voice1", GuildID: "g1", Type: discordgo.ChannelTypeGuildVoice}
	ops.channel["stage1"] = &discordgo.Channel{ID: "stage1", GuildID: "g1", Type: discordgo.ChannelTypeGuildStageVoice}
	ops.channel["forum1"] = &discordgo.Channel{ID: "forum1", GuildID: "g1", Type: discordgo.ChannelTypeGuildForum}
	settings := newFakeSettings()
	settings.allowed["g1"] = []string{"text1", "news1", "voice1", "stage1", "forum1", "missing1"}
	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())

	got := p.announceDestinations("g1", "invoking1")

	want := []string{"invoking1", "text1", "news1"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i, id := range want {
		if got[i] != id {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
}

func TestAnnounceDestinationsDedupesWhenInvokingChannelIsAllowlisted(t *testing.T) {
	ops := newFakeOps()
	ops.channel["chan1"] = &discordgo.Channel{ID: "chan1", GuildID: "g1", Type: discordgo.ChannelTypeGuildText}
	settings := newFakeSettings()
	settings.allowed["g1"] = []string{"chan1"}
	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())

	got := p.announceDestinations("g1", "chan1")

	if len(got) != 1 || got[0] != "chan1" {
		t.Fatalf("expected chan1 exactly once, got %v", got)
	}
}

func TestAnnounceDestinationsFallsBackToInvokingChannelOnListError(t *testing.T) {
	ops := newFakeOps()
	ops.guildChannelsErr = context.DeadlineExceeded
	settings := newFakeSettings()
	settings.allowed["g1"] = []string{"text1"}
	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())

	got := p.announceDestinations("g1", "chan1")

	if len(got) != 1 || got[0] != "chan1" {
		t.Fatalf("expected only the invoking channel when the channel list can't be fetched, got %v", got)
	}
}

func TestAnnounceDestinationsIsJustInvokingChannelWhenNothingAllowlisted(t *testing.T) {
	ops := newFakeOps()
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	got := p.announceDestinations("g1", "chan1")

	if len(got) != 1 || got[0] != "chan1" {
		t.Fatalf("expected only the invoking channel, got %v", got)
	}
}

func TestAnnounceJailBroadcastsToAllowlistedTextChannelToo(t *testing.T) {
	ops := newFakeOps()
	ops.channel["waiting-room"] = &discordgo.Channel{ID: "waiting-room", GuildID: "g1", Type: discordgo.ChannelTypeGuildText}
	settings := newFakeSettings()
	settings.allowed["g1"] = []string{"waiting-room"}
	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())

	p.announceJail(context.Background(), "g1", "chan1", []string{"u1"}, time.Hour, "")

	if len(ops.dmSends) != 2 {
		t.Fatalf("expected the announcement posted to both the invoking channel and the waiting room, got %d sends", len(ops.dmSends))
	}
	posted := map[string]bool{}
	for _, s := range ops.dmSends {
		posted[s.channelID] = true
	}
	if !posted["chan1"] || !posted["waiting-room"] {
		t.Fatalf("expected posts to chan1 and waiting-room, got %+v", posted)
	}
}
