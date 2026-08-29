package aimod

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordguard"
	"github.com/6586x57890143/merlin/internal/voice"
)

// webhookName is the webhook this plugin creates per channel to repost a
// rewritten message under the author's own name and avatar.
//
// One per channel, reused. Discord caps a channel at 15 webhooks, and a
// plugin that created one per rewrite would exhaust that in an afternoon and
// then start failing with an error nobody would connect to moderation.
const webhookName = "merlin message cleanup"

// rewriteMarker is appended to a reposted message.
//
// Not optional, and not configurable. The repost wears the author's name and
// avatar, which is the only way to keep a conversation readable, but it is
// also words attributed to somebody that they did not write in that form.
// Everyone reading the channel is entitled to know that happened, and the
// author is entitled to have it be obvious rather than deniable. If this
// ever needs to be shorter, it does not need to be absent.
const rewriteMarker = "\n-# edited by merlin - `/aimod why` for details"

// enforce carries out one confirmed verdict.
//
// Ordering is the correctness argument, and it is the same one as
// roles.applyJail: the incident is recorded before the message is touched.
// Deleting first and then failing to record leaves the message gone with no
// copy of what it said, nothing for /aimod undo to restore, and nothing to
// show a member who asks what happened. Recording first can only fail the
// other way, leaving a row for a message that is still there, which reads
// correctly in /aimod why and is undone by nothing at all.
func (p *Plugin) enforce(ctx context.Context, cfg Config, c candidate, bucket Bucket, action Action, v deepVerdict) {
	// A rewrite with nothing publishable left is a removal, and has to be
	// recorded, audited and explained as one.
	//
	// rewriteMessage degrades to a delete on an empty replacement, which is
	// correct: the deep pass is explicitly told to return an empty string
	// when the whole message was the violation, and a message that is
	// nothing but a slur has no cleaned-up version. What was wrong was that
	// the decision happened further down, after the action had already been
	// baked into the incident row, the audit title and the choice of DM. A
	// moderator then read "message rewritten" for a message that was
	// deleted, and went looking for a broken webhook. Deciding here keeps
	// all four in agreement.
	if action == ActionRewrite && strings.TrimSpace(v.Rewrite) == "" {
		action = ActionRemove
	}
	// A forward cannot be rewritten, for the same reason it is decided here
	// rather than further down: rewriteMessage deletes and reposts through a
	// webhook wearing the author's name, so a rewritten forward would publish
	// somebody else's words as this member's own plain text and erase the
	// fact that it was a forward at all. Removing says what happened.
	if action == ActionRewrite && c.Forwarded {
		action = ActionRemove
	}

	inc := Incident{
		GuildID:    cfg.GuildID,
		ChannelID:  c.ChannelID,
		MessageID:  c.MessageID,
		AuthorID:   c.AuthorID,
		Bucket:     bucket,
		Action:     action,
		Confidence: v.Confidence,
		Reason:     v.Reason,
		CreatedAt:  p.now(),
	}
	// Evidence is stored only if the guild asked for it to be. A guild on
	// evidence 0 gets an incident row with no text, which is spec.MD 4.5's
	// "log IDs, not content" and costs it the ability to undo.
	if cfg.EvidenceHours > 0 {
		inc.Content = c.Content
		inc.Replacement = v.Rewrite
	}
	if _, err := p.store.RecordIncident(ctx, inc); err != nil {
		p.log.Error("aimod: record incident, taking no action",
			"guild", cfg.GuildID, "channel", c.ChannelID, "message", c.MessageID, "err", err)
		return
	}

	auditAction := "aimod." + string(action)
	if action == ActionFlag || cfg.Mode == ModeFlag {
		// Mode flag overrides every per-bucket action: the guild has asked
		// to watch the filter work before letting it act.
		p.audit(ctx, cfg.GuildID, "aimod.flagged", c, bucket, v)
		return
	}

	var err error
	switch action {
	case ActionRemove:
		err = p.removeMessage(ctx, cfg.GuildID, c)
	case ActionRewrite:
		err = p.rewriteMessage(ctx, cfg.GuildID, c, v.Rewrite)
	default:
		return
	}

	switch {
	case err == nil:
		p.audit(ctx, cfg.GuildID, auditAction, c, bucket, v)
		p.notifyAuthor(ctx, cfg, c, action, v)
		// Only after the message was actually dealt with. Jailing somebody
		// over a removal that failed would punish them for something still
		// sitting in the channel, and the sanction row is what their next
		// offence counts, so writing one for a non-event would inflate every
		// future sentence they get.
		p.sanction(ctx, cfg, c, bucket, v.Reason)
	case discordguard.Skipped(err):
		// Paused or dry-run. The guild deliberately stopped the bot acting,
		// so the record of what would have happened is the whole output, and
		// it is not a failure.
		p.audit(ctx, cfg.GuildID, "aimod.dryrun", c, bucket, v)
	case core.IsUnknownResource(err):
		// Somebody deleted it first, or the channel is gone. Nothing to do
		// and nothing wrong; the incident row already records the verdict.
		p.log.Info("aimod: message already gone",
			"guild", cfg.GuildID, "channel", c.ChannelID, "message", c.MessageID)
	default:
		p.log.Error("aimod: enforcement failed",
			"guild", cfg.GuildID, "channel", c.ChannelID, "message", c.MessageID,
			"action", action, "err", err)
	}
}

