package core

import (
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
)

type permsFor struct {
	name string
	per  map[string]discord.Permissions
}

func (p permsFor) Name() string                           { return p.name }
func (p permsFor) Perms(guild string) discord.Permissions { return p.per[guild] }

type gateOff map[string]bool // "guild/plugin" switched off

func (g gateOff) PluginEnabled(guild, plugin string) bool { return !g[guild+"/"+plugin] }

// The ask is what plugins switched on somewhere need there now, and nothing
// for a plugin switched off where it would have needed it.
func TestInstallIsWhatIsInUse(t *testing.T) {
	base := permsFor{"rotation", map[string]discord.Permissions{"1": discord.PermissionManageChannels, "2": discord.PermissionManageChannels}}
	ai := permsFor{"aimod", map[string]discord.Permissions{"2": discord.PermissionManageMessages}}
	bans := permsFor{"rapsheet", map[string]discord.Permissions{"1": discord.PermissionBanMembers, "2": discord.PermissionBanMembers}}
	plugins := []Permissioned{base, ai, bans}

	got := Install([]string{"1", "2"}, plugins, gateOff{"1/rapsheet": true, "2/rapsheet": true})
	if got != discord.PermissionManageChannels|discord.PermissionManageMessages {
		t.Fatalf("install %b", got)
	}
	if got := Install([]string{"1"}, plugins, gateOff{}); got.Has(discord.PermissionManageMessages) {
		t.Fatal("a plugin not in use in any guild was asked for")
	}
	if got := Install(nil, plugins, gateOff{}); got != 0 {
		t.Fatalf("no guilds asked for %b", got)
	}
}

type apps struct {
	rest.Applications
	updates int
}

func (a *apps) UpdateCurrentApplication(u discord.ApplicationUpdate, _ ...rest.RequestOpt) (*discord.Application, error) {
	a.updates++
	return &discord.Application{}, nil
}

// The settings are written only when they would change.
func TestSyncInstallWritesOnlyAChange(t *testing.T) {
	a := &apps{}
	perms := discord.PermissionManageChannels | discord.PermissionManageRoles
	have := &discord.Application{IntegrationTypesConfig: discord.ApplicationIntegrationTypesConfig{
		discord.ApplicationIntegrationTypeGuildInstall: {OAuth2InstallParams: &discord.InstallParams{
			Scopes:      []discord.OAuth2Scope{discord.OAuth2ScopeBot, discord.OAuth2ScopeApplicationsCommands},
			Permissions: perms,
		}},
	}}
	if wrote, err := SyncInstall(a, have, perms); wrote || err != nil || a.updates != 0 {
		t.Fatalf("an unchanged install was written: %v %v", wrote, err)
	}
	if wrote, err := SyncInstall(a, have, perms|discord.PermissionBanMembers); !wrote || err != nil || a.updates != 1 {
		t.Fatalf("a changed install was not written: %v %v", wrote, err)
	}
}
