package discordguard

import (
	"errors"
	"slices"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/omit"
	"github.com/disgoorg/snowflake/v2"
)

// The calls below are GuildOps on disgo's REST, named and shaped as disgo
// has them so they sit beside the discordgo ones until each plugin has
// moved. Every write goes through the same gate as the discordgo writes:
// pause, dry-run, the guild's cap for its op, the breaker and the journal.

// WithRest is the disgo client the methods below send through.
func (g *Guard) WithRest(r rest.Rest) *Guard {
	g.rest = r
	return g
}

// errNoID is a call naming ID 0, which is what core.ID makes of a stored ID
// that is not a snowflake. Refused here rather than spent on a 404 that
// counts against Discord's invalid request limit.
var errNoID = errors.New("discordguard: no such ID")

func known(ids ...snowflake.ID) error {
	if slices.Contains(ids, 0) {
		return errNoID
	}
	return nil
}

// noPings is the AllowedMentions every message merlin sends carries: an
// explicit empty parse list. disgo writes an empty AllowedMentions as
// "parse":null, and what nothing pinging rests on should not be how
// Discord reads a null.
func noPings() *discord.AllowedMentions {
	return &discord.AllowedMentions{Parse: []discord.AllowedMentionType{}}
}

// gated runs call as op against target under the guild's gate, recording
// the outcome.
func gated[T any](o *GuildOps, op string, target snowflake.ID, call func() (T, error)) (T, error) {
	var zero T
	if err := o.allow(op); err != nil {
		return zero, err
	}
	jid := o.beginJournal(op, target.String())
	v, err := call()
	return v, o.record(jid, err)
}

func gatedErr(o *GuildOps, op string, target snowflake.ID, call func() error) error {
	_, err := gated(o, op, target, func() (struct{}, error) { return struct{}{}, call() })
	return err
}

// --- reads: never gated ---

func (o *GuildOps) GetChannel(channelID snowflake.ID) (discord.Channel, error) {
	if err := known(channelID); err != nil {
		return nil, err
	}
	return o.guard.rest.GetChannel(channelID)
}

func (o *GuildOps) GetGuildChannels(guildID snowflake.ID) ([]discord.GuildChannel, error) {
	if err := known(guildID); err != nil {
		return nil, err
	}
	return o.guard.rest.GetGuildChannels(guildID)
}

// GetActiveGuildThreads is every active thread in the guild; callers filter
// by parent. The channel-scoped endpoint was removed in API v10.
func (o *GuildOps) GetActiveGuildThreads(guildID snowflake.ID) (*discord.GuildActiveThreads, error) {
	if err := known(guildID); err != nil {
		return nil, err
	}
	return o.guard.rest.GetActiveGuildThreads(guildID)
}

func (o *GuildOps) GetRoles(guildID snowflake.ID) ([]discord.Role, error) {
	if err := known(guildID); err != nil {
		return nil, err
	}
	return o.guard.rest.GetRoles(guildID)
}

// GetPublicArchivedThreads is the other half of "every thread under this
// channel": the active list leaves archived threads out, and a forum post
// archives itself after its parent's inactivity window. Pages run from
// before (zero for the newest) backwards.
func (o *GuildOps) GetPublicArchivedThreads(channelID snowflake.ID, before time.Time, limit int) (*discord.GetThreads, error) {
	if err := known(channelID); err != nil {
		return nil, err
	}
	return o.guard.rest.GetPublicArchivedThreads(channelID, before, limit)
}

func (o *GuildOps) GetMessage(channelID, messageID snowflake.ID) (*discord.Message, error) {
	if err := known(channelID, messageID); err != nil {
		return nil, err
	}
	return o.guard.rest.GetMessage(channelID, messageID)
}

// GetMessages is up to limit of channelID's messages older than before (0
// for the newest), newest first.
func (o *GuildOps) GetMessages(channelID, before snowflake.ID, limit int) ([]discord.Message, error) {
	if err := known(channelID); err != nil {
		return nil, err
	}
	return o.guard.rest.GetMessages(channelID, 0, before, 0, limit)
}

func (o *GuildOps) GetWebhooks(channelID snowflake.ID) ([]discord.Webhook, error) {
	if err := known(channelID); err != nil {
		return nil, err
	}
	return o.guard.rest.GetWebhooks(channelID)
}

