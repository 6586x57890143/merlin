package discordguard

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeSession counts every call that reaches Discord. The whole point of the
// guard is that this counter stays at zero while paused or rehearsing, so
// the tests below assert on it rather than on the returned errors alone.
type fakeSession struct {
	lastWebhook *discordgo.WebhookParams
	writes int
	reads  int
	// sends records the full payload of every message send, so a test can
	// assert on what was actually put on the wire (mention suppression)
	// rather than only that a send happened.
	sends []*discordgo.MessageSend
}

func (f *fakeSession) Channel(string, ...discordgo.RequestOption) (*discordgo.Channel, error) {
	f.reads++
	return &discordgo.Channel{}, nil
}

func (f *fakeSession) GuildChannels(string, ...discordgo.RequestOption) ([]*discordgo.Channel, error) {
	f.reads++
	return nil, nil
}

func (f *fakeSession) GuildThreadsActive(string, ...discordgo.RequestOption) (*discordgo.ThreadsList, error) {
	f.reads++
	return &discordgo.ThreadsList{}, nil
}

func (f *fakeSession) ThreadsArchived(string, *time.Time, int, ...discordgo.RequestOption) (*discordgo.ThreadsList, error) {
	f.reads++
	return &discordgo.ThreadsList{}, nil
}

func (f *fakeSession) ChannelMessages(string, int, string, string, string, ...discordgo.RequestOption) ([]*discordgo.Message, error) {
	f.reads++
	return nil, nil
}

func (f *fakeSession) User(string, ...discordgo.RequestOption) (*discordgo.User, error) {
	f.reads++
	return &discordgo.User{}, nil
}

func (f *fakeSession) GuildMember(string, string, ...discordgo.RequestOption) (*discordgo.Member, error) {
	f.reads++
	return &discordgo.Member{}, nil
}

func (f *fakeSession) GuildMembers(string, string, int, ...discordgo.RequestOption) ([]*discordgo.Member, error) {
	f.reads++
	return nil, nil
}

func (f *fakeSession) GuildRoles(string, ...discordgo.RequestOption) ([]*discordgo.Role, error) {
	f.reads++
	return nil, nil
}

func (f *fakeSession) GuildChannelCreateComplex(string, discordgo.GuildChannelCreateData, ...discordgo.RequestOption) (*discordgo.Channel, error) {
	f.writes++
	return &discordgo.Channel{}, nil
}

func (f *fakeSession) ChannelEditComplex(string, *discordgo.ChannelEdit, ...discordgo.RequestOption) (*discordgo.Channel, error) {
	f.writes++
	return &discordgo.Channel{}, nil
}

func (f *fakeSession) ChannelDelete(string, ...discordgo.RequestOption) (*discordgo.Channel, error) {
	f.writes++
	return &discordgo.Channel{}, nil
}

func (f *fakeSession) ChannelMessageSend(string, string, ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.writes++
	return &discordgo.Message{}, nil
}

func (f *fakeSession) Guild(string, ...discordgo.RequestOption) (*discordgo.Guild, error) {
	f.reads++
	return &discordgo.Guild{Name: "Test Guild"}, nil
}

func (f *fakeSession) UserChannelCreate(string, ...discordgo.RequestOption) (*discordgo.Channel, error) {
	f.reads++
	return &discordgo.Channel{ID: "dm"}, nil
}

func (f *fakeSession) ChannelMessageSendComplex(_ string, data *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.writes++
	f.sends = append(f.sends, data)
	return &discordgo.Message{}, nil
}

func (f *fakeSession) ChannelMessageSendEmbed(string, *discordgo.MessageEmbed, ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.writes++
	return &discordgo.Message{}, nil
}

func (f *fakeSession) ChannelMessagePin(string, string, ...discordgo.RequestOption) error {
	f.writes++
	return nil
}

func (f *fakeSession) ChannelPermissionSet(string, string, discordgo.PermissionOverwriteType, int64, int64, ...discordgo.RequestOption) error {
	f.writes++
	return nil
}

func (f *fakeSession) ChannelPermissionDelete(string, string, ...discordgo.RequestOption) error {
	f.writes++
	return nil
}

