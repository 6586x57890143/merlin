package rapsheet

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// Alternate accounts: linking them, and noticing them on the way in.
//
// A member who is jailed, leaves and comes back on a fresh account arrives
// with a clean sheet, which is the one thing a ledger must not hand out for
// free. Discord gives almost nothing to go on (no device, no IP, nothing
// across accounts), so what is here is four cheap signals read off the
// join event and the identity snapshot already kept per member on file:
// the same uploaded avatar, the same name once the digits are stripped, an
// account created within minutes of one already on file, and a join within
// minutes of somebody else's jail, ban or kick. Each is weak alone and they
// are summed, and the sum is a HINT: it is stored, shown on both sheets and
// posted for a moderator with a Link button, and it never links anybody by
// itself. A wrongly merged stranger inherits somebody else's record, which
// is worse than an alt getting a few days' head start.
//
// A link is a moderator's decision, made with /rapsheet link or the button.
// Linked accounts share one score and one sheet, so an alt's first offence
// lands on top of everything the other account did.

const (
	actionLink = "rapsheet.link"

	altPrefix        = "rapsheet:alt:"
	altLinkPrefix    = altPrefix + "link:"
	altDismissPrefix = altPrefix + "dismiss:"

	// maxAltCandidates bounds how many members on file a joiner is compared
	// with: the newest, since an evader comes back soon after leaving.
	maxAltCandidates = 500
	// altHintThreshold is the score at which a hint is kept and shown on
	// the sheet; altNoticeThreshold is where it is also posted for a mod.
	altHintThreshold   = 2
	altNoticeThreshold = 3
	// createdNear is how close two account creation times have to be to
	// count as one sitting; joinedAfterAction how soon after somebody else's
	// consequence a join has to land to look like a return.
	createdNear       = 10 * time.Minute
	joinedAfterAction = 15 * time.Minute
	// minNamePrefix is the shortest shared prefix that counts. "dan" and
	// "dana" are a coincidence; "dana_k" and "dana_k2" are not.
	minNamePrefix = 5
)

// altSignals scores one joiner against one member on file. recentAction is
// the candidate's latest jail, ban or kick, if any, and joinedAt the
// joiner's join time. Pure, so the whole table can be tested.
func altSignals(joiner *discordgo.User, joinedAt time.Time, candidate CaseFile, recentAction *Entry) (signals []string, score int) {
	if joiner == nil || joiner.ID == candidate.UserID {
		return nil, 0
	}
	if joiner.Avatar != "" && joiner.Avatar == candidate.AvatarHash {
		// An avatar hash is a hash of the uploaded bytes. Two accounts with
		// the same one uploaded the same picture, which is the strongest
		// thing Discord lets a bot see.
		signals, score = append(signals, "same avatar"), score+3
	}
	if n := nameMatch(joiner, candidate); n > 0 {
		signals, score = append(signals, "same name"), score+n
	}
	if created, err := discordgo.SnowflakeTimestamp(joiner.ID); err == nil {
		if other, err := discordgo.SnowflakeTimestamp(candidate.UserID); err == nil {
			if d := created.Sub(other); d > -createdNear && d < createdNear {
				signals, score = append(signals, "accounts made minutes apart"), score+2
			}
		}
	}
	if recentAction != nil && !joinedAt.IsZero() {
		if d := joinedAt.Sub(recentAction.CreatedAt); d >= 0 && d < joinedAfterAction {
			signals, score = append(signals, fmt.Sprintf("joined %s after being %s", humanGap(d), kindWords(*recentAction))), score+2
		}
	}
	return signals, score
}

// humanGap is a short gap in words: "40 seconds", "12 minutes".
func humanGap(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	}
	return fmt.Sprintf("%d minutes", int(d.Round(time.Minute).Minutes()))
}

// nameMatch compares every name the joiner has with every name on file,
// letters only and lowercased, so "Dana_K" and "danak2" meet. 2 for an
// exact match, 1 for one being a prefix of the other, 0 otherwise.
func nameMatch(joiner *discordgo.User, candidate CaseFile) int {
	mine := []string{normName(joiner.Username), normName(joiner.GlobalName)}
	theirs := []string{normName(candidate.Username), normName(candidate.GlobalName)}
	best := 0
	for _, a := range mine {
		for _, b := range theirs {
			switch {
			case a == "" || b == "":
			case a == b:
				return 2
			case len(a) >= minNamePrefix && len(b) >= minNamePrefix && (strings.HasPrefix(a, b) || strings.HasPrefix(b, a)):
				best = 1
			}
		}
	}
	return best
}

