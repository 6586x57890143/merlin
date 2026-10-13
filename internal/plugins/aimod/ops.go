package aimod

import (
	"time"

	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordguard"
)

// DiscordOps is what aimod does in Discord. Reads come back as aimod's own
// views, with merlin's string IDs; writes take disgo's payloads. In
// production it is GuardOps, so every write is gated by pause, dry-run, the
// per-guild rate cap and the action journal without this package knowing
// any of that exists.
type DiscordOps interface {
	DeleteMessage(channelID, messageID string) error
	// History is up to limit of channelID's messages older than before (""
	// for the newest), newest first.
	History(channelID, before string, limit int) ([]Line, error)
	Webhooks(channelID string) ([]Hook, error)
	CreateWebhook(channelID, name string) (Hook, error)
	// PostAs posts m through hook, returning the message's ID when wait.
	PostAs(hook Hook, m discord.WebhookMessageCreate, wait bool) (string, error)
	Member(guildID, userID string) (Member, error)
	TimeoutMember(guildID, userID string, until *time.Time) error
	Guild(guildID string) (Guild, error)
	// DM sends m to userID privately.
	DM(userID string, m discord.MessageCreate) error
}

// Line is one message of a channel's history: who wrote it and what. An
// empty AuthorID is an author Discord did not say.
type Line struct{ AuthorID, Content string }

// Hook is a webhook merlin can post through. Token is empty for one another
// application made, which cannot be.
type Hook struct{ ID, Name, Token string }

// Member is what aimod reads of a member: the name and avatar a rewrite is
// reposted under, and the roles the rank check reads.
type Member struct {
	Name, AvatarURL string
	RoleIDs         []string
}

// Guild is what aimod reads of a server: its name for a DM, its owner, and
// what each role permits for the rank check.
type Guild struct {
	Name, OwnerID string
	RolePerms     map[string]discord.Permissions
}

// GuardOps is DiscordOps on the guard's disgo calls.
func GuardOps(o *discordguard.GuildOps) DiscordOps { return guardOps{o} }

type guardOps struct{ o *discordguard.GuildOps }

func (g guardOps) DeleteMessage(channelID, messageID string) error {
	return g.o.DeleteMessage(core.ID(channelID), core.ID(messageID))
}

func (g guardOps) History(channelID, before string, limit int) ([]Line, error) {
	msgs, err := g.o.GetMessages(core.ID(channelID), core.ID(before), limit)
	if err != nil {
		return nil, err
	}
	out := make([]Line, len(msgs))
	for i, m := range msgs {
		out[i].Content = m.Content
		if m.Author.ID != 0 {
			out[i].AuthorID = m.Author.ID.String()
		}
	}
	return out, nil
}

func (g guardOps) Webhooks(channelID string) ([]Hook, error) {
	hooks, err := g.o.GetWebhooks(core.ID(channelID))
	if err != nil {
		return nil, err
	}
	var out []Hook
	for _, h := range hooks {
		if in, ok := h.(discord.IncomingWebhook); ok {
			out = append(out, Hook{ID: in.ID().String(), Name: in.Name(), Token: in.Token})
		}
	}
	return out, nil
}

func (g guardOps) CreateWebhook(channelID, name string) (Hook, error) {
	h, err := g.o.CreateWebhook(core.ID(channelID), name)
	if err != nil {
		return Hook{}, err
	}
	return Hook{ID: h.ID().String(), Name: h.Name(), Token: h.Token}, nil
}

func (g guardOps) PostAs(hook Hook, m discord.WebhookMessageCreate, wait bool) (string, error) {
	msg, err := g.o.CreateWebhookMessage(core.ID(hook.ID), hook.Token, m, wait)
	if err != nil || msg == nil {
		return "", err
	}
	return msg.ID.String(), nil
}

func (g guardOps) Member(guildID, userID string) (Member, error) {
	m, err := g.o.GetMember(core.ID(guildID), core.ID(userID))
	if err != nil {
		return Member{}, err
	}
	out := Member{Name: m.EffectiveName(), AvatarURL: m.EffectiveAvatarURL()}
	for _, r := range m.RoleIDs {
		out.RoleIDs = append(out.RoleIDs, r.String())
	}
	return out, nil
}

func (g guardOps) TimeoutMember(guildID, userID string, until *time.Time) error {
	return g.o.TimeoutMember(core.ID(guildID), core.ID(userID), until)
}

func (g guardOps) Guild(guildID string) (Guild, error) {
	rg, err := g.o.GetGuild(core.ID(guildID))
	if err != nil {
		return Guild{}, err
	}
	out := Guild{Name: rg.Name, OwnerID: rg.OwnerID.String(), RolePerms: make(map[string]discord.Permissions, len(rg.Roles))}
	for _, r := range rg.Roles {
		out.RolePerms[r.ID.String()] = r.Permissions
	}
	return out, nil
}

func (g guardOps) DM(userID string, m discord.MessageCreate) error {
	dm, err := g.o.CreateDMChannel(core.ID(userID))
	if err != nil {
		return err
	}
	_, err = g.o.CreateMessage(dm.ID(), m)
	return err
}