func (f *fakeSession) GuildMemberEdit(string, string, *discordgo.GuildMemberParams, ...discordgo.RequestOption) (*discordgo.Member, error) {
	f.writes++
	return &discordgo.Member{}, nil
}

func (f *fakeSession) GuildMemberRoleAdd(string, string, string, ...discordgo.RequestOption) error {
	f.writes++
	return nil
}

func (f *fakeSession) GuildMemberRoleRemove(string, string, string, ...discordgo.RequestOption) error {
	f.writes++
	return nil
}

func (f *fakeSession) GuildRoleCreate(string, *discordgo.RoleParams, ...discordgo.RequestOption) (*discordgo.Role, error) {
	f.writes++
	return &discordgo.Role{}, nil
}

func (f *fakeSession) GuildRoleEdit(string, string, *discordgo.RoleParams, ...discordgo.RequestOption) (*discordgo.Role, error) {
	f.writes++
	return &discordgo.Role{}, nil
}

func (f *fakeSession) ChannelMessageDelete(string, string, ...discordgo.RequestOption) error {
	f.writes++
	return nil
}

func (f *fakeSession) ChannelWebhooks(string, ...discordgo.RequestOption) ([]*discordgo.Webhook, error) {
	return nil, nil
}

func (f *fakeSession) WebhookCreate(string, string, string, ...discordgo.RequestOption) (*discordgo.Webhook, error) {
	f.writes++
	return &discordgo.Webhook{ID: "w", Token: "t"}, nil
}

func (f *fakeSession) WebhookExecute(_, _ string, _ bool, data *discordgo.WebhookParams, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.writes++
	f.lastWebhook = data
	return &discordgo.Message{}, nil
}

func (f *fakeSession) GuildMemberTimeout(string, string, *time.Time, ...discordgo.RequestOption) error {
	f.writes++
	return nil
}

type fakeGate struct {
	paused map[string]bool
	dryRun map[string]bool
}

func (f fakeGate) WritesPaused(guildID string) bool { return f.paused[guildID] }
func (f fakeGate) WritesDryRun(guildID string) bool { return f.dryRun[guildID] }

// callEveryWrite exercises every gated method so a newly added one can't
// quietly skip the check: if someone adds a write that forgets allow(), the
// write counter here goes up while the test expects zero.
func callEveryWrite(o *GuildOps) []error {
	var errs []error
	_, err := o.GuildChannelCreateComplex("g", discordgo.GuildChannelCreateData{})
	errs = append(errs, err)
	_, err = o.ChannelEditComplex("c", &discordgo.ChannelEdit{})
	errs = append(errs, err)
	_, err = o.ChannelDelete("c")
	errs = append(errs, err)
	_, err = o.ChannelMessageSend("c", "hi")
	errs = append(errs, err)
	_, err = o.ChannelMessageSendEmbed("c", &discordgo.MessageEmbed{})
	errs = append(errs, err)
	errs = append(errs, o.ChannelMessagePin("c", "m"))
	errs = append(errs, o.ChannelPermissionSet("c", "r", discordgo.PermissionOverwriteTypeRole, 0, 0))
	errs = append(errs, o.ChannelPermissionDelete("c", "r"))
	_, err = o.GuildMemberEdit("g", "u", &discordgo.GuildMemberParams{})
	errs = append(errs, err)
	errs = append(errs, o.GuildMemberRoleAdd("g", "u", "r"))
	errs = append(errs, o.GuildMemberRoleRemove("g", "u", "r"))
	_, err = o.GuildRoleCreate("g", &discordgo.RoleParams{})
	errs = append(errs, err)
	errs = append(errs, o.ChannelMessageDelete("c", "m"))
	_, err = o.WebhookCreate("c", "n", "")
	errs = append(errs, err)
	errs = append(errs, o.WebhookExecute("w", "t", &discordgo.WebhookParams{}))
	errs = append(errs, o.GuildMemberTimeout("g", "u", nil))
	return errs
}

