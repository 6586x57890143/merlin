package discordguard

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeGate struct {
	paused map[string]bool
	dryRun map[string]bool
}

func (f fakeGate) WritesPaused(guildID string) bool { return f.paused[guildID] }
func (f fakeGate) WritesDryRun(guildID string) bool { return f.dryRun[guildID] }

// callEveryWrite exercises every gated method so a newly added one can't
// quietly skip the check: a write that forgets allow() reaches the wire
// while the test expects nothing there.
func callEveryWrite(o *GuildOps) []error {
	errs := []error{
		o.UpdatePermissionOverwrite(2, 3, discord.RolePermissionOverwriteUpdate{}),
		o.DeleteChannel(2),
		o.PinMessage(2, 4),
		o.DeleteMessage(2, 4),
		o.DisconnectMember(100, 3),
		o.AddMemberRole(100, 3, 6),
		o.RemoveMemberRole(100, 3, 6),
		o.DeletePermissionOverwrite(2, 3),
		o.AddBan(100, 3, 0, "r"),
		o.DeleteBan(100, 3),
		o.RemoveMember(100, 3, "r"),
		o.TimeoutMember(100, 3, nil),
	}
	add := func(_ any, err error) { errs = append(errs, err) }
	add(o.CreateGuildChannel(100, discord.GuildTextChannelCreate{Name: "n"}))
	add(o.UpdateChannel(2, discord.GuildTextChannelUpdate{}))
	add(o.CreateMessage(2, discord.MessageCreate{}))
	add(o.CreateWebhook(2, "n"))
	add(o.CreateWebhookMessage(5, "t", discord.WebhookMessageCreate{}, false))
	add(o.UpdateMember(100, 3, discord.MemberUpdate{}))
	add(o.CreateRole(100, discord.RoleCreate{}))
	add(o.UpdateRolePositions(100, nil))
	add(o.CreatePostInThreadChannel(2, discord.ThreadChannelPostCreate{}))
	add(o.UpdateMessage(2, 4, discord.MessageUpdate{}))
	return errs
}

func TestPausedRefusesEveryWrite(t *testing.T) {
	g, w := restGuard(fakeGate{paused: map[string]bool{"100": true}}, `{}`)
	for _, err := range callEveryWrite(g.For("100")) {
		if !errors.Is(err, ErrPaused) {
			t.Errorf("got %v, want ErrPaused", err)
		}
	}
	if len(w.requests) != 0 {
		t.Errorf("paused guard let writes through to Discord: %v", w.requests)
	}
}

func TestGlobalPauseOverridesGuildSettings(t *testing.T) {
	g, w := restGuard(fakeGate{}, `{}`)
	g.PauseAll(true)
	for _, err := range callEveryWrite(g.For("100")) {
		if !errors.Is(err, ErrPaused) {
			t.Errorf("got %v, want ErrPaused", err)
		}
	}
	if len(w.requests) != 0 {
		t.Errorf("global pause let writes through: %v", w.requests)
	}

	// And it can be released without a restart.
	g.PauseAll(false)
	if err := g.For("100").DeleteChannel(2); err != nil {
		t.Fatalf("after PauseAll(false): %v", err)
	}
	if len(w.requests) != 1 {
		t.Errorf("%d requests after releasing pause, want 1", len(w.requests))
	}
}

func TestDryRunRefusesEveryWrite(t *testing.T) {
	g, w := restGuard(fakeGate{dryRun: map[string]bool{"100": true}}, `{}`)
	for _, err := range callEveryWrite(g.For("100")) {
		if !errors.Is(err, ErrDryRun) {
			t.Errorf("got %v, want ErrDryRun", err)
		}
	}
	if len(w.requests) != 0 {
		t.Errorf("dry-run guard let writes through to Discord: %v", w.requests)
	}
}

// Pausing must not blind an admin: every inspect path still has to work, or
// whoever hit the emergency stop can't see what the bot thinks is going on.
func TestReadsAreNeverGated(t *testing.T) {
	g, w := restGuard(fakeGate{paused: map[string]bool{"100": true}, dryRun: map[string]bool{"100": true}}, `[]`)
	g.PauseAll(true)
	o := g.For("100")
	_, e1 := o.GetGuildChannels(100)
	_, e2 := o.GetRoles(100)
	_, e3 := o.GetMessages(2, 0, 1)
	_, e4 := o.GetWebhooks(2)
	_, e5 := o.GetMembers(100, 1, 0)
	for _, err := range []error{e1, e2, e3, e4, e5} {
		if err != nil {
			t.Errorf("a read failed: %v", err)
		}
	}
	// Reads whose answer is one object; only that they were sent matters.
	_, _ = o.GetChannel(2)
	_, _ = o.GetActiveGuildThreads(100)
	_, _ = o.GetPublicArchivedThreads(2, time.Time{}, 1)
	_, _ = o.GetMessage(2, 4)
	_, _ = o.GetMember(100, 3)
	_, _ = o.GetGuild(100)
	_, _ = o.GetUser(3)
	_, _ = o.CreateDMChannel(3)
	if len(w.requests) != 13 {
		t.Errorf("%d reads reached Discord, want 13", len(w.requests))
	}
}

