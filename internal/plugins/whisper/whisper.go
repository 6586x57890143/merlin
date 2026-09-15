// Package whisper lets a member Discord has chat-restricted talk through
// merlin. /whisper <text> posts the text in the channel it was run in,
// through a per-channel webhook wearing the member's display name and
// avatar, with a subtext line under it naming their real Discord username.
//
// A restricted account cannot send messages but can still run slash
// commands, which is the whole gap this fills, and it is also why the
// plugin is off in every guild until an admin runs
// /config plugins set whisper true: the bot publishing on members' behalf is
// something a server should choose, not discover.
//
// Every whisper is screened before it exists. The free regex suite in
// filter.go runs first and refuses on any hit; then aimod.Screen runs rung 1
// (hard slurs, credentials, phishing) unconditionally and the model rungs
// where the guild has them funded, falling back to the regex alone when the
// model is unavailable. Nothing is ever rewritten: a whisper that trips
// anything is refused, the member is told exactly why, and the refusal is
// audited without its text.
package whisper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordguard"
)

// webhookName is the webhook this plugin creates per channel. Its own name
// rather than aimod's, so a moderator reading the channel's integrations
// tab can tell a rewrite from a whisper.
const webhookName = "merlin whisper"

// Screener judges text before merlin posts it in somebody's name.
// Satisfied by *aimod.Plugin, wired in cmd/bot/main.go; this package never
// imports aimod. An empty refusal means post it; err means the model was
// unavailable and the free rungs are all that judged it.
type Screener interface {
	Screen(ctx context.Context, guildID, channelID, authorID, text string) (refusal string, err error)
}