// The webhook sender carries text a *member* wrote, reposted under that
// member's own name, which makes it the one place where a missing mention
// suppression would let somebody make merlin ping on their behalf. Asserted
// rather than assumed for that reason.
func TestWebhookExecuteSuppressesMentions(t *testing.T) {
	sess := &fakeSession{}
	g := New(sess, fakeGate{}, testLogger(), false)

	if err := g.For("g1").WebhookExecute("w", "t", &discordgo.WebhookParams{Content: "hi @everyone"}); err != nil {
		t.Fatalf("WebhookExecute: %v", err)
	}
	if sess.lastWebhook == nil || sess.lastWebhook.AllowedMentions == nil {
		t.Fatal("AllowedMentions was left nil, so Discord will parse every mention in the content")
	}
	if len(sess.lastWebhook.AllowedMentions.Parse) != 0 {
		t.Errorf("AllowedMentions.Parse = %v, want empty", sess.lastWebhook.AllowedMentions.Parse)
	}
}

// A caller must not be able to opt back into pinging by supplying its own
// AllowedMentions, for the same reason ChannelMessageSendComplex overwrites
// rather than defaults it.
func TestWebhookExecuteOverridesCallerMentions(t *testing.T) {
	sess := &fakeSession{}
	g := New(sess, fakeGate{}, testLogger(), false)

	err := g.For("g1").WebhookExecute("w", "t", &discordgo.WebhookParams{
		Content:         "hi",
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{discordgo.AllowedMentionTypeEveryone}},
	})
	if err != nil {
		t.Fatalf("WebhookExecute: %v", err)
	}
	if len(sess.lastWebhook.AllowedMentions.Parse) != 0 {
		t.Errorf("caller's AllowedMentions survived: %v", sess.lastWebhook.AllowedMentions.Parse)
	}
}

func TestPausedRefusesEveryWrite(t *testing.T) {
	sess := &fakeSession{}
	gate := fakeGate{paused: map[string]bool{"g1": true}}
	g := New(sess, gate, testLogger(), false)

	for _, err := range callEveryWrite(g.For("g1")) {
		if !errors.Is(err, ErrPaused) {
			t.Errorf("got %v, want ErrPaused", err)
		}
	}
	if sess.writes != 0 {
		t.Errorf("paused guard let %d write(s) through to Discord, want 0", sess.writes)
	}
}

func TestGlobalPauseOverridesGuildSettings(t *testing.T) {
	sess := &fakeSession{}
	// Guild has nothing paused; only the host-level stop is engaged.
	g := New(sess, fakeGate{}, testLogger(), true)

	for _, err := range callEveryWrite(g.For("g1")) {
		if !errors.Is(err, ErrPaused) {
			t.Errorf("got %v, want ErrPaused", err)
		}
	}
	if sess.writes != 0 {
		t.Errorf("global pause let %d write(s) through, want 0", sess.writes)
	}

	// And it can be released without a restart.
	g.PauseAll(false)
	if _, err := g.For("g1").ChannelDelete("c"); err != nil {
		t.Fatalf("after PauseAll(false): %v", err)
	}
	if sess.writes != 1 {
		t.Errorf("writes = %d after releasing pause, want 1", sess.writes)
	}
}

func TestDryRunRefusesEveryWrite(t *testing.T) {
	sess := &fakeSession{}
	gate := fakeGate{dryRun: map[string]bool{"g1": true}}
	g := New(sess, gate, testLogger(), false)

	for _, err := range callEveryWrite(g.For("g1")) {
		if !errors.Is(err, ErrDryRun) {
			t.Errorf("got %v, want ErrDryRun", err)
		}
	}
	if sess.writes != 0 {
		t.Errorf("dry-run guard let %d write(s) through to Discord, want 0", sess.writes)
	}
}