// Every message this bot sends goes out with mentions suppressed, even one
// whose caller asked otherwise, and suppression must not alter the text.
func TestMessageSendsSuppressMentions(t *testing.T) {
	g, w := restGuard(fakeGate{}, `{"id":"9","channel_id":"2"}`)
	_, err := g.For("100").CreateMessage(2, discord.MessageCreate{
		Content:         "hey @everyone <@123> <@&456>",
		AllowedMentions: &discord.AllowedMentions{Parse: []discord.AllowedMentionType{discord.AllowedMentionTypeEveryone}, Users: []snowflake.ID{123}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(w.requests) != 1 {
		t.Fatalf("sent %v", w.requests)
	}
	var sent struct {
		Content         string `json:"content"`
		AllowedMentions struct {
			Parse []string `json:"parse"`
			Users []string `json:"users"`
		} `json:"allowed_mentions"`
	}
	body := w.requests[0][strings.IndexByte(w.requests[0], '{'):]
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Content != "hey @everyone <@123> <@&456>" {
		t.Errorf("content was rewritten: %q", sent.Content)
	}
	if sent.AllowedMentions.Parse == nil || len(sent.AllowedMentions.Parse) != 0 || len(sent.AllowedMentions.Users) != 0 {
		t.Errorf("mentions not suppressed: %s", body)
	}
}

// A call naming ID 0, what core.ID makes of a stored ID that is not a
// snowflake, is refused before anything is sent, read or write.
func TestEveryCallRefusesIDZero(t *testing.T) {
	g, w := restGuard(fakeGate{}, `{}`)
	o := g.For("100")
	errs := []error{
		o.UpdatePermissionOverwrite(0, 3, discord.RolePermissionOverwriteUpdate{}),
		o.DeleteChannel(0),
		o.PinMessage(2, 0),
		o.DeleteMessage(0, 4),
		o.DisconnectMember(100, 0),
		o.AddMemberRole(100, 3, 0),
		o.RemoveMemberRole(0, 3, 6),
		o.DeletePermissionOverwrite(2, 0),
		o.AddBan(100, 0, 0, "r"),
		o.DeleteBan(0, 3),
		o.RemoveMember(100, 0, "r"),
		o.TimeoutMember(0, 3, nil),
	}
	add := func(_ any, err error) { errs = append(errs, err) }
	add(o.GetChannel(0))
	add(o.GetGuildChannels(0))
	add(o.GetActiveGuildThreads(0))
	add(o.GetRoles(0))
	add(o.GetPublicArchivedThreads(0, time.Time{}, 1))
	add(o.GetMessage(2, 0))
	add(o.GetMessages(0, 0, 1))
	add(o.GetWebhooks(0))
	add(o.GetMember(100, 0))
	add(o.GetGuild(0))
	add(o.GetMembers(0, 1, 0))
	add(o.GetUser(0))
	add(o.CreateDMChannel(0))
	add(o.CreateGuildChannel(0, discord.GuildTextChannelCreate{}))
	add(o.UpdateChannel(0, discord.GuildTextChannelUpdate{}))
	add(o.CreateMessage(0, discord.MessageCreate{}))
	add(o.CreateWebhook(0, "n"))
	add(o.CreateWebhookMessage(0, "t", discord.WebhookMessageCreate{}, false))
	add(o.UpdateMember(0, 3, discord.MemberUpdate{}))
	add(o.CreateRole(0, discord.RoleCreate{}))
	add(o.UpdateRolePositions(0, nil))
	add(o.CreatePostInThreadChannel(0, discord.ThreadChannelPostCreate{}))
	add(o.UpdateMessage(2, 0, discord.MessageUpdate{}))
	for i, err := range errs {
		if !errors.Is(err, errNoID) {
			t.Errorf("call %d: got %v, want errNoID", i, err)
		}
	}
	if len(w.requests) != 0 {
		t.Errorf("ID 0 reached Discord: %v", w.requests)
	}
}

// The guard is per guild: pausing one server must not stop another.
func TestPauseIsScopedToItsGuild(t *testing.T) {
	g, w := restGuard(fakeGate{paused: map[string]bool{"100": true}}, `{}`)
	if err := g.For("100").DeleteChannel(2); !errors.Is(err, ErrPaused) {
		t.Errorf("paused guild: got %v, want ErrPaused", err)
	}
	if err := g.For("200").DeleteChannel(2); err != nil {
		t.Errorf("unpaused guild: %v", err)
	}
	if len(w.requests) != 1 {
		t.Errorf("%d requests, want 1 (only the unpaused guild)", len(w.requests))
	}
}

func TestSkipped(t *testing.T) {
	if !Skipped(ErrPaused) || !Skipped(ErrDryRun) {
		t.Error("Skipped must recognize both sentinels")
	}
	// A real failure must never be mistaken for a deliberate skip, or a
	// Scheduler job would report success for work that actually broke.
	if Skipped(errors.New("500 internal server error")) {
		t.Error("Skipped must not swallow genuine failures")
	}
	if Skipped(nil) {
		t.Error("Skipped(nil) must be false")
	}
}
