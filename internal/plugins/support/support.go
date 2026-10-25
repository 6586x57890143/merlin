// Package support is merlin's half of backing the birds: /support posts
// the advert that sends people to cygnet.lol/support, and a new supporter
// (roles publishes core.EventSupportersJoined from cygnet's ledger) gets
// the welcome card in the channel picked on /roles settings. The words are
// cygnet/support's, shared by every bird; the layout is merlin's panel,
// led by her banner on the advert and her heart-in-beak face on the
// welcome.
package support

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	cards "github.com/6586x57890143/cygnet/support"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/voice"
)

// Sender posts the welcome card. discordguard satisfies it, so a paused or
// dry-run guild posts nothing.
type Sender interface {
	CreateMessage(channelID snowflake.ID, data discord.MessageCreate) (*discord.Message, error)
}

// Settings is what the plugin reads: whether it is on in a guild, and
// where new supporters are thanked.
type Settings interface {
	PluginEnabled(guildID, plugin string) bool
	SupporterChannelID(guildID string) string
}

// cooldown is how long after /support posts in a channel a repeat answers
// only whoever asked: it is public so anyone can share it, and the
// cooldown is what stops anyone repeating it into a flood.
const cooldown = 10 * time.Minute

// maxThanked is the most new supporters one card names; past it "and N
// more", so a backlog is still one card.
const maxThanked = 10

// sendTimeout bounds posting the welcome card, which runs off the bus.
const sendTimeout = 15 * time.Second

type Plugin struct {
	ops      func(guildID string) Sender
	settings Settings
	voice    voice.Source
	owner    string
	log      *slog.Logger
	now      func() time.Time

	mu     sync.Mutex
	posted map[string]time.Time // channel ID -> when /support last posted there
}

// New makes the plugin; owner is the Discord ID of whoever builds the
// birds (the bootstrap operator), named on the advert, or "".
func New(ops func(guildID string) Sender, settings Settings, speaker voice.Source, owner string) *Plugin {
	return &Plugin{ops: ops, settings: settings, voice: speaker, owner: owner, now: time.Now, posted: map[string]time.Time{}}
}

func (p *Plugin) Name() string { return "support" }

func (p *Plugin) Init(deps core.Deps) error {
	p.log = deps.Logger
	deps.Commands.RegisterCommand(p.Name(), discord.SlashCommandCreate{
		Name:        "support",
		Description: "How to back the bird bots",
	})
	deps.Commands.HandleIx("support", "", core.PermSpec{Tier: core.TierPublic}, p.handleSupport)
	if deps.Bus != nil {
		deps.Bus.Subscribe(core.EventSupportersJoined, p.Name(), p.handleJoined)
	}
	return nil
}

func (p *Plugin) Start(context.Context) error    { return nil }
func (p *Plugin) Shutdown(context.Context) error { return nil }

// brand lends the cards merlin's app emoji and the owner's mention.
func (p *Plugin) brand() cards.Brand {
	return cards.Brand{
		Inline: func(key string) string { return core.AppEmoji("support_" + key) },
		Button: func(key string) *discord.ComponentEmoji { return core.AppComponentEmoji("support_" + key) },
		Owner:  p.owner,
	}
}

// accent is the cards' bar: the frame copper of the perk tiles and the
// campaign banner, so the bar matches the art beside it.
const accent = 0xD9774B

// panel lays a card out in merlin's panel: the campaign's copper, her rule,
// and the campaign banner or her heart-in-beak face.
func panel(pg cards.Page) core.Panel {
	return core.Panel{Color: accent, Mood: core.MoodSupporter, Title: pg.Title, Line: pg.Line, Parts: pg.Parts, Footer: pg.Footer, Hero: pg.Hero}
}

// handleSupport posts the advert in the channel, or, inside the cooldown
// after the last one there, shows it to whoever asked.
func (p *Plugin) handleSupport(ctx context.Context, ix *core.Ix) {
	pn := panel(cards.Advert(p.brand()))
	if !p.claim(ix.ChannelID) {
		if err := ix.RespondPanel(pn); err != nil {
			p.log.Error("support: respond", "err", err)
		}
		return
	}
	if err := ix.RespondPanelPublic(pn); err != nil {
		p.log.Error("support: post", "err", err)
	}
}

// claim reports whether /support may post in channelID now, and if so
// starts its cooldown there.
func (p *Plugin) claim(channelID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if at, ok := p.posted[channelID]; ok && now.Sub(at) < cooldown {
		return false
	}
	p.posted[channelID] = now
	return true
}

// handleJoined thanks new supporters, off the bus's goroutine since it
// talks to Discord.
func (p *Plugin) handleJoined(ctx context.Context, ev core.Event) {
	joined, ok := ev.Payload.(core.SupportersJoinedPayload)
	if !ok || len(joined.UserIDs) == 0 || !p.settings.PluginEnabled(ev.GuildID, p.Name()) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
		defer cancel()
		p.thank(ctx, ev.GuildID, joined.UserIDs)
	}()
}

// thank posts the welcome card naming userIDs, in the channel picked on
// /roles settings; none picked, no card. Nothing pings: the guard's
// senders zero AllowedMentions.
func (p *Plugin) thank(ctx context.Context, guildID string, userIDs []string) {
	channel := p.settings.SupporterChannelID(guildID)
	if channel == "" {
		return
	}
	names, more := userIDs, ""
	if len(names) > maxThanked {
		names, more = names[:maxThanked], " and "+strconv.Itoa(len(userIDs)-maxThanked)+" more"
	}
	line := p.voice.Line(ctx, guildID, voice.KeySupporterThanks, map[string]string{"members": listMentions(names) + more})
	if line == "" {
		return
	}
	pn := panel(cards.Welcome(p.brand(), line))
	if _, err := p.ops(guildID).CreateMessage(core.ID(channel), discord.MessageCreate{
		Components: pn.Layout(), Files: pn.Uploads(), Flags: discord.MessageFlagIsComponentsV2,
	}); err != nil {
		p.log.Info("support: could not thank new supporters", "guild", guildID, "err", err)
	}
}

// listMentions is user mentions joined the owner's way: "a, b and c".
func listMentions(ids []string) string {
	m := make([]string, len(ids))
	for i, id := range ids {
		m[i] = core.MentionUser(id)
	}
	if len(m) < 2 {
		return strings.Join(m, "")
	}
	return strings.Join(m[:len(m)-1], ", ") + " and " + m[len(m)-1]
}

// Help is this plugin's page in /help.
func (p *Plugin) Help() core.Help {
	return core.Help{
		Title: "Support",
		Line:  "how to back the bird bots",
		About: "`/support` shows how to back the bird bots at cygnet.lol/support, and what it buys: a supporter role, preview builds, early access and a say. anyone can post it; in a channel where it was just posted, it shows only to you. when somebody starts backing the birds, i thank them in the channel picked on `/roles settings`. nothing i do is ever locked behind paying.",
	}
}