func (p *Plugin) removeMessage(ctx context.Context, guildID string, c candidate) error {
	return p.ops(guildID).ChannelMessageDelete(c.ChannelID, c.MessageID)
}

// rewriteMessage deletes the original and reposts the cleaned text through a
// channel webhook wearing the author's name and avatar.
//
// A bot cannot edit another user's message, so "rewrite" is necessarily
// delete-and-repost, and the webhook is what keeps the result readable: the
// alternative, merlin quoting the member back at the channel, turns every
// rewrite into an interruption and a small public shaming.
//
// An empty replacement is treated as a removal. That is not a failure case:
// the deep pass is explicitly told to return an empty string when nothing
// publishable remains, and posting an empty message is not possible anyway.
func (p *Plugin) rewriteMessage(ctx context.Context, guildID string, c candidate, replacement string) error {
	replacement = strings.TrimSpace(replacement)
	if replacement == "" {
		return p.removeMessage(ctx, guildID, c)
	}

	// The webhook is resolved before the delete. Doing it the other way
	// round means a webhook failure leaves the message deleted with nothing
	// reposted, which is a silent downgrade from rewrite to remove at the
	// worst possible moment.
	hook, err := p.resolveWebhook(ctx, guildID, c.ChannelID)
	if err != nil {
		return fmt.Errorf("aimod: resolve webhook: %w", err)
	}

	author, err := p.ops(guildID).GuildMember(guildID, c.AuthorID)
	if err != nil {
		return fmt.Errorf("aimod: fetch author: %w", err)
	}

	if err := p.removeMessage(ctx, guildID, c); err != nil {
		return err
	}
	return p.ops(guildID).WebhookExecute(hook.ID, hook.Token, &discordgo.WebhookParams{
		Content:   replacement + rewriteMarker,
		Username:  displayName(author),
		AvatarURL: author.AvatarURL(""),
	})
}

func displayName(m *discordgo.Member) string {
	if m == nil {
		return "member"
	}
	if m.Nick != "" {
		return m.Nick
	}
	if m.User != nil {
		if m.User.GlobalName != "" {
			return m.User.GlobalName
		}
		return m.User.Username
	}
	return "member"
}

// resolveWebhook finds or creates this plugin's webhook for a channel,
// caching it per process. Same find-by-name-or-create shape as
// roles.resolveJailRole and rotation.resolveArchiveCategory.
func (p *Plugin) resolveWebhook(ctx context.Context, guildID, channelID string) (*discordgo.Webhook, error) {
	p.webhookMu.Lock()
	defer p.webhookMu.Unlock()
	if hook, ok := p.webhooks[channelID]; ok {
		return hook, nil
	}

	existing, err := p.ops(guildID).ChannelWebhooks(channelID)
	if err != nil {
		return nil, err
	}
	for _, hook := range existing {
		// Token, not just name: Discord omits the token from webhooks this
		// application did not create, and a tokenless webhook cannot be
		// executed, so one would be cached and then fail on every rewrite.
		if hook.Name == webhookName && hook.Token != "" {
			p.webhooks[channelID] = hook
			return hook, nil
		}
	}

	created, err := p.ops(guildID).WebhookCreate(channelID, webhookName, "")
	if err != nil {
		return nil, err
	}
	p.webhooks[channelID] = created
	return created, nil
}

// forgetWebhook drops a cached webhook, so the next rewrite re-resolves or
// recreates it rather than failing forever against a deleted one. The same
// hole roles.HandleRoleDeleted closes for the jail marker role.
func (p *Plugin) forgetWebhook(channelID string) {
	p.webhookMu.Lock()
	defer p.webhookMu.Unlock()
	delete(p.webhooks, channelID)
}

