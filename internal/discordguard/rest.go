package discordguard

import (
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/omit"
	"github.com/disgoorg/snowflake/v2"
)

// The calls below are GuildOps on disgo's REST, named as disgo names them
// so they sit beside the discordgo ones until each plugin has moved. IDs
// are merlin's strings, parsed here, so a plugin keeps its own; payloads
// are disgo's. Every write goes through the same gate as the discordgo
// writes: pause, dry-run, the guild's cap for its op, the breaker and the
// journal.

// WithRest is the disgo client the methods below send through.
func (g *Guard) WithRest(r rest.Rest) *Guard {
	g.rest = r
	return g
}

// noPings is the AllowedMentions every message merlin sends carries: an
// explicit empty parse list. disgo writes an empty AllowedMentions as
// "parse":null, and what nothing pinging rests on should not be how
// Discord reads a null.
func noPings() *discord.AllowedMentions {
	return &discord.AllowedMentions{Parse: []discord.AllowedMentionType{}}
}

// ids parses each of s, stopping at the first that is not a snowflake.
func ids(s ...string) ([]snowflake.ID, error) {
	out := make([]snowflake.ID, len(s))
	for i, v := range s {
		id, err := snowflake.Parse(v)
		if err != nil {
			return nil, err
		}
		out[i] = id
	}
	return out, nil
}

// gated runs call as op against target under the guild's gate, recording
// the outcome.
func gated[T any](o *GuildOps, op, target string, call func() (T, error)) (T, error) {
	var zero T
	if err := o.allow(op); err != nil {
		return zero, err
	}
	jid := o.beginJournal(op, target)
	v, err := call()
	return v, o.record(jid, err)
}

// --- reads: never gated ---

// GetMessages is up to limit of channelID's messages older than before
// ("" for the newest), newest first.
func (o *GuildOps) GetMessages(channelID, before string, limit int) ([]discord.Message, error) {
	c, err := ids(channelID)
	if err != nil {
		return nil, err
	}
	var b snowflake.ID
	if before != "" {
		if b, err = snowflake.Parse(before); err != nil {
			return nil, err
		}
	}
	return o.guard.rest.GetMessages(c[0], 0, b, 0, limit)
}

func (o *GuildOps) GetWebhooks(channelID string) ([]discord.Webhook, error) {
	c, err := ids(channelID)
	if err != nil {
		return nil, err
	}
	return o.guard.rest.GetWebhooks(c[0])
}

func (o *GuildOps) GetMember(guildID, userID string) (*discord.Member, error) {
	x, err := ids(guildID, userID)
	if err != nil {
		return nil, err
	}
	return o.guard.rest.GetMember(x[0], x[1])
}

func (o *GuildOps) GetGuild(guildID string) (*discord.RestGuild, error) {
	g, err := ids(guildID)
	if err != nil {
		return nil, err
	}
	return o.guard.rest.GetGuild(g[0], false)
}

// CreateDMChannel opens a DM with userID. Not a write anybody sees, so
// ungated, as UserChannelCreate is.
func (o *GuildOps) CreateDMChannel(userID string) (*discord.DMChannel, error) {
	u, err := ids(userID)
	if err != nil {
		return nil, err
	}
	return o.guard.rest.CreateDMChannel(u[0])
}

// --- writes: gated ---

func (o *GuildOps) DeleteMessage(channelID, messageID string) error {
	x, err := ids(channelID, messageID)
	if err != nil {
		return err
	}
	_, err = gated(o, opMessageDelete, messageID, func() (struct{}, error) {
		return struct{}{}, o.guard.rest.DeleteMessage(x[0], x[1])
	})
	return err
}

// CreateMessage posts m with every mention suppressed, whatever m says.
func (o *GuildOps) CreateMessage(channelID string, m discord.MessageCreate) (*discord.Message, error) {
	c, err := ids(channelID)
	if err != nil {
		return nil, err
	}
	m.AllowedMentions = noPings()
	return gated(o, opMessageSend, channelID, func() (*discord.Message, error) {
		return o.guard.rest.CreateMessage(c[0], m)
	})
}

func (o *GuildOps) CreateWebhook(channelID, name string) (*discord.IncomingWebhook, error) {
	c, err := ids(channelID)
	if err != nil {
		return nil, err
	}
	return gated(o, opWebhookCreate, channelID, func() (*discord.IncomingWebhook, error) {
		return o.guard.rest.CreateWebhook(c[0], discord.WebhookCreate{Name: name})
	})
}

// CreateWebhookMessage posts through a webhook with every mention
// suppressed. It carries text a member wrote moments ago, reposted under
// their own name: without this a rewrite of "@everyone" would ping the
// server in that member's name. wait returns the message posted.
func (o *GuildOps) CreateWebhookMessage(webhookID, token string, m discord.WebhookMessageCreate, wait bool) (*discord.Message, error) {
	w, err := ids(webhookID)
	if err != nil {
		return nil, err
	}
	m.AllowedMentions = noPings()
	return gated(o, opWebhookExecute, webhookID, func() (*discord.Message, error) {
		return o.guard.rest.CreateWebhookMessage(w[0], token, m, rest.CreateWebhookMessageParams{Wait: wait})
	})
}

// TimeoutMember times userID out until until; nil lifts a timeout.
func (o *GuildOps) TimeoutMember(guildID, userID string, until *time.Time) error {
	x, err := ids(guildID, userID)
	if err != nil {
		return err
	}
	_, err = gated(o, opMemberTimeout, userID, func() (*discord.Member, error) {
		return o.guard.rest.UpdateMember(x[0], x[1], discord.MemberUpdate{CommunicationDisabledUntil: omit.New(until)})
	})
	return err
}
