package adminconfig

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/settings"
)

type stubDB struct{ err error }

func (s stubDB) Healthy(ctx context.Context) error { return s.err }

type stubJobs struct {
	total, failing int
	err            error
}

func (s stubJobs) JobHealth(ctx context.Context, guildID string) (int, int, error) {
	return s.total, s.failing, s.err
}

// settingsWith returns a SettingsAdmin whose GuildSettings is gs, enough for
// the status lines that don't touch Discord.
type stubSettings struct {
	fakeSettingsAdmin
	gs settings.GuildSettings
}

func (s stubSettings) GuildSettings(guildID string) settings.GuildSettings { return s.gs }

// statusBody renders the parts of /config status that need no Discord
// session, so the reporting logic can be asserted without one.
func statusBody(t *testing.T, p *Plugin, gs settings.GuildSettings) string {
	t.Helper()
	return p.configuredResourceLines(gs)
}

// A guild left paused or in dry-run is the single most likely explanation for
// "the bot stopped doing anything", so status has to say so unmistakably.
func TestStatusReportsMissingConfiguration(t *testing.T) {
	p := New(stubSettings{}, "config.yaml", stubDB{}, stubJobs{})

	body := statusBody(t, p, settings.GuildSettings{})
	for _, want := range []string{"not configured", "none configured"} {
		if !strings.Contains(body, want) {
			t.Errorf("unconfigured guild status missing %q:\n%s", want, body)
		}
	}

	body = statusBody(t, p, settings.GuildSettings{
		AuditLogChannelID: "c1",
		StatusChannelID:   "c2",
		ModRoleIDs:        []string{"r1", "r2"},
		AdminUserIDs:      []string{"u1"},
	})
	if strings.Contains(body, "not configured") {
		t.Errorf("fully configured guild still reported as missing config:\n%s", body)
	}
	if !strings.Contains(body, "2 configured") {
		t.Errorf("mod role count missing from:\n%s", body)
	}
}

// The database line is the first thing rendered because everything after it
// is suspect when it fails.
func TestStatusDBHealthSurfacesTheError(t *testing.T) {
	if (stubDB{err: errors.New("connection refused")}).Healthy(context.Background()) == nil {
		t.Fatal("stub should report the error")
	}
	if (stubDB{}).Healthy(context.Background()) != nil {
		t.Fatal("healthy stub should report nil")
	}
}

// The audit log and the status channel carry moderation actions, config
// changes and raw job errors. Configuring either as a channel the whole
// server can read publishes all of it, and nothing about the channel
// filling up normally says so. The check has to lean toward warning: a
// spurious warning wastes a moment, a missed one is a privacy failure
// nobody finds out about.
func TestEveryoneCanReadLeansTowardWarning(t *testing.T) {
	const guildID = "g1"
	everyone := guildID // @everyone's role ID is the guild ID

	cases := []struct {
		name string
		ch   *discordgo.Channel
		want bool
	}{
		{
			name: "no overwrites at all is a plain public channel",
			ch:   &discordgo.Channel{GuildID: guildID},
			want: true,
		},
		{
			name: "explicit @everyone view deny is the private case",
			ch: &discordgo.Channel{GuildID: guildID, PermissionOverwrites: []*discordgo.PermissionOverwrite{
				{ID: everyone, Type: discordgo.PermissionOverwriteTypeRole, Deny: discordgo.PermissionViewChannel},
			}},
			want: false,
		},
		{
			name: "a deny on some other permission does not hide the channel",
			ch: &discordgo.Channel{GuildID: guildID, PermissionOverwrites: []*discordgo.PermissionOverwrite{
				{ID: everyone, Type: discordgo.PermissionOverwriteTypeRole, Deny: discordgo.PermissionSendMessages},
			}},
			want: true,
		},
		{
			name: "denying one role is not denying everyone",
			ch: &discordgo.Channel{GuildID: guildID, PermissionOverwrites: []*discordgo.PermissionOverwrite{
				{ID: "some-other-role", Type: discordgo.PermissionOverwriteTypeRole, Deny: discordgo.PermissionViewChannel},
			}},
			want: true,
		},
		{
			name: "a member overwrite sharing the guild ID is not the @everyone role",
			ch: &discordgo.Channel{GuildID: guildID, PermissionOverwrites: []*discordgo.PermissionOverwrite{
				{ID: everyone, Type: discordgo.PermissionOverwriteTypeMember, Deny: discordgo.PermissionViewChannel},
			}},
			want: true,
		},
		{
			name: "unknown channel warns rather than reassuring",
			ch:   nil,
			want: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := everyoneCanRead(c.ch); got != c.want {
				t.Errorf("everyoneCanRead = %v, want %v", got, c.want)
			}
		})
	}
}
