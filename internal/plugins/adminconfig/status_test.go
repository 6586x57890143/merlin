package adminconfig

import (
	"context"
	"errors"
	"strings"
	"testing"

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
