package core

import (
	"fmt"

	"github.com/bwmarrin/discordgo"
)

// NewSession builds the single shared *discordgo.Session used by every
// plugin. Intents are the minimum this binary currently needs — GUILD_MEMBERS
// and MESSAGE_CONTENT are privileged intents requiring Discord approval at
// scale, so neither is requested until a plugin genuinely needs it.
func NewSession(token string) (*discordgo.Session, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	s.Identify.Intents = discordgo.IntentsGuilds
	return s, nil
}