func normName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// HandleMemberJoin is called from cmd/bot/main.go on GUILD_MEMBER_ADD.
// Everything runs off the caller's goroutine; nothing here can fail the
// join and nothing here changes anybody's roles.
func (p *Plugin) HandleMemberJoin(_ context.Context, guildID string, m *discordgo.Member) {
	if m == nil || m.User == nil || m.User.Bot || !p.enabled(guildID) {
		return
	}
	joiner, joinedAt := m.User, m.JoinedAt
	p.detached(func(ctx context.Context) {
		cfg := p.config(ctx, guildID)
		if !cfg.AltHints {
			return
		}
		candidates, err := p.store.CaseFiles(ctx, guildID, maxAltCandidates)
		if err != nil {
			p.log.Error("rapsheet: list case files for alt hints", "guild", guildID, "err", err)
			return
		}
		if len(candidates) == 0 {
			return
		}
		recent, err := p.store.RecentActioned(ctx, guildID, joinedAt.Add(-joinedAfterAction))
		if err != nil {
			p.log.Error("rapsheet: list recent actions for alt hints", "guild", guildID, "err", err)
			recent = nil
		}
		latest := map[string]*Entry{}
		for i := range recent {
			e := &recent[i]
			if cur := latest[e.UserID]; cur == nil || e.CreatedAt.After(cur.CreatedAt) {
				latest[e.UserID] = e
			}
		}

		var hints []AltHint
		for _, c := range candidates {
			signals, score := altSignals(joiner, joinedAt, c, latest[c.UserID])
			if score >= altHintThreshold {
				hints = append(hints, AltHint{GuildID: guildID, UserID: joiner.ID, CandidateID: c.UserID, Signals: signals, Score: score})
			}
		}
		if len(hints) == 0 {
			return
		}
		sort.Slice(hints, func(i, j int) bool { return hints[i].Score > hints[j].Score })
		for _, h := range hints {
			if err := p.store.UpsertHint(ctx, h); err != nil {
				p.log.Error("rapsheet: store alt hint", "guild", guildID, "err", err)
			}
		}
		best := hints[0]
		if best.Score < altNoticeThreshold || cfg.ModChannelID == "" {
			return
		}
		for _, c := range candidates {
			if c.UserID == best.CandidateID {
				if op := p.altOpinion(ctx, cfg, joiner, joinedAt, best, c); op != "" {
					best.Opinion = op
					if err := p.store.UpsertHint(ctx, best); err != nil {
						p.log.Error("rapsheet: store alt opinion", "guild", guildID, "err", err)
					}
				}
			}
		}
		p.postAltNotice(ctx, cfg, joiner, best)
	})
}

func (p *Plugin) postAltNotice(ctx context.Context, cfg Config, joiner *discordgo.User, h AltHint) {
	embed, components := altNoticeEmbed(joiner, h, p.altContext(ctx, cfg, h), "", false)
	if _, err := p.ops(cfg.GuildID).ChannelMessageSendComplex(cfg.ModChannelID, &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{embed}, Components: components, Files: core.EmbedFiles(embed),
	}); err != nil {
		p.log.Error("rapsheet: post alt notice", "guild", cfg.GuildID, "err", err)
	}
}

// altContext is what a mod wants next to a hint before clicking: how bad
// the record they would be linking to is, and how new the joiner's account
// is. Both are cheap (one sheet read, one snowflake), and both are left out
// rather than guessed when they cannot be had.
func (p *Plugin) altContext(ctx context.Context, cfg Config, h AltHint) string {
	var lines []string
	if sh, err := p.loadSheet(ctx, cfg, cfg.GuildID, h.CandidateID); err == nil {
		line := fmt.Sprintf("**%s:** score %.0f", core.MentionUser(h.CandidateID), sh.Score)
		if sh.Rec.Action != ActionNone {
			line += ", " + recWords(sh.Rec) + " band"
		}
		if st, ok := standing(sh.Entries, p.now()); ok {
			line += ", " + standingWords(st)
		}
		lines = append(lines, line)
	}
	if made, err := discordgo.SnowflakeTimestamp(h.UserID); err == nil {
		lines = append(lines, fmt.Sprintf("**%s:** account made %s", core.MentionUser(h.UserID), relativeTimestamp(made)))
	}
	return strings.Join(lines, "\n")
}

