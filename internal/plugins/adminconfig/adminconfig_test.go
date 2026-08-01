package adminconfig

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/settings"
)

// fakeSettingsAdmin is a no-op SettingsAdmin — enough for Init to run and
// register its full command tree without touching Postgres.
type fakeSettingsAdmin struct{}

func (fakeSettingsAdmin) GuildSettings(guildID string) settings.GuildSettings {
	return settings.GuildSettings{}
}
func (fakeSettingsAdmin) Overrides(guildID string) []settings.ActionOverride           { return nil }
func (fakeSettingsAdmin) AddModRole(ctx context.Context, guildID, roleID string) error { return nil }
func (fakeSettingsAdmin) RemoveModRole(ctx context.Context, guildID, roleID string) error {
	return nil
}
func (fakeSettingsAdmin) AddAdmin(ctx context.Context, guildID, userID string) error { return nil }
func (fakeSettingsAdmin) RemoveAdmin(ctx context.Context, guildID, userID string) error {
	return nil
}
func (fakeSettingsAdmin) SetAuditLogChannel(ctx context.Context, guildID, channelID string) error {
	return nil
}
func (fakeSettingsAdmin) SetStatusChannel(ctx context.Context, guildID, channelID string) error {
	return nil
}
func (fakeSettingsAdmin) GrantOverride(ctx context.Context, guildID, action, roleID, userID string) error {
	return nil
}
func (fakeSettingsAdmin) RevokeOverride(ctx context.Context, guildID, action, roleID, userID string) error {
	return nil
}
func (fakeSettingsAdmin) DenyOverride(ctx context.Context, guildID, action, roleID, userID string) error {
	return nil
}
func (fakeSettingsAdmin) UndenyOverride(ctx context.Context, guildID, action, roleID, userID string) error {
	return nil
}
func (fakeSettingsAdmin) SetActionTier(ctx context.Context, guildID, action string, tier core.PermTier) error {
	return nil
}
func (fakeSettingsAdmin) ClearActionTier(ctx context.Context, guildID, action string) error {
	return nil
}
func (fakeSettingsAdmin) DisabledPlugins(guildID string) []string { return nil }
func (fakeSettingsAdmin) DisablePlugin(ctx context.Context, guildID, pluginName string) error {
	return nil
}
func (fakeSettingsAdmin) EnablePlugin(ctx context.Context, guildID, pluginName string) error {
	return nil
}
func (fakeSettingsAdmin) ImportFromLegacyYAML(ctx context.Context, path string) ([]string, error) {
	return nil, nil
}
func (fakeSettingsAdmin) MarkOnboardingNudgeSent(ctx context.Context, guildID string) error {
	return nil
}

// fakeAuthData/fakeGate are minimal core.GuildAuthData/core.PluginGate
// implementations — Init doesn't exercise authorization, but
// core.NewCommandRouter/NewPermissions need something satisfying the
// interfaces to construct.
type fakeAuthData struct{}

func (fakeAuthData) ModRoleIDs(guildID string) []string   { return nil }
func (fakeAuthData) AdminUserIDs(guildID string) []string { return nil }
func (fakeAuthData) ActionPolicy(guildID, action string) core.ActionPolicy {
	return core.ActionPolicy{}
}

type fakeGate struct{}

func (fakeGate) PluginEnabled(guildID, pluginName string) bool { return true }

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestInitRegistersAFullyWiredCommandTree guards against exactly the mistake
// this package's growth risks: /config's command tree (admins, mod-roles,
// permissions incl. set-tier/clear-tier/grant/revoke/block/unblock/list,
// plugins incl. list/enable/disable, setup, import) must have a Handle call
// for every declared subcommand, each with a valid PermSpec — Finalize is
// the same fail-closed check core.CommandRouter itself is tested with,
// exercised here against this plugin's actual registration code instead of
// a hand-built test tree.
func TestInitRegistersAFullyWiredCommandTree(t *testing.T) {
	log := discardLogger()
	perms := core.NewPermissions(nil, fakeAuthData{}, "")
	router := core.NewCommandRouter(perms, fakeGate{}, log)

	p := New(fakeSettingsAdmin{}, "config.yaml")
	deps := core.Deps{Commands: router, Logger: log, Session: &discordgo.Session{}}
	if err := p.Init(deps); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := router.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
}
