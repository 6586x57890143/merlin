package core

import (
	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

// ID is s as a snowflake, or 0 if it is not one. merlin stores Discord's
// IDs as strings that came from Discord, so 0 only ever means a stored row
// that was never a real ID, and discordguard refuses 0 before sending.
func ID(s string) snowflake.ID {
	id, _ := snowflake.Parse(s)
	return id
}

// IDs is ss as snowflakes, in a new slice.
func IDs(ss []string) []snowflake.ID {
	out := make([]snowflake.ID, 0, len(ss))
	for _, s := range ss {
		out = append(out, ID(s))
	}
	return out
}

// IDStrings is ids as merlin stores them.
func IDStrings(ids []snowflake.ID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// DenyEveryone is DenyEveryoneExceptBot on disgo's types: @everyone denied
// View Channel and the bot granted botAllow, merged into src's own
// overwrites for either rather than added beside them.
func DenyEveryone(src []discord.PermissionOverwrite, guildID, botID snowflake.ID, botAllow discord.Permissions) []discord.PermissionOverwrite {
	out := make([]discord.PermissionOverwrite, 0, len(src)+2)
	foundEveryone, foundBot := false, false
	for _, ow := range src {
		switch o := ow.(type) {
		case discord.RolePermissionOverwrite:
			if o.RoleID == guildID {
				o.Deny |= discord.PermissionViewChannel
				o.Allow &^= discord.PermissionViewChannel
				foundEveryone = true
			}
			ow = o
		case discord.MemberPermissionOverwrite:
			if o.UserID == botID {
				o.Allow |= botAllow
				o.Deny &^= botAllow
				foundBot = true
			}
			ow = o
		}
		out = append(out, ow)
	}
	if !foundEveryone {
		out = append(out, discord.RolePermissionOverwrite{RoleID: guildID, Deny: discord.PermissionViewChannel})
	}
	if !foundBot {
		out = append(out, discord.MemberPermissionOverwrite{UserID: botID, Allow: botAllow})
	}
	return out
}

// DenyEveryoneExceptBot returns a permission-overwrite list that denies
// @everyone VIEW_CHANNEL while guaranteeing the bot itself keeps whatever
// access botAllow grants, merging into any pre-existing overwrite for
// either ID rather than appending a duplicate.
//
// This exists because "deny everyone, then separately remember to grant
// the bot" is exactly the bug this codebase hit three times over: adminconfig's
// setup channels, rotation's hidden staging channel, and rotation's archived
// channel all denied @everyone without a matching bot grant, locking the
// bot itself out of channels it had just created. Centralizing both halves
// into one call makes forgetting the bot grant impossible rather than
// something every future caller has to remember, the same reasoning as
// core.LeafArgs/core.RespondOK.
//
// botAllow should only include permission bits the bot's own Discord role
// actually holds at the guild level: Discord rejects the whole request
// (403 "Missing Permissions") if an overwrite tries to grant a bit the
// actor doesn't have, so don't reach for e.g. PermissionManageMessages here
// unless the bot's invite scope actually requests it.
func DenyEveryoneExceptBot(src []*discordgo.PermissionOverwrite, guildID, botUserID string, botAllow int64) []*discordgo.PermissionOverwrite {
	out := make([]*discordgo.PermissionOverwrite, 0, len(src)+2)
	foundEveryone, foundBot := false, false
	for _, ow := range src {
		clone := *ow
		if ow.Type == discordgo.PermissionOverwriteTypeRole && ow.ID == guildID {
			clone.Deny |= discordgo.PermissionViewChannel
			clone.Allow &^= discordgo.PermissionViewChannel
			foundEveryone = true
		}
		if ow.Type == discordgo.PermissionOverwriteTypeMember && ow.ID == botUserID {
			clone.Allow |= botAllow
			clone.Deny &^= botAllow
			foundBot = true
		}
		out = append(out, &clone)
	}
	if !foundEveryone {
		out = append(out, &discordgo.PermissionOverwrite{
			ID:   guildID,
			Type: discordgo.PermissionOverwriteTypeRole,
			Deny: discordgo.PermissionViewChannel,
		})
	}
	if !foundBot {
		out = append(out, &discordgo.PermissionOverwrite{
			ID:    botUserID,
			Type:  discordgo.PermissionOverwriteTypeMember,
			Allow: botAllow,
		})
	}
	return out
}
