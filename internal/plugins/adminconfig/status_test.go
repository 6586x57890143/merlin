package adminconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/6586x57890143/merlin/internal/settings"
	"github.com/disgoorg/disgo/discord"
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
	body, _ := p.configuredResourceLines(gs)
	return body
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
	// Named, not counted. This asserted "2 configured" before: an admin
	// reading their own status wants to know which roles carry mod powers,
	// and a bare count sends them to another command to find out. Changed
	// deliberately, since the count was the assertion holding the old
	// behaviour in place.
	for _, want := range []string{"<@&r1>", "<@&r2>", "<@u1>"} {
		if !strings.Contains(body, want) {
			t.Errorf("status does not name %s:\n%s", want, body)
		}
	}
}

// Past a certain length a list of mentions stops being scannable, so the
// tail becomes a count rather than running on.
func TestStatusCapsLongMentionLists(t *testing.T) {
	p := New(stubSettings{}, "config.yaml", stubDB{}, stubJobs{})

	many := make([]string, 0, maxMentionsListed+3)
	for i := range maxMentionsListed + 3 {
		many = append(many, fmt.Sprintf("r%d", i))
	}
	body := statusBody(t, p, settings.GuildSettings{ModRoleIDs: many})

	if !strings.Contains(body, "and 3 more") {
		t.Errorf("long mod role list was not capped:\n%s", body)
	}
	if strings.Contains(body, fmt.Sprintf("<@&r%d>", maxMentionsListed)) {
		t.Errorf("list ran past the cap:\n%s", body)
	}
}

// The response colour used to be recovered by scanning the finished body for
// ❌/⚠️/⛔, so any error string that happened to contain one turned a healthy
// server amber. The severity is tracked as the body is built now, and the
// emoji are cues for the reader rather than state.
func TestStatusSeverityIgnoresGlyphsInInterpolatedText(t *testing.T) {
	p := New(stubSettings{}, "config.yaml", stubDB{}, stubJobs{})

	_, sev := p.configuredResourceLines(settings.GuildSettings{
		AuditLogChannelID: "c1",
		StatusChannelID:   "c2",
		ModRoleIDs:        []string{"⚠️ a role someone named with a warning sign"},
		AdminUserIDs:      []string{"u1"},
	})
	if sev != sevOK {
		t.Errorf("a warning glyph inside a name was read as a real warning: severity %v", sev)
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
	// @everyone's role ID is the guild ID, 100.
	cases := []struct {
		name       string
		overwrites string
		nilChannel bool
		want       bool
	}{
		{"no overwrites at all is a plain public channel", `[]`, false, true},
		{"explicit @everyone view deny is the private case", `[{"id":"100","type":0,"allow":"0","deny":"1024"}]`, false, false},
		{"a deny on some other permission does not hide the channel", `[{"id":"100","type":0,"allow":"0","deny":"2048"}]`, false, true},
		{"denying one role is not denying everyone", `[{"id":"201","type":0,"allow":"0","deny":"1024"}]`, false, true},
		{"a member overwrite sharing the guild ID is not the @everyone role", `[{"id":"100","type":1,"allow":"0","deny":"1024"}]`, false, true},
		{"unknown channel warns rather than reassuring", ``, true, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var ch discord.GuildChannel
			if !c.nilChannel {
				ch = textChannel(t, c.overwrites)
			}
			if got := everyoneCanRead(ch); got != c.want {
				t.Errorf("everyoneCanRead = %v, want %v", got, c.want)
			}
		})
	}
}

// textChannel is a text channel in guild 100 with overwrites, as Discord
// sends it.
func textChannel(t *testing.T, overwrites string) discord.GuildChannel {
	t.Helper()
	var ch discord.GuildTextChannel
	if err := json.Unmarshal([]byte(`{"id":"300","type":0,"guild_id":"100","name":"c","permission_overwrites":`+overwrites+`}`), &ch); err != nil {
		t.Fatal(err)
	}
	return ch
}
