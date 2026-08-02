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
func (fakeSettingsAdmin) SetWritesPaused(ctx context.Context, guildID string, paused bool) error {
	return nil
}
func (fakeSettingsAdmin) SetWritesDryRun(ctx context.Context, guildID string, dryRun bool) error {
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

	p := New(fakeSettingsAdmin{}, "config.yaml", nil, nil)
	deps := core.Deps{Commands: router, Logger: log, Session: &discordgo.Session{}}
	if err := p.Init(deps); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := router.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
}

// TestConfigMutateCannotBeLoweredBelowAdmin guards the invariant the whole
// tier model rests on: config.mutate covers /config admins add, so allowing
// a guild to set it to Admins+Mods would let any mod grant themselves admin
// — a one-command collapse of the separation between the two tiers. The
// compiled-in PermSpec says TierAdmin, but a per-guild tier override could
// silently undo that, which is why this is checked at the mutation and not
// left to the registration table.
func TestConfigMutateCannotBeLoweredBelowAdmin(t *testing.T) {
	if err := validateTierChange(actionMutate, core.TierMod); err == nil {
		t.Fatal("lowering config.mutate to Admins+Mods must be refused — it lets any mod self-promote to admin")
	}
	if err := validateTierChange(actionMutate, core.TierPublic); err == nil {
		t.Fatal("lowering config.mutate to public must be refused")
	}
	if err := validateTierChange(actionMutate, core.TierAdmin); err != nil {
		t.Fatalf("re-asserting config.mutate as Admins only must be allowed, got %v", err)
	}
}

// TestOtherActionsStayFreelyRetierable is the counterweight: the guard above
// is one deliberate exception, not a general freeze. Every feature action
// must remain adjustable in both directions, which is the entire point of
// /config permissions set-tier.
func TestOtherActionsStayFreelyRetierable(t *testing.T) {
	for _, action := range []string{actionRead, "rotation.configure_structural", "roles.jail", "scheduler.run_now"} {
		for _, tier := range []core.PermTier{core.TierMod, core.TierAdmin} {
			if err := validateTierChange(action, tier); err != nil {
				t.Errorf("validateTierChange(%q, %v) = %v, want nil", action, tier, err)
			}
		}
	}
}