// Pausing must not blind an admin: every inspect path still has to work, or
// whoever hit the emergency stop can't see what the bot thinks is going on.
func TestReadsAreNeverGated(t *testing.T) {
	sess := &fakeSession{}
	gate := fakeGate{paused: map[string]bool{"g1": true}, dryRun: map[string]bool{"g1": true}}
	o := New(sess, gate, testLogger(), true).For("g1")

	if _, err := o.Channel("c"); err != nil {
		t.Errorf("Channel: %v", err)
	}
	if _, err := o.GuildChannels("g1"); err != nil {
		t.Errorf("GuildChannels: %v", err)
	}
	if _, err := o.GuildMember("g1", "u"); err != nil {
		t.Errorf("GuildMember: %v", err)
	}
	if _, err := o.GuildRoles("g1"); err != nil {
		t.Errorf("GuildRoles: %v", err)
	}
	if _, err := o.GuildThreadsActive("g"); err != nil {
		t.Errorf("GuildThreadsActive: %v", err)
	}
	if _, err := o.ChannelMessages("c", 1, "", "", ""); err != nil {
		t.Errorf("ChannelMessages: %v", err)
	}
	if _, err := o.User("@me"); err != nil {
		t.Errorf("User: %v", err)
	}
	if sess.reads != 7 {
		t.Errorf("reads = %d, want 7; a read was gated", sess.reads)
	}
}

// Every message this bot sends must go out with mentions suppressed.
//
// Rotation reposts operator-supplied sticky text into the fresh channel on
// every single cycle, so an unfiltered send turns one configured mention
// into a recurring ping: a named user gets pinged forever on a schedule, a
// mentionable role pings its whole membership. Plain discordgo omits
// allowed_mentions entirely, which Discord reads as "parse everything", so
// the safe behaviour is the one that has to be written down, not the
// default. Asserting on the payload rather than the call count because the
// call count is identical either way.
func TestMessageSendsSuppressMentions(t *testing.T) {
	sess := &fakeSession{}
	g := New(sess, fakeGate{}, testLogger(), false)

	if _, err := g.For("g1").ChannelMessageSend("c", "hey @everyone <@123> <@&456>"); err != nil {
		t.Fatalf("ChannelMessageSend: %v", err)
	}
	if len(sess.sends) != 1 {
		t.Fatalf("sends = %d, want 1; the send did not go through ChannelMessageSendComplex", len(sess.sends))
	}

	sent := sess.sends[0]
	if sent.Content != "hey @everyone <@123> <@&456>" {
		t.Errorf("content was rewritten to %q; suppression must not alter the text, only whether it pings", sent.Content)
	}
	if sent.AllowedMentions == nil {
		t.Fatal("AllowedMentions is nil, so it is dropped by omitempty and Discord parses every mention in the content")
	}
	if len(sent.AllowedMentions.Parse) != 0 {
		t.Errorf("AllowedMentions.Parse = %v, want empty; a non-empty Parse whitelists that whole mention class", sent.AllowedMentions.Parse)
	}
	if len(sent.AllowedMentions.Roles) != 0 || len(sent.AllowedMentions.Users) != 0 {
		t.Errorf("AllowedMentions allows specific roles/users (%v/%v), want neither",
			sent.AllowedMentions.Roles, sent.AllowedMentions.Users)
	}
}

// Suppression must not become a way around the pause or the rate cap: it is
// a property of how a send is formed, not a different path to Discord.
func TestSuppressedSendIsStillGated(t *testing.T) {
	sess := &fakeSession{}
	gate := fakeGate{paused: map[string]bool{"g1": true}}
	g := New(sess, gate, testLogger(), false)

	if _, err := g.For("g1").ChannelMessageSend("c", "hello"); !errors.Is(err, ErrPaused) {
		t.Errorf("got %v, want ErrPaused", err)
	}
	if len(sess.sends) != 0 || sess.writes != 0 {
		t.Errorf("a paused guild still reached Discord: %d sends, %d writes", len(sess.sends), sess.writes)
	}
}

// The guard is per guild: pausing one server must not stop another.
func TestPauseIsScopedToItsGuild(t *testing.T) {
	sess := &fakeSession{}
	gate := fakeGate{paused: map[string]bool{"paused-guild": true}}
	g := New(sess, gate, testLogger(), false)

	if _, err := g.For("paused-guild").ChannelDelete("c"); !errors.Is(err, ErrPaused) {
		t.Errorf("paused guild: got %v, want ErrPaused", err)
	}
	if _, err := g.For("other-guild").ChannelDelete("c"); err != nil {
		t.Errorf("unpaused guild: %v", err)
	}
	if sess.writes != 1 {
		t.Errorf("writes = %d, want 1 (only the unpaused guild)", sess.writes)
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
