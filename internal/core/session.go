package core

import (
	"fmt"

	"github.com/bwmarrin/discordgo"
)

// NewSession builds the single shared *discordgo.Session used by every
// plugin. Intents are the minimum this binary needs — MESSAGE_CONTENT is
// never requested, and GUILD_MEMBERS only when the operator opts in.
//
// withMembers adds GUILD_MEMBERS, which is what lets the roles plugin react
// to a rejoin the instant it happens instead of on the next sweep. It is off
// by default and deliberately a decision the operator makes: it is a
// privileged intent (a Developer Portal toggle below 100 guilds, Discord
// approval above), and jail already survives a rejoin without it — see
// roles.reapplyEvadedJails. Enabling it narrows the window, it does not
// create the protection.
func NewSession(token string, withMembers bool) (*discordgo.Session, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	intents := discordgo.IntentsGuilds
	if withMembers {
		intents |= discordgo.IntentsGuildMembers
	}
	s.Identify.Intents = intents
	return s, nil
}
