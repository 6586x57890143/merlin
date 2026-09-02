package contest

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/voice"
)

// linkButtonPrefix is the CustomID every "give me my link" button carries.
// The slug rides on the end, so a button on an announcement from three
// contests ago still resolves to the right one rather than to whatever is
// running now.
const linkButtonPrefix = "contest:link:"

// discordTS renders an instant as Discord's own relative timestamp.
//
// This is the one place merlin deliberately does not phrase a duration
// herself. Discord renders <t:...:R> in each reader's locale and timezone
// and keeps it counting down after the message is posted, which a string
// merlin computed at send time cannot do: "in 3 hours" is wrong an hour
// later, on an announcement people scroll back to.
func discordTS(unix int64) string { return "<t:" + strconv.FormatInt(unix, 10) + ":R>" }

func marshalResults(rs []resultView) ([]byte, error) {
	b, err := json.Marshal(rs)
	if err != nil {
		return nil, fmt.Errorf("contest: encode results: %w", err)
	}
	return b, nil
}

func unmarshalResults(b []byte) ([]resultView, error) {
	var rs []resultView
	if err := json.Unmarshal(b, &rs); err != nil {
		return nil, fmt.Errorf("contest: decode results: %w", err)
	}
	return rs, nil
}

// post sends one announcement into the contest's announce channel.
//
// Every phase gets its own message rather than editing one in place. That is
// partly because editing needs a guard method that does not exist, and
// mostly because it is right: each transition is news, and news that quietly
// rewrites a message from two days ago is news nobody sees.
func (p *Plugin) post(ctx context.Context, c Contest, embed *discordgo.MessageEmbed, components []discordgo.MessageComponent) string {
	if c.AnnounceChannelID == "" {
		return ""
	}
	msg, err := p.opsFor(c.GuildID).ChannelMessageSendComplex(c.AnnounceChannelID, &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{embed},
		// EmbedFiles rather than a hand-listed attachment: an
		// attachment:// URL with no matching upload renders as a broken
		// frame, and reading the URLs back off the finished embed is what
		// stops the two drifting apart.
		Files:      core.EmbedFiles(embed),
		Components: components,
	})
	if err != nil {
		// An announcement failing must never fail the phase transition that
		// triggered it. The contest has already moved; the same
		// log-and-continue policy every audit call site uses.
		p.log.Error("contest: announce", "guild", c.GuildID, "contest", c.ID, "err", err)
		return ""
	}
	return msg.ID
}

// linkRow is the button that hands out a per-member link. A plain URL button
// cannot work here: the token is minted for one member and expires, so the
// click has to come back through Discord.
func linkRow(slug, label string) []discordgo.MessageComponent {
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: label, Style: discordgo.PrimaryButton, CustomID: linkButtonPrefix + slug},
	}}}
}

// AnnounceCreated posts the opening card. Called from /contest new rather
// than from a phase transition, since creating the contest is the event.
func (p *Plugin) announceCreated(ctx context.Context, c Contest) {
	body := p.speak(ctx, c.GuildID, voice.KeyContestAnnounce, map[string]string{
		"title": c.Title,
		"opens": discordTS(c.SubmitAt.Unix()),
	}, "contest time: "+c.Title+". submissions open "+discordTS(c.SubmitAt.Unix())+".")

	fields := []*discordgo.MessageEmbedField{}
	if c.Theme != "" {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name: "theme", Value: core.TruncateEmbedField(c.Theme),
		})
	}
	fields = append(fields,
		&discordgo.MessageEmbedField{Name: "submissions open", Value: discordTS(c.SubmitAt.Unix()), Inline: true},
		&discordgo.MessageEmbedField{Name: "voting opens", Value: discordTS(c.VoteAt.Unix()), Inline: true},
		&discordgo.MessageEmbedField{Name: "winners", Value: discordTS(c.ResultsAt.Unix()), Inline: true},
		&discordgo.MessageEmbedField{Name: "prizes", Value: "pledge one with `/contest prize`"},
	)

	embed := core.NewLandmarkEmbed(core.ColorPrimary, c.Title, body, fields...)
	if id := p.post(ctx, c, embed, nil); id != "" {
		if err := p.store.SetAnnounceMessage(ctx, c.ID, c.AnnounceChannelID, id); err != nil {
			p.log.Error("contest: record announce message", "contest", c.ID, "err", err)
		}
	}
}