func (o *GuildOps) GetMember(guildID, userID snowflake.ID) (*discord.Member, error) {
	if err := known(guildID, userID); err != nil {
		return nil, err
	}
	return o.guard.rest.GetMember(guildID, userID)
}

func (o *GuildOps) GetGuild(guildID snowflake.ID) (*discord.RestGuild, error) {
	if err := known(guildID); err != nil {
		return nil, err
	}
	return o.guard.rest.GetGuild(guildID, false)
}

// GetMembers pages the guild's member list. A read, but the one whose cost
// scales with guild size, and it needs the GUILD_MEMBERS intent to return
// anything.
func (o *GuildOps) GetMembers(guildID snowflake.ID, limit int, after snowflake.ID) ([]discord.Member, error) {
	if err := known(guildID); err != nil {
		return nil, err
	}
	return o.guard.rest.GetMembers(guildID, limit, after)
}

func (o *GuildOps) GetUser(userID snowflake.ID) (*discord.User, error) {
	if err := known(userID); err != nil {
		return nil, err
	}
	return o.guard.rest.GetUser(userID)
}

// CreateDMChannel opens a DM with userID. Not a write anybody sees, so
// ungated, as UserChannelCreate is.
func (o *GuildOps) CreateDMChannel(userID snowflake.ID) (*discord.DMChannel, error) {
	if err := known(userID); err != nil {
		return nil, err
	}
	return o.guard.rest.CreateDMChannel(userID)
}

// --- writes: gated ---

func (o *GuildOps) CreateGuildChannel(guildID snowflake.ID, c discord.GuildChannelCreate) (discord.GuildChannel, error) {
	if err := known(guildID); err != nil {
		return nil, err
	}
	return gated(o, opChannelCreate, guildID, func() (discord.GuildChannel, error) {
		return o.guard.rest.CreateGuildChannel(guildID, c)
	})
}

func (o *GuildOps) UpdateChannel(channelID snowflake.ID, u discord.ChannelUpdate) (discord.Channel, error) {
	if err := known(channelID); err != nil {
		return nil, err
	}
	return gated(o, opChannelEdit, channelID, func() (discord.Channel, error) {
		return o.guard.rest.UpdateChannel(channelID, u)
	})
}

// UpdatePermissionOverwrite replaces one target's overwrite on channelID.
func (o *GuildOps) UpdatePermissionOverwrite(channelID, targetID snowflake.ID, u discord.PermissionOverwriteUpdate) error {
	if err := known(channelID, targetID); err != nil {
		return err
	}
	return gatedErr(o, opChannelPermissions, channelID, func() error {
		return o.guard.rest.UpdatePermissionOverwrite(channelID, targetID, u)
	})
}

func (o *GuildOps) DeleteChannel(channelID snowflake.ID) error {
	if err := known(channelID); err != nil {
		return err
	}
	return gatedErr(o, opChannelDelete, channelID, func() error { return o.guard.rest.DeleteChannel(channelID) })
}

func (o *GuildOps) PinMessage(channelID, messageID snowflake.ID) error {
	if err := known(channelID, messageID); err != nil {
		return err
	}
	return gatedErr(o, opMessagePin, channelID, func() error { return o.guard.rest.PinMessage(channelID, messageID) })
}

func (o *GuildOps) DeleteMessage(channelID, messageID snowflake.ID) error {
	if err := known(channelID, messageID); err != nil {
		return err
	}
	return gatedErr(o, opMessageDelete, messageID, func() error { return o.guard.rest.DeleteMessage(channelID, messageID) })
}

// CreateMessage posts m with every mention suppressed, whatever m says.
func (o *GuildOps) CreateMessage(channelID snowflake.ID, m discord.MessageCreate) (*discord.Message, error) {
	if err := known(channelID); err != nil {
		return nil, err
	}
	m.AllowedMentions = noPings()
	return gated(o, opMessageSend, channelID, func() (*discord.Message, error) {
		return o.guard.rest.CreateMessage(channelID, m)
	})
}

func (o *GuildOps) CreateWebhook(channelID snowflake.ID, name string) (*discord.IncomingWebhook, error) {
	if err := known(channelID); err != nil {
		return nil, err
	}
	return gated(o, opWebhookCreate, channelID, func() (*discord.IncomingWebhook, error) {
		return o.guard.rest.CreateWebhook(channelID, discord.WebhookCreate{Name: name})
	})
}

