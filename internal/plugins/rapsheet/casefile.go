package rapsheet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// Case files: one forum post per member on file, mirroring their entries.
//
// The ledger is the record. The forum is a view of it that moderators can
// browse, search with Discord's own tools, and talk in: the thread under a
// member's name is where "what do we do about this one" happens, next to
// the facts. It is created lazily, on the member's first entry, so a server
// of five thousand people does not get five thousand empty threads, and
// every later entry is one message in it. A void or an edit edits the
// mirrored message in place rather than posting again, because a thread
// that reads "warned / warned (voided)" twice is a thread nobody trusts.
//
// Everything here is best effort and never fails the entry it mirrors. It
// runs on a detached goroutine because the forum post costs a REST call the
// command and the bus handler should not wait on, and because the mirror
// shares the guild's message.send budget with everything else merlin says.
// /rapsheet status counts what is not mirrored yet.

const (
	// caseFileForumName is what /rapsheet configure forum creates when it
	// is not handed an existing forum.
	caseFileForumName = "rapsheets"

	// caseFileBotAllow is what merlin needs in the forum: to see it, open
	// posts, write in them, edit its own messages, and attach the mood icon.
	caseFileBotAllow = discordgo.PermissionViewChannel | discordgo.PermissionSendMessages |
		discordgo.PermissionSendMessagesInThreads | discordgo.PermissionCreatePublicThreads |
		discordgo.PermissionReadMessageHistory | discordgo.PermissionEmbedLinks | discordgo.PermissionAttachFiles

	// caseFileModAllow is what a mod role gets: read, and talk in the
	// threads. Not the right to open posts, since a post that is not a case
	// file confuses the mirror, and not manage, since deleting a case file
	// deletes nothing from the ledger and only breaks the link.
	caseFileModAllow = discordgo.PermissionViewChannel | discordgo.PermissionReadMessageHistory |
		discordgo.PermissionSendMessagesInThreads

	// threadAutoArchive is Discord's longest: a week. The mirror sends into
	// an archived thread anyway (Discord unarchives on send), so this only
	// decides how long a quiet case file stays in the active list.
	threadAutoArchive = 10080

	// channelCapHeadroom keeps the forum clear of Discord's 500-channel guild
	// cap, the same self-throttle rotation and contest apply.
	channelCapHeadroom = 20

	// maxThreadName is Discord's limit on a thread title.
	maxThreadName = 100

	// mirrorTimeout bounds one mirror attempt, thread creation included.
	mirrorTimeout = 20 * time.Second
)

// botUserID resolves merlin's own id once. Init runs before the session
// opens, so it cannot be read off the session's state at that point.
func (p *Plugin) botUserID(guildID string) (string, error) {
	p.mu.Lock()
	id := p.botID
	p.mu.Unlock()
	if id != "" {
		return id, nil
	}
	u, err := p.ops(guildID).User("@me")
	if err != nil {
		return "", fmt.Errorf("rapsheet: resolve bot user: %w", err)
	}
	p.mu.Lock()
	p.botID = u.ID
	p.mu.Unlock()
	return u.ID, nil
}

// createForum makes the case-file forum: hidden from @everyone, readable
// by the guild's mod roles, writable by merlin.
func (p *Plugin) createForum(guildID string) (*discordgo.Channel, error) {
	channels, err := p.ops(guildID).GuildChannels(guildID)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	if len(channels) >= 500-channelCapHeadroom {
		return nil, fmt.Errorf("this server has %d channels, too close to Discord's 500 cap to add one", len(channels))
	}
	botID, err := p.botUserID(guildID)
	if err != nil {
		return nil, err
	}
	overwrites := core.DenyEveryoneExceptBot(nil, guildID, botID, caseFileBotAllow)
	for _, roleID := range p.modRoles.ModRoleIDs(guildID) {
		overwrites = append(overwrites, &discordgo.PermissionOverwrite{
			ID: roleID, Type: discordgo.PermissionOverwriteTypeRole, Allow: caseFileModAllow,
		})
	}
	ch, err := p.ops(guildID).GuildChannelCreateComplex(guildID, discordgo.GuildChannelCreateData{
		Name:                 caseFileForumName,
		Type:                 discordgo.ChannelTypeGuildForum,
		Topic:                "One post per member with a moderation record, kept by merlin. Talk in the threads; the entries themselves are edited with /rapsheet.",
		PermissionOverwrites: overwrites,
	})
	if err != nil {
		return nil, fmt.Errorf("create forum: %w", err)
	}
	return ch, nil
}

