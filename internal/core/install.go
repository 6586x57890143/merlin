package core

import (
	"slices"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
)

// Permissioned is a plugin that acts as itself in Discord. Perms is what
// merlin's role needs in guildID for it now, given how that guild uses it:
// aimod asks for nothing where its mode is off. Interaction replies need
// none of it; only calls merlin makes as herself do.
type Permissioned interface {
	Name() string
	Perms(guildID string) discord.Permissions
}

// Install is what merlin asks a server for: the union, over every guild she
// is in, of what each plugin switched on there needs now. Least privilege
// over time: nobody is asked for delete, ban or timeout until something in
// use somewhere needs it, and the ask goes when it stops.
func Install(guilds []string, plugins []Permissioned, gate PluginGate) discord.Permissions {
	var p discord.Permissions
	for _, g := range guilds {
		for _, pl := range plugins {
			if gate.PluginEnabled(g, pl.Name()) {
				p |= pl.Perms(g)
			}
		}
	}
	return p
}

// SyncInstall writes perms into the application's guild install settings,
// which is what Discord's own invite link and cygnet both read, unless they
// already say exactly that. Reports whether it wrote. From skua.
func SyncInstall(r rest.Applications, app *discord.Application, perms discord.Permissions) (bool, error) {
	want := discord.InstallParams{
		Scopes:      []discord.OAuth2Scope{discord.OAuth2ScopeApplicationsCommands, discord.OAuth2ScopeBot},
		Permissions: perms,
	}
	guild := discord.ApplicationIntegrationTypeGuildInstall
	if have, ok := app.IntegrationTypesConfig[guild]; ok && len(app.IntegrationTypesConfig) == 1 &&
		have.OAuth2InstallParams != nil && sameInstall(*have.OAuth2InstallParams, want) {
		return false, nil
	}
	_, err := r.UpdateCurrentApplication(discord.ApplicationUpdate{
		IntegrationTypesConfig: &discord.ApplicationIntegrationTypesConfig{guild: {OAuth2InstallParams: &want}},
	})
	return err == nil, err
}

func sameInstall(a, b discord.InstallParams) bool {
	if a.Permissions != b.Permissions || len(a.Scopes) != len(b.Scopes) {
		return false
	}
	x, y := slices.Clone(a.Scopes), slices.Clone(b.Scopes)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}
