package core

import "github.com/bwmarrin/discordgo"

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