// mirror schedules e's message in the member's case file. Never blocks the
// caller, never returns an error.
func (p *Plugin) mirror(cfg Config, e Entry) {
	if cfg.ForumChannelID == "" {
		return
	}
	p.detached(func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, mirrorTimeout)
		defer cancel()
		if err := p.mirrorNow(ctx, cfg, e); err != nil {
			p.log.Error("rapsheet: mirror entry into case file", "guild", e.GuildID, "case", e.ID, "err", err)
		}
	})
}

// mirrorNow posts e into the case file, opening the post if the member has
// none. A thread Discord says is gone is dropped and opened again once;
// a forum Discord says is gone is dropped from the config, since the next
// entry would only fail the same way.
func (p *Plugin) mirrorNow(ctx context.Context, cfg Config, e Entry) error {
	cf, ok, err := p.store.CaseFile(ctx, e.GuildID, e.UserID)
	if err != nil {
		return err
	}
	if !ok {
		// record() opens the case file before it writes the entry, so this
		// is the upsert having failed. Open it now rather than give up.
		cf = CaseFile{GuildID: e.GuildID, UserID: e.UserID}
		if err := p.store.UpsertCaseFile(ctx, cf); err != nil {
			return err
		}
	}
	embed := entryEmbed(e)
	send := &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed}, Files: core.EmbedFiles(embed)}

	if cf.ThreadID != "" {
		msg, err := p.ops(e.GuildID).ChannelMessageSendComplex(cf.ThreadID, send)
		if err == nil {
			return p.store.SetThreadMessage(ctx, e.ID, msg.ID)
		}
		if !core.IsUnknownResource(err) {
			return err
		}
		// The post was deleted. Forget it and open a fresh one below.
		p.log.Info("rapsheet: case-file thread is gone, opening a new one", "guild", e.GuildID, "user", e.UserID)
		if err := p.store.SetCaseThread(ctx, e.GuildID, e.UserID, ""); err != nil {
			return err
		}
	}

	th, err := p.ops(e.GuildID).ForumThreadStartComplex(cfg.ForumChannelID, &discordgo.ThreadStart{
		Name:                threadName(cf, e.UserID),
		AutoArchiveDuration: threadAutoArchive,
	}, send)
	if err != nil {
		if core.IsUnknownResource(err) {
			p.log.Warn("rapsheet: case-file forum is gone, unsetting it; run /rapsheet configure forum to choose another",
				"guild", e.GuildID, "forum", cfg.ForumChannelID)
			cfg.ForumChannelID = ""
			if cerr := p.store.SetConfig(ctx, cfg); cerr != nil {
				return errors.Join(err, cerr)
			}
		}
		return err
	}
	if err := p.store.SetCaseThread(ctx, e.GuildID, e.UserID, th.ID); err != nil {
		return err
	}
	// A forum post's starter message shares the thread's id.
	return p.store.SetThreadMessage(ctx, e.ID, th.ID)
}