// DiscordOps is this plugin's narrow view of Discord, satisfied structurally
// by *discordguard.GuildOps so the webhook post is gated by pause, dry-run
// and the per-guild rate cap, and so every mention in it is suppressed.
type DiscordOps interface {
	Channel(channelID string, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	ChannelWebhooks(channelID string, options ...discordgo.RequestOption) ([]*discordgo.Webhook, error)
	WebhookCreate(channelID, name, avatar string, options ...discordgo.RequestOption) (*discordgo.Webhook, error)
	WebhookExecute(webhookID, token string, data *discordgo.WebhookParams, options ...discordgo.RequestOption) error
}

type OpsProvider func(guildID string) DiscordOps

type Plugin struct {
	screener Screener
	ops      OpsProvider
	audit    core.AuditWriter
	log      *slog.Logger
	now      func() time.Time
	limits   *limiter

	webhookMu sync.Mutex
	webhooks  map[string]*discordgo.Webhook
}

func New(screener Screener, ops OpsProvider) *Plugin {
	return &Plugin{
		screener: screener,
		ops:      ops,
		log:      slog.Default(),
		now:      func() time.Time { return time.Now().UTC() },
		limits:   newLimiter(),
		webhooks: make(map[string]*discordgo.Webhook),
	}
}

func (p *Plugin) Name() string { return "whisper" }

func (p *Plugin) Init(deps core.Deps) error {
	p.audit = deps.Audit
	if deps.Logger != nil {
		p.log = deps.Logger
	}
	deps.Commands.RegisterCommand(p.Name(), &discordgo.ApplicationCommand{
		Name:        "whisper",
		Description: "Say something here through merlin, if Discord will not let you",
		Options: []*discordgo.ApplicationCommandOption{{
			Type:        discordgo.ApplicationCommandOptionString,
			Name:        "text",
			Description: "One line. No links, no mentions. Posted under your name, with your username underneath",
			Required:    true,
			// A client hint; check() re-measures, because a limit the
			// client enforces is not a limit.
			MaxLength: maxLen,
		}},
	})
	// TierPublic with an Action, which is what lets a guild
	// /config permissions deny whisper.say user:@somebody, or raise the tier,
	// with no code involved.
	deps.Commands.Handle("whisper", "", core.PermSpec{Tier: core.TierPublic, Action: "whisper.say"}, p.handle)
	return nil
}

func (p *Plugin) Start(context.Context) error    { return nil }
func (p *Plugin) Shutdown(context.Context) error { return nil }

func (p *Plugin) handle(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Member == nil || i.Member.User == nil {
		core.RespondErr(s, i, "Not here", errors.New("whispers only work inside a server"))
		return
	}
	var text string
	if arg := core.LeafArgs(i)["text"]; arg != nil {
		text = arg.StringValue()
	}
	// Always deferred: the channel lookup and the model screen together run
	// well past Discord's three seconds, and a refusal that is instant loses
	// nothing by landing as a follow-up.
	if err := core.DeferResponse(s, i); err != nil {
		return
	}
	refusal, err := p.post(ctx, i.GuildID, i.ChannelID, i.Member, text)
	switch {
	case refusal != "":
		_ = core.FollowUpErr(s, i, "Not posted", errors.New(refusal))
	case discordguard.Skipped(err):
		_ = core.FollowUpErr(s, i, "Not posted", errors.New("merlin is paused in this server right now"))
	case err != nil:
		p.log.Error("whisper: post failed", "guild", i.GuildID, "channel", i.ChannelID, "user", i.Member.User.ID, "err", err)
		_ = core.FollowUpErr(s, i, "Could not post that", errors.New("something went wrong on merlin's side; try again in a moment"))
	default:
		// The whisper is in the channel; a confirmation on top of it would
		// be litter in a conversation.
		_ = s.InteractionResponseDelete(i.Interaction)
	}
}

// post runs the whole ladder for one whisper and, if it passes, publishes
// it. refusal is a sentence for the member; err is a failure to post.
func (p *Plugin) post(ctx context.Context, guildID, channelID string, m *discordgo.Member, text string) (refusal string, err error) {
	userID := m.User.ID
	text = strings.TrimSpace(text)

	// The limiter first, before anything costs a call, and charged whether or
	// not the whisper goes out, so a refusal is not a free retry.
	if !p.limits.allow(guildID+":"+userID, p.now(), userGap, userHourly) {
		return p.refuse(ctx, guildID, channelID, userID, "slow down: a few seconds between whispers, and not more than a few dozen an hour"), nil
	}
	if !p.limits.allow(guildID, p.now(), 0, guildHourly) {
		return p.refuse(ctx, guildID, channelID, userID, "this server has whispered as much as it can for the hour"), nil
	}
	if r := check(text); r != "" {
		return p.refuse(ctx, guildID, channelID, userID, r), nil
	}

	ops := p.ops(guildID)
	ch, err := ops.Channel(channelID)
	if err != nil {
		return "", fmt.Errorf("whisper: read channel: %w", err)
	}
	if ch.Type != discordgo.ChannelTypeGuildText {
		return p.refuse(ctx, guildID, channelID, userID, "whispers only work in ordinary text channels"), nil
	}
	// Being in the room is the requirement, and only that. A member who can
	// see the channel may whisper into it, whatever the server's Send
	// Messages bit says for them: the whole audience of this command is
	// people who cannot send, and a channel the server keeps them out of is
	// already one they cannot run the command in. Member.Permissions is
	// computed by Discord for this channel on every interaction, so this is
	// free, and no overwrite is read by hand.
	if m.Permissions&discordgo.PermissionViewChannel == 0 {
		return p.refuse(ctx, guildID, channelID, userID, "you cannot see this channel, so nothing can be posted into it for you"), nil
	}

	// No screener wired means nothing checked the text for a slur, and the
	// answer to that is no, never "post it anyway".
	if p.screener == nil {
		return p.refuse(ctx, guildID, channelID, userID, "screening is unavailable, so nothing can be posted"), nil
	}
	r, err := p.screener.Screen(ctx, guildID, channelID, userID, text)
	if r != "" {
		return p.refuse(ctx, guildID, channelID, userID, r), nil
	}
	if err != nil {
		// The model was unavailable. The free rungs all ran, and the guild
		// chose to post on those alone rather than go quiet with the model.
		p.log.Warn("whisper: model screen unavailable, posting on the free rungs alone",
			"guild", guildID, "channel", channelID, "user", userID, "err", err)
	}

	hook, err := p.resolveWebhook(ops, channelID)
	if err != nil {
		return "", fmt.Errorf("whisper: resolve webhook: %w", err)
	}
	if err := ops.WebhookExecute(hook.ID, hook.Token, &discordgo.WebhookParams{
		Content:   text + marker(m.User.Username),
		Username:  webhookUsername(m),
		AvatarURL: m.AvatarURL(""),
	}); err != nil {
		// Whatever it was, re-resolve next time rather than fail forever
		// against a webhook somebody deleted.
		p.forgetWebhook(channelID)
		return "", err
	}
	p.log.Info("whisper: posted", "guild", guildID, "channel", channelID, "user", userID)
	return "", nil
}

// marker is the line under every whisper. The Discord username rather than
// the display name a webhook post already wears: a webhook message has no
// profile to click, so this is the one place a reader can learn who is
// actually talking, and a username cannot be made to look like a mod's.
func marker(username string) string {
	return "\n-# whispered through merlin by @" + username
}

// refuse audits a refusal, with the reason and never the text, and hands
// the reason back to the caller. Log-and-continue on an audit failure, like
// every audit call site in this codebase.
func (p *Plugin) refuse(ctx context.Context, guildID, channelID, userID, reason string) string {
	if p.audit != nil {
		detail := fmt.Sprintf("%s in %s: %s", core.MentionUser(userID), core.MentionChannel(channelID), reason)
		if err := p.audit.Record(ctx, guildID, userID, "whisper.refused", "", detail); err != nil {
			p.log.Error("whisper: audit record failed", "guild", guildID, "err", err)
		}
	}
	return reason
}

// webhookUsername is the name the post wears: nick, then global name, then
// username. Discord refuses a webhook name containing "clyde" or "discord",
// and a member who set such a nick should still be able to whisper, so those
// fall through to the username, which cannot contain either.
func webhookUsername(m *discordgo.Member) string {
	for _, name := range []string{m.Nick, m.User.GlobalName} {
		lower := strings.ToLower(name)
		if name != "" && !strings.Contains(lower, "clyde") && !strings.Contains(lower, "discord") {
			return name
		}
	}
	return m.User.Username
}

// resolveWebhook finds or creates this plugin's webhook for a channel,
// cached per process. The same find-by-name-or-create shape as aimod's.
func (p *Plugin) resolveWebhook(ops DiscordOps, channelID string) (*discordgo.Webhook, error) {
	p.webhookMu.Lock()
	defer p.webhookMu.Unlock()
	if hook, ok := p.webhooks[channelID]; ok {
		return hook, nil
	}
	existing, err := ops.ChannelWebhooks(channelID)
	if err != nil {
		return nil, err
	}
	for _, hook := range existing {
		// Token, not just name: Discord omits the token from webhooks this
		// application did not create, and one without it cannot be executed.
		if hook.Name == webhookName && hook.Token != "" {
			p.webhooks[channelID] = hook
			return hook, nil
		}
	}
	created, err := ops.WebhookCreate(channelID, webhookName, "")
	if err != nil {
		return nil, err
	}
	p.webhooks[channelID] = created
	return created, nil
}

func (p *Plugin) forgetWebhook(channelID string) {
	p.webhookMu.Lock()
	defer p.webhookMu.Unlock()
	delete(p.webhooks, channelID)
}
