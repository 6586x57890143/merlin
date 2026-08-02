package discordguard

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeSession counts every call that reaches Discord. The whole point of the
// guard is that this counter stays at zero while paused or rehearsing, so
// the tests below assert on it rather than on the returned errors alone.
type fakeSession struct {
	writes int
	reads  int
}

func (f *fakeSession) Channel(string, ...discordgo.RequestOption) (*discordgo.Channel, error) {
	f.reads++
	return &discordgo.Channel{}, nil
}

func (f *fakeSession) GuildChannels(string, ...discordgo.RequestOption) ([]*discordgo.Channel, error) {
	f.reads++
	return nil, nil
}

func (f *fakeSession) ThreadsActive(string, ...discordgo.RequestOption) (*discordgo.ThreadsList, error) {
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

type fakeGate struct {
	paused map[string]bool
	dryRun map[string]bool
}

func (f fakeGate) WritesPaused(guildID string) bool { return f.paused[guildID] }
func (f fakeGate) WritesDryRun(guildID string) bool { return f.dryRun[guildID] }

// callEveryWrite exercises every gated method so a newly added one can't
// quietly skip the check — if someone adds a write that forgets allow(), the
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
	return errs
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
	if _, err := o.ThreadsActive("c"); err != nil {
		t.Errorf("ThreadsActive: %v", err)
	}
	if _, err := o.ChannelMessages("c", 1, "", "", ""); err != nil {
		t.Errorf("ChannelMessages: %v", err)
	}
	if _, err := o.User("@me"); err != nil {
		t.Errorf("User: %v", err)
	}
	if sess.reads != 7 {
		t.Errorf("reads = %d, want 7 — a read was gated", sess.reads)
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