// remirror edits e's mirrored message after a void or a reason change. An
// entry that was never mirrored is mirrored now instead, which is also how
// a case file catches up after a forum outage.
func (p *Plugin) remirror(cfg Config, e Entry) {
	if cfg.ForumChannelID == "" {
		return
	}
	if e.ThreadMessageID == "" {
		p.mirror(cfg, e)
		return
	}
	p.detached(func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, mirrorTimeout)
		defer cancel()
		cf, ok, err := p.store.CaseFile(ctx, e.GuildID, e.UserID)
		if err != nil || !ok || cf.ThreadID == "" {
			p.log.Error("rapsheet: re-mirror: no case file for a mirrored entry", "guild", e.GuildID, "case", e.ID, "err", err)
			return
		}
		embed := entryEmbed(e)
		files := core.EmbedFiles(embed)
		attachments := make([]*discordgo.MessageAttachment, 0, len(files))
		for _, f := range files {
			attachments = append(attachments, &discordgo.MessageAttachment{Filename: f.Name})
		}
		_, err = p.ops(e.GuildID).ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel: cf.ThreadID,
			ID:      e.ThreadMessageID,
			Embeds:  &[]*discordgo.MessageEmbed{embed},
			Files:   files,
			// Replaced, not appended: the mood icon changes with the colour
			// and an edit that keeps the old attachment shows two.
			Attachments: &attachments,
		})
		if err != nil {
			if core.IsUnknownResource(err) {
				// The message or the thread is gone. Post it again so the
				// case file still shows the void.
				_ = p.store.SetThreadMessage(ctx, e.ID, "")
				if cerr := p.mirrorNow(ctx, cfg, e); cerr != nil {
					p.log.Error("rapsheet: re-mirror after a missing message", "guild", e.GuildID, "case", e.ID, "err", cerr)
				}
				return
			}
			p.log.Error("rapsheet: edit mirrored entry", "guild", e.GuildID, "case", e.ID, "err", err)
		}
	})
}

// threadName titles a case file: the member's name for reading, their id
// for searching, since names change and ids do not.
func threadName(cf CaseFile, userID string) string {
	name := cf.Username
	if cf.GlobalName != "" {
		name = cf.GlobalName
	}
	if name == "" {
		name = "member"
	}
	title := fmt.Sprintf("%s (%s)", name, userID)
	if len(title) > maxThreadName {
		title = title[:maxThreadName]
	}
	return title
}

// entryEmbed is one entry as a case-file message. The same words the sheet
// uses, laid out as fields so a thread reads as a log.
func entryEmbed(e Entry) *discordgo.MessageEmbed {
	title := fmt.Sprintf("#%d · %s", e.ID, kindWords(e))
	color := core.ColorInfo
	switch e.Kind {
	case KindWarn, KindTimeout, KindJail, KindSuggestion:
		color = core.ColorWarning
	case KindKick, KindBan, KindRemoval:
		color = core.ColorError
	case KindUnban, KindRelease:
		color = core.ColorSuccess
	}
	if e.Voided() {
		title = "~~" + title + "~~ (voided)"
		color = core.ColorWarning
	}
	fields := []*discordgo.MessageEmbedField{
		{Name: "Member", Value: core.MentionUser(e.UserID), Inline: true},
		{Name: "By", Value: actorWords(e.ActorID, false), Inline: true},
	}
	if e.Category != "" && (e.Category != CategoryOther || e.Points > 0) {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Category", Value: categoryLabel(e.Category), Inline: true})
	}
	if e.Points > 0 {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Points", Value: fmt.Sprintf("%d", e.Points), Inline: true})
	}
	if e.EndsAt != nil {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Until", Value: absoluteTimestamp(*e.EndsAt), Inline: true})
	}
	if e.Source != SourceCommand {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Source", Value: string(e.Source), Inline: true})
	}
	if e.Voided() {
		v := "by " + actorWords(e.VoidedBy, false)
		if e.VoidReason != "" {
			v += ": " + e.VoidReason
		}
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Voided", Value: core.TruncateEmbedField(v)})
	}
	desc := strings.TrimSpace(e.Reason)
	if desc == "" {
		desc = "(no reason given)"
	}
	embed := core.NewEmbed(color, title, core.TruncateEmbedDescription(desc), fields...)
	if e.Voided() {
		embed = core.WithMood(embed, core.MoodIdle)
	}
	return embed
}