// altNoticeEmbed is the mod-channel post. context is altContext's block;
// note is appended when there is something to say; settled drops the
// buttons and the invitation to click them.
func altNoticeEmbed(joiner *discordgo.User, h AltHint, context, note string, settled bool) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	desc := fmt.Sprintf("%s just joined and looks like they might be %s, who is on record here.\n\n**Why:** %s.",
		core.MentionUser(joiner.ID), core.MentionUser(h.CandidateID), strings.Join(h.Signals, ", "))
	if context != "" {
		desc += "\n\n" + context
	}
	if h.Opinion != "" {
		desc += "\n\n**Second opinion:** " + h.Opinion
	}
	if !settled {
		desc += "\n\nLinking them puts both accounts on one sheet with one score. If this is a coincidence, dismiss it; nothing happens either way until somebody clicks."
	}
	if note != "" {
		desc += "\n\n" + note
	}
	color := core.ColorWarning
	if settled {
		color = core.ColorInfo
	}
	embed := core.NewEmbed(color, "Possible alt", core.TruncateEmbedDescription(desc))
	if settled {
		return embed, nil
	}
	id := h.UserID + ":" + h.CandidateID
	return embed, []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Link them", Style: discordgo.PrimaryButton, CustomID: altLinkPrefix + id},
		discordgo.Button{Label: "Dismiss", Style: discordgo.SecondaryButton, CustomID: altDismissPrefix + id},
	}}}
}

// handleAltButton is both buttons on the notice.
func (p *Plugin) handleAltButton(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	link := strings.HasPrefix(customID, altLinkPrefix)
	rest := strings.TrimPrefix(strings.TrimPrefix(customID, altLinkPrefix), altDismissPrefix)
	userID, candidateID, ok := strings.Cut(rest, ":")
	if !ok || userID == "" || candidateID == "" {
		p.log.Error("rapsheet: malformed alt custom id", "custom_id", customID)
		return
	}
	if err := core.DeferUpdate(s, i); err != nil {
		p.log.Error("rapsheet: defer alt click", "err", err)
		return
	}
	h := AltHint{GuildID: i.GuildID, UserID: userID, CandidateID: candidateID}
	for _, stored := range p.hintsFor(ctx, i.GuildID, userID) {
		if stored.CandidateID == candidateID {
			h = stored
		}
	}
	joiner := &discordgo.User{ID: userID}
	if link {
		if err := p.link(ctx, i.GuildID, userID, candidateID, actorID(i), "alt notice: "+strings.Join(h.Signals, ", ")); err != nil {
			embed, comps := altNoticeEmbed(joiner, h, "", fmt.Sprintf("%s tried to link them and it failed: %v", core.MentionUser(actorID(i)), err), false)
			_ = core.UpdateEmbedWithComponents(s, i, embed, comps)
			return
		}
		embed, comps := altNoticeEmbed(joiner, h, "", fmt.Sprintf("Linked by %s. They now share one sheet.", core.MentionUser(actorID(i))), true)
		_ = core.UpdateEmbedWithComponents(s, i, embed, comps)
		return
	}
	if err := p.store.DeleteHint(ctx, i.GuildID, userID, candidateID); err != nil {
		p.log.Error("rapsheet: dismiss alt hint", "guild", i.GuildID, "err", err)
	}
	embed, comps := altNoticeEmbed(joiner, h, "", fmt.Sprintf("Dismissed by %s.", core.MentionUser(actorID(i))), true)
	_ = core.UpdateEmbedWithComponents(s, i, embed, comps)
}

func (p *Plugin) hintsFor(ctx context.Context, guildID, userID string) []AltHint {
	hints, err := p.store.Hints(ctx, guildID, userID)
	if err != nil {
		p.log.Error("rapsheet: read hints", "guild", guildID, "user", userID, "err", err)
	}
	return hints
}