func (p *Plugin) announceSubmissionsOpen(ctx context.Context, c Contest) {
	body := p.speak(ctx, c.GuildID, voice.KeyContestSubmissionsOpen, map[string]string{
		"title": c.Title,
		"until": discordTS(c.VoteAt.Unix()),
	}, "submissions are open for "+c.Title+", closing "+discordTS(c.VoteAt.Unix())+".")

	embed := core.NewEmbed(core.ColorSuccess, "submissions open", body,
		&discordgo.MessageEmbedField{Name: "where", Value: core.MentionChannel(c.ForumChannelID)},
		&discordgo.MessageEmbedField{Name: "closes", Value: discordTS(c.VoteAt.Unix()), Inline: true},
		&discordgo.MessageEmbedField{Name: "entries", Value: "one each", Inline: true},
	)
	p.post(ctx, c, embed, nil)
}

func (p *Plugin) announceVotingOpen(ctx context.Context, c Contest) {
	subs, err := p.store.Submissions(ctx, c.ID)
	if err != nil {
		p.log.Error("contest: count entries", "contest", c.ID, "err", err)
	}
	body := p.speak(ctx, c.GuildID, voice.KeyContestVotingOpen, map[string]string{
		"title": c.Title,
		"count": strconv.Itoa(len(subs)),
		"until": discordTS(c.ResultsAt.Unix()),
	}, "voting is open on "+c.Title+", "+strconv.Itoa(len(subs))+" entries, closing "+discordTS(c.ResultsAt.Unix())+".")

	fields := []*discordgo.MessageEmbedField{
		{Name: "entries", Value: strconv.Itoa(len(subs)), Inline: true},
		{Name: "picks each", Value: strconv.Itoa(c.MaxVotes), Inline: true},
		{Name: "closes", Value: discordTS(c.ResultsAt.Unix()), Inline: true},
	}
	var components []discordgo.MessageComponent
	if p.worker.Configured() {
		components = linkRow(c.Slug, "vote")
	} else {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:  "heads up",
			Value: "no voting page is set up on this deployment, so nothing can be voted on.",
		})
	}
	p.post(ctx, c, core.NewEmbed(core.ColorPrimary, "voting open", body, fields...), components)
}

// announceNoVotes closes a contest that drew entries but no votes.
//
// The body is code-authored rather than a voice line, for the same reason
// the funding address is: this is the sentence explaining why nobody won and
// why no prize went out, and voice.Line picks at random and falls back
// silently, which is right for a greeting and wrong for this.
func (p *Plugin) announceNoVotes(ctx context.Context, c Contest) {
	body := "voting on " + c.Title + " closed with no votes cast, so there's no winner to call and no prizes were handed out. every entry is still up."
	var fields []*discordgo.MessageEmbedField
	if p.worker.Configured() {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name: "the entries", Value: p.worker.PageURL(c.Slug),
		})
	}
	p.post(ctx, c, core.NewEmbed(core.ColorWarning, c.Title, body, fields...), nil)
}

func (p *Plugin) announceNoEntries(ctx context.Context, c Contest) {
	body := p.speak(ctx, c.GuildID, voice.KeyContestNoEntries,
		map[string]string{"title": c.Title},
		"nobody entered "+c.Title+". that happens.")
	p.post(ctx, c, core.NewEmbed(core.ColorWarning, c.Title, body), nil)
}

// announceWinners posts the podium. The numbers come from the tally merlin
// already stored, never from a fresh count: a results post and the page have
// to agree, and the way to guarantee that is for there to be one count.
func (p *Plugin) announceWinners(ctx context.Context, c Contest, subs []Submission, results []resultView) {
	byID := make(map[string]Submission, len(subs))
	for _, s := range subs {
		byID[s.ID] = s
	}

	var winner Submission
	var winnerVotes int
	if len(results) > 0 {
		winner = byID[results[0].ID]
		winnerVotes = results[0].Votes
	}

	body := p.speak(ctx, c.GuildID, voice.KeyContestWinner, map[string]string{
		"winner": winner.Author,
		"votes":  strconv.Itoa(winnerVotes),
	}, winner.Author+" takes it with "+strconv.Itoa(winnerVotes)+" votes.")

	var podium strings.Builder
	for i, r := range results {
		if i >= 5 {
			break
		}
		s := byID[r.ID]
		fmt.Fprintf(&podium, "%s. %s, %s (%d)\n",
			strconv.Itoa(r.Rank), s.Author, entryTitle(s), r.Votes)
	}

	fields := []*discordgo.MessageEmbedField{
		{Name: "standings", Value: core.TruncateEmbedField(podium.String())},
	}
	if p.worker.Configured() {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name: "the whole thing", Value: p.worker.PageURL(c.Slug),
		})
	}
	p.post(ctx, c, core.NewLandmarkEmbed(core.ColorSuccess, c.Title+": winners", body, fields...), nil)

	// Pin the winning post so the forum keeps a marker of who won without
	// anybody having to scroll. Non-fatal: the results are already
	// announced, and a failed pin is cosmetic.
	if winner.ThreadID != "" {
		if err := p.opsFor(c.GuildID).ChannelMessagePin(winner.ThreadID, winner.ThreadID); err != nil {
			p.log.Error("contest: pin winner", "thread", winner.ThreadID, "err", err)
		}
	}
}