// notifyAuthor DMs the member what happened and why.
//
// Never optional. A message that vanishes with no explanation is how a
// moderation bot earns a reputation for being broken and arbitrary, and this
// one acts without a human in the loop, so the explanation is the only thing
// standing between an automated deletion and a member who has no idea what
// they did. The original text goes with it, so nothing they wrote is lost to
// them even when it is removed from the channel.
//
// Failure is logged and ignored, like every other audit-adjacent step: a
// member with closed DMs must not turn a successful removal into a failed
// one.
func (p *Plugin) notifyAuthor(ctx context.Context, cfg Config, c candidate, action Action, v deepVerdict) {
	guildID := cfg.GuildID
	key := voice.KeyAIModRemoved
	if action == ActionRewrite {
		key = voice.KeyAIModRewritten
	}

	guildName := guildID
	if g, err := p.ops(guildID).Guild(guildID); err == nil && g.Name != "" {
		guildName = g.Name
	}

	line := p.voice.Line(ctx, guildID, key, map[string]string{"guild": guildName})
	if line == "" {
		return
	}

	fields := []*discordgo.MessageEmbedField{
		{Name: "Why", Value: core.TruncateEmbedField(reasonOrPolicy(v))},
	}
	// The member's own words back to them, so a false positive costs them
	// nothing they wrote. On the default 24h evidence window, which is what
	// almost every guild runs, this is what they get.
	//
	// Gated on that window rather than sent unconditionally, and the losing
	// argument is worth recording. A DM is the member's own text going back
	// to that member at the moment it stops existing anywhere they can
	// reach, which is a good thing to do and reads nothing like retention.
	// But it lands in a DM channel this bot cannot clear afterwards, and an
	// admin who set evidence to 0 asked for no durable copies. Their setting
	// decides, not this function's view of what would be kinder: the whole
	// point of making retention configurable is that the guild chooses. A
	// guild that wants the member to keep their words leaves the window at
	// its default and they do.
	if cfg.EvidenceHours > 0 && c.Content != "" {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:  "What you posted",
			Value: core.TruncateEmbedField(c.Content),
		})
	}

	embed := core.NewEmbed(core.ColorWarning, "A message of yours was moderated", line, fields...)
	dm, err := p.ops(guildID).UserChannelCreate(c.AuthorID)
	if err != nil {
		p.log.Info("aimod: could not open DM with author", "guild", guildID, "user", c.AuthorID)
		return
	}
	if _, err := p.ops(guildID).ChannelMessageSendComplex(dm.ID, &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{embed},
		Files:  core.EmbedFiles(embed),
	}); err != nil && !discordguard.Skipped(err) {
		p.log.Info("aimod: could not DM author", "guild", guildID, "user", c.AuthorID)
	}
}

func reasonOrPolicy(v deepVerdict) string {
	if strings.TrimSpace(v.Reason) != "" {
		return v.Reason
	}
	return "It matched Discord's " + strings.ReplaceAll(string(v.Bucket), "_", " ") + " policy."
}

// audit records one verdict. Log-and-continue on failure, the policy every
// call site in this codebase uses: the message has already been acted on and
// the incident row is already durable, so a failed audit post must never be
// reported as a failed enforcement.
//
// The message text is deliberately not in the embed. A mod who needs it runs
// /aimod why, which reads the evidence row and is subject to the guild's own
// retention window; copying it into the audit channel would put it somewhere
// that window does not reach.
func (p *Plugin) audit(ctx context.Context, guildID, action string, c candidate, bucket Bucket, v deepVerdict) {
	detail := fmt.Sprintf("%s in %s by %s (confidence %.0f%%): %s",
		bucket, core.MentionChannel(c.ChannelID), core.MentionUser(c.AuthorID),
		v.Confidence*100, reasonOrPolicy(v))
	if err := p.auditWriter.Record(ctx, guildID, core.ActorSystem, action, c.MessageID, detail); err != nil {
		p.log.Error("aimod: audit record failed", "guild", guildID, "action", action, "err", err)
	}
}

// ErrNothingToUndo reports that an incident has no stored text to restore,
// which is what /aimod undo gets once the guild's evidence window has passed
// or when the guild keeps no evidence at all.
var ErrNothingToUndo = errors.New("aimod: the original text is no longer stored, so it cannot be restored")

// undo reposts a removed message through the webhook and marks the incident
// reversed. It cannot un-delete the original (Discord has no such call), so
// what a member gets back is their words, in their name, in the same
// channel, which is the most that is available.
func (p *Plugin) undo(ctx context.Context, guildID string, inc Incident) error {
	if inc.Content == "" {
		return ErrNothingToUndo
	}
	hook, err := p.resolveWebhook(ctx, guildID, inc.ChannelID)
	if err != nil {
		return fmt.Errorf("aimod: resolve webhook: %w", err)
	}
	author, err := p.ops(guildID).GuildMember(guildID, inc.AuthorID)
	if err != nil {
		return fmt.Errorf("aimod: fetch author: %w", err)
	}
	if err := p.ops(guildID).WebhookExecute(hook.ID, hook.Token, &discordgo.WebhookParams{
		Content:   inc.Content,
		Username:  displayName(author),
		AvatarURL: author.AvatarURL(""),
	}); err != nil {
		return err
	}
	return p.store.MarkUndone(ctx, inc.ID)
}