// link puts userID in otherID's group (or a new group of the two), audits
// it, and drops the hint that led here.
func (p *Plugin) link(ctx context.Context, guildID, userID, otherID, actor, reason string) error {
	if userID == otherID {
		return fmt.Errorf("that is the same account")
	}
	otherGroup, err := p.store.Group(ctx, guildID, otherID)
	if err != nil {
		return err
	}
	// The group id is the other account's group if it has one, else the
	// other account itself: whichever is joined keeps its label, so linking
	// a third account to either of two already linked lands in the same
	// group.
	groupID := otherID
	if links, err := p.store.GroupLinks(ctx, guildID, otherID); err == nil && len(links) > 0 {
		groupID = links[0].GroupID
	} else if err := p.store.Link(ctx, Link{GuildID: guildID, UserID: otherID, GroupID: groupID, LinkedBy: actor, Reason: reason}); err != nil {
		return err
	}
	if err := p.store.Link(ctx, Link{GuildID: guildID, UserID: userID, GroupID: groupID, LinkedBy: actor, Reason: reason}); err != nil {
		return err
	}
	_ = p.store.DeleteHint(ctx, guildID, userID, otherID)
	if err := p.audit.Record(ctx, guildID, actor, "rapsheet.linked", "",
		fmt.Sprintf("user=%s linked_to=%s group=%d reason=%q", core.MentionUser(userID), core.MentionUser(otherID), len(otherGroup)+1, reason)); err != nil {
		p.log.Error("rapsheet: audit link", "guild", guildID, "err", err)
	}
	return nil
}

func (p *Plugin) handleLink(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	args := core.LeafArgs(i)
	userID := args["user"].Value.(string)
	otherID := args["other"].Value.(string)
	reason := ""
	if a, ok := args["reason"]; ok {
		reason = strings.TrimSpace(a.StringValue())
	}
	if err := p.link(ctx, i.GuildID, userID, otherID, actorID(i), reason); err != nil {
		core.RespondErr(s, i, "Link", err)
		return
	}
	group, _ := p.store.Group(ctx, i.GuildID, userID)
	mentions := make([]string, 0, len(group))
	for _, id := range group {
		mentions = append(mentions, core.MentionUser(id))
	}
	core.RespondOK(s, i, "Linked", fmt.Sprintf("%s now share one sheet and one score. `/rapsheet unlink` takes an account back out.", strings.Join(mentions, ", ")))
}

func (p *Plugin) handleUnlink(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	userID := core.LeafArgs(i)["user"].Value.(string)
	group, err := p.store.Group(ctx, i.GuildID, userID)
	if err != nil {
		core.RespondErr(s, i, "Unlink", err)
		return
	}
	if len(group) < 2 {
		core.RespondErr(s, i, "Unlink", fmt.Errorf("%s is not linked to anyone", core.MentionUser(userID)))
		return
	}
	if err := p.store.Unlink(ctx, i.GuildID, userID); err != nil {
		core.RespondErr(s, i, "Unlink", err)
		return
	}
	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "rapsheet.unlinked", "", "user="+core.MentionUser(userID)); err != nil {
		p.log.Error("rapsheet: audit unlink", "guild", i.GuildID, "err", err)
	}
	core.RespondOK(s, i, "Unlinked", fmt.Sprintf("%s is on their own sheet again, with the entries recorded against them; the rest of the group keeps theirs.", core.MentionUser(userID)))
}

func (p *Plugin) handleConfigureAltHints(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	on := core.LeafArgs(i)["enabled"].BoolValue()
	cfg := p.config(ctx, i.GuildID)
	old := cfg.AltHints
	cfg.AltHints = on
	p.setConfig(ctx, s, i, cfg, "alt_hints", fmt.Sprint(old), fmt.Sprint(on))
	if on {
		core.RespondOK(s, i, "Alt hints", "When somebody joins who resembles a member on record, merlin notes it on both sheets and, if it looks strong, posts it to the mod channel. Nothing is linked without a moderator.")
		return
	}
	core.RespondOK(s, i, "Alt hints", "Joins are no longer compared with members on record. Existing hints and links are kept.")
}