func entryTitle(s Submission) string {
	if s.Title != "" {
		return s.Title
	}
	return "untitled"
}

// awardPrizes pairs pledges with winners in rank order and delivers them.
//
// The ordering here is the same argument roles.applyJail makes: the pairing
// is recorded before the code is wiped, and the code is wiped only after the
// DM carrying it has actually landed. A DM that fails leaves the ciphertext
// in place and the pairing on record, which is exactly the state
// /contest claim needs to finish the job.
func (p *Plugin) awardPrizes(ctx context.Context, c Contest, subs []Submission, results []resultView) {
	prizes, err := p.store.Prizes(ctx, c.ID)
	if err != nil {
		p.log.Error("contest: read prizes", "contest", c.ID, "err", err)
		return
	}
	if len(prizes) == 0 {
		return
	}

	byID := make(map[string]Submission, len(subs))
	for _, s := range subs {
		byID[s.ID] = s
	}

	var undelivered []string
	var spare int
	for i, pr := range prizes {
		if pr.AwardedAt != nil {
			continue
		}
		if i >= len(results) {
			// More pledges than entries. Left unawarded and said out loud
			// rather than piled onto the winner, because who gets a spare
			// prize is the donor's call and not merlin's.
			spare++
			continue
		}
		winner := byID[results[i].ID]
		if winner.UserID == "" {
			spare++
			continue
		}
		if err := p.store.MarkPrizeAwarded(ctx, pr.ID, winner.UserID, p.now()); err != nil {
			p.log.Error("contest: mark prize awarded", "prize", pr.ID, "err", err)
			continue
		}
		if err := p.deliverPrize(ctx, c, pr, winner.UserID); err != nil {
			p.log.Error("contest: deliver prize", "prize", pr.ID, "err", err)
			undelivered = append(undelivered, core.MentionUser(winner.UserID))
			continue
		}
		if pr.HasSecret() {
			if err := p.store.ClearPrizeSecret(ctx, pr.ID); err != nil {
				p.log.Error("contest: clear prize secret", "prize", pr.ID, "err", err)
			}
		}
		if err := p.audit.Record(ctx, c.GuildID, core.ActorSystem, "contest.prize_awarded",
			"", pr.Title+" to "+core.MentionUser(winner.UserID)); err != nil {
			p.log.Error("contest: audit prize award", "prize", pr.ID, "err", err)
		}
	}

	var notes []string
	if len(undelivered) > 0 {
		notes = append(notes, "couldn't dm "+strings.Join(undelivered, " ")+", run `/contest claim` to pick it up")
	}
	if spare > 0 {
		notes = append(notes, strconv.Itoa(spare)+" prizes had nobody to go to, the people who pledged them can sort that out")
	}
	if len(notes) > 0 {
		p.post(ctx, c, core.NewEmbed(core.ColorWarning, "prizes", strings.Join(notes, "\n")), nil)
	}
}

// deliverPrize DMs one prize to its winner. The sealed code, if there is
// one, is opened here and nowhere else, exists for the length of this
// function, and goes into a direct message and into no log line.
func (p *Plugin) deliverPrize(ctx context.Context, c Contest, pr Prize, winnerID string) error {
	ops := p.opsFor(c.GuildID)
	dm, err := ops.UserChannelCreate(winnerID)
	if err != nil {
		return fmt.Errorf("contest: open dm: %w", err)
	}

	fields := []*discordgo.MessageEmbedField{
		{Name: "prize", Value: core.TruncateEmbedField(pr.Title)},
		{Name: "from", Value: core.MentionUser(pr.DonorID), Inline: true},
	}
	if pr.Details != "" {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name: "details", Value: core.TruncateEmbedField(pr.Details),
		})
	}
	if pr.HasSecret() {
		code, err := p.sealer.Open(pr.SecretSealed)
		if err != nil {
			return fmt.Errorf("contest: open prize secret: %w", err)
		}
		fields = append(fields, &discordgo.MessageEmbedField{
			Name: "your code", Value: "```" + core.TruncateEmbedField(code) + "```",
		})
	} else {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:  "how you get it",
			Value: "merlin holds nothing here, so talk to " + core.MentionUser(pr.DonorID) + " directly.",
		})
	}

	embed := core.NewEmbed(core.ColorSuccess, "you won "+c.Title,
		"nice one. here is what you won.", fields...)
	_, err = ops.ChannelMessageSendComplex(dm.ID, &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{embed},
		Files:  core.EmbedFiles(embed),
	})
	if err != nil {
		return fmt.Errorf("contest: send prize dm: %w", err)
	}
	return nil
}
