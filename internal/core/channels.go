package core

import (
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

// DenyEveryone returns src with @everyone denied View Channel and the bot
// granted botAllow, merged into src's own overwrites for either rather than
// added beside them.
//
// This exists because "deny everyone, then separately remember to grant
// the bot" is exactly the bug this codebase hit three times over: adminconfig's
// setup channels, rotation's hidden staging channel, and rotation's archived
// channel all denied @everyone without a matching bot grant, locking the
// bot itself out of channels it had just created. One call for both halves
// makes forgetting the bot grant impossible.
//
// botAllow should only include permission bits the bot's own Discord role
// actually holds at the guild level: Discord rejects the whole request
// (403 "Missing Permissions") if an overwrite tries to grant a bit the
// actor doesn't have.
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