// CreateWebhookMessage posts through a webhook with every mention
// suppressed. It carries text a member wrote moments ago, reposted under
// their own name: without this a rewrite of "@everyone" would ping the
// server in that member's name. wait returns the message posted.
func (o *GuildOps) CreateWebhookMessage(webhookID snowflake.ID, token string, m discord.WebhookMessageCreate, wait bool) (*discord.Message, error) {
	if err := known(webhookID); err != nil {
		return nil, err
	}
	m.AllowedMentions = noPings()
	return gated(o, opWebhookExecute, webhookID, func() (*discord.Message, error) {
		return o.guard.rest.CreateWebhookMessage(webhookID, token, m, rest.CreateWebhookMessageParams{Wait: wait})
	})
}

// UpdateMember edits a member: in merlin, replaces their roles.
func (o *GuildOps) UpdateMember(guildID, userID snowflake.ID, u discord.MemberUpdate) (*discord.Member, error) {
	if err := known(guildID, userID); err != nil {
		return nil, err
	}
	return gated(o, opMemberEdit, userID, func() (*discord.Member, error) {
		return o.guard.rest.UpdateMember(guildID, userID, u)
	})
}

// DisconnectMember drops userID from whatever voice channel they are in.
// That is a channel_id of null, which disgo's MemberUpdate cannot say (a
// nil pointer is left out), so the request is written by hand.
func (o *GuildOps) DisconnectMember(guildID, userID snowflake.ID) error {
	if err := known(guildID, userID); err != nil {
		return err
	}
	return gatedErr(o, opMemberEdit, userID, func() error {
		return o.guard.rest.Do(rest.UpdateMember.Compile(nil, guildID, userID), map[string]any{"channel_id": nil}, nil)
	})
}

// AddMemberRole adds one role. opts carries an audit log reason where the
// caller has one.
func (o *GuildOps) AddMemberRole(guildID, userID, roleID snowflake.ID, opts ...rest.RequestOpt) error {
	if err := known(guildID, userID, roleID); err != nil {
		return err
	}
	return gatedErr(o, opMemberRoleAdd, userID, func() error {
		return o.guard.rest.AddMemberRole(guildID, userID, roleID, opts...)
	})
}

func (o *GuildOps) RemoveMemberRole(guildID, userID, roleID snowflake.ID) error {
	if err := known(guildID, userID, roleID); err != nil {
		return err
	}
	return gatedErr(o, opMemberRoleRemove, userID, func() error {
		return o.guard.rest.RemoveMemberRole(guildID, userID, roleID)
	})
}

func (o *GuildOps) CreateRole(guildID snowflake.ID, r discord.RoleCreate) (*discord.Role, error) {
	if err := known(guildID); err != nil {
		return nil, err
	}
	return gated(o, opRoleCreate, guildID, func() (*discord.Role, error) {
		return o.guard.rest.CreateRole(guildID, r)
	})
}

// UpdateRolePositions moves roles in the hierarchy. Same budget as an
// edit, since to Discord it is one: a PATCH on the roles collection.
func (o *GuildOps) UpdateRolePositions(guildID snowflake.ID, u []discord.RolePositionUpdate) ([]discord.Role, error) {
	if err := known(guildID); err != nil {
		return nil, err
	}
	return gated(o, opRoleEdit, guildID, func() ([]discord.Role, error) {
		return o.guard.rest.UpdateRolePositions(guildID, u)
	})
}

func (o *GuildOps) DeletePermissionOverwrite(channelID, targetID snowflake.ID) error {
	if err := known(channelID, targetID); err != nil {
		return err
	}
	return gatedErr(o, opChannelPermissions, channelID, func() error {
		return o.guard.rest.DeletePermissionOverwrite(channelID, targetID)
	})
}

// TimeoutMember times userID out until until; nil lifts a timeout.
func (o *GuildOps) TimeoutMember(guildID, userID snowflake.ID, until *time.Time) error {
	if err := known(guildID, userID); err != nil {
		return err
	}
	return gatedErr(o, opMemberTimeout, userID, func() error {
		_, err := o.guard.rest.UpdateMember(guildID, userID, discord.MemberUpdate{CommunicationDisabledUntil: omit.New(until)})
		return err
	})
}
