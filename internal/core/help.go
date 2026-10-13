package core

import (
	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgo/discord"
)

// Helper is a plugin with a page in /help. Optional: a plugin without one is
// simply not listed.
type Helper interface {
	Help() Help
}

// Help is a plugin's page: Line is the one subtext line under its name in
// the index and on its page, About what it does and how to use it, said
// before anything else. Written in merlin's voice (internal/voice/PERSONA.md):
// lowercase, plain, first person, no wit where somebody is trying to find out
// how a thing works.
type Help struct {
	Title string
	Line  string
	About string
}

// Plugins is every registered plugin, in registration order, for /help to
// read pages and names off.
func (r *Registry) Plugins() []Plugin {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Plugin(nil), r.plugins...)
}

// CommandsOf is every top-level command plugin registered, for a page's
// command grid.
func (r *CommandRouter) CommandsOf(plugin string) []discord.SlashCommandCreate {
	var out []discord.SlashCommandCreate
	for _, cmd := range r.topLevel {
		if r.topLevelPlugin[cmd.Name] == plugin {
			out = append(out, cmd)
		}
	}
	return out
}

// LeafTier is the compiled-in tier of one command leaf, path as Handle names
// it, or tierUnset for one that does not exist. The compiled-in tier, not a
// guild's override: /help describes the bot, and the panels say what a guild
// changed.
func (r *CommandRouter) LeafTier(topLevel, path string) PermTier {
	if leaf, ok := r.leaves[leafKey(topLevel, path)]; ok {
		return leaf.spec.Tier
	}
	return tierUnset
}

// AvatarIcon is merlin's avatar for a thumbnail, as an attachment.
func AvatarIcon() (string, *discordgo.File) {
	return avatarAttachmentURL, avatarFile()
}
