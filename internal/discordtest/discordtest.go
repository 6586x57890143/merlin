// Package discordtest builds disgo values for module tests. disgo's
// channels are immutable values with unexported fields, so a fake keeps
// Channel records it can edit and hands disgo's out, built from the same
// JSON Discord sends.
package discordtest

import (
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/json/v2"
	"github.com/disgoorg/snowflake/v2"
)

// Channel is a channel or thread as a fake holds it. IDs are strings, as
// merlin stores them, and must be snowflakes.
type Channel struct {
	ID, GuildID, ParentID, OwnerID, Name, Topic string
	Type                                        discord.ChannelType
	Position, RateLimitPerUser                  int
	NSFW                                        bool
	PermissionOverwrites                        []*Overwrite
	// ArchivedAt is a thread's archive timestamp.
	ArchivedAt time.Time
}

// Overwrite is one permission overwrite, string-ID'd like Channel.
type Overwrite struct {
	ID          string
	Type        discord.PermissionOverwriteType
	Allow, Deny discord.Permissions
}

// Disgo is c as disgo has it.
func (c *Channel) Disgo() discord.GuildChannel {
	ows := make([]discord.PermissionOverwrite, len(c.PermissionOverwrites))
	for i, o := range c.PermissionOverwrites {
		id, _ := snowflake.Parse(o.ID)
		if o.Type == discord.PermissionOverwriteTypeMember {
			ows[i] = discord.MemberPermissionOverwrite{UserID: id, Allow: o.Allow, Deny: o.Deny}
		} else {
			ows[i] = discord.RolePermissionOverwrite{RoleID: id, Allow: o.Allow, Deny: o.Deny}
		}
	}
	raw := map[string]any{
		"name": c.Name, "type": c.Type, "position": c.Position,
		"topic": c.Topic, "nsfw": c.NSFW, "rate_limit_per_user": c.RateLimitPerUser,
		"permission_overwrites": ows,
	}
	for k, v := range map[string]string{"id": c.ID, "guild_id": c.GuildID, "parent_id": c.ParentID, "owner_id": c.OwnerID} {
		if v != "" {
			raw[k] = v
		}
	}
	if !c.ArchivedAt.IsZero() {
		raw["thread_metadata"] = map[string]any{"archived": true, "archive_timestamp": c.ArchivedAt}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		panic(err)
	}
	var u discord.UnmarshalChannel
	if err := json.Unmarshal(b, &u); err != nil {
		panic(err)
	}
	return u.Channel.(discord.GuildChannel)
}

// Thread is c as a public thread.
func (c *Channel) Thread() discord.GuildThread {
	cp := *c
	cp.Type = discord.ChannelTypeGuildPublicThread
	return cp.Disgo().(discord.GuildThread)
}

// Overwrites is ows as fixtures.
func Overwrites(ows []discord.PermissionOverwrite) []*Overwrite {
	out := make([]*Overwrite, 0, len(ows))
	for _, ow := range ows {
		o := &Overwrite{ID: ow.ID().String(), Type: ow.Type()}
		switch v := ow.(type) {
		case discord.RolePermissionOverwrite:
			o.Allow, o.Deny = v.Allow, v.Deny
		case discord.MemberPermissionOverwrite:
			o.Allow, o.Deny = v.Allow, v.Deny
		}
		out = append(out, o)
	}
	return out
}

// Find is ows' entry for one target, or nil.
func Find(ows []*Overwrite, id string, kind discord.PermissionOverwriteType) *Overwrite {
	for _, o := range ows {
		if o.ID == id && o.Type == kind {
			return o
		}
	}
	return nil
}

// DM is a DM channel with id, which disgo, too, only builds from JSON.
func DM(id string) *discord.DMChannel {
	var c discord.DMChannel
	if err := json.Unmarshal([]byte(`{"id":"`+id+`","type":1}`), &c); err != nil {
		panic(err)
	}
	return &c
}
