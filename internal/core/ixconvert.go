package core

import (
	"encoding/json"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgo/discord"
)

// EmbedOf and LayoutOf carry an embed or components built on discordgo's
// types over to disgo's, through the Discord JSON both libraries speak. They
// are for the one shape the port meets mid-way: a message a plugin posts as
// itself over discordgo REST, then redraws in place when somebody clicks a
// button on it, which is an interaction answered through an Ix. Building it
// once and converting it here keeps the two from drifting; ixconvert_test.go
// holds the round trip to the same JSON. Both go when REST moves to disgo.

// EmbedOf is e on disgo's types, the zero Embed for nil.
func EmbedOf(e *discordgo.MessageEmbed) discord.Embed {
	var out discord.Embed
	if e == nil {
		return out
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

// LayoutOf is cs on disgo's types, nil for none. A component disgo cannot
// read is dropped rather than failing the whole message.
func LayoutOf(cs []discordgo.MessageComponent) []discord.LayoutComponent {
	if len(cs) == 0 {
		return nil
	}
	raw, err := json.Marshal(cs)
	if err != nil {
		return nil
	}
	var m discord.Message
	if err := json.Unmarshal([]byte(`{"id":"0","channel_id":"0","components":`+string(raw)+`}`), &m); err != nil {
		return nil
	}
	return m.Components
}

// Actor is who is acting, as CanModerate reads them: an ID and the roles
// they hold. Nil means nobody, as for an automatic action.
type Actor struct {
	ID      string
	RoleIDs []string
}

// Actor is the invoking member as an Actor, nil outside a server.
func (ix *Ix) Actor() *Actor {
	m := ix.Member()
	if m == nil {
		return nil
	}
	return &Actor{ID: ix.UserID, RoleIDs: IDStrings(m.RoleIDs)}
}

// ResolvedUser is the user a command's user option named, as Discord
// resolved it on the interaction; nil if it did not.
func (ix *Ix) ResolvedUser(id string) *discord.User {
	c, ok := ix.I.(discord.ApplicationCommandInteraction)
	if !ok {
		return nil
	}
	d, ok := c.Data.(discord.SlashCommandInteractionData)
	if !ok {
		return nil
	}
	for sf, u := range d.Resolved.Users {
		if sf.String() == id {
			return &u
		}
	}
	return nil
}
